package main

import (
	"fmt"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/version"
	"github.com/thanos-io/thanos/pkg/extflag"
	"gopkg.in/alecthomas/kingpin.v2"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const extpromPrefix = "thanos_bucket_"

var (
	inspectColumns = []string{"ULID", "FROM", "RANGE", "LVL", "RES", "#SAMPLES", "#CHUNKS", "LABELS", "SRC"}
)

func main() {
	app := kingpin.New(filepath.Base(os.Args[0]), "Tooling to work with Thanos blobs in object storage").Version(version.Print("thanos-kit"))
	app.HelpFlag.Short('h')
	logLevel := app.Flag("log.level", "Log filtering level (info, debug).").
		Default("info").Enum("error", "warn", "info", "debug")

	inspectCmd := app.Command("inspect", "Inspect all blocks in the bucket in detailed, table-like way")
	inspectStoreConfig := regCommonObjStoreFlags(inspectCmd, "", true)
	inspectSelector := inspectCmd.Flag("selector", "Selects blocks based on label, e.g. '-l key1=\\\"value1\\\" -l key2=\\\"value2\\\"'. All key value pairs must match.").Short('l').
		PlaceHolder("<name>=\\\"<value>\\\"").Strings()
	inspectSortBy := inspectCmd.Flag("sort-by", "Sort by columns. It's also possible to sort by multiple columns, e.g. '--sort-by FROM --sort-by LABELS'. I.e., if the 'FROM' value is equal the rows are then further sorted by the 'LABELS' value.").
		Default("FROM", "LABELS").Enums(inspectColumns...)

	analyzeCmd := app.Command("analyze", "Analyze churn, label pair cardinality.")
	analyzeStoreConfig := regCommonObjStoreFlags(analyzeCmd, "", true)
	analyzeULID := analyzeCmd.Arg("ULID", "Block id to analyze (ULID).").Required().String()
	analyzeLimit := analyzeCmd.Flag("limit", "How many items to show in each list.").Default("20").Int()
	analyzeDataDir := analyzeCmd.Flag("data-dir", "Data directory in which to cache blocks").
		Default("./data").String()

	dumpCmd := app.Command("dump", "Dump samples from a TSDB.")
	dumpStoreConfig := regCommonObjStoreFlags(dumpCmd, "", true)
	dumpULIDs := dumpCmd.Arg("ULID", "Block(s) id (ULID) to dump (multiple ids should be separated by space)").Required().Strings()
	dumpDataDir := dumpCmd.Flag("data-dir", "Data directory in which to cache blocks").
		Default("./data").String()
	dumpMinTime := dumpCmd.Flag("min-time", "Minimum timestamp to dump.").Default("0").Int64()
	dumpMaxTime := dumpCmd.Flag("max-time", "Maximum timestamp to dump.").Default(strconv.FormatInt(math.MaxInt64, 10)).Int64()

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

	switch parsedCmd {
	case inspectCmd.FullCommand():
		os.Exit(checkErr(inspect(inspectStoreConfig, inspectSelector, *inspectSortBy, logger, metrics)))
	case analyzeCmd.FullCommand():
		os.Exit(checkErr(analyze(analyzeStoreConfig, analyzeULID, analyzeDataDir, analyzeLimit, logger, metrics)))
	case dumpCmd.FullCommand():
		os.Exit(checkErr(dump(dumpStoreConfig, dumpULIDs, dumpDataDir, dumpMinTime, dumpMaxTime, logger, metrics)))
	}
	fmt.Println(parsedCmd, inspectStoreConfig, inspectSelector, logLevel)
}

func regCommonObjStoreFlags(cmd *kingpin.CmdClause, suffix string, required bool, extraDesc ...string) *extflag.PathOrContent {
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
