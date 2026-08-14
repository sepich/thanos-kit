package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-kit/log"
	"github.com/oklog/ulid"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/relabel"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/tsdbutil"
	"github.com/thanos-io/objstore"
	"github.com/thanos-io/objstore/client"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"gopkg.in/yaml.v2"
)

const testResolution = int64(300000)

var matchAllTest = &labels.Matcher{Name: "__name__", Type: labels.MatchNotEqual, Value: ""}

// buildSourceBlock writes a source TSDB block to dir with heterogeneous tenants
// (mixed prometheus-only / location-only / both), float + native-histogram +
// float-histogram + a float->histogram transition series, a >5000-sample series,
// and one series dropped by relabel. Series share metric names across tenants so
// the label-sorted Select stream is metric-major and interleaves tenants. Returns
// the produced block ULID.
func buildSourceBlock(t *testing.T, dir string) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	bw, err := tsdb.NewBlockWriter(log.NewNopLogger(), dir, getCompatibleBlockDuration(math.MaxInt64))
	if err != nil {
		t.Fatalf("new block writer: %v", err)
	}
	app := bw.Appender(ctx)

	addFloats := func(lbls labels.Labels, n int) {
		for ts := int64(0); ts < int64(n); ts++ {
			if _, err := app.Append(0, lbls, ts, float64(ts)); err != nil {
				t.Fatalf("append float %v: %v", lbls, err)
			}
		}
	}
	addHist := func(lbls labels.Labels, n int) {
		for ts, h := range tsdbutil.GenerateTestHistograms(n) {
			if _, err := app.AppendHistogram(0, lbls, int64(ts), h, nil); err != nil {
				t.Fatalf("append hist %v: %v", lbls, err)
			}
		}
	}
	addFloatHist := func(lbls labels.Labels, n int) {
		for ts, fh := range tsdbutil.GenerateTestFloatHistograms(n) {
			if _, err := app.AppendHistogram(0, lbls, int64(ts), nil, fh); err != nil {
				t.Fatalf("append floathist %v: %v", lbls, err)
			}
		}
	}

	addFloats(labels.FromStrings("__name__", "cpu", "prometheus", "A"), 3)
	addFloats(labels.FromStrings("__name__", "mem", "prometheus", "A"), 3)
	addFloats(labels.FromStrings("__name__", "cpu", "location", "dc2"), 3)
	addFloats(labels.FromStrings("__name__", "mem", "location", "dc2"), 3)
	addHist(labels.FromStrings("__name__", "cpu", "location", "dc2", "prometheus", "C"), 3)
	addFloats(labels.FromStrings("__name__", "disk", "prometheus", "D"), 6000)
	addFloatHist(labels.FromStrings("__name__", "disk", "prometheus", "E"), 3)

	// transition series: floats at ts 0,1 (values 1,2) then native histograms at ts 2,3
	tf := labels.FromStrings("__name__", "disk", "prometheus", "F")
	if _, err := app.Append(0, tf, 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Append(0, tf, 1, 2); err != nil {
		t.Fatal(err)
	}
	for i, h := range tsdbutil.GenerateTestHistograms(2) {
		if _, err := app.AppendHistogram(0, tf, int64(2+i), h, nil); err != nil {
			t.Fatal(err)
		}
	}

	// dropped by relabel (drop="yes")
	addFloats(labels.FromStrings("__name__", "cpu", "prometheus", "A", "drop", "yes"), 3)

	if err := app.Commit(); err != nil {
		t.Fatalf("commit source: %v", err)
	}
	id, err := bw.Flush(ctx)
	if err != nil {
		t.Fatalf("flush source: %v", err)
	}
	if err := bw.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}
	_ = os.MkdirAll(path.Join(dir, "wal"), 0777)
	return id
}

func mustRelabel(t *testing.T, y string) []*relabel.Config {
	t.Helper()
	var cfg []*relabel.Config
	if err := yaml.Unmarshal([]byte(y), &cfg); err != nil {
		t.Fatalf("unmarshal relabel: %v", err)
	}
	return cfg
}

// testRelabel drops drop="yes" series and splits the rest by prometheus;location.
func testRelabel(t *testing.T) []*relabel.Config {
	return mustRelabel(t, "- source_labels: [drop]\n  regex: \"yes\"\n  action: drop\n- target_label: __meta_ext_labels\n  replacement: prometheus;location\n")
}

func openSourceQuerier(t *testing.T, dir string) (storage.Querier, func()) {
	t.Helper()
	db, err := tsdb.OpenDBReadOnly(dir, log.NewNopLogger())
	if err != nil {
		t.Fatalf("open source db: %v", err)
	}
	q, err := db.Querier(context.Background(), 0, math.MaxInt64)
	if err != nil {
		t.Fatalf("source querier: %v", err)
	}
	return q, func() { q.Close(); db.Close() }
}

type tenantSum struct {
	resolution int64
	src        string
	series     map[string]int // output series labels string -> sample count
}

// summarize reads every produced block and groups them by tenant (the Thanos
// ext-labels, i.e. labels minus the source label "src"). A duplicate tenant key
// means a tenant produced more than one block (overlap / thrashing) and fails.
func summarize(t *testing.T, outDir string, ids []ulid.ULID) map[string]*tenantSum {
	t.Helper()
	out := map[string]*tenantSum{}
	for _, id := range ids {
		bdir := path.Join(outDir, id.String())
		meta, err := metadata.ReadFromDir(bdir)
		if err != nil {
			t.Fatalf("read meta %s: %v", id, err)
		}
		key := extKey(meta.Thanos.Labels)
		if _, dup := out[key]; dup {
			t.Errorf("tenant %s produced more than one block (overlap/thrash)", key)
		}
		src := ""
		if v, ok := meta.Thanos.Labels["src"]; ok {
			src = v
		}
		sum := &tenantSum{resolution: meta.Thanos.Downsample.Resolution, src: src, series: map[string]int{}}

		b, err := tsdb.OpenBlock(log.NewNopLogger(), bdir, nil)
		if err != nil {
			t.Fatalf("open block %s: %v", id, err)
		}
		bq, err := tsdb.NewBlockQuerier(b, math.MinInt64, math.MaxInt64)
		if err != nil {
			t.Fatalf("block querier %s: %v", id, err)
		}
		ss := bq.Select(false, nil, matchAllTest)
		for ss.Next() {
			s := ss.At()
			n := 0
			it := s.Iterator(nil)
			for vt := it.Next(); vt != chunkenc.ValNone; vt = it.Next() {
				n++
			}
			if err := it.Err(); err != nil {
				t.Fatalf("iter %s: %v", id, err)
			}
			sum.series[s.Labels().String()] += n
		}
		if err := ss.Err(); err != nil {
			t.Fatalf("select %s: %v", id, err)
		}
		bq.Close()
		b.Close()
		out[key] = sum
	}
	return out
}

// blockSymbols returns a block's on-disk symbol table (already sorted).
func blockSymbols(t *testing.T, bdir string) []string {
	t.Helper()
	b, err := tsdb.OpenBlock(log.NewNopLogger(), bdir, nil)
	if err != nil {
		t.Fatalf("open block %s: %v", bdir, err)
	}
	defer b.Close()
	ir, err := b.Index()
	if err != nil {
		t.Fatalf("index %s: %v", bdir, err)
	}
	defer ir.Close()
	var syms []string
	it := ir.Symbols()
	for it.Next() {
		syms = append(syms, strings.Clone(it.At())) // At() aliases the mmap freed at b.Close()
	}
	if err := it.Err(); err != nil {
		t.Fatalf("symbols %s: %v", bdir, err)
	}
	return syms
}

// referencedStrings returns every label name and value used by the block's series.
func referencedStrings(t *testing.T, bdir string) map[string]struct{} {
	t.Helper()
	b, err := tsdb.OpenBlock(log.NewNopLogger(), bdir, nil)
	if err != nil {
		t.Fatalf("open block %s: %v", bdir, err)
	}
	defer b.Close()
	bq, err := tsdb.NewBlockQuerier(b, math.MinInt64, math.MaxInt64)
	if err != nil {
		t.Fatalf("querier %s: %v", bdir, err)
	}
	defer bq.Close()
	ref := map[string]struct{}{}
	ss := bq.Select(false, nil, matchAllTest)
	for ss.Next() {
		for _, l := range ss.At().Labels() {
			// Labels alias the mmap freed at b.Close(); clone before storing.
			ref[strings.Clone(l.Name)] = struct{}{}
			ref[strings.Clone(l.Value)] = struct{}{}
		}
	}
	if err := ss.Err(); err != nil {
		t.Fatalf("select %s: %v", bdir, err)
	}
	return ref
}

// extKey is the canonical string of a block's ext-labels (thanos labels minus "src").
func extKey(thanos map[string]string) string {
	m := map[string]string{}
	for k, v := range thanos {
		if k == "src" {
			continue
		}
		m[k] = v
	}
	return labels.FromMap(m).String()
}

// drainSeries returns, for the single series matching wantSeries in the block of
// tenant wantExt, the ordered (type,ts,value) signature, for value-level checks.
func drainSeries(t *testing.T, outDir string, ids []ulid.ULID, wantExt, wantSeries string) []string {
	t.Helper()
	for _, id := range ids {
		bdir := path.Join(outDir, id.String())
		meta, err := metadata.ReadFromDir(bdir)
		if err != nil {
			t.Fatalf("read meta %s: %v", id, err)
		}
		if extKey(meta.Thanos.Labels) != wantExt {
			continue
		}
		b, err := tsdb.OpenBlock(log.NewNopLogger(), bdir, nil)
		if err != nil {
			t.Fatalf("open block %s: %v", id, err)
		}
		defer b.Close()
		bq, err := tsdb.NewBlockQuerier(b, math.MinInt64, math.MaxInt64)
		if err != nil {
			t.Fatalf("block querier %s: %v", id, err)
		}
		defer bq.Close()
		ss := bq.Select(false, nil, matchAllTest)
		for ss.Next() {
			s := ss.At()
			if s.Labels().String() != wantSeries {
				continue
			}
			var sig []string
			it := s.Iterator(nil)
			for vt := it.Next(); vt != chunkenc.ValNone; vt = it.Next() {
				switch vt {
				case chunkenc.ValFloat:
					ts, v := it.At()
					sig = append(sig, fmt.Sprintf("f@%d=%g", ts, v))
				case chunkenc.ValHistogram:
					ts, _ := it.AtHistogram()
					sig = append(sig, fmt.Sprintf("h@%d", ts))
				case chunkenc.ValFloatHistogram:
					ts, _ := it.AtFloatHistogram()
					sig = append(sig, fmt.Sprintf("fh@%d", ts))
				}
			}
			if err := it.Err(); err != nil {
				t.Fatalf("iter: %v", err)
			}
			return sig
		}
	}
	t.Fatalf("series %s of tenant %s not found", wantSeries, wantExt)
	return nil
}

func TestSplitBlock(t *testing.T) {
	logger := log.NewNopLogger()
	inDir := path.Join(t.TempDir(), "in")
	buildSourceBlock(t, inDir)
	cfg := testRelabel(t)
	origMeta := metadata.Meta{Thanos: metadata.Thanos{
		Labels:     map[string]string{"src": "orig"},
		Downsample: metadata.ThanosDownsample{Resolution: testResolution},
	}}
	duration := getCompatibleBlockDuration(math.MaxInt64)

	expected := map[string]tenantSum{
		`{prometheus="A"}`:                 {testResolution, "orig", map[string]int{`{__name__="cpu"}`: 3, `{__name__="mem"}`: 3}},
		`{location="dc2"}`:                 {testResolution, "orig", map[string]int{`{__name__="cpu"}`: 3, `{__name__="mem"}`: 3}},
		`{location="dc2", prometheus="C"}`: {testResolution, "orig", map[string]int{`{__name__="cpu"}`: 3}},
		`{prometheus="D"}`:                 {testResolution, "orig", map[string]int{`{__name__="disk"}`: 6000}},
		`{prometheus="E"}`:                 {testResolution, "orig", map[string]int{`{__name__="disk"}`: 3}},
		`{prometheus="F"}`:                 {testResolution, "orig", map[string]int{`{__name__="disk"}`: 4}},
	}

	run := func(maxOpen int) (map[string]*tenantSum, string, []ulid.ULID) {
		outDir := path.Join(t.TempDir(), "out")
		if err := os.MkdirAll(outDir, 0777); err != nil {
			t.Fatal(err)
		}
		q, closeQ := openSourceQuerier(t, inDir)
		defer closeQ()
		ids, err := splitBlock(context.Background(), q, cfg, outDir, origMeta, duration, maxOpen, logger)
		if err != nil {
			t.Fatalf("splitBlock(maxOpen=%d): %v", maxOpen, err)
		}
		if len(ids) != len(expected) {
			t.Fatalf("maxOpen=%d: got %d blocks, want %d", maxOpen, len(ids), len(expected))
		}
		return summarize(t, outDir, ids), outDir, ids
	}

	// maxOpen=1: one tenant per pass -> forces N batch passes (multi-Select reuse)
	// over a metric-major-interleaved stream, the exact pattern the LRU thrashed on.
	got, outDir, ids := run(1)
	total := 0
	for key, exp := range expected {
		g, ok := got[key]
		if !ok {
			t.Errorf("missing tenant block %s", key)
			continue
		}
		if g.src != exp.src {
			t.Errorf("%s: src=%q want %q", key, g.src, exp.src)
		}
		if g.resolution != exp.resolution {
			t.Errorf("%s: resolution=%d want %d", key, g.resolution, exp.resolution)
		}
		if len(g.series) != len(exp.series) {
			t.Errorf("%s: %d output series want %d (%v)", key, len(g.series), len(exp.series), g.series)
		}
		for sl, n := range exp.series {
			if g.series[sl] != n {
				t.Errorf("%s series %s: %d samples want %d", key, sl, g.series[sl], n)
			}
		}
	}
	for _, g := range got {
		for _, n := range g.series {
			total += n
		}
	}
	if total != 6022 {
		t.Errorf("total samples %d want 6022", total)
	}

	// Value-level fidelity (counts alone would miss timestamp/value/type corruption):
	// the 6000-float series must keep exact (ts,value)=(i,i).
	dsig := drainSeries(t, outDir, ids, `{prometheus="D"}`, `{__name__="disk"}`)
	if len(dsig) != 6000 {
		t.Fatalf("D series: %d samples want 6000", len(dsig))
	}
	for i, s := range dsig {
		if want := fmt.Sprintf("f@%d=%d", i, i); s != want {
			t.Fatalf("D sample %d = %q want %q", i, s, want)
			break
		}
	}
	// the transition series must keep its float->histogram type+ts sequence exactly.
	fsig := strings.Join(drainSeries(t, outDir, ids, `{prometheus="F"}`, `{__name__="disk"}`), ",")
	if want := "f@0=1,f@1=2,h@2,h@3"; fsig != want {
		t.Errorf("F transition sequence = %q want %q", fsig, want)
	}

	// Sweep: maxOpen=0 (all tenants in a single pass) must yield identical content.
	got0, _, _ := run(0)
	if !sameSummaries(got, got0) {
		t.Errorf("maxOpen=1 and maxOpen=0 produced different content:\n k=1: %v\n k=0: %v", got, got0)
	}
}

func sameSummaries(a, b map[string]*tenantSum) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || av.resolution != bv.resolution || av.src != bv.src || len(av.series) != len(bv.series) {
			return false
		}
		for sl, n := range av.series {
			if bv.series[sl] != n {
				return false
			}
		}
	}
	return true
}

// assertWithinSource fails for any output block whose meta time range escapes
// the source block's half-open [mint, maxt) range.
func assertWithinSource(t *testing.T, outDir string, ids []ulid.ULID, mint, maxt int64) {
	t.Helper()
	for _, id := range ids {
		meta, err := metadata.ReadFromDir(path.Join(outDir, id.String()))
		if err != nil {
			t.Fatalf("read meta %s: %v", id, err)
		}
		if meta.MinTime < mint || meta.MaxTime > maxt {
			t.Errorf("block %s range [%d, %d) escapes source range [%d, %d)", id, meta.MinTime, meta.MaxTime, mint, maxt)
		}
	}
}

// TestStreamMatchesHead is the differential oracle for the streaming (no-Head)
// writer: on the same source block it must produce per-tenant output identical to
// the Head path - same blocks, series, sample counts and isolated ext-labels.
func TestStreamMatchesHead(t *testing.T) {
	logger := log.NewNopLogger()
	inDir := path.Join(t.TempDir(), "in")
	id := buildSourceBlock(t, inDir)
	cfg := testRelabel(t)
	origMeta := metadata.Meta{Thanos: metadata.Thanos{
		Labels:     map[string]string{"src": "orig"},
		Downsample: metadata.ThanosDownsample{Resolution: testResolution},
	}}
	duration := getCompatibleBlockDuration(math.MaxInt64)

	headOut := path.Join(t.TempDir(), "head")
	if err := os.MkdirAll(headOut, 0777); err != nil {
		t.Fatal(err)
	}
	q, closeQ := openSourceQuerier(t, inDir)
	headIDs, err := splitBlock(context.Background(), q, cfg, headOut, origMeta, duration, 1, logger)
	closeQ()
	if err != nil {
		t.Fatalf("splitBlock: %v", err)
	}

	streamOut := path.Join(t.TempDir(), "stream")
	if err := os.MkdirAll(streamOut, 0777); err != nil {
		t.Fatal(err)
	}
	blk, err := tsdb.OpenBlock(logger, path.Join(inDir, id.String()), chunkenc.NewPool())
	if err != nil {
		t.Fatalf("open block: %v", err)
	}
	srcBM := blk.Meta()
	streamIDs, err := streamSplitBlock(context.Background(), blk, cfg, streamOut, origMeta, duration, logger)
	blk.Close()
	if err != nil {
		t.Fatalf("streamSplitBlock: %v", err)
	}

	if len(streamIDs) != len(headIDs) {
		t.Fatalf("stream produced %d blocks, head %d", len(streamIDs), len(headIDs))
	}
	head := summarize(t, headOut, headIDs)
	stream := summarize(t, streamOut, streamIDs)
	if !sameSummaries(head, stream) {
		t.Errorf("stream output differs from Head:\n head:   %v\n stream: %v", head, stream)
	}
	// Both paths must stay inside the source block's own half-open [MinTime,
	// MaxTime) range: meta MaxTime is exclusive, so an output ending even 1ms
	// past the source bound overlaps the next contiguous 2h block and feeds
	// endless vertical compaction in a downstream Thanos compactor.
	assertWithinSource(t, headOut, headIDs, srcBM.MinTime, srcBM.MaxTime)
	assertWithinSource(t, streamOut, streamIDs, srcBM.MinTime, srcBM.MaxTime)
	// value-level spot check: the 6000-float series and the transition series must
	// be byte-faithful through the chunk copy.
	if got := strings.Join(drainSeries(t, streamOut, streamIDs, `{prometheus="F"}`, `{__name__="disk"}`), ","); got != "f@0=1,f@1=2,h@2,h@3" {
		t.Errorf("stream F transition = %q", got)
	}
	if n := len(drainSeries(t, streamOut, streamIDs, `{prometheus="D"}`, `{__name__="disk"}`)); n != 6000 {
		t.Errorf("stream D samples = %d want 6000", n)
	}

	// Symbol-table check: --stream must prune each output block's symbol table to
	// the strings its series reference, not inherit the whole source symbol set
	// (thanos-compact unions input symbols and never prunes, so foreign strings
	// would bake into long-lived compacted blocks). Assert no unreferenced symbol
	// survives in any stream block.
	for _, id := range streamIDs {
		bdir := path.Join(streamOut, id.String())
		ref := referencedStrings(t, bdir)
		for _, s := range blockSymbols(t, bdir) {
			if s == "" {
				continue
			}
			if _, ok := ref[s]; !ok {
				t.Errorf("stream block %s carries unreferenced symbol %q: symbol table not pruned", id, s)
			}
		}
	}
}

// TestUnwrapBlockE2E covers unwrapBlock orchestration around splitBlock: the
// dry-run guard, the (data-destructive) unconditional source delete, the
// zero-output-block delete, and the meta-relabel whole-block drop.
func TestUnwrapBlockE2E(t *testing.T) {
	logger := log.NewNopLogger()

	setupSrc := func(t *testing.T) (objstore.Bucket, ulid.ULID, string) {
		bktDir := t.TempDir()
		id := buildSourceBlock(t, bktDir)
		// give the source a Thanos meta.json (labels) so unwrapBlock reads real labels
		b, err := tsdb.OpenBlock(logger, filepath.Join(bktDir, id.String()), nil)
		if err != nil {
			t.Fatalf("open source block: %v", err)
		}
		bm := b.Meta()
		b.Close()
		if err := writeThanosMeta(bm, map[string]string{"src": "orig"}, 0, bktDir, logger); err != nil {
			t.Fatalf("write source thanos meta: %v", err)
		}
		bkt, err := client.NewBucket(logger, []byte("{type: FILESYSTEM, config: {directory: "+bktDir+"}}"), "thanos-kit")
		if err != nil {
			t.Fatalf("src bucket: %v", err)
		}
		return bkt, id, bktDir
	}
	dstBucket := func(t *testing.T) (objstore.Bucket, string) {
		dstDir := t.TempDir()
		bkt, err := client.NewBucket(logger, []byte("{type: FILESYSTEM, config: {directory: "+dstDir+"}}"), "thanos-kit")
		if err != nil {
			t.Fatalf("dst bucket: %v", err)
		}
		return bkt, dstDir
	}
	countBlocks := func(dir string) int {
		n := 0
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if e.IsDir() {
				if _, err := os.Stat(filepath.Join(dir, e.Name(), "meta.json")); err == nil {
					n++
				}
			}
		}
		return n
	}
	srcExists := func(srcDir string, id ulid.ULID) bool {
		_, err := os.Stat(filepath.Join(srcDir, id.String(), "meta.json"))
		return err == nil
	}

	t.Run("non-dry-run uploads per-tenant blocks and deletes source", func(t *testing.T) {
		src, id, srcDir := setupSrc(t)
		dst, dstDir := dstBucket(t)
		if err := unwrapBlock(src, Block{Id: id}, testRelabel(t), nil, t.TempDir(), false, dst, 1, false, logger); err != nil {
			t.Fatalf("unwrapBlock: %v", err)
		}
		if got := countBlocks(dstDir); got != 6 {
			t.Errorf("dst blocks=%d want 6", got)
		}
		if srcExists(srcDir, id) {
			t.Errorf("source block %s not deleted", id)
		}
	})

	t.Run("dry-run uploads and deletes nothing", func(t *testing.T) {
		src, id, srcDir := setupSrc(t)
		dst, dstDir := dstBucket(t)
		if err := unwrapBlock(src, Block{Id: id}, testRelabel(t), nil, t.TempDir(), true, dst, 1, false, logger); err != nil {
			t.Fatalf("unwrapBlock: %v", err)
		}
		if got := countBlocks(dstDir); got != 0 {
			t.Errorf("dry-run dst blocks=%d want 0", got)
		}
		if !srcExists(srcDir, id) {
			t.Errorf("dry-run must keep source block %s", id)
		}
	})

	t.Run("zero output blocks still deletes source", func(t *testing.T) {
		src, id, srcDir := setupSrc(t)
		dst, dstDir := dstBucket(t)
		dropAll := mustRelabel(t, "- source_labels: [__name__]\n  regex: \".*\"\n  action: drop\n")
		if err := unwrapBlock(src, Block{Id: id}, dropAll, nil, t.TempDir(), false, dst, 1, false, logger); err != nil {
			t.Fatalf("unwrapBlock: %v", err)
		}
		if got := countBlocks(dstDir); got != 0 {
			t.Errorf("dst blocks=%d want 0 (all series dropped)", got)
		}
		if srcExists(srcDir, id) {
			t.Errorf("source must be deleted even with zero output blocks")
		}
	})

	t.Run("meta-relabel whole-block drop keeps source", func(t *testing.T) {
		src, id, srcDir := setupSrc(t)
		dst, dstDir := dstBucket(t)
		// meta-relabel that drops the block: keep only blocks whose src label matches
		// something the source's src="orig" does not.
		metaDrop := mustRelabel(t, "- source_labels: [src]\n  regex: \"nomatch\"\n  action: keep\n")
		if err := unwrapBlock(src, Block{Id: id}, testRelabel(t), metaDrop, t.TempDir(), false, dst, 1, false, logger); err != nil {
			t.Fatalf("unwrapBlock: %v", err)
		}
		if got := countBlocks(dstDir); got != 0 {
			t.Errorf("dst blocks=%d want 0 (block dropped by meta-relabel)", got)
		}
		if !srcExists(srcDir, id) {
			t.Errorf("meta-relabel drop must keep source block %s", id)
		}
	})
}
