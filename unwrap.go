package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/efficientgo/tools/extkingpin"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/oklog/ulid"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/relabel"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	tsdb_errors "github.com/prometheus/prometheus/tsdb/errors"
	"github.com/thanos-io/objstore"
	"github.com/thanos-io/objstore/client"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"github.com/thanos-io/thanos/pkg/model"
	"github.com/thanos-io/thanos/pkg/runutil"
	"gopkg.in/yaml.v2"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const metaExtLabels = "__meta_ext_labels"

func unwrap(bkt objstore.Bucket, unwrapRelabel extkingpin.PathOrContent, unwrapMetaRelabel extkingpin.PathOrContent, recursive bool, dir *string, wait *time.Duration, unwrapDry bool, outConfig *extkingpin.PathOrContent, maxTime *model.TimeOrDurationValue, unwrapSrc *string, maxOpen int, stream bool, logger log.Logger) (err error) {
	relabelContentYaml, err := unwrapRelabel.Content()
	if err != nil {
		return fmt.Errorf("get content of relabel configuration: %w", err)
	}
	var relabelConfig []*relabel.Config
	if err := yaml.Unmarshal(relabelContentYaml, &relabelConfig); err != nil {
		return fmt.Errorf("parse relabel configuration: %w", err)
	}
	metaRelabelContentYaml, err := unwrapMetaRelabel.Content()
	if err != nil {
		return fmt.Errorf("get content of meta-relabel configuration: %w", err)
	}
	var metaRelabel []*relabel.Config
	if err := yaml.Unmarshal(metaRelabelContentYaml, &metaRelabel); err != nil {
		return fmt.Errorf("parse relabel configuration: %w", err)
	}

	objStoreYaml, err := outConfig.Content()
	if err != nil {
		return err
	}
	dst, err := client.NewBucket(logger, objStoreYaml, "thanos-kit")
	if err != nil {
		return err
	}

	processBucket := func() error {
		begin := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		blocks, err := getBlocks(ctx, bkt, recursive, maxTime)
		if err != nil {
			return err
		}
		for _, b := range blocks {
			if *unwrapSrc != "" {
				m, err := getMeta(ctx, b, bkt, logger)
				if bkt.IsObjNotFoundErr(err) {
					continue // Meta.json was deleted between bkt.Exists and here.
				}
				if err != nil {
					return err
				}
				if string(m.Thanos.Source) != *unwrapSrc {
					continue
				}
			}
			if err := unwrapBlock(bkt, b, relabelConfig, metaRelabel, *dir, unwrapDry, dst, maxOpen, stream, logger); err != nil {
				return err
			}
		}
		level.Info(logger).Log("msg", "bucket iteration done", "blocks", len(blocks), "duration", time.Since(begin), "sleeping", wait)
		return nil
	}

	if *wait == 0 {
		return processBucket()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return runutil.Repeat(*wait, ctx.Done(), func() error {
		return processBucket()
	})
}

func unwrapBlock(bkt objstore.Bucket, b Block, relabelConfig []*relabel.Config, metaRelabel []*relabel.Config, dir string, unwrapDry bool, dst objstore.Bucket, maxOpen int, stream bool, logger log.Logger) (err error) {
	if err := runutil.DeleteAll(dir); err != nil {
		return fmt.Errorf("unable to cleanup cache folder %s: %w", dir, err)
	}
	inDir := path.Join(dir, "in")
	outDir := path.Join(dir, "out")

	// prepare input
	ctxd, canceld := context.WithTimeout(context.Background(), 10*time.Minute)
	defer canceld()
	pb := objstore.NewPrefixedBucket(bkt, b.Prefix)
	if err := downloadBlock(ctxd, inDir, b.Id.String(), pb, logger); err != nil {
		return err
	}
	os.Mkdir(path.Join(inDir, "wal"), 0777)
	origMeta, err := metadata.ReadFromDir(path.Join(inDir, b.Id.String()))
	if err != nil {
		return fmt.Errorf("fail to read meta.json for %s: %w", b.Id.String(), err)
	}
	lbls, keep := relabel.Process(labels.FromMap(origMeta.Thanos.Labels), metaRelabel...)
	if !keep {
		return nil
	}
	origMeta.Thanos.Labels = lbls.Map()

	// split into one block per ext-label tenant (writes their Thanos meta.json too)
	os.Mkdir(outDir, 0777)
	// The huge block duration is load-bearing: series are appended unsorted (Select
	// yields them in label order, not time order) and across periodic commits, so the
	// chunk range must dwarf the source span to avoid out-of-bounds/too-old rejects.
	duration := getCompatibleBlockDuration(math.MaxInt64)
	var blocks []ulid.ULID
	if stream {
		// Experimental: copy chunks straight from the source block (no in-memory
		// Head), so peak memory is O(largest single series) instead of O(largest
		// tenant). Reuses LeveledCompactor via a per-tenant block view.
		blk, berr := tsdb.OpenBlock(logger, path.Join(inDir, b.Id.String()), chunkenc.NewPool())
		if berr != nil {
			return berr
		}
		blocks, err = streamSplitBlock(context.Background(), blk, relabelConfig, outDir, *origMeta, duration, logger)
		if cerr := blk.Close(); err == nil {
			err = cerr
		}
		if errors.Is(err, errStreamDuplicateSeries) {
			// Series that are identical after stripping need their samples merged,
			// which only the Head path does. streamSplitBlock fails before writing
			// any tenant, so outDir is still empty.
			level.Warn(logger).Log("msg", "--stream cannot write this block, falling back to the Head path", "block", b.Id, "err", err)
			stream = false
		} else if err != nil {
			return err
		}
	}
	if !stream {
		db, derr := tsdb.OpenDBReadOnly(inDir, logger)
		if derr != nil {
			return derr
		}
		defer func() {
			err = tsdb_errors.NewMulti(err, db.Close()).Err()
		}()
		q, qerr := db.Querier(context.Background(), 0, math.MaxInt64)
		if qerr != nil {
			return qerr
		}
		defer q.Close()
		blocks, err = splitBlock(context.Background(), q, relabelConfig, outDir, *origMeta, duration, maxOpen, logger)
		if err != nil {
			return err
		}
	}

	if unwrapDry {
		level.Info(logger).Log("msg", "dry-run: skipping upload of created blocks and delete of original block", "ulids", fmt.Sprint(blocks), "orig", b.Id)
		return nil
	}
	for _, id := range blocks {
		begin := time.Now()
		ctxu, cancelu := context.WithTimeout(context.Background(), 10*time.Minute)
		err = block.Upload(ctxu, logger, dst, filepath.Join(outDir, id.String()), metadata.SHA256Func)
		cancelu()
		if err != nil {
			return fmt.Errorf("upload block %s: %v", id, err)
		}
		level.Info(logger).Log("msg", "uploaded block", "ulid", id, "duration", time.Since(begin))
	}
	// Always delete the original after a successful pass, even when it produced zero
	// blocks (all series dropped by relabel), so wait-interval mode does not loop on it.
	level.Info(logger).Log("msg", "deleting original block", "ulid", b.Id)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := block.Delete(ctx, logger, pb, b.Id); err != nil {
		return fmt.Errorf("delete block %s%s: %v", b.Prefix, b.Id, err)
	}
	return nil
}

// extractLabels splits given labels to two sets: `el` for the labels named in
// `names` (the ext-labels) and `res` for the rest (dropping non-__name__ __
// labels), preserving sort order. `names` must already be sorted (callers
// memoize and sort it). Both results are freshly allocated (the Head retains
// `res` by reference, so buffers must not be shared across calls).
func extractLabels(ls labels.Labels, names []string) (res labels.Labels, el labels.Labels) {
	res = make(labels.Labels, 0, len(ls))
	el = make(labels.Labels, 0, len(names))
	i, j := 0, 0
	for i < len(ls) && j < len(names) {
		switch {
		// https://prometheus.io/docs/prometheus/latest/configuration/configuration/#relabel_config
		// Labels starting with __ will be removed from the label set after target relabeling is completed.
		case strings.HasPrefix(ls[i].Name, "__") && ls[i].Name != "__name__":
			i++
		case names[j] < ls[i].Name:
			j++
		case ls[i].Name < names[j]:
			res = append(res, ls[i])
			i++
		default:
			el = append(el, ls[i])
			i++
			j++
		}
	}
	res = append(res, ls[i:]...)
	return res, el
}

// blockTenant is one open output block writer while its batch is being filled.
type blockTenant struct {
	writer  *tsdb.BlockWriter
	app     storage.Appender
	ext     labels.Labels // this tenant's ext-labels, written into meta.json
	samples int           // uncommitted samples in the current appender
}

// splitBlock reads every series from q and writes one output TSDB block per
// distinct ext-label combination ("tenant"), as defined by the labels named in
// __meta_ext_labels (set by relabelConfig). Each block gets its own Thanos
// meta.json carrying origMeta's labels overlaid with that tenant's ext-labels.
//
// Tenants are processed in batches of maxOpen (<=0 means all tenants in a single
// pass): a batch keeps at most maxOpen Heads open at once, so peak memory is
// bounded by the largest batch, not by the total tenant count. Each tenant is
// opened, filled in one full pass over the source, and flushed exactly once, so
// it yields exactly one non-overlapping block (no LRU eviction, no thrashing).
func splitBlock(ctx context.Context, q storage.Querier, relabelConfig []*relabel.Config, outDir string, origMeta metadata.Meta, blockSize int64, maxOpen int, logger log.Logger) (ids []ulid.ULID, err error) {
	matchAll := &labels.Matcher{Name: "__name__", Type: labels.MatchNotEqual, Value: ""}

	// splitNames memoizes the sorted ext-label name list per distinct
	// __meta_ext_labels value (constant in the common case), so we split+sort once
	// per value instead of once per series on every pass.
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

	// route maps a source series to its tenant key, the tenant's ext-labels, and
	// the labels to keep on the output series. Pure function of the (immutable,
	// read-only) source series, so discovery and batch passes agree.
	route := func(s storage.Series) (key string, ext labels.Labels, out labels.Labels, keep bool) {
		rl, k := relabel.Process(s.Labels(), relabelConfig...)
		if !k {
			return "", nil, nil, false
		}
		out, ext = extractLabels(rl, splitNames(rl.Get(metaExtLabels)))
		return ext.String(), ext, out, true
	}

	// Discovery pass: collect the distinct tenants in a stable order. Reads labels
	// only (never iterates samples), so it is an index scan with no chunk decode.
	tenants := map[string]labels.Labels{}
	var order []string
	ss := q.Select(false, nil, matchAll)
	for ss.Next() {
		key, ext, _, keep := route(ss.At())
		if !keep {
			continue
		}
		if _, ok := tenants[key]; !ok {
			tenants[key] = ext
			order = append(order, key)
		}
	}
	if err := ss.Err(); err != nil {
		return nil, err
	}
	if ws := ss.Warnings(); len(ws) > 0 {
		return nil, tsdb_errors.NewMulti(ws...).Err()
	}

	batch := maxOpen
	if batch <= 0 || batch > len(order) {
		batch = len(order)
	}

	extOf := map[ulid.ULID]labels.Labels{}
	for start := 0; start < len(order); start += batch {
		end := start + batch
		if end > len(order) {
			end = len(order)
		}
		// Open one block writer per tenant in this batch.
		writers := make(map[string]*blockTenant, end-start)
		for _, key := range order[start:end] {
			w, werr := tsdb.NewBlockWriter(logger, outDir, blockSize)
			if werr != nil {
				return nil, werr
			}
			writers[key] = &blockTenant{writer: w, app: w.Appender(ctx), ext: tenants[key]}
		}

		// One pass over the source; append each batch tenant's series to its head.
		bss := q.Select(false, nil, matchAll)
		for bss.Next() {
			s := bss.At()
			key, _, out, keep := route(s)
			if !keep {
				continue
			}
			bt, ok := writers[key]
			if !ok {
				// Not in this batch: it must be a tenant discovered earlier (it will
				// be processed in another batch). If it is unknown, discovery and this
				// pass disagree (non-deterministic relabel) and we would silently drop
				// data - fail loudly instead.
				if _, known := tenants[key]; !known {
					return nil, fmt.Errorf("series routed to undiscovered tenant %q: relabel is non-deterministic", key)
				}
				continue
			}
			it := s.Iterator(nil)
			// Single drain loop: chunkenc iterators return samples in timestamp order,
			// NOT grouped by value type, so per-type loops would drop samples at type
			// transitions (e.g. float -> native histogram on the same series).
			for vt := it.Next(); vt != chunkenc.ValNone; vt = it.Next() {
				switch vt {
				case chunkenc.ValFloat:
					ts, v := it.At()
					if _, err := bt.app.Append(0, out, ts, v); err != nil {
						return nil, err
					}
				case chunkenc.ValHistogram:
					ts, h := it.AtHistogram()
					if _, err := bt.app.AppendHistogram(0, out, ts, h, nil); err != nil {
						return nil, err
					}
				case chunkenc.ValFloatHistogram:
					ts, fh := it.AtFloatHistogram()
					if _, err := bt.app.AppendHistogram(0, out, ts, nil, fh); err != nil {
						return nil, err
					}
				}
				bt.samples++
			}
			if err := it.Err(); err != nil {
				return nil, err
			}
			// Bound only the uncommitted appender staging buffer (NOT Head memory,
			// which is bounded by the tenant and freed at Flush). Per-tenant counter.
			if bt.samples > 5000 {
				if err := bt.app.Commit(); err != nil {
					return nil, err
				}
				bt.app = bt.writer.Appender(ctx)
				bt.samples = 0
			}
		}
		if err := bss.Err(); err != nil {
			return nil, err
		}
		if ws := bss.Warnings(); len(ws) > 0 {
			return nil, tsdb_errors.NewMulti(ws...).Err()
		}

		// Flush every tenant in this batch: exactly one block per tenant.
		for _, key := range order[start:end] {
			bt := writers[key]
			if err := bt.app.Commit(); err != nil {
				return nil, err
			}
			id, ferr := bt.writer.Flush(ctx)
			// An empty head yields (zero ULID, nil) in this prometheus version (older
			// versions return ErrNoSeriesAppended); either way no block was written.
			if ferr != nil && !errors.Is(ferr, tsdb.ErrNoSeriesAppended) {
				return nil, ferr
			}
			if cerr := bt.writer.Close(); cerr != nil {
				return nil, cerr
			}
			if id == (ulid.ULID{}) {
				continue
			}
			extOf[id] = bt.ext
			ids = append(ids, id)
		}
	}

	// Write a per-block Thanos meta.json. Clone origMeta's labels into a fresh map
	// per block and overlay this tenant's ext-labels, so labels never leak across
	// blocks. Resolution is preserved from origMeta.
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
