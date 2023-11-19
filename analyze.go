package main

import (
	"context"
	"fmt"
	"github.com/go-kit/log"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunks"
	tsdb_errors "github.com/prometheus/prometheus/tsdb/errors"
	"github.com/prometheus/prometheus/tsdb/index"
	"github.com/thanos-io/objstore"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"golang.org/x/exp/slices"
	"path/filepath"
	"strings"
	"time"
)

func analyze(bkt objstore.Bucket, id, dir *string, analyzeLimit *int, analyzeMatchers *string, logger log.Logger) error {
	ctx := context.Background()
	if err := downloadBlock(ctx, *dir, *id, bkt, logger); err != nil {
		return err
	}
	return analyzeBlock(*dir, *id, *analyzeLimit, *analyzeMatchers)
}

// https://github.com/prometheus/prometheus/blob/main/cmd/promtool/tsdb.go#L415
func analyzeBlock(path, blockID string, limit int, matchers string) error {
	var (
		selectors []*labels.Matcher
		err       error
	)
	if len(matchers) > 0 {
		selectors, err = parser.ParseMetricSelector(matchers)
		if err != nil {
			return err
		}
	}
	db, block, err := openBlock(path, blockID)
	if err != nil {
		return err
	}
	defer func() {
		err = tsdb_errors.NewMulti(err, db.Close()).Err()
	}()

	meta := block.Meta()
	fmt.Printf("Block ID: %s\n", meta.ULID)
	m, err := metadata.ReadFromDir(filepath.Join(path, blockID))
	if err != nil {
		return fmt.Errorf("fail to read meta.json for %s: %w", blockID, err)
	}
	fmt.Printf("Thanos Labels: %s\n", labelsToString(m.Thanos.Labels))

	// Presume 1ms resolution that Prometheus uses.
	fmt.Printf("Duration: %s\n", (time.Duration(meta.MaxTime-meta.MinTime) * 1e6).String())
	fmt.Printf("Total Series: %d\n", meta.Stats.NumSeries)
	if len(matchers) > 0 {
		fmt.Printf("Matcher: %s\n", matchers)
	}
	ir, err := block.Index()
	if err != nil {
		return err
	}
	defer ir.Close()

	allLabelNames, err := ir.LabelNames(selectors...)
	if err != nil {
		return err
	}
	fmt.Printf("Label names: %d\n", len(allLabelNames))

	type postingInfo struct {
		key    string
		metric uint64
	}
	postingInfos := []postingInfo{}

	printInfo := func(postingInfos []postingInfo) {
		slices.SortFunc(postingInfos, func(a, b postingInfo) bool { return a.metric > b.metric })

		for i, pc := range postingInfos {
			if i >= limit {
				break
			}
			fmt.Printf("%d %s\n", pc.metric, pc.key)
		}
	}

	labelsUncovered := map[string]uint64{}
	labelpairsUncovered := map[string]uint64{}
	labelpairsCount := map[string]uint64{}
	entries := 0
	var (
		p    index.Postings
		refs []storage.SeriesRef
	)
	if len(matchers) > 0 {
		p, err = tsdb.PostingsForMatchers(ir, selectors...)
		if err != nil {
			return err
		}
		// Expand refs first and cache in memory.
		// So later we don't have to expand again.
		refs, err = index.ExpandPostings(p)
		if err != nil {
			return err
		}
		fmt.Printf("Matched series: %d\n", len(refs))
		p = index.NewListPostings(refs)
	} else {
		p, err = ir.Postings("", "") // The special all key.
		if err != nil {
			return err
		}
	}

	chks := []chunks.Meta{}
	builder := labels.ScratchBuilder{}
	first := true
	splitLabels := make(map[string]bool) // labels appearing in all series
	for p.Next() {
		if err = ir.Series(p.At(), &builder, &chks); err != nil {
			return err
		}
		// Amount of the block time range not covered by this series.
		uncovered := uint64(meta.MaxTime-meta.MinTime) - uint64(chks[len(chks)-1].MaxTime-chks[0].MinTime)
		builder.Labels().Range(func(lbl labels.Label) {
			key := lbl.Name + "=" + lbl.Value
			labelsUncovered[lbl.Name] += uncovered
			labelpairsUncovered[key] += uncovered
			labelpairsCount[key]++
			entries++
		})
		if first {
			for _, l := range builder.Labels() {
				if l.Name != "__name__" {
					splitLabels[l.Name] = true
				}
			}
			first = false
		}
		for l, _ := range splitLabels {
			if !builder.Labels().Has(l) {
				delete(splitLabels, l)
			}
		}
	}
	if p.Err() != nil {
		return p.Err()
	}
	fmt.Printf("Postings (unique label pairs): %d\n", len(labelpairsUncovered))
	fmt.Printf("Postings entries (total label pairs): %d\n", entries)
	tmp := []string{}
	for k, _ := range splitLabels {
		tmp = append(tmp, k)
	}
	fmt.Printf("Label names appearing in all Series: [%s]\n", strings.Join(tmp, ", "))

	postingInfos = postingInfos[:0]
	for k, m := range labelpairsUncovered {
		postingInfos = append(postingInfos, postingInfo{k, uint64(float64(m) / float64(meta.MaxTime-meta.MinTime))})
	}

	fmt.Printf("\nLabel pairs most involved in churning:\n")
	printInfo(postingInfos)

	postingInfos = postingInfos[:0]
	for k, m := range labelsUncovered {
		postingInfos = append(postingInfos, postingInfo{k, uint64(float64(m) / float64(meta.MaxTime-meta.MinTime))})
	}

	fmt.Printf("\nLabel names most involved in churning:\n")
	printInfo(postingInfos)

	postingInfos = postingInfos[:0]
	for k, m := range labelpairsCount {
		postingInfos = append(postingInfos, postingInfo{k, m})
	}

	fmt.Printf("\nMost common label pairs:\n")
	printInfo(postingInfos)

	postingInfos = postingInfos[:0]
	for _, n := range allLabelNames {
		values, err := ir.SortedLabelValues(n, selectors...)
		if err != nil {
			return err
		}
		var cumulativeLength uint64
		for _, str := range values {
			cumulativeLength += uint64(len(str))
		}
		postingInfos = append(postingInfos, postingInfo{n, cumulativeLength})
	}

	fmt.Printf("\nLabel names with highest cumulative label value length:\n")
	printInfo(postingInfos)

	postingInfos = postingInfos[:0]
	for _, n := range allLabelNames {
		lv, err := ir.SortedLabelValues(n, selectors...)
		if err != nil {
			return err
		}
		postingInfos = append(postingInfos, postingInfo{n, uint64(len(lv))})
	}
	fmt.Printf("\nHighest cardinality labels:\n")
	printInfo(postingInfos)

	postingInfos = postingInfos[:0]
	lv, err := ir.SortedLabelValues("__name__", selectors...)
	if err != nil {
		return err
	}
	for _, n := range lv {
		postings, err := ir.Postings("__name__", n)
		if err != nil {
			return err
		}
		postings = index.Intersect(postings, index.NewListPostings(refs))
		count := 0
		for postings.Next() {
			count++
		}
		if postings.Err() != nil {
			return postings.Err()
		}
		postingInfos = append(postingInfos, postingInfo{n, uint64(count)})
	}
	fmt.Printf("\nHighest cardinality metric names:\n")
	printInfo(postingInfos)

	return nil
}

func openBlock(path, blockID string) (*tsdb.DBReadOnly, tsdb.BlockReader, error) {
	db, err := tsdb.OpenDBReadOnly(path, nil)
	if err != nil {
		return nil, nil, err
	}

	if blockID == "" {
		blockID, err = db.LastBlockID()
		if err != nil {
			return nil, nil, err
		}
	}

	b, err := db.Block(blockID)
	if err != nil {
		return nil, nil, err
	}

	return db, b, nil
}
