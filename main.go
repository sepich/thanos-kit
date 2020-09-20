package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/version"
	"github.com/thanos-io/thanos/pkg/extflag"
	"gopkg.in/alecthomas/kingpin.v2"
)

const extpromPrefix = "thanos_bucket_"

func main() {
	app := kingpin.New(filepath.Base(os.Args[0]), "Tooling to work with Thanos blobs in object storage").Version(version.Print("thanos-kit"))
	app.HelpFlag.Short('h')
	logLevel := app.Flag("log.level", "Log filtering level (info, debug)").
		Default("info").Enum("error", "warn", "info", "debug")
	objStoreConfig := regCommonObjStoreFlags(app, "", true)

	inspectCmd := app.Command("inspect", "Inspect all blocks in the bucket in detailed, table-like way")
	inspectSelector := inspectCmd.Flag("label", `Selects blocks based on label, e.g. '-l key1="value1" -l key2="value2"'. All key value pairs must match. To select all blocks for some key use "*" as value.`).Short('l').PlaceHolder(`<name>="<value>"`).Strings()
	inspectSortBy := inspectCmd.Flag("sort-by", "Sort by columns. It's also possible to sort by multiple columns, e.g. '--sort-by FROM --sort-by LABELS'. I.e., if the 'FROM' value is equal the rows are then further sorted by the 'LABELS' value").
		Default("FROM", "LABELS").Enums(inspectColumns...)

	analyzeCmd := app.Command("analyze", "Analyze churn, label pair cardinality")
	analyzeULID := analyzeCmd.Arg("ULID", "Block id to analyze (ULID)").Required().String()
	analyzeLimit := analyzeCmd.Flag("limit", "How many items to show in each list").Default("20").Int()
	analyzeDataDir := analyzeCmd.Flag("data-dir", "Data directory in which to cache blocks").
		Default("./data").String()

	dumpCmd := app.Command("dump", "Dump samples from a TSDB")
	dumpULIDs := dumpCmd.Arg("ULID", "Block(s) id (ULID) to dump (multiple ids should be separated by space)").Required().Strings()
	dumpDataDir := dumpCmd.Flag("data-dir", "Data directory in which to cache blocks").Default("./data").String()
	dumpMinTime := dumpCmd.Flag("min-time", "Minimum timestamp to dump").Default("0").Int64()
	dumpMaxTime := dumpCmd.Flag("max-time", "Maximum timestamp to dump").Default(strconv.FormatInt(math.MaxInt64, 10)).Int64()

	importCmd := app.Command("import", "Import samples to TSDB blocks")
	importFromFile := importCmd.Flag("input-file", "disables reading from stdin and using file to import samples from. If empty input is required").String()
	importBlockSize := importCmd.Flag("block-size", "The maximum block size. The actual block timestamps will be aligned with Prometheus time ranges").Default("2h").Hidden().Duration()
	importDataDir := importCmd.Flag("data-dir", "Data directory in which to cache blocks").
		Default("./data").String()
	importLabels := importCmd.Flag("label", "Labels to add as Thanos block metadata (repeated)").Short('l').PlaceHolder(`<name>="<value>"`).Required().Strings()

	parsedCmd := kingpin.MustParse(app.Parse(os.Args[1:]))
	var logger log.Logger
	{
		var lvl level.Option
		switch *logLevel {
		case "debug":
			lvl = level.AllowDebug()
		default:
			lvl = level.AllowInfo()
		}
		logger = log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
		logger = level.NewFilter(logger, lvl)
		logger = log.With(logger, "ts", log.DefaultTimestampUTC, "caller", log.DefaultCaller)
	}
	metrics := prometheus.NewRegistry()
	metrics.MustRegister(
		version.NewCollector("thanos"),
		prometheus.NewGoCollector(),
	)
	objStoreYaml, err := objStoreConfig.Content()
	if err != nil {
		os.Exit(checkErr(err))
	}

	switch parsedCmd {
	case inspectCmd.FullCommand():
		os.Exit(checkErr(inspect(objStoreYaml, inspectSelector, *inspectSortBy, logger, metrics)))
	case analyzeCmd.FullCommand():
		os.Exit(checkErr(analyze(objStoreYaml, analyzeULID, analyzeDataDir, analyzeLimit, logger, metrics)))
	case dumpCmd.FullCommand():
		os.Exit(checkErr(dump(objStoreYaml, dumpULIDs, dumpDataDir, dumpMinTime, dumpMaxTime, os.Stdout, logger, metrics)))
	case importCmd.FullCommand():
		os.Exit(checkErr(backfill(objStoreYaml, importFromFile, importBlockSize, importDataDir, importLabels, logger, metrics)))
	}
}

func regCommonObjStoreFlags(cmd *kingpin.Application, suffix string, required bool, extraDesc ...string) *extflag.PathOrContent {
	help := fmt.Sprintf("YAML file that contains object store%s configuration. See format details: https://thanos.io/storage.md/#configuration ", suffix)
	help = strings.Join(append([]string{help}, extraDesc...), " ")

	return extflag.RegisterPathOrContent(cmd, fmt.Sprintf("objstore%s.config", suffix), help, required)
}

func checkErr(err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
