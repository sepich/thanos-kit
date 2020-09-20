package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunks"
	tsdb_errors "github.com/prometheus/prometheus/tsdb/errors"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"github.com/thanos-io/thanos/pkg/objstore/client"
	"github.com/thanos-io/thanos/pkg/runutil"
)

var merr tsdb_errors.MultiError

func analyze(objStoreConfig []byte, id *string, dir *string, limit *int, logger log.Logger, metrics *prometheus.Registry) error {
	bkt, err := client.NewBucket(logger, objStoreConfig, metrics, "bucket")
	if err != nil {
		return err
	}

	// Ensure we close up everything properly.
	defer func() {
		if err != nil {
			runutil.CloseWithLogOnErr(logger, bkt, "bucket client")
		}
	}()

	ctx, _ := context.WithCancel(context.Background())
	bdir := filepath.Join(*dir, *id)
	begin := time.Now()
	err = block.Download(ctx, logger, bkt, ulid.MustParse(*id), bdir)
	if err != nil {
		return errors.Wrapf(err, "download block %s", *id)
	}
	level.Info(logger).Log("msg", "downloaded block", "id", *id, "duration", time.Since(begin))

	return analyzeBlock(*dir, *id, *limit)
}

// https://github.com/prometheus/prometheus/blob/6573bf42f2431470e375faa515f282eb36865007/cmd/promtool/tsdb.go#L416
func analyzeBlock(path, blockID string, limit int) error {
	db, block, err := openBlock(path, blockID)
	if err != nil {
		return err
	}
	defer func() {
		merr.Add(err)
		merr.Add(db.Close())
		err = merr.Err()
	}()

	meta := block.Meta()
	fmt.Printf("Block ID: %s\n", meta.ULID)
	m, err := metadata.Read(filepath.Join(path, blockID))
	if err != nil {
		return errors.Wrapf(err, "fail to read meta.json for %s", blockID)
	}
	fmt.Printf("Thanos Labels: %s\n", labelsToString(m.Thanos.Labels))

	// Presume 1ms resolution that Prometheus uses.
	fmt.Printf("Duration: %s\n", (time.Duration(meta.MaxTime-meta.MinTime) * 1e6).String())
	fmt.Printf("Series: %d\n", meta.Stats.NumSeries)
	ir, err := block.Index()
	if err != nil {
		return err
	}
	defer ir.Close()

	allLabelNames, err := ir.LabelNames()
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
		sort.Slice(postingInfos, func(i, j int) bool { return postingInfos[i].metric > postingInfos[j].metric })

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
	p, err := ir.Postings("", "") // The special all key.
	if err != nil {
		return err
	}
	lbls := labels.Labels{}
	chks := []chunks.Meta{}
	for p.Next() {
		if err = ir.Series(p.At(), &lbls, &chks); err != nil {
			return err
		}
		// Amount of the block time range not covered by this series.
		uncovered := uint64(meta.MaxTime-meta.MinTime) - uint64(chks[len(chks)-1].MaxTime-chks[0].MinTime)
		for _, lbl := range lbls {
			key := lbl.Name + "=" + lbl.Value
			labelsUncovered[lbl.Name] += uncovered
			labelpairsUncovered[key] += uncovered
			labelpairsCount[key]++
			entries++
		}
	}
	if p.Err() != nil {
		return p.Err()
	}
	fmt.Printf("Postings (unique label pairs): %d\n", len(labelpairsUncovered))
	fmt.Printf("Postings entries (total label pairs): %d\n", entries)

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
		values, err := ir.SortedLabelValues(n)
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
		lv, err := ir.SortedLabelValues(n)
		if err != nil {
			return err
		}
		postingInfos = append(postingInfos, postingInfo{n, uint64(len(lv))})
	}
	fmt.Printf("\nHighest cardinality labels:\n")
	printInfo(postingInfos)

	postingInfos = postingInfos[:0]
	lv, err := ir.SortedLabelValues("__name__")
	if err != nil {
		return err
	}
	for _, n := range lv {
		postings, err := ir.Postings("__name__", n)
		if err != nil {
			return err
		}
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
	return os.RemoveAll(filepath.Join(path, blockID))
}

func openBlock(path, blockID string) (*tsdb.DBReadOnly, tsdb.BlockReader, error) {
	db, err := tsdb.OpenDBReadOnly(path, nil)
	if err != nil {
		return nil, nil, err
	}
	blocks, err := db.Blocks()
	if err != nil {
		return nil, nil, err
	}
	var block tsdb.BlockReader
	if blockID != "" {
		for _, b := range blocks {
			if b.Meta().ULID.String() == blockID {
				block = b
				break
			}
		}
	} else if len(blocks) > 0 {
		block = blocks[len(blocks)-1]
	}
	if block == nil {
		return nil, nil, fmt.Errorf("block %s not found", blockID)
	}
	return db, block, nil
}

// labelsToString returns labels as comma separated k=v pairs
func labelsToString(lables map[string]string) string {
	pairs := make([]string, len(lables))
	i := 0
	for k, v := range lables {
		pairs[i] = fmt.Sprintf("%s=%s", k, v)
		i++
	}
	return strings.Join(pairs, ", ")
}
