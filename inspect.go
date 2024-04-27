package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-kit/log"
	"github.com/oklog/ulid"
	"github.com/olekukonko/tablewriter"
	"github.com/pkg/errors"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/thanos-io/objstore"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	mtd "github.com/thanos-io/thanos/pkg/model"
	"github.com/thanos-io/thanos/pkg/runutil"
	"golang.org/x/sync/errgroup"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const concurrency = 32

var (
	inspectColumns = []string{"DIR", "ULID", "FROM", "RANGE", "LVL", "RES", "#SAMPLES", "#CHUNKS", "LABELS", "SRC"}
)

type Meta struct {
	metadata.Meta
	Prefix string
}

func inspect(bkt objstore.Bucket, recursive *bool, selector *[]string, sortBy *[]string, maxTime *mtd.TimeOrDurationValue, logger log.Logger) error {
	selectorLabels, err := parseFlagLabels(*selector)
	if err != nil {
		return errors.Wrap(err, "error parsing selector flag")
	}

	ctx := context.Background() // Ctrl-C instead of 5min limit
	metas, err := getAllMetas(ctx, bkt, *recursive, maxTime, logger)
	if err != nil {
		return err
	}

	return printTable(metas, selectorLabels, *sortBy, *recursive)
}

func parseFlagLabels(s []string) (labels.Labels, error) {
	var lset labels.Labels
	for _, l := range s {
		parts := strings.SplitN(l, "=", 2)
		if len(parts) != 2 {
			return nil, errors.Errorf("unrecognized label %q", l)
		}
		if !model.LabelName.IsValid(model.LabelName(parts[0])) {
			return nil, errors.Errorf("unsupported format for label %s", l)
		}
		val := parts[1]
		if strings.Index(val, `"`) != -1 {
			var err error
			val, err = strconv.Unquote(parts[1])
			if err != nil {
				return nil, errors.Wrap(err, "unquote label value")
			}
		}
		lset = append(lset, labels.Label{Name: parts[0], Value: val})
	}
	sort.Sort(lset)
	return lset, nil
}

// read mata.json from all blocks in bucket
func getAllMetas(ctx context.Context, bkt objstore.Bucket, recursive bool, maxTime *mtd.TimeOrDurationValue, logger log.Logger) (map[ulid.ULID]*Meta, error) {
	blocks, err := getBlocks(ctx, bkt, recursive, maxTime)
	if err != nil {
		return nil, err
	}
	res := make(map[ulid.ULID]*Meta, len(blocks))
	mu := sync.RWMutex{}
	eg := errgroup.Group{}
	ch := make(chan Block, concurrency)
	// workers
	for i := 0; i < concurrency; i++ {
		eg.Go(func() error {
			for b := range ch {
				m, err := getMeta(ctx, b, bkt, logger)
				if bkt.IsObjNotFoundErr(err) {
					continue // Meta.json was deleted between bkt.Exists and here.
				}
				if err != nil {
					return err
				}
				mu.Lock()
				res[b.Id] = &Meta{Meta: *m, Prefix: b.Prefix}
				mu.Unlock()
			}
			return nil
		})
	}

	// distribute work
	eg.Go(func() error {
		defer close(ch)
		for _, b := range blocks {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- b:
			}
		}
		return nil
	})

	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return res, nil
}

func printTable(blockMetas map[ulid.ULID]*Meta, selectorLabels labels.Labels, sortBy []string, recursive bool) error {
	var lines [][]string
	p := message.NewPrinter(language.English)

	for _, blockMeta := range blockMetas {
		if !matchesSelector(blockMeta, selectorLabels) {
			continue
		}

		timeRange := time.Duration((blockMeta.MaxTime - blockMeta.MinTime) * int64(time.Millisecond))

		var line []string
		if recursive {
			line = append(line, blockMeta.Prefix)
		}
		line = append(line, blockMeta.ULID.String())
		line = append(line, time.Unix(blockMeta.MinTime/1000, 0).Format("2006-01-02 15:04:05"))
		line = append(line, humanizeDuration(timeRange))
		line = append(line, p.Sprintf("%d", blockMeta.Compaction.Level))
		line = append(line, humanizeDuration(time.Duration(blockMeta.Thanos.Downsample.Resolution*int64(time.Millisecond))))
		line = append(line, p.Sprintf("%d", blockMeta.Stats.NumSamples))
		line = append(line, p.Sprintf("%d", blockMeta.Stats.NumChunks))
		line = append(line, labelsToString(blockMeta.Thanos.Labels))
		line = append(line, string(blockMeta.Thanos.Source))
		lines = append(lines, line)
	}

	if !recursive {
		inspectColumns = inspectColumns[1:]
	}
	var sortByColNum []int
	for _, col := range sortBy {
		index := getStrIndex(inspectColumns, col)
		if index == -1 {
			return errors.Errorf("column %s not found", col)
		}
		sortByColNum = append(sortByColNum, index)
	}

	t := Table{Header: inspectColumns, Lines: lines, SortIndices: sortByColNum}
	sort.Sort(t)

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader(inspectColumns)
	table.SetBorders(tablewriter.Border{Left: false, Top: false, Right: false, Bottom: false})
	table.SetCenterSeparator("|")
	table.SetAutoWrapText(false)
	table.SetReflowDuringAutoWrap(false)
	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.AppendBulk(lines)
	table.Render()

	return nil
}

// matchesSelector checks if blockMeta contains every label from
// the selector with the correct value.
func matchesSelector(blockMeta *Meta, selectorLabels labels.Labels) bool {
	for _, l := range selectorLabels {
		if v, ok := blockMeta.Thanos.Labels[l.Name]; !ok || (l.Value != "*" && v != l.Value) {
			return false
		}
	}
	return true
}

// getKeysAlphabetically return sorted keys of a given map
func getKeysAlphabetically(labels map[string]string) []string {
	var keys []string
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// labelsToString returns labels as comma separated k=v pairs
func labelsToString(lables map[string]string) string {
	pairs := []string{}
	for _, k := range getKeysAlphabetically(lables) {
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, lables[k]))
	}
	return strings.Join(pairs, ", ")
}

// humanizeDuration returns more humane string for duration
func humanizeDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d.Seconds() > 58 {
		d = d.Round(time.Minute)
	}
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// getStrIndex calculates the index of s in strings.
func getStrIndex(strings []string, s string) int {
	for i, col := range strings {
		if col == s {
			return i
		}
	}
	return -1
}

// Table implements sort.Interface
type Table struct {
	Header      []string
	Lines       [][]string
	SortIndices []int
}

func (t Table) Len() int { return len(t.Lines) }

func (t Table) Swap(i, j int) { t.Lines[i], t.Lines[j] = t.Lines[j], t.Lines[i] }

func (t Table) Less(i, j int) bool {
	for _, index := range t.SortIndices {
		if t.Lines[i][index] == t.Lines[j][index] {
			continue
		}
		return compare(t.Lines[i][index], t.Lines[j][index])
	}
	return compare(t.Lines[i][0], t.Lines[j][0])
}

// compare values can be either Time, Duration, comma-delimited integers or strings.
func compare(s1, s2 string) bool {
	s1Time, s1Err := time.Parse("2006-01-02 15:04:05", s1)
	s2Time, s2Err := time.Parse("2006-01-02 15:04:05", s2)
	if s1Err != nil || s2Err != nil {
		s1Duration, s1Err := time.ParseDuration(s1)
		s2Duration, s2Err := time.ParseDuration(s2)
		if s1Err != nil || s2Err != nil {
			s1Int, s1Err := strconv.ParseUint(strings.Replace(s1, ",", "", -1), 10, 64)
			s2Int, s2Err := strconv.ParseUint(strings.Replace(s2, ",", "", -1), 10, 64)
			if s1Err != nil || s2Err != nil {
				return s1 < s2
			}
			return s1Int < s2Int
		}
		return s1Duration < s2Duration
	}
	return s1Time.Before(s2Time)
}

// get block Metadata from bucket
func getMeta(ctx context.Context, b Block, bkt objstore.Bucket, logger log.Logger) (*metadata.Meta, error) {
	metaFile := b.Prefix + b.Id.String() + "/" + metadata.MetaFilename
	r, err := bkt.Get(ctx, metaFile)
	if bkt.IsObjNotFoundErr(err) {
		return nil, err // continue
	}
	if err != nil {
		return nil, errors.Wrapf(err, "get meta file: %v", metaFile)
	}

	defer runutil.CloseWithLogOnErr(logger, r, "close bkt meta get")

	metaContent, err := io.ReadAll(r)
	if err != nil {
		return nil, errors.Wrapf(err, "read meta file: %v", metaFile)
	}

	m := &metadata.Meta{}
	if err := json.Unmarshal(metaContent, m); err != nil {
		return nil, errors.Wrapf(err, "%s unmarshal", metaFile)
	}
	if m.Version != metadata.ThanosVersion1 {
		return nil, errors.Errorf("unexpected meta file: %s version: %d", metaFile, m.Version)
	}
	return m, nil
}
