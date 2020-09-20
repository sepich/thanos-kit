package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/tsdb"
	tsdb_errors "github.com/prometheus/prometheus/tsdb/errors"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/objstore/client"
	"github.com/thanos-io/thanos/pkg/runutil"
)

func dump(objStoreConfig []byte, ids *[]string, dir *string, mint, maxt *int64, out io.Writer, logger log.Logger, metrics *prometheus.Registry) error {
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

	err = wipeDir(*dir, logger)
	if err != nil {
		return errors.Wrapf(err, "cleanup dir %s", *dir)
	}

	ctx, _ := context.WithCancel(context.Background())
	for _, id := range *ids {
		bdir := filepath.Join(*dir, id)
		begin := time.Now()
		err = block.Download(ctx, logger, bkt, ulid.MustParse(id), bdir)
		if err != nil {
			return errors.Wrapf(err, "download block %s", id)
		}
		level.Info(logger).Log("msg", "downloaded block", "id", id, "duration", time.Since(begin))
	}
	os.Mkdir(filepath.Join(*dir, "wal"), 0777)
	return dumpSamples(*dir, *mint, *maxt, out, logger)
}

// https://github.com/prometheus/prometheus/blob/6573bf42f2431470e375faa515f282eb36865007/cmd/promtool/tsdb.go#L566
func dumpSamples(path string, mint, maxt int64, out io.Writer, logger log.Logger) (err error) {
	db, err := tsdb.OpenDBReadOnly(path, nil)
	if err != nil {
		return err
	}
	defer func() {
		merr.Add(err)
		merr.Add(db.Close())
		err = merr.Err()
	}()
	q, err := db.Querier(context.TODO(), mint, maxt)
	if err != nil {
		return err
	}
	defer q.Close()

	ss := q.Select(false, nil, labels.MustNewMatcher(labels.MatchRegexp, "", ".*"))

	for ss.Next() {
		series := ss.At()
		lbs := series.Labels()
		nm := lbs.Get("__name__")
		lbs = lbs.WithoutLabels("__name__")
		it := series.Iterator()
		for it.Next() {
			ts, val := it.At()
			fmt.Fprintf(out,"%s%s %g %d\n", nm, lbs, val, ts)
		}
		if it.Err() != nil {
			return ss.Err()
		}
	}

	if ws := ss.Warnings(); len(ws) > 0 {
		var merr tsdb_errors.MultiError
		for _, w := range ws {
			merr.Add(w)
		}
		return merr.Err()
	}

	if ss.Err() != nil {
		return ss.Err()
	}

	err = wipeDir(path, logger)
	if err != nil {
		return errors.Wrapf(err, "cleanup dir %s", path)
	}
	return nil
}
