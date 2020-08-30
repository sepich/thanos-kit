package main

import (
	"fmt"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/version"
	"github.com/thanos-io/thanos/pkg/extflag"
	"gopkg.in/alecthomas/kingpin.v2"
	"os"
	"path/filepath"
	"strings"
)

const extpromPrefix = "thanos_bucket_"
var (
	inspectColumns = []string{"ULID", "FROM", "RANGE", "LVL", "RES", "#SAMPLES", "#CHUNKS", "LABELS", "SRC" }
)

func main() {
	app := kingpin.New(filepath.Base(os.Args[0]), "Tooling to work with Thanos blobs in object storage")
	app.HelpFlag.Short('h')
	logLevel := app.Flag("log.level", "Log filtering level (info, debug).").
		Default("info").Enum("error", "warn", "info", "debug")

	cmdInspect := app.Command("inspect", "Inspect all blocks in the bucket in detailed, table-like way")
	objStoreConfig := regCommonObjStoreFlags(cmdInspect, "", true)
	selector := cmdInspect.Flag("selector", "Selects blocks based on label, e.g. '-l key1=\\\"value1\\\" -l key2=\\\"value2\\\"'. All key value pairs must match.").Short('l').
		PlaceHolder("<name>=\\\"<value>\\\"").Strings()
	sortBy := cmdInspect.Flag("sort-by", "Sort by columns. It's also possible to sort by multiple columns, e.g. '--sort-by FROM --sort-by LABELS'. I.e., if the 'FROM' value is equal the rows are then further sorted by the 'LABELS' value.").
		Default("FROM", "LABELS").Enums(inspectColumns...)

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
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	switch parsedCmd {
		case cmdInspect.FullCommand():
			os.Exit(checkErr(Inspect(objStoreConfig, selector, *sortBy, logger, metrics)))
	}
	fmt.Println(parsedCmd, objStoreConfig, selector, logLevel)
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
