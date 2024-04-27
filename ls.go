package main

import (
	"context"
	"fmt"
	"github.com/oklog/ulid"
	"github.com/thanos-io/objstore"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/model"
	"strings"
)

func ls(bkt objstore.Bucket, recursive *bool, maxTime *model.TimeOrDurationValue) error {
	blocks, err := getBlocks(context.Background(), bkt, *recursive, maxTime)
	if err == nil {
		for _, b := range blocks {
			fmt.Println(b.Prefix+b.Id.String(), ulid.Time(b.Id.Time()).UTC().Format("06-01-02T15:04:05Z"))
		}
	}
	return err
}

type Block struct {
	Prefix string
	Id     ulid.ULID
}

func getBlocks(ctx context.Context, bkt objstore.Bucket, recursive bool, maxTime *model.TimeOrDurationValue) (found []Block, err error) {
	if recursive {
		err = bkt.Iter(ctx, "", func(name string) error {
			parts := strings.Split(name, "/")
			if len(parts) < 2 {
				return nil
			}
			dir, file := parts[len(parts)-2], parts[len(parts)-1]
			if !block.IsBlockMetaFile(file) {
				return nil
			}
			if id, ok := block.IsBlockDir(dir); ok {
				prefix := ""
				if len(parts) > 2 {
					prefix = strings.Join(parts[0:len(parts)-2], "/") + "/"
				}
				if id.Time() < uint64(maxTime.PrometheusTimestamp()) {
					found = append(found, Block{Prefix: prefix, Id: id})
				}
			}
			return nil
		}, objstore.WithRecursiveIter)
	} else {
		err = bkt.Iter(ctx, "", func(name string) error {
			if id, ok := block.IsBlockDir(name); ok && id.Time() < uint64(maxTime.PrometheusTimestamp()) {
				found = append(found, Block{Prefix: "", Id: id})
			}
			return nil
		})
	}
	return found, err
}
