package main

import (
	"context"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/objstore/client"
	"github.com/thanos-io/thanos/pkg/runutil"

	"github.com/sepich/thanos-kit/importer"
	"github.com/sepich/thanos-kit/importer/blocks"
)

func backfill(objStoreConfig []byte, importFromFile *string, importBlockSize *time.Duration, importDataDir *string, importLabels *[]string, logger log.Logger, metrics *prometheus.Registry) (err error) {


	err = wipeDir(*importDataDir, logger)
	if err != nil {
		return errors.Wrapf(err, "cleanup dir %s", *importDataDir)
	}

	input := os.Stdin
	if *importFromFile != "" {
		input, err = os.Open(*importFromFile)
		if err != nil {
			return err
		}
		defer func() {
			merr.Add(err)
			merr.Add(input.Close())
			err = merr.Err()
		}()
	}
	labels, err := parseFlagLabels(*importLabels)
	if err != nil {
		return errors.Wrap(err, "parse thanos labels")
	}

	p := importer.NewParser(input)
	ids, err := importer.Import(logger, p, blocks.NewMultiWriter(logger, *importDataDir, blocks.DurToMillis(*importBlockSize), labels))
	if err != nil {
		return errors.Wrap(err, "create tsdb blocks")
	}

	// upload blocks to obj storage
	bkt, err := client.NewBucket(logger, objStoreConfig, metrics, "bucket")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			runutil.CloseWithLogOnErr(logger, bkt, "bucket client")
		}
	}()

	for _, id := range ids {
		bdir := filepath.Join(*importDataDir, id.String())
		begin := time.Now()
		err = block.Upload(context.TODO(), logger, bkt, bdir)
		if err != nil {
			return errors.Wrapf(err, "upload block %s", id.String())
		}
		level.Info(logger).Log("msg", "uploaded block", "id", id.String(), "duration", time.Since(begin))
	}

	err = wipeDir(*importDataDir, logger)
	if err != nil {
		return errors.Wrapf(err, "cleanup dir %s", *importDataDir)
	}
	return nil
}

// wipeDir cleans up content of temp dir
func wipeDir(dir string, logger log.Logger) error {
	level.Info(logger).Log("msg", "cleanup cache dir")
	rd, err := ioutil.ReadDir(dir)
	if err != nil {
		return errors.Wrapf(err, "read dir %s", dir)
	}
	for _, d := range rd {
		if err := os.RemoveAll(path.Join(dir, d.Name())); err != nil {
			return errors.Wrapf(err, "cleanup %s", d.Name())
		}
	}
	return nil
}
