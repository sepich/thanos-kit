# thanos-kit
Aftermarket thanos tools and utilities.  
Work in progress...
```
$ ./thanos-kit
usage: thanos-kit [<flags>] <command> [<args> ...]

Tooling to work with Thanos blobs in object storage

Flags:
  -h, --help            Show context-sensitive help (also try --help-long and --help-man).
      --log.level=info  Log filtering level (info, debug).

Commands:
  help [<command>...]
    Show help.

  inspect [<flags>]
    Inspect all blocks in the bucket in detailed, table-like way

  analyze [<flags>] <ULID>
    Analyze churn, label pair cardinality.

  dump [<flags>] <ULID>...
    Dump samples from a TSDB.
```
Master builds are available on [Docker Hub](https://hub.docker.com/repository/docker/sepa/thanos-kit/tags)
