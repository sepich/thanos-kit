# thanos-kit
Aftermarket thanos tools and utilities.  

- `inspect` - Inspect all blocks in the bucket in detailed, table-like way (same as `thanos tools bucket inspect`)
- `analyze` - Analyze churn, label pair cardinality for specific block
- `dump` - Dump samples from a TSDB to text format
- `import` - Import samples to TSDB blocks. Read about [Backfill](#backfill) below

Cli arguments mostly are the same as for `thanos`, help is available for each sub-command:  
```
$ ./thanos-kit -h
usage: thanos-kit [<flags>] <command> [<args> ...]

Tooling to work with Thanos blobs in object storage

Flags:
  -h, --help            Show context-sensitive help (also try --help-long and --help-man).
      --version         Show application version.
      --log.level=info  Log filtering level (info, debug)

Commands:
  help [<command>...]
    Show help.

  inspect [<flags>]
    Inspect all blocks in the bucket in detailed, table-like way

  analyze [<flags>] <ULID>
    Analyze churn, label pair cardinality

  dump [<flags>] <ULID>...
    Dump samples from a TSDB

  import --label=<name>="<value>" [<flags>]
    Import samples to TSDB blocks
```

### Get it
Master builds are available on [Docker Hub](https://hub.docker.com/repository/docker/sepa/thanos-kit/tags)

### Backfill

([Original PR](https://github.com/prometheus/prometheus/pull/7586))  
You can use it to restore another prometheus partial dump, or any metrics exported from any system. The only supported input format is Prometheus text format.
You are free to export/convert your existing data to this format, into one **time-sorted** text file.

Sample file `rrd_exported_data.txt` (`[metric]{[labels]} [number value] [timestamp ms]`):
```ini
collectd_df_complex{host="myserver.fqdn.com",df="var-log",dimension="free"} 5.8093906125e+10 1599771600000
collectd_varnish_derive{host="myserver.fqdn.com",varnish="request_rate",dimension="MAIN.client_req"} 2.3021666667e+01 1599771600000
collectd_df_complex{host="myserver.fqdn.com",df="var-log",dimension="free"} 5.8093906125e+10 1599771615000
collectd_load{host="myserver.fqdn.com",type="midterm"} 0.0155 1599771615000
collectd_varnish_derive{host="myserver.fqdn.com",varnish="request_rate",dimension="MAIN.client_req"} 2.3021666667e+01 1599771630000
collectd_load{host="myserver.fqdn.com",type="midterm"} 1.5500000000e-02 1599771630000
collectd_varnish_derive{host="myserver.fqdn.com",varnish="request_rate",dimension="MAIN.cache_hit"} 3.8054166667e+01 1599771645000
collectd_df_complex{host="myserver.fqdn.com",df="var-log",dimension="free"} 5.8093906125e+10 1599771645000
collectd_varnish_derive{host="myserver.fqdn.com",varnish="request_rate",dimension="MAIN.s_pipe"} 0 1599771660000
collectd_varnish_derive{host="myserver.fqdn.com",varnish="request_rate",dimension="MAIN.cache_hit"} 3.8054166667e+01 1599771675000
collectd_load{host="myserver.fqdn.com",type="shortterm"} 1.1000000000e-02 1599771675000
collectd_varnish_derive{host="myserver.fqdn.com",varnish="request_rate",dimension="MAIN.client_req"} 2.3021666667e+01 1599771690000
collectd_load{host="myserver.fqdn.com",type="shortterm"} 1.1000000000e-02 1599771690000
collectd_load{host="myserver.fqdn.com",type="shortterm"} 1.1000000000e-02 1599771705000
collectd_load{host="myserver.fqdn.com",type="longterm"} 2.5500000000e-02 1599771720000
collectd_varnish_derive{host="myserver.fqdn.com",varnish="request_rate",dimension="MAIN.s_pipe"} 0 1599771735000
collectd_load{host="myserver.fqdn.com"type="longterm"} 2.5500000000e-02 1599771735000
collectd_varnish_derive{host="myserver.fqdn.com",varnish="request_rate",dimension="MAIN.cache_hit"} 3.8054166667e+01 1599771750000
collectd_load{host="myserver.fqdn.com",type="midterm"} 1.5500000000e-02 1599771750000
```

Note, `[number value]` can be mixed as normal or scientific number as per your preference. You are free to put custom labels on each metric, don't forget that "relabelling rules" defined in prometheus will not be applied on them! You should produce the final labels on your import file.

This format is simple to produce, but not optimized or compressed, so it's normal if your data file is huge.  
Example of a 19G OpenMetrics file, with ~20k timeseries and 200M data points (samples) on 2y period. Globally resolution is very very low in this example. 
Import will take around 2h and uncompacted new TSDB blocks will be around 2.1G for 7600 blocks. When prometheus scan them, it starts automatically compacting them in the background. Once compaction is completed (~30min), TSDB blocks will be around 970M for 80 blocks (without loss of data points).  
The size, and number of blocks depends on timeseries numbers and metrics resolution, but it gives you an order of sizes.

Apart from labels set for each metric in text file, you would also need to set Thanos Metadata Labels for the whole batch of blocks you are importing (consider this as prometheus `external_labels` which scraped the metrics from text file)

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
Current implementation reads all imported data into memory, and then flushes as TSDB 2h blocks. In case you have OOM but want to import large amount of data, you can split imported file by days (or 2h windows) and do multiple separate imports. Anyway these separate blocks would be compacted to larger one on bucket side via your compactor.
