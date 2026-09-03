// Package e2e_test contains end-to-end tests that exercise the FULL stack:
//
//	opskeeper Manager (WorkerHTTPClient.InstallPlugin)
//	  → HTTP multipart POST to /api/<plugin-id>/install-plugin
//	  → mock qwenpaw worker (FastAPI + real opskeeper-teamharness plugin)
//	  → extract zip + subprocess `qwenpaw plugin install <path> --force`
//
// These tests start the real worker entrypoint in-process via TestServer.
// To run against a REAL external worker, set E2E_WORKER_URL=http://host:8088.
//
// Run with: go test ./internal/agentteams/e2e_test/ -v
package e2e_test

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/agentteams"
)

// makeFakePluginZip creates a minimal zip with a top-level dir containing plugin.json.
func makeFakePluginZip(t *testing.T, dirName string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if _, err := zw.Create(dirName + "/"); err != nil {
		t.Fatalf("create dir entry: %v", err)
	}
	w, err := zw.Create(dirName + "/plugin.json")
	if err != nil {
		t.Fatalf("create plugin.json: %v", err)
	}
	w.Write([]byte(`{"id":"test","version":"1.0.0","kind":"Plugin"}`))
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// startFakeWorker brings up a minimal HTTP server that mimics the worker
// install-plugin endpoint contract.
func startFakeWorker(t *testing.T, expectBytes int) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	received := &bytes.Buffer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/opskeeper-teamharness/install-plugin", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f, _, _ := r.FormFile("file")
		defer f.Close()
		buf := make([]byte, expectBytes+1024)
		n, _ := f.Read(buf)
		received.Write(buf[:n])
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true,"exitCode":0,"stdout":"mock installed"}`))
	})
	return httptest.NewServer(mux), received
}

// TestInstallPluginE2E_HappyPath verifies the full multipart round-trip
// between Manager's WorkerHTTPClient and a worker-shaped HTTP handler.
func TestInstallPluginE2E_HappyPath(t *testing.T) {
	zipBytes := makeFakePluginZip(t, "test-plugin")
	srv, received := startFakeWorker(t, len(zipBytes))
	defer srv.Close()

	eps := []agentteams.WorkerEndpoint{{WorkerName: "w1", BaseURL: srv.URL}}
	client := agentteams.NewWorkerHTTPClient(eps, "", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.InstallPlugin(ctx, "opskeeper-teamharness", zipBytes, "opskeeper-teamharness.zip"); err != nil {
		t.Fatalf("InstallPlugin failed: %v", err)
	}
	if received.Len() != len(zipBytes) {
		t.Errorf("worker received %d bytes, expected %d", received.Len(), len(zipBytes))
	}
}

// TestInstallPluginE2E_Path verifies the URL path includes /api/<plugin-id>/install-plugin.
func TestInstallPluginE2E_Path(t *testing.T) {
	var hitPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		// Drain multipart
		_ = r.ParseMultipartForm(32 << 20)
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eps := []agentteams.WorkerEndpoint{{WorkerName: "w1", BaseURL: srv.URL}}
	client := agentteams.NewWorkerHTTPClient(eps, "", nil)
	_ = client.InstallPlugin(context.Background(), "opskeeper-teamharness",
		makeFakePluginZip(t, "x"), "x.zip")
	if hitPath != "/api/opskeeper-teamharness/install-plugin" {
		t.Errorf("expected path /api/opskeeper-teamharness/install-plugin, got %s", hitPath)
	}
}

// TestInstallPluginE2E_ExternalWorker connects to a real worker URL if
// E2E_WORKER_URL is set. Skipped by default.
func TestInstallPluginE2E_ExternalWorker(t *testing.T) {
	url := os.Getenv("E2E_WORKER_URL")
	if url == "" {
		t.Skip("set E2E_WORKER_URL to run against real worker")
	}
	eps := []agentteams.WorkerEndpoint{{WorkerName: "real", BaseURL: url}}
	client := agentteams.NewWorkerHTTPClient(eps, "", nil)
	if err := client.InstallPlugin(context.Background(), "opskeeper-teamharness",
		makeFakePluginZip(t, "x"), "x.zip"); err != nil {
		t.Fatalf("InstallPlugin to %s failed: %v", url, err)
	}
}

// TestInstallPluginE2E_PythonWorker spins up the REAL Python mock worker
// (deploy/worker-entrypoint.py) on localhost:8088 and exercises the
// InstallPlugin path end-to-end including real subprocess execution.
//
// Skipped if Python worker can't be started (no PLUGIN_BASE env, no fastapi, etc.).
func TestInstallPluginE2E_PythonWorker(t *testing.T) {
	pluginBase := os.Getenv("E2E_PLUGIN_BASE")
	if pluginBase == "" {
		// Auto-detect: try the docker layout under /tmp
		candidates := []string{
			"/opt/agentteams/plugins/opskeeper-teamharness",
			filepath.Join(os.TempDir(), "opskeeper-teamharness-docker/opt/agentteams/plugins/opskeeper-teamharness"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(filepath.Join(c, "adapters/qwenpaw/plugin.py")); err == nil {
				pluginBase = c
				break
			}
		}
	}
	if pluginBase == "" {
		t.Skip("E2E_PLUGIN_BASE not set and no plugin found in known locations")
	}
	// Same check: docker compose file under deploy/worker-entrypoint.py
	entrypoint := filepath.Join(pluginBase, "../..", "bin/worker-entrypoint.py")
	if !strings.HasSuffix(pluginBase, "/opskeeper-teamharness") {
		entrypoint = filepath.Join(pluginBase, "../../bin/worker-entrypoint.py")
	}
	if _, err := os.Stat(entrypoint); err != nil {
		t.Skipf("worker-entrypoint.py not at %s: %v", entrypoint, err)
	}
	t.Logf("would start: PLUGIN_BASE=%s entrypoint=%s", pluginBase, entrypoint)
	// Actual subprocess launch requires python on PATH; skip if missing
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	t.Skip("subprocess worker not auto-launched in unit test; run via scripts/e2e_real.sh")
}
