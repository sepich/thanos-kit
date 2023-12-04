# thanos-kit
Tooling to work with Thanos blocks in object storage.

- **ls** - List all blocks ULIDs in the bucket, also show ULID as time (same as `thanos tools bucket ls` but with mimir support)
- **inspect** - Inspect all blocks in the bucket in detailed, table-like way (same as `thanos tools bucket inspect` but with mimir support)
- **analyze** - Analyze churn, label pair cardinality for specific block. (same as `promtool tsdb analyze` but also show Labels suitable for block split)
- **dump** - Dump samples from a TSDB to text format (same as `promtool tsdb dump` but to promtext format)
- **import** - Import samples to TSDB blocks (same as `promtool tsdb create-blocks-from openmetrics` but from promtext format). Read more about [backfill](#backfill) below
- **unwrap** - Split one TSDB block to multiple based on Label values. Read more [below](#unwrap)

Cli arguments are mostly the same as for `thanos`, help is available for each sub-command:
```
$ docker run sepa/thanos-kit -h
usage: thanos-kit [<flags>] <command> [<args> ...]

Tooling for Thanos blocks in object storage

Flags:
  -h, --help            Show context-sensitive help (also try --help-long and --help-man).
      --version         Show application version.
      --log.level=info  Log filtering level (info, debug)
      --objstore.config-file=<file-path>
                        Path to YAML file that contains object store configuration. See format details: https://thanos.io/tip/thanos/storage.md/
      --objstore.config=<content>
                        Alternative to 'objstore.config-file' flag (mutually exclusive). Content of YAML file that contains object store configuration. See format details: https://thanos.io/tip/thanos/storage.md/

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

  unwrap [<flags>]
    Split TSDB block to multiple blocks by Label
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

### Unwrap

This could be useful for incorporating Mimir to Thanos world by replacing thanos-receive component. Currently Mimir could accept remote-write, and do instant queries via [sidecar](https://grafana.com/docs/mimir/latest/set-up/migrate/migrate-from-thanos-to-mimir-with-thanos-sidecar/) scheme. But long-term queries via thanos-store would not work with Mimir blocks, as they have no Thanos metadata set. 

Consider this example, we have 2 prometheuses in different locations configured like so:
```yml
# first
global:
  external_labels:
    prometheus: A
    location: dc1

# second
global:
  external_labels:
    prometheus: B
    location: dc2
```

And they do remote-write to Mimir, let's take a look at produced block:
```bash
# thanos-kit analyze
Block ID: 01GXGKXC3PA1DE6QNAH2BM2P0R
Thanos Labels:
Label names appearing in all Series: [instance, job, prometheus, location]
```

The block has no Thanos labels set, and each metric inside has labels `[prometheus, location]` coming from external_labels. We can split this block to 2 separate blocks for each original prometheus like this:
```bash
# thanos-kit unwrap --relabel-config='[{target_label: __meta_ext_labels, replacement: prometheus}]'
uploaded block ulid=001
uploaded block ulid=002

# thanos-kit analyze 001
Block ID: 001
Thanos Labels: prometheus=A
Label names appearing in all Series: [instance, job, location]

# thanos-kit analyze 002
Block ID: 002
Thanos Labels: prometheus=B
Label names appearing in all Series: [instance, job, location]
```
Unwrap command takes usual [relabel_config](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#relabel_config) and can modify block data. But additionally special label `__meta_ext_labels` is parsed if assigned, and all label names in it are extracted to unique blocks. You can specify multiple values separated by `;`, in example above to split original Mimir block by both prometheus+location sets:
```yaml
  - |
    --relabel-config=
    - target_label: __meta_ext_labels
      replacement: prometheus;location
```
And resulting blocks are the same, as would be shipped by `thanos-sidecar`. So they would be compacted without duplicates and wasted space on S3 by compactor afterward. 

This could also be used for blocks produced by thanos-receive too, existing Thanos labels would be merged with extracted ones. Smaller blocks ease thanos-compactor/store sharding, and helps to have different retentions.

### Alternatives
- [thanos tools bucket](https://thanos.io/tip/components/tools.md/#bucket)
- [promtool tsdb](https://prometheus.io/docs/prometheus/latest/command-line/promtool/#promtool-tsdb)
- [mimirtool](https://grafana.com/docs/mimir/latest/manage/tools/mimirtool/#backfill)
