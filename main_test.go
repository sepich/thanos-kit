package main

import (
	"fmt"
	"github.com/thanos-io/objstore/client"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/thanos-io/thanos/pkg/block/metadata"
)

func Test_e2e(t *testing.T) {
	tmpDir := "./tmp"
	bktDir := tmpDir + "/bucket"
	if err := os.MkdirAll(bktDir, 0750); err != nil {
		t.Fatalf("Unable to create %s directory for testing", bktDir)
	}
	cacheDir := tmpDir + "/cache"
	if err := os.MkdirAll(cacheDir, 0750); err != nil {
		t.Fatalf("Unable to create %s directory for testing", cacheDir)
	}

	// prepare test data
	inputFile := tmpDir + "/import.prom"
	f, _ := os.Create(inputFile)
	end := time.Now().Unix()
	for i := end - 3600; i <= end; i += 15 {
		fmt.Fprintf(f, "test_metric_one{label=\"test1\"} %g %d000\n", float64(rand.Int()), i)
		fmt.Fprintf(f, "test_metric_two{label=\"test2\", new=\"another label\"} %g %d500\n", rand.Float64(), i)
	}
	f.Close()

	// import metrics to local objStore
	logger := log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
	bkt, err := client.NewBucket(logger, []byte("{type: FILESYSTEM, config: {directory: "+bktDir+"}}"), "thanos-kit")
	if err != nil {
		t.Fatalf("Open bucket: %v", err)
	}
	blockSize := 2 * time.Hour
	importLabels := []string{
		`prometheus="prometheus-a"`,
		"datacenter=us",
	}
	if err := importMetrics(bkt, &inputFile, &blockSize, &cacheDir, &importLabels, true, logger); err != nil {
		t.Fatalf("Import of %s failed: %v", inputFile, err)
	}
	os.RemoveAll(cacheDir)

	// check thanos labels
	dirs, _ := os.ReadDir(bktDir)
	ids := []string{}
	for _, d := range dirs {
		ids = append(ids, d.Name())
		meta, err := metadata.ReadFromDir(filepath.Join(bktDir, d.Name()))
		if err != nil {
			t.Fatalf("fail to read meta.json for %s: %v", d.Name(), err)
		}
		if len(meta.Thanos.Labels) != 2 ||
			meta.Thanos.Labels["prometheus"] != "prometheus-a" ||
			meta.Thanos.Labels["datacenter"] != "us" {
			t.Fatalf("Wrong Thanos Labels in object storage block %s: %v", d.Name(), meta.Thanos.Labels)
		}
	}

	// export data from object storage
	outFile := tmpDir + "/export.prom"
	f, _ = os.Create(outFile)
	minT := int64(0)
	maxT := int64(math.MaxInt64)
	match := "{__name__=~'(?s:.*)'}"
	if err := dump(bkt, f, &ids, &cacheDir, &minT, &maxT, &match, logger); err != nil {
		t.Fatalf("Export of %s failed: %v", ids, err)
	}
	f.Close()

	// compare results
	cmd := exec.Command("bash", "-c", fmt.Sprintf("diff <(sort %s) <(sort %s)", inputFile, outFile))
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("Compare files %s and %s result: %v", inputFile, outFile, err)
	}

	// Cleanup
	os.RemoveAll(tmpDir)
}
