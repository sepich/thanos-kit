package main

import (
	"context"
	"fmt"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	tsdb_errors "github.com/prometheus/prometheus/tsdb/errors"
	"github.com/thanos-io/objstore"
	"github.com/thanos-io/thanos/pkg/block"
	"io"
	"os"
	"path/filepath"
	"time"
)

func dump(bkt objstore.Bucket, out io.Writer, ids *[]string, dir *string, mint, maxt *int64, match *string, logger log.Logger) error {
	ctx := context.Background()
	for _, id := range *ids {
		if _, err := ulid.Parse(id); err != nil {
			return errors.Wrapf(err, `invalid ULID "%s"`, id)
		}
		if err := downloadBlock(ctx, *dir, id, bkt, logger); err != nil {
			return err
		}
	}
	os.Mkdir(filepath.Join(*dir, "wal"), 0777)
	return dumpSamples(out, *dir, *mint, *maxt, *match)
}

// https://github.com/prometheus/prometheus/blob/main/cmd/promtool/tsdb.go#L703
func dumpSamples(out io.Writer, path string, mint, maxt int64, match string) (err error) {
	db, err := tsdb.OpenDBReadOnly(path, nil)
	if err != nil {
		return err
	}
	defer func() {
		err = tsdb_errors.NewMulti(err, db.Close()).Err()
	}()
	q, err := db.Querier(context.Background(), mint, maxt)
	if err != nil {
		return err
	}
	defer q.Close()

	matchers, err := parser.ParseMetricSelector(match)
	if err != nil {
		return err
	}
	ss := q.Select(false, nil, matchers...)

	for ss.Next() {
		series := ss.At()
		name := series.Labels().Get("__name__")
		lbs := series.Labels().MatchLabels(false, "__name__")
		// todo: add thanos labels?
		it := series.Iterator(nil)
		for it.Next() == chunkenc.ValFloat {
			ts, val := it.At()
			fmt.Fprintf(out, "%s%s %g %d\n", name, lbs, val, ts)
		}
		for it.Next() == chunkenc.ValFloatHistogram {
			ts, fh := it.AtFloatHistogram()
			fmt.Fprintf(out, "%s%s %s %d\n", name, lbs, fh.String(), ts)
		}
		for it.Next() == chunkenc.ValHistogram {
			ts, h := it.AtHistogram()
			fmt.Fprintf(out, "%s%s %s %d\n", name, lbs, h.String(), ts)
		}
		if it.Err() != nil {
			return ss.Err()
		}
	}

	if ws := ss.Warnings(); len(ws) > 0 {
		return tsdb_errors.NewMulti(ws...).Err()
	}

	if ss.Err() != nil {
		return ss.Err()
	}
	return nil
}

// download block id to dir
func downloadBlock(ctx context.Context, dir, id string, bkt objstore.Bucket, logger log.Logger) error {
	dest := filepath.Join(dir, id)
	begin := time.Now()
	err := block.Download(ctx, logger, bkt, ulid.MustParse(id), dest)
	if err != nil {
		return errors.Wrapf(err, "download block %s", id)
	}
	return level.Info(logger).Log("msg", "downloaded block", "id", id, "duration", time.Since(begin))
}
