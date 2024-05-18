package main

import (
	"context"
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
	"sync"
	"time"
)

const metaExtLabels = "__meta_ext_labels"

func unwrap(bkt objstore.Bucket, unwrapRelabel extkingpin.PathOrContent, unwrapMetaRelabel extkingpin.PathOrContent, recursive bool, dir *string, wait *time.Duration, unwrapDry bool, outConfig *extkingpin.PathOrContent, maxTime *model.TimeOrDurationValue, unwrapSrc *string, logger log.Logger) (err error) {
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
			if err := unwrapBlock(bkt, b, relabelConfig, metaRelabel, *dir, unwrapDry, dst, logger); err != nil {
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

func unwrapBlock(bkt objstore.Bucket, b Block, relabelConfig []*relabel.Config, metaRelabel []*relabel.Config, dir string, unwrapDry bool, dst objstore.Bucket, logger log.Logger) error {
	if err := runutil.DeleteAll(dir); err != nil {
		return fmt.Errorf("unable to cleanup cache folder %s: %w", dir, err)
	}
	inDir := path.Join(dir, "in")
	outDir := path.Join(dir, "out")
	matchAll := &labels.Matcher{
		Name:  "__name__",
		Type:  labels.MatchNotEqual,
		Value: "",
	}

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
	db, err := tsdb.OpenDBReadOnly(inDir, logger)
	if err != nil {
		return err
	}
	defer func() {
		err = tsdb_errors.NewMulti(err, db.Close()).Err()
	}()
	q, err := db.Querier(context.Background(), 0, math.MaxInt64)
	if err != nil {
		return err
	}
	defer q.Close()

	// prepare output
	os.Mkdir(outDir, 0777)
	duration := getCompatibleBlockDuration(math.MaxInt64) //todo
	mw, err := newMultiBlockWriter(outDir, logger, duration, *origMeta)
	if err != nil {
		return err
	}

	// unwrap
	ss := q.Select(false, nil, matchAll)
	for ss.Next() {
		series := ss.At()
		rl, keep := relabel.Process(series.Labels(), relabelConfig...)
		if !keep {
			continue
		}

		lbs, extl := extractLabels(rl, strings.Split(rl.Get(metaExtLabels), ";"))
		tdb, err := mw.getTenant(context.Background(), extl)
		if err != nil {
			return err
		}
		it := series.Iterator(nil)
		for it.Next() == chunkenc.ValFloat {
			ts, val := it.At()
			if _, err := tdb.appender.Append(0, lbs, ts, val); err != nil {
				return err
			}
			tdb.samples++
		}
		for it.Next() == chunkenc.ValFloatHistogram {
			ts, fh := it.AtFloatHistogram()
			if _, err := tdb.appender.AppendHistogram(0, lbs, ts, nil, fh); err != nil {
				return err
			}
			tdb.samples++
		}
		for it.Next() == chunkenc.ValHistogram {
			ts, h := it.AtHistogram()
			if _, err := tdb.appender.AppendHistogram(0, lbs, ts, h, nil); err != nil {
				return err
			}
			tdb.samples++
		}
		if it.Err() != nil {
			return ss.Err()
		}
	}

	if ss.Err() != nil {
		return ss.Err()
	}
	if ws := ss.Warnings(); len(ws) > 0 {
		return tsdb_errors.NewMulti(ws...).Err()
	}

	blocks, err := mw.flush(context.Background())
	if unwrapDry {
		level.Info(logger).Log("msg", "dry-run: skipping upload of created blocks and delete of original block", "ulids", fmt.Sprint(blocks), "orig", b.Id)
	} else {
		for _, id := range blocks {
			begin := time.Now()
			ctxu, cancelu := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancelu()
			err = block.Upload(ctxu, logger, dst, filepath.Join(outDir, id.String()), metadata.SHA256Func)
			if err != nil {
				return fmt.Errorf("upload block %s: %v", id, err)
			}
			level.Info(logger).Log("msg", "uploaded block", "ulid", id, "duration", time.Since(begin))
		}
		level.Info(logger).Log("msg", "deleting original block", "ulid", b.Id)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := block.Delete(ctx, logger, pb, b.Id); err != nil {
			return fmt.Errorf("delete block %s%s: %v", b.Prefix, b.Id, err)
		}
	}

	return nil
}

// extractLabels splits given labels to two sets: for given `names` and the rest without metaExtLabels preserving sort order
func extractLabels(ls labels.Labels, names []string) (res labels.Labels, el labels.Labels) {
	slices.Sort(names)
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

type MultiBlockWriter struct {
	db        *tsdb.DBReadOnly
	origMeta  metadata.Meta
	blockSize int64 // in ms
	tenants   map[uint64]*tenant
	logger    log.Logger
	dir       string
	mu        sync.Mutex
}

type tenant struct {
	appender  storage.Appender
	writer    *tsdb.BlockWriter
	samples   int
	extLables labels.Labels
}

// newMultiBlockWriter creates a new multi-tenant tsdb BlockWriter
func newMultiBlockWriter(dir string, logger log.Logger, blockSize int64, meta metadata.Meta) (*MultiBlockWriter, error) {
	return &MultiBlockWriter{
		blockSize: blockSize,
		origMeta:  meta,
		tenants:   make(map[uint64]*tenant),
		logger:    logger,
		dir:       dir,
	}, nil
}

func (m *MultiBlockWriter) getTenant(ctx context.Context, lbls labels.Labels) (*tenant, error) {
	id := lbls.Hash()
	if _, ok := m.tenants[id]; !ok {
		w, err := tsdb.NewBlockWriter(m.logger, m.dir, m.blockSize)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.tenants[id] = &tenant{
			writer:    w,
			appender:  w.Appender(ctx),
			samples:   0,
			extLables: lbls,
		}
		m.mu.Unlock()
	}
	// reduce Mem usage, commit appender
	if m.tenants[id].samples > 5000 {
		if err := m.commit(ctx, id); err != nil {
			return nil, err
		}
	}

	return m.tenants[id], nil
}

func (m *MultiBlockWriter) commit(ctx context.Context, id uint64) error {
	err := m.tenants[id].appender.Commit()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants[id].appender = m.tenants[id].writer.Appender(ctx)
	m.tenants[id].samples = 0
	return err
}

// flush writes all blocks and reset MultiBlockWriter tenants
func (m *MultiBlockWriter) flush(ctx context.Context) (ids []ulid.ULID, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tenants {
		if err := t.appender.Commit(); err != nil {
			return nil, err
		}
		id, err := t.writer.Flush(ctx)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
		if err := t.writer.Close(); err != nil {
			return nil, err
		}

		meta, err := metadata.ReadFromDir(path.Join(m.dir, id.String()))
		if err != nil {
			return nil, fmt.Errorf("read %s metadata: %w", id, err)
		}
		l := m.origMeta.Thanos.Labels
		for _, e := range t.extLables {
			l[e.Name] = e.Value
		}
		if err = writeThanosMeta(meta.BlockMeta, l, m.origMeta.Thanos.Downsample.Resolution, m.dir, m.logger); err != nil {
			return nil, fmt.Errorf("write %s metadata: %w", id, err)
		}
	}
	m.tenants = make(map[uint64]*tenant)
	return ids, nil
}
