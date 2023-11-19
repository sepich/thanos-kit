# thanos-kit
Tooling to work with Thanos blocks in object storage.

- **ls** - List all blocks ULIDs in the bucket, also show ULID as time (same as `thanos tools bucket ls` but with mimir support)
- **inspect** - Inspect all blocks in the bucket in detailed, table-like way (same as `thanos tools bucket inspect` but with mimir support)
- **analyze** - Analyze churn, label pair cardinality for specific block. (same as `promtool tsdb analyze` but also show Labels suitable for block split)
- **dump** - Dump samples from a TSDB to text format (same as `promtool tsdb dump` but to promtext format)
- **import** - Import samples to TSDB blocks (same as `promtool tsdb create-blocks-from openmetrics` but from promtext format). Read more about [backfill](#backfill) below

Cli arguments are mostly the same as for `thanos`, help is available for each sub-command:
```
$ docker run sepa/thanos-kit -h
usage: thanos-kit [<flags>] <command> [<args> ...]

Tooling to work with Thanos blocks in object storage

Flags:
  -h, --help            Show context-sensitive help (also try --help-long and --help-man).
      --version         Show application version.
      --log.level=info  Log filtering level (info, debug)
      --objstore.config-file=<file-path>
                        Path to YAML file that contains object store%s configuration. See format details: https://thanos.io/tip/thanos/storage.md/
      --objstore.config=<content>
                        Alternative to 'objstore.config-file' flag (mutually exclusive). Content of YAML file that contains object store%s configuration. See format details: https://thanos.io/tip/thanos/storage.md/

Commands:
  help [<command>...]
    Show help.

  ls [<flags>]
    List all blocks in the bucket.

  inspect [<flags>]
    Inspect all blocks in the bucket in detailed, table-like way

  analyze [<flags>] <ULID>
    Analyze churn, label pair cardinality and find labels to split on

  dump [<flags>] <ULID>...
    Dump samples from a TSDB to text

  import --input-file=INPUT-FILE --label=<name>="<value>" [<flags>]
    Import samples from text to TSDB blocks
```

### Get it
Docker images are available on [Docker Hub](https://hub.docker.com/repository/docker/sepa/thanos-kit/tags)

### Backfill
([Original PR](https://github.com/prometheus/prometheus/pull/7586))  
Supported input format is Prometheus text format.
You are free to export/convert your existing data to this format, into one **time-sorted** text file.

`metric{[labels]} value timestamp_ms`

For example:
```ini
k8s_ns_hourly_cost{namespace="kube-system"} 5.7 1599771600000
k8s_ns_hourly_cost{namespace="kube-system"} 5.0 1599771630000
...
```

Note, `value` can be mixed as normal or scientific number as per your preference.

This format is simple to produce, but not optimized or compressed, so it's normal if your data file is huge.  
Example of a 19G OpenMetrics file, with ~20k timeseries and 200M data points (samples) on 2y period. Globally resolution is very low in this example.
Import will take around 2h and uncompacted new TSDB blocks will be around 2.1G for 7600 blocks. When thanos-compact scan them, it starts automatically compacting them in the background. Once compaction is completed (~30min), TSDB blocks will be around 970M for 80 blocks.  
The size, and number of blocks depends on timeseries numbers and metrics resolution, but it gives you an order of sizes.

Apart from labels set for each metric in text file, you would also need to set Thanos Metadata Labels for the whole batch of blocks you are importing (consider this as prometheus `external_labels` which scraped the metrics from the text file)

Example of command for importing data from `data.prom` (above) to GCS bucket `bucketname`:
```bash
docker run -it --rm \ 
    -v `pwd`:/work -w /work \
    -e GOOGLE_APPLICATION_CREDENTIALS=/work/svc.json \ 
    sepa/thanos-kit import \
        --objstore.config='{type: GCS, config: {bucket: bucketname}}' \
        --input-file data.prom \
        --label=replica=\"prom-a\" \ 
        --label=location=\"us-east1\"
```
Please note that compactor has default `--consistency-delay=30m` which is based on file upload time (not ULID), so it could take some time before compactor would start processing these blocks.

### Cache dir
By default, `thanos-kit` will cache blocks from object storage to `./data` directory, and the dir is not cleaned up on exit. This is to speed up subsequent runs, and to avoid deleting user data when `--data-dir=/tmp` is used for example.

Important note that `dump` command downloads specified blocks to cache dir, but then dump TSDB as a whole (including blocks already present there)

### Alternatives
- [thanos tools bucket](https://thanos.io/tip/components/tools.md/#bucket)
- [promtool tsdb](https://prometheus.io/docs/prometheus/latest/command-line/promtool/#promtool-tsdb)
- [mimirtool](https://grafana.com/docs/mimir/latest/manage/tools/mimirtool/#backfill)