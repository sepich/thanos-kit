package main

import (
	"context"
	"fmt"
	"github.com/alecthomas/units"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/textparse"
	"github.com/prometheus/prometheus/tsdb"
	tsdb_errors "github.com/prometheus/prometheus/tsdb/errors"
	"github.com/prometheus/prometheus/tsdb/fileutil"
	"github.com/thanos-io/objstore"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"
	"time"
)

func importMetrics(bkt objstore.Bucket, file *string, importBlockSize *time.Duration, dir *string, importLabels *[]string, upload bool, logger log.Logger) error {
	inputFile, err := fileutil.OpenMmapFile(*file)
	if err != nil {
		return err
	}
	defer inputFile.Close()

	if err := os.MkdirAll(*dir, 0o777); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	labels, err := parseFlagLabels(*importLabels)
	if err != nil {
		return errors.Wrap(err, "parse thanos labels")
	}

	p := textparse.NewPromParser(inputFile.Bytes())
	maxt, mint, err := getMinAndMaxTimestamps(p)
	if err != nil {
		return fmt.Errorf("getting min and max timestamp: %w", err)
	}
	ids, err := createBlocks(inputFile.Bytes(), mint, maxt, int64(*importBlockSize/time.Millisecond), 5000, *dir, true, labels, logger)
	if err != nil {
		return fmt.Errorf("block creation: %w", err)
	}

	if upload {
		for _, id := range ids {
			begin := time.Now()
			err = block.Upload(context.Background(), logger, bkt, filepath.Join(*dir, id.String()), metadata.SHA256Func)
			if err != nil {
				return errors.Wrapf(err, "upload block %s", id.String())
			}
			level.Info(logger).Log("msg", "uploaded block", "id", id.String(), "duration", time.Since(begin))
		}
	}
	return nil
}

func getMinAndMaxTimestamps(p textparse.Parser) (int64, int64, error) {
	var maxt, mint int64 = math.MinInt64, math.MaxInt64

	for {
		entry, err := p.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, 0, fmt.Errorf("next: %w", err)
		}

		if entry != textparse.EntrySeries {
			continue
		}

		_, ts, _ := p.Series()
		if ts == nil {
			return 0, 0, fmt.Errorf("expected timestamp for series got none")
		}

		if *ts > maxt {
			maxt = *ts
		}
		if *ts < mint {
			mint = *ts
		}
	}

	if maxt == math.MinInt64 {
		maxt = 0
	}
	if mint == math.MaxInt64 {
		mint = 0
	}

	return maxt, mint, nil
}

func getCompatibleBlockDuration(maxBlockDuration int64) int64 {
	blockDuration := tsdb.DefaultBlockDuration
	if maxBlockDuration > tsdb.DefaultBlockDuration {
		ranges := tsdb.ExponentialBlockRanges(tsdb.DefaultBlockDuration, 10, 3)
		idx := len(ranges) - 1 // Use largest range if user asked for something enormous.
		for i, v := range ranges {
			if v > maxBlockDuration {
				idx = i - 1
				break
			}
		}
		blockDuration = ranges[idx]
	}
	return blockDuration
}

// https://github.com/prometheus/prometheus/blob/main/cmd/promtool/backfill.go#L87
func createBlocks(input []byte, mint, maxt, maxBlockDuration int64, maxSamplesInAppender int, outputDir string, humanReadable bool, lbls labels.Labels, logger log.Logger) (ids []ulid.ULID, returnErr error) {
	blockDuration := getCompatibleBlockDuration(maxBlockDuration)
	mint = blockDuration * (mint / blockDuration)

	db, err := tsdb.OpenDBReadOnly(outputDir, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		returnErr = tsdb_errors.NewMulti(returnErr, db.Close()).Err()
	}()

	var (
		wroteHeader  bool
		nextSampleTs int64 = math.MaxInt64
	)

	for t := mint; t <= maxt; t += blockDuration {
		tsUpper := t + blockDuration
		if nextSampleTs != math.MaxInt64 && nextSampleTs >= tsUpper {
			// The next sample is not in this timerange, we can avoid parsing
			// the file for this timerange.
			continue
		}
		nextSampleTs = math.MaxInt64

		err := func() error {
			// To prevent races with compaction, a block writer only allows appending samples
			// that are at most half a block size older than the most recent sample appended so far.
			// However, in the way we use the block writer here, compaction doesn't happen, while we
			// also need to append samples throughout the whole block range. To allow that, we
			// pretend that the block is twice as large here, but only really add sample in the
			// original interval later.
			w, err := tsdb.NewBlockWriter(logger, outputDir, 2*blockDuration)
			if err != nil {
				return fmt.Errorf("block writer: %w", err)
			}
			defer func() {
				err = tsdb_errors.NewMulti(err, w.Close()).Err()
			}()

			ctx := context.Background()
			app := w.Appender(ctx)
			p := textparse.NewPromParser(input)
			samplesCount := 0
			for {
				e, err := p.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return fmt.Errorf("parse: %w", err)
				}
				if e != textparse.EntrySeries {
					continue
				}

				_, ts, v := p.Series()
				if ts == nil {
					l := labels.Labels{}
					p.Metric(&l)
					return fmt.Errorf("expected timestamp for series %v, got none", l)
				}
				if *ts < t {
					continue
				}
				if *ts >= tsUpper {
					if *ts < nextSampleTs {
						nextSampleTs = *ts
					}
					continue
				}

				l := labels.Labels{}
				p.Metric(&l)

				if _, err := app.Append(0, l, *ts, v); err != nil {
					return fmt.Errorf("add sample: %w", err)
				}

				samplesCount++
				if samplesCount < maxSamplesInAppender {
					continue
				}

				// If we arrive here, the samples count is greater than the maxSamplesInAppender.
				// Therefore the old appender is committed and a new one is created.
				// This prevents keeping too many samples lined up in an appender and thus in RAM.
				if err := app.Commit(); err != nil {
					return fmt.Errorf("commit: %w", err)
				}

				app = w.Appender(ctx)
				samplesCount = 0
			}

			if err := app.Commit(); err != nil {
				return fmt.Errorf("commit: %w", err)
			}

			blk, err := w.Flush(ctx)
			switch {
			case err == nil:
				ids = append(ids, blk)
				blocks, err := db.Blocks()
				if err != nil {
					return fmt.Errorf("get blocks: %w", err)
				}
				for _, b := range blocks {
					if b.Meta().ULID == blk {
						if err = writeThanosMeta(b.Meta(), lbls.Map(), 0, outputDir, logger); err != nil {
							return fmt.Errorf("write metadata: %w", err)
						}
						printBlocks([]tsdb.BlockReader{b}, !wroteHeader, humanReadable)
						wroteHeader = true
						break
					}
				}
			case errors.Is(err, tsdb.ErrNoSeriesAppended):
			default:
				return fmt.Errorf("flush: %w", err)
			}

			return nil
		}()
		if err != nil {
			return nil, fmt.Errorf("process blocks: %w", err)
		}
	}
	return ids, nil
}

func printBlocks(blocks []tsdb.BlockReader, writeHeader, humanReadable bool) {
	tw := tabwriter.NewWriter(os.Stdout, 13, 0, 2, ' ', 0)
	defer tw.Flush()

	if writeHeader {
		fmt.Fprintln(tw, "BLOCK ULID\tMIN TIME\tMAX TIME\tDURATION\tNUM SAMPLES\tNUM CHUNKS\tNUM SERIES\tSIZE")
	}

	for _, b := range blocks {
		meta := b.Meta()

		fmt.Fprintf(tw,
			"%v\t%v\t%v\t%v\t%v\t%v\t%v\t%v\n",
			meta.ULID,
			getFormatedTime(meta.MinTime, humanReadable),
			getFormatedTime(meta.MaxTime, humanReadable),
			time.Duration(meta.MaxTime-meta.MinTime)*time.Millisecond,
			meta.Stats.NumSamples,
			meta.Stats.NumChunks,
			meta.Stats.NumSeries,
			getFormatedBytes(b.Size(), humanReadable),
		)
	}
}

func getFormatedTime(timestamp int64, humanReadable bool) string {
	if humanReadable {
		return time.Unix(timestamp/1000, 0).UTC().String()
	}
	return strconv.FormatInt(timestamp, 10)
}

func getFormatedBytes(bytes int64, humanReadable bool) string {
	if humanReadable {
		return units.Base2Bytes(bytes).String()
	}
	return strconv.FormatInt(bytes, 10)
}

func writeThanosMeta(meta tsdb.BlockMeta, lbls map[string]string, res int64, dir string, logger log.Logger) error {
	m := metadata.Meta{
		BlockMeta: meta,
		Thanos: metadata.Thanos{
			Version:    metadata.ThanosVersion1,
			Labels:     lbls,
			Downsample: metadata.ThanosDownsample{Resolution: res},
			Source:     "thanos-kit",
		},
	}
	return m.WriteToDir(logger, dir+"/"+meta.ULID.String())
}
