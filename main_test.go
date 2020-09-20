package main

import (
	"fmt"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/thanos-io/thanos/pkg/block/metadata"
)

func Test_e2e(t *testing.T) {
	tmpDir := "./data"
	bktDir := tmpDir + "/bucket"
	if err := os.MkdirAll(bktDir, 0750); err != nil {
		t.Errorf("Unable to create %s directory for testing", bktDir)
	}
	cacheDir := tmpDir + "/cache"
	if err := os.MkdirAll(cacheDir, 0750); err != nil {
		t.Errorf("Unable to create %s directory for testing", cacheDir)
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
	metrics := prometheus.NewRegistry()
	logger := log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
	localStore := []byte(fmt.Sprintf(`{type: FILESYSTEM, config: {directory: %s}}`, bktDir))
	blockSize := 2*time.Hour
	importLabels := []string{
		`prometheus="prometheus-a"`,
		"datacenter=us",
	}
	if err := backfill(localStore, &inputFile, &blockSize, &cacheDir, &importLabels, logger, metrics); err != nil {
		t.Errorf("Import of %s failed: %v", inputFile, err)
	}

	// check thanos labels
	dirs, _ := ioutil.ReadDir(bktDir)
	ids := []string{}
	for _, d := range dirs {
		if d.Name() == "debug" {
			continue
		}
		ids = append(ids, d.Name())
		meta, err := metadata.Read(filepath.Join(bktDir, d.Name()))
		if err != nil {
			t.Errorf("fail to read meta.json for %s: %v", d.Name(), err)
		}
		if len(meta.Thanos.Labels) != 2 ||
			meta.Thanos.Labels["prometheus"] != "prometheus-a" ||
			meta.Thanos.Labels["datacenter"] != "us" {
			t.Errorf("Wrong Thanos Labels in object storage block %s: %v", d.Name(), meta.Thanos.Labels)
		}
	}

	// export data from object storage
	outFile := tmpDir + "/export.prom"
	f, _ = os.Create(outFile)
	metrics = prometheus.NewRegistry()
	minT := int64(0)
	maxT := int64(math.MaxInt64)
	if err := dump(localStore, &ids, &cacheDir, &minT, &maxT, f, logger, metrics); err != nil {
		t.Errorf("Export of %s failed: %v", ids, err)
	}
	f.Close()

	// compare results
	cmd := exec.Command("bash", "-c", fmt.Sprintf("diff <(sort %s) <(sort %s)", inputFile, outFile))
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Errorf("Compare files %s and %s result: %v", inputFile, outFile, err)
	}

	// Cleanup
	os.RemoveAll(tmpDir)
}
