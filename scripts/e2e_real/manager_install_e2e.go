// 完整 e2e against REAL Python mock worker (not httptest mock)
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/vincent-wuhan/opskeeper/internal/agentteams"
)

func main() {
	eps := []agentteams.WorkerEndpoint{{
		WorkerName: "real-python-worker",
		BaseURL:    "http://127.0.0.1:8088",
	}}
	client := agentteams.NewWorkerHTTPClient(eps, "", nil)

	// Build zip with test-plugin/plugin.json (NOT plugin.yaml, since worker
	// install_plugin checks for plugin.json)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("test-plugin/plugin.json")
	w.Write([]byte(`{"id":"test-plugin","version":"1.0.0","kind":"Plugin"}`))
	zw.Close()
	zipBytes := buf.Bytes()
	fmt.Printf("[manager] zip_bytes=%d\n", len(zipBytes))

	if err := client.InstallPlugin(context.Background(), "opskeeper-teamharness", zipBytes, "opskeeper-teamharness.zip"); err != nil {
		fmt.Printf("[FAIL] %v\n", err)
		os.Exit(1)
	}
	fmt.Println("[OK] complete Go Manager → real Python worker e2e")
}
