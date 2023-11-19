package main

import (
	"fmt"
	"github.com/efficientgo/tools/extkingpin"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/common/version"
	"github.com/thanos-io/objstore/client"
	"gopkg.in/alecthomas/kingpin.v2"
	"math"
	"os"
	"path/filepath"
	"strconv"
)

func main() {
	app := kingpin.New(filepath.Base(os.Args[0]), "Tooling for Thanos blocks in object storage").Version(version.Print("thanos-split"))
	app.HelpFlag.Short('h')
	logLevel := app.Flag("log.level", "Log filtering level (info, debug)").Default("info").Enum("error", "warn", "info", "debug")
	objStoreConfig := extkingpin.RegisterPathOrContent(app, "objstore.config", "YAML file that contains object store%s configuration. See format details: https://thanos.io/tip/thanos/storage.md/ ", extkingpin.WithEnvSubstitution(), extkingpin.WithRequired())

	lsCmd := app.Command("ls", "List all blocks in the bucket.")
	recursive := lsCmd.Flag("recursive", "Recurse one level into bucket to get blocks (Mimir has tenants on top level)").Short('r').Default("false").Bool()

	inspectCmd := app.Command("inspect", "Inspect all blocks in the bucket in detailed, table-like way")
	recursive = inspectCmd.Flag("recursive", "Recurse one level into bucket to get blocks (Mimir has tenants on top level)").Short('r').Default("false").Bool()
	inspectSelector := inspectCmd.Flag("label", `Filter by Thanos block label, e.g. '-l key1="value1" -l key2="value2"'. All key value pairs must match. To select all blocks for some key use "*" as value.`).Short('l').PlaceHolder(`<name>="<value>"`).Strings()
	inspectSortBy := inspectCmd.Flag("sort-by", "Sort by columns. It's also possible to sort by multiple columns, e.g. '--sort-by FROM --sort-by LABELS'. I.e., if the 'FROM' value is equal the rows are then further sorted by the 'LABELS' value").
		Default("FROM", "LABELS").Enums(inspectColumns...)

	analyzeCmd := app.Command("analyze", "Analyze churn, label pair cardinality and find labels to split on")
	analyzeULID := analyzeCmd.Arg("ULID", "Block id to analyze (ULID)").Required().String()
	analyzeLimit := analyzeCmd.Flag("limit", "How many items to show in each list").Default("20").Int()
	analyzeDataDir := analyzeCmd.Flag("data-dir", "Data directory in which to cache blocks").
		Default("./data").String()
	analyzeMatchers := analyzeCmd.Flag("match", "Series selector to analyze. Only 1 set of matchers is supported now.").String()

	dumpCmd := app.Command("dump", "Dump samples from a TSDB to text")
	dumpULIDs := dumpCmd.Arg("ULID", "Blocks id (ULID) to dump (repeated)").Required().Strings()
	dumpDataDir := dumpCmd.Flag("data-dir", "Data directory in which to cache blocks").Default("./data").String()
	dumpMinTime := dumpCmd.Flag("min-time", "Minimum timestamp to dump").Default("0").Int64()
	dumpMaxTime := dumpCmd.Flag("max-time", "Maximum timestamp to dump").Default(strconv.FormatInt(math.MaxInt64, 10)).Int64()
	dumpMatch := dumpCmd.Flag("match", "Series selector.").Default("{__name__=~'(?s:.*)'}").String()

	importCmd := app.Command("import", "Import samples from text to TSDB blocks")
	importFromFile := importCmd.Flag("input-file", "Promtext file to read samples from.").Short('f').Required().String()
	importBlockSize := importCmd.Flag("block-size", "The maximum block size. The actual block timestamps will be aligned with Prometheus time ranges").Default("2h").Duration()
	importDataDir := importCmd.Flag("data-dir", "Data directory in which to cache blocks").
		Default("./data").String()
	importLabels := importCmd.Flag("label", "Labels to add as Thanos block metadata (repeated)").Short('l').PlaceHolder(`<name>="<value>"`).Required().Strings()
	importUpload := importCmd.Flag("upload", "Upload imported blocks to object storage").Default("false").Bool()

	cmd := kingpin.MustParse(app.Parse(os.Args[1:]))
	var logger log.Logger
	{
		lvl := level.AllowInfo()
		if *logLevel == "debug" {
			lvl = level.AllowDebug()
		}
		logger = log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
		logger = level.NewFilter(logger, lvl)
		logger = log.With(logger, "ts", log.DefaultTimestampUTC, "caller", log.DefaultCaller)
	}

	objStoreYaml, err := objStoreConfig.Content()
	if err != nil {
		exitCode(err)
	}
	bkt, err := client.NewBucket(logger, objStoreYaml, "thanos-kit")
	if err != nil {
		exitCode(err)
	}

	switch cmd {
	case lsCmd.FullCommand():
		exitCode(ls(bkt, recursive))
	case inspectCmd.FullCommand():
		exitCode(inspect(bkt, recursive, inspectSelector, inspectSortBy, logger))
	case analyzeCmd.FullCommand():
		exitCode(analyze(bkt, analyzeULID, analyzeDataDir, analyzeLimit, analyzeMatchers, logger))
	case dumpCmd.FullCommand():
		exitCode(dump(bkt, os.Stdout, dumpULIDs, dumpDataDir, dumpMinTime, dumpMaxTime, dumpMatch, logger))
	case importCmd.FullCommand():
		exitCode(importMetrics(bkt, importFromFile, importBlockSize, importDataDir, importLabels, *importUpload, logger))
	}
}

func exitCode(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return
}
