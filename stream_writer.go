package main

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/oklog/ulid"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/relabel"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/index"
	"github.com/prometheus/prometheus/tsdb/tombstones"
	"github.com/thanos-io/thanos/pkg/block/metadata"
)

// tenantBlockReader presents one source block as if it held only a single
// tenant's series, with that tenant's ext-labels stripped. Feeding it to
// LeveledCompactor.Write produces the tenant's output block while the compactor
// streams one series' chunks at a time and copies chunks by reference - so peak
// memory is O(largest single series + the tenant's series refs), not O(tenant).
// Chunk refs, postings, meta.json, tombstone application and the atomic write are
// all done by the (battle-tested) compactor; we filter, relabel, and prune the
// symbol table to this tenant's referenced strings (see tenantIndexReader.Symbols).
type tenantBlockReader struct {
	src     *tsdb.Block
	refs    []storage.SeriesRef                       // this tenant's series, sorted by output labels
	keep    func(labels.Labels) (labels.Labels, bool) // relabel + ext-strip, applied per series
	symbols []string                                  // sorted, deduped strings referenced by this tenant's output series
}

func (r *tenantBlockReader) Index() (tsdb.IndexReader, error) {
	ir, err := r.src.Index()
	if err != nil {
		return nil, err
	}
	return &tenantIndexReader{IndexReader: ir, refs: r.refs, keep: r.keep, symbols: r.symbols}, nil
}
func (r *tenantBlockReader) Chunks() (tsdb.ChunkReader, error)      { return r.src.Chunks() }
func (r *tenantBlockReader) Tombstones() (tombstones.Reader, error) { return r.src.Tombstones() }
func (r *tenantBlockReader) Meta() tsdb.BlockMeta                   { return r.src.Meta() }
func (r *tenantBlockReader) Size() int64                            { return r.src.Size() }

type tenantIndexReader struct {
	tsdb.IndexReader
	refs    []storage.SeriesRef
	keep    func(labels.Labels) (labels.Labels, bool)
	symbols []string
}

// Symbols returns only the strings referenced by this tenant's output series,
// overriding the embedded reader's whole-block symbol table. Without it the
// compactor writes the union of the input block's Symbols() into every tenant
// block (populateBlock adds them verbatim, it does not prune to the labels it
// actually writes), so each output block would inherit all other tenants' label
// values, the ext-label values and dropped __ labels - and thanos-compact never
// cleans them up (it unions input symbols too), baking the bloat into long-lived
// compacted blocks. This makes --stream match what the Head path emits.
func (t *tenantIndexReader) Symbols() index.StringIter { return index.NewStringListIter(t.symbols) }

// Postings ignores the matcher and returns this tenant's series; on a single
// block the compactor only asks for AllPostingsKey.
func (t *tenantIndexReader) Postings(name string, values ...string) (index.Postings, error) {
	return index.NewListPostings(t.refs), nil
}

// SortedPostings is identity: streamSplitBlock already put refs in output label
// order. That is usually the source order, but not always - stripping labels can
// move a series (see streamSplitBlock), and the compactor's AddSeries fails on any
// series not strictly greater than the previous one.
func (t *tenantIndexReader) SortedPostings(p index.Postings) index.Postings { return p }

// Series returns the source series with its ext-labels stripped. The chunk metas
// are the source ones (with source refs); the compactor copies the chunks by
// reference and remaps the refs.
func (t *tenantIndexReader) Series(ref storage.SeriesRef, builder *labels.ScratchBuilder, chks *[]chunks.Meta) error {
	if err := t.IndexReader.Series(ref, builder, chks); err != nil {
		return err
	}
	out, ok := t.keep(builder.Labels())
	if !ok {
		return fmt.Errorf("series %d in tenant postings dropped by relabel: relabel is non-deterministic", ref)
	}
	builder.Reset()
	for _, l := range out {
		builder.Add(l.Name, l.Value)
	}
	return nil
}

// streamSplitBlock is the no-Head equivalent of splitBlock: it writes one output
// block per ext-label tenant by streaming chunks straight from src (see
// tenantBlockReader), with the same ext-label split and per-block Thanos meta.
// Peak memory is bounded by the largest single series, not the largest tenant,
// except while a disordered tenant is sorted (see below).
//
// Output order: refs start in source order, which matches the output order only
// while stripping labels does not move a series. It does move one when a stripped
// label decides the source order - a "__*" label (it sorts before lowercase names,
// so {__service="x", department=".."} precedes {arch=".."} and follows it once
// stripped), an ext-label that is the last label of one series but not of a
// similar one ({a="1", prometheus="A"} vs {a="1", b="2", prometheus="A"}), or a
// relabel that rewrites kept labels. Discovery detects that per tenant with one
// compare per series, and only such tenants are sorted (see sortTenant). Series
// before the first misplaced one and after the last stay where they are; the range
// between them is re-read and sorted by symbol-index keys, about 8 bytes per output
// label plus 16 per series of that range, the whole tenant at worst. Series that become
// identical after stripping cannot be written from one reader (their samples need
// merging), so the block fails with errStreamDuplicateSeries before any tenant is
// written.
//
// Tombstoned sources are not handled differently from splitBlock (the compactor
// applies the source tombstones via tenantBlockReader.Tombstones).
func streamSplitBlock(ctx context.Context, src *tsdb.Block, relabelConfig []*relabel.Config, outDir string, origMeta metadata.Meta, blockSize int64, logger log.Logger) (ids []ulid.ULID, err error) {
	ir, err := src.Index()
	if err != nil {
		return nil, err
	}
	defer ir.Close()

	splitCache := map[string][]string{}
	splitNames := func(raw string) []string {
		if v, ok := splitCache[raw]; ok {
			return v
		}
		v := strings.Split(raw, ";")
		slices.Sort(v)
		splitCache[raw] = v
		return v
	}
	route := func(s labels.Labels) (key string, ext labels.Labels, out labels.Labels, keep bool) {
		rl, k := relabel.Process(s, relabelConfig...)
		if !k {
			return "", nil, nil, false
		}
		out, ext = extractLabels(rl, splitNames(rl.Get(metaExtLabels)))
		return ext.String(), ext, out, true
	}

	// Discovery: collect each tenant's source refs (in sorted postings order) and
	// its ext-labels. Reads labels only; chunk bytes are not touched here.
	k, v := index.AllPostingsKey()
	all, err := ir.Postings(k, v)
	if err != nil {
		return nil, err
	}
	all = ir.SortedPostings(all)
	type tenant struct {
		refs     []storage.SeriesRef
		ext      labels.Labels
		syms     map[string]uint32 // distinct strings referenced by this tenant's output labels; the value is the index in symbols, set before the sort
		symbols  []string          // syms, sorted
		counts   labelCounts       // output labels per series, to size the sort
		last     labels.Labels     // output labels of the previous series
		disorder disorder
	}
	tenants := map[string]*tenant{}
	var order []string
	var builder labels.ScratchBuilder
	var chks []chunks.Meta
	for all.Next() {
		if err := ir.Series(all.At(), &builder, &chks); err != nil {
			return nil, err
		}
		key, ext, out, keep := route(builder.Labels())
		if !keep {
			continue
		}
		t := tenants[key]
		if t == nil {
			t = &tenant{ext: ext, syms: map[string]uint32{}}
			tenants[key] = t
			order = append(order, key)
		}
		if len(t.refs) > 0 {
			switch c := labels.Compare(out, t.last); {
			case c == 0:
				return nil, fmt.Errorf("tenant %s: %w: %s", key, errStreamDuplicateSeries, out)
			case c < 0:
				t.disorder.add(t.last, out)
			}
		}
		t.refs = append(t.refs, all.At())
		t.counts.add(len(out))
		t.last = out
		// Collect the symbols this tenant's output block will reference, so its
		// Symbols() can be pruned to them (source strings are stable while src is open).
		for _, l := range out {
			t.syms[l.Name] = 0
			t.syms[l.Value] = 0
		}
	}
	if err := all.Err(); err != nil {
		return nil, err
	}

	// keep is tenant-independent (it only strips ext-labels); the refs decide which
	// series each output block contains. By now splitCache holds every distinct
	// __meta_ext_labels value, so keep only reads it.
	keep := func(s labels.Labels) (labels.Labels, bool) {
		_, _, out, ok := route(s)
		return out, ok
	}
	// Sort (and check for duplicates) before writing anything, so a block that
	// fails here leaves outDir empty for the caller to retry another way.
	for _, key := range order {
		t := tenants[key]
		t.last = nil
		t.symbols = make([]string, 0, len(t.syms))
		for s := range t.syms {
			t.symbols = append(t.symbols, s)
		}
		slices.Sort(t.symbols)
		if t.disorder.minStart != nil {
			for i, s := range t.symbols {
				t.syms[s] = uint32(i) // overwrites existing keys, no map growth
			}
			begin := time.Now()
			lo, hi, err := sortTenant(ir, t.refs, t.disorder, keep, t.symbols, t.syms, t.counts)
			if err != nil {
				return nil, fmt.Errorf("tenant %s: %w", key, err)
			}
			level.Info(logger).Log("msg", "stripped labels reorder series, sorted tenant", "tenant", key, "series", len(t.refs), "sorted", hi-lo, "duration", time.Since(begin))
		}
		t.disorder = disorder{}
		t.syms = nil
	}
	compactor, err := tsdb.NewLeveledCompactor(ctx, nil, logger, []int64{blockSize}, chunkenc.NewPool(), nil)
	if err != nil {
		return nil, err
	}
	srcMeta := src.Meta()
	extOf := map[ulid.ULID]labels.Labels{}
	for _, key := range order {
		t := tenants[key]
		reader := &tenantBlockReader{src: src, refs: t.refs, keep: keep, symbols: t.symbols}
		// srcMeta.MaxTime is already an exclusive bound ([MinTime, MaxTime)) - passing
		// MaxTime+1 here would spill the output meta 1ms past the source block and make
		// contiguous outputs overlap (endless vertical compaction downstream).
		id, werr := compactor.Write(outDir, reader, srcMeta.MinTime, srcMeta.MaxTime, nil)
		if werr != nil {
			return nil, fmt.Errorf("write tenant %s: %w", key, werr)
		}
		if id == (ulid.ULID{}) { // empty block (e.g. all samples tombstoned)
			continue
		}
		extOf[id] = t.ext
		ids = append(ids, id)
	}

	// Per-block Thanos meta.json (R3): clone origMeta labels + overlay tenant ext labels.
	for _, id := range ids {
		meta, rerr := metadata.ReadFromDir(path.Join(outDir, id.String()))
		if rerr != nil {
			return nil, fmt.Errorf("read %s metadata: %w", id, rerr)
		}
		l := make(map[string]string, len(origMeta.Thanos.Labels)+len(extOf[id]))
		for k, v := range origMeta.Thanos.Labels {
			l[k] = v
		}
		for _, e := range extOf[id] {
			l[e.Name] = e.Value
		}
		if werr := writeThanosMeta(meta.BlockMeta, l, origMeta.Thanos.Downsample.Resolution, outDir, logger); werr != nil {
			return nil, fmt.Errorf("write %s metadata: %w", id, werr)
		}
	}
	return ids, nil
}

// errStreamDuplicateSeries means two source series of one tenant differ only in
// stripped labels. Their output series are identical and their samples need
// merging, which LeveledCompactor.Write cannot do from a single reader.
var errStreamDuplicateSeries = errors.New("series differ only in stripped labels")
