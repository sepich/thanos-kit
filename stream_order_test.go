package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/go-kit/log"
	"github.com/oklog/ulid"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/thanos-io/objstore/client"
	"github.com/thanos-io/thanos/pkg/block/metadata"
)

type floatSeries struct {
	lset       labels.Labels
	mint, maxt int64 // samples at every ts in [mint, maxt)
}

// buildFloatBlock writes a source block with the given float series to dir.
func buildFloatBlock(t *testing.T, dir string, series []floatSeries) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	bw, err := tsdb.NewBlockWriter(log.NewNopLogger(), dir, getCompatibleBlockDuration(math.MaxInt64))
	if err != nil {
		t.Fatalf("new block writer: %v", err)
	}
	app := bw.Appender(ctx)
	for _, s := range series {
		for ts := s.mint; ts < s.maxt; ts++ {
			if _, err := app.Append(0, s.lset, ts, float64(ts)); err != nil {
				t.Fatalf("append %v: %v", s.lset, err)
			}
		}
	}
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

// TestStreamStripOrder covers source blocks whose series change order once labels
// are stripped: --stream must sort them and match the Head path, and series that
// become identical must fail with errStreamDuplicateSeries before anything is
// written.
func TestStreamStripOrder(t *testing.T) {
	const splitByPrometheus = "- target_label: __meta_ext_labels\n  replacement: prometheus\n"
	cases := []struct {
		name    string
		relabel string
		series  []floatSeries
		dup     bool
	}{
		{
			// "__service" sorts before "arch", "department" after it.
			name:    "dunder label decides source order",
			relabel: splitByPrometheus,
			series: []floatSeries{
				{labels.FromStrings("__name__", "target_info", "__service", "x", "department", "d", "prometheus", "A"), 0, 3},
				{labels.FromStrings("__name__", "target_info", "arch", "amd64", "prometheus", "A"), 0, 3},
				{labels.FromStrings("__name__", "target_info", "__service", "x", "department", "d", "prometheus", "B"), 0, 3},
				{labels.FromStrings("__name__", "target_info", "arch", "amd64", "prometheus", "B"), 0, 3},
				{labels.FromStrings("__name__", "up", "job", "j", "prometheus", "A"), 0, 3},
			},
		},
		{
			// {a} is a prefix of {a, b} only once prometheus is stripped.
			name:    "ext-label is the last label of one series",
			relabel: splitByPrometheus,
			series: []floatSeries{
				{labels.FromStrings("__name__", "m", "a", "1", "prometheus", "A"), 0, 3},
				{labels.FromStrings("__name__", "m", "a", "1", "b", "2", "prometheus", "A"), 0, 3},
			},
		},
		{
			// the relabel rewrites a="1" to a="3" on the series that sorts first.
			name:    "relabel rewrites a kept label",
			relabel: "- source_labels: [b]\n  regex: x\n  target_label: a\n  replacement: \"3\"\n" + splitByPrometheus,
			series: []floatSeries{
				{labels.FromStrings("__name__", "m", "a", "1", "b", "x", "prometheus", "A"), 0, 3},
				{labels.FromStrings("__name__", "m", "a", "2", "prometheus", "A"), 0, 3},
			},
		},
		{
			name:    "series identical after the strip",
			relabel: splitByPrometheus,
			series: []floatSeries{
				{labels.FromStrings("__name__", "m", "__service", "x", "job", "j", "prometheus", "A"), 0, 3},
				{labels.FromStrings("__name__", "m", "job", "j", "prometheus", "A"), 3, 6},
			},
			dup: true,
		},
		{
			// every series is less than its predecessor once __service is stripped: 300 runs
			name:    "many runs: dunder label reverses a family",
			relabel: splitByPrometheus,
			series:  reversedFamily(300),
		},
		{
			name:    "ext label last across a chain",
			relabel: splitByPrometheus,
			series:  extLastChains(50),
		},
		{
			// the copy of pod=123 sorts after the whole family in the source, so it is
			// not adjacent to its twin in discovery
			name:    "duplicate far apart in different runs",
			relabel: splitByPrometheus,
			series:  append(reversedFamily(400), floatSeries{labels.FromStrings("__name__", "m", "pod", "123", "prometheus", "A"), 3, 6}),
			dup:     true,
		},
		{
			// tenant A comes first in source order and is already in order, tenant B
			// has a non-adjacent duplicate
			name:    "duplicate in second tenant, first tenant clean",
			relabel: splitByPrometheus,
			series: []floatSeries{
				{labels.FromStrings("__name__", "a", "job", "j", "prometheus", "A"), 0, 3},
				{labels.FromStrings("__name__", "m", "__service", "x", "job", "j", "prometheus", "B"), 0, 3},
				{labels.FromStrings("__name__", "m", "arch", "amd64", "prometheus", "B"), 0, 3},
				{labels.FromStrings("__name__", "m", "job", "j", "prometheus", "B"), 3, 6},
				{labels.FromStrings("__name__", "m", "job", "k", "prometheus", "A"), 0, 3},
			},
			dup: true,
		},
	}
	logger := log.NewNopLogger()
	duration := getCompatibleBlockDuration(math.MaxInt64)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inDir := path.Join(t.TempDir(), "in")
			id := buildFloatBlock(t, inDir, tc.series)
			cfg := mustRelabel(t, tc.relabel)
			origMeta := metadata.Meta{Thanos: metadata.Thanos{Labels: map[string]string{"src": "orig"}}}

			headOut := t.TempDir()
			q, closeQ := openSourceQuerier(t, inDir)
			headIDs, err := splitBlock(context.Background(), q, cfg, headOut, origMeta, duration, 1, logger)
			closeQ()
			if err != nil {
				t.Fatalf("splitBlock: %v", err)
			}

			streamOut := t.TempDir()
			blk, err := tsdb.OpenBlock(logger, path.Join(inDir, id.String()), chunkenc.NewPool())
			if err != nil {
				t.Fatalf("open block: %v", err)
			}
			streamIDs, err := streamSplitBlock(context.Background(), blk, cfg, streamOut, origMeta, duration, logger)
			blk.Close()

			if tc.dup {
				if !errors.Is(err, errStreamDuplicateSeries) {
					t.Fatalf("streamSplitBlock err = %v, want errStreamDuplicateSeries", err)
				}
				if ents, _ := os.ReadDir(streamOut); len(ents) != 0 {
					t.Fatalf("streamSplitBlock left %d entries in outDir, want none", len(ents))
				}
				return
			}
			if err != nil {
				t.Fatalf("streamSplitBlock: %v", err)
			}
			head := summarize(t, headOut, headIDs)
			stream := summarize(t, streamOut, streamIDs)
			if !sameSummaries(head, stream) {
				t.Errorf("stream output differs from Head:\n head:   %v\n stream: %v", head, stream)
			}
		})
	}
}

// TestUnwrapBlockStreamDuplicateFallback checks that unwrapBlock --stream converts
// a block with series that are identical after the strip through the Head path,
// merging their samples, instead of failing.
func TestUnwrapBlockStreamDuplicateFallback(t *testing.T) {
	logger := log.NewNopLogger()
	bktDir := t.TempDir()
	id := buildFloatBlock(t, bktDir, []floatSeries{
		{labels.FromStrings("__name__", "m", "__service", "x", "job", "j", "prometheus", "A"), 0, 3},
		{labels.FromStrings("__name__", "m", "job", "j", "prometheus", "A"), 3, 6},
	})
	b, err := tsdb.OpenBlock(logger, filepath.Join(bktDir, id.String()), nil)
	if err != nil {
		t.Fatalf("open source block: %v", err)
	}
	bm := b.Meta()
	b.Close()
	if err := writeThanosMeta(bm, map[string]string{"src": "orig"}, 0, bktDir, logger); err != nil {
		t.Fatalf("write source thanos meta: %v", err)
	}
	src, err := client.NewBucket(logger, []byte("{type: FILESYSTEM, config: {directory: "+bktDir+"}}"), "thanos-kit")
	if err != nil {
		t.Fatalf("src bucket: %v", err)
	}
	dstDir := t.TempDir()
	dst, err := client.NewBucket(logger, []byte("{type: FILESYSTEM, config: {directory: "+dstDir+"}}"), "thanos-kit")
	if err != nil {
		t.Fatalf("dst bucket: %v", err)
	}

	cfg := mustRelabel(t, "- target_label: __meta_ext_labels\n  replacement: prometheus\n")
	if err := unwrapBlock(src, Block{Id: id}, cfg, nil, t.TempDir(), false, dst, 0, true, logger); err != nil {
		t.Fatalf("unwrapBlock --stream: %v", err)
	}
	ents, err := os.ReadDir(dstDir)
	if err != nil || len(ents) != 1 {
		t.Fatalf("dst entries=%d (%v), want 1 block", len(ents), err)
	}
	sum := summarize(t, dstDir, []ulid.ULID{ulid.MustParse(ents[0].Name())})
	got := sum[extKey(map[string]string{"src": "orig", "prometheus": "A"})]
	if got == nil || len(got.series) != 1 || got.series[`{__name__="m", job="j"}`] != 6 {
		t.Errorf("merged output = %v, want one series {__name__=\"m\", job=\"j\"} with 6 samples", sum)
	}
}

// TestStreamMatchesHeadRealBlock compares --stream and Head output on a real
// source block, e.g. one pulled from the Mimir bucket. Opt-in, the block is not
// part of the repo:
//
//	THANOS_KIT_REAL_BLOCK=/path/to/block-dir [THANOS_KIT_REAL_EXT='prometheus'] go test -run TestStreamMatchesHeadRealBlock -v .
//
// The Head path holds the largest tenant in memory, so size the machine for it.
func TestStreamMatchesHeadRealBlock(t *testing.T) {
	srcBlock := os.Getenv("THANOS_KIT_REAL_BLOCK")
	if srcBlock == "" {
		t.Skip("THANOS_KIT_REAL_BLOCK not set")
	}
	ext := os.Getenv("THANOS_KIT_REAL_EXT")
	if ext == "" {
		ext = "prometheus"
	}
	logger := log.NewNopLogger()
	origMeta, err := metadata.ReadFromDir(srcBlock)
	if err != nil {
		t.Fatalf("read source meta: %v", err)
	}
	// The Head path opens the copy as a TSDB, which only picks up ULID-named block
	// directories, so name the copy after the block whatever srcBlock is called.
	inDir := path.Join(t.TempDir(), "in")
	blockDir := path.Join(inDir, origMeta.ULID.String())
	copyDir(t, srcBlock, blockDir)
	_ = os.MkdirAll(path.Join(inDir, "wal"), 0777)
	cfg := mustRelabel(t, "- target_label: __meta_ext_labels\n  replacement: "+ext+"\n")
	duration := getCompatibleBlockDuration(math.MaxInt64)

	headOut := t.TempDir()
	q, closeQ := openSourceQuerier(t, inDir)
	headIDs, err := splitBlock(context.Background(), q, cfg, headOut, *origMeta, duration, 1, logger)
	closeQ()
	if err != nil {
		t.Fatalf("splitBlock: %v", err)
	}

	streamOut := t.TempDir()
	blk, err := tsdb.OpenBlock(logger, blockDir, chunkenc.NewPool())
	if err != nil {
		t.Fatalf("open block: %v", err)
	}
	srcBM := blk.Meta()
	streamIDs, err := streamSplitBlock(context.Background(), blk, cfg, streamOut, *origMeta, duration, logger)
	blk.Close()
	if errors.Is(err, errStreamDuplicateSeries) {
		t.Skipf("block needs the Head fallback: %v", err)
	}
	if err != nil {
		t.Fatalf("streamSplitBlock: %v", err)
	}

	head := summarize(t, headOut, headIDs)
	stream := summarize(t, streamOut, streamIDs)
	if !sameSummaries(head, stream) {
		t.Fatalf("stream output differs from Head (%d vs %d tenants)", len(head), len(stream))
	}
	n := 0
	for _, s := range stream {
		n += len(s.series)
	}
	t.Logf("%d tenants, %d series: stream output matches Head", len(stream), n)
	assertWithinSource(t, streamOut, streamIDs, srcBM.MinTime, srcBM.MaxTime)
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0777)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
}
