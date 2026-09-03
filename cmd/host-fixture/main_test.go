package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testFixtureToken = "0123456789abcdef0123456789abcdef"

func newTestController(t *testing.T) (*Controller, string) {
	t.Helper()
	stateDir := t.TempDir()
	controller, err := NewController(testFixtureToken, stateDir, nil)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	t.Cleanup(func() {
		if err := controller.Shutdown(); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	})
	return controller, stateDir
}

func addFixtureAuth(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+testFixtureToken)
	request.Header.Set("X-Opskeeper-Version", "v1")
}

func TestControllerUsesManifestIDWithoutPID(t *testing.T) {
	controller, stateDir := newTestController(t)
	started, err := controller.Start(context.Background(), StartRequest{
		CaseID: "host-cpu-spike", IncidentID: "incident-real-001", Count: 2, TTLSeconds: 60,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.Status != manifestStateRunning || started.ProcessCount != 2 ||
		started.OwnedProcessCount != 2 || started.ManifestID == "" || started.Resource != "host:fixture" {
		t.Fatalf("started manifest = %+v", started)
	}
	manifestData, err := os.ReadFile(filepath.Join(stateDir, started.ManifestID+".json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if strings.Contains(strings.ToLower(string(manifestData)), "pid") {
		t.Fatalf("manifest must not expose a PID: %s", manifestData)
	}
	terminated, err := controller.Terminate(started.ManifestID)
	if err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if terminated.Status != manifestStateTerminated || terminated.OwnedProcessCount != 0 {
		t.Fatalf("terminated manifest = %+v", terminated)
	}
}

func TestControllerStartIsIdempotentByCaseAndIncident(t *testing.T) {
	controller, _ := newTestController(t)
	request := StartRequest{CaseID: "host-cpu-spike", IncidentID: "incident-real-001", Count: 2, TTLSeconds: 60}
	first, err := controller.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	second, err := controller.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("idempotent start: %v", err)
	}
	if first.ManifestID != second.ManifestID {
		t.Fatalf("manifest changed: first=%s second=%s", first.ManifestID, second.ManifestID)
	}
	request.Count = 3
	if _, err := controller.Start(context.Background(), request); err == nil {
		t.Fatal("same case/incident with different count unexpectedly succeeded")
	}
}

func TestHandlerProtectsReadyMetricsAndLifecycle(t *testing.T) {
	controller, _ := newTestController(t)
	server := httptest.NewServer(NewHandler(controller))
	t.Cleanup(server.Close)
	client := &http.Client{Timeout: 5 * time.Second}

	ready := httptest.NewRequest(http.MethodGet, server.URL+"/readyz", nil)
	readyRecorder := httptest.NewRecorder()
	NewHandler(controller).ServeHTTP(readyRecorder, ready)
	if readyRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated readyz status = %d", readyRecorder.Code)
	}

	body := []byte(`{"case_id":"host-cpu-spike","incident_id":"incident-real-001","count":2,"ttl_seconds":60}`)
	createRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/fixtures", bytes.NewReader(body))
	addFixtureAuth(createRequest)
	createResponse, err := client.Do(createRequest)
	if err != nil {
		t.Fatal(err)
	}
	var created apiResponse
	if err := json.NewDecoder(createResponse.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	_ = createResponse.Body.Close()
	if createResponse.StatusCode != http.StatusCreated || created.Code != http.StatusCreated {
		t.Fatalf("create response = %d %+v", createResponse.StatusCode, created)
	}
	var manifest ProcessManifest
	if err := json.Unmarshal(created.Data, &manifest); err != nil {
		t.Fatal(err)
	}

	metricsRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/fixtures/"+manifest.ManifestID+"/metrics", nil)
	addFixtureAuth(metricsRequest)
	metricsResponse, err := client.Do(metricsRequest)
	if err != nil {
		t.Fatal(err)
	}
	metricsBytes, err := io.ReadAll(metricsResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = metricsResponse.Body.Close()
	if !strings.Contains(string(metricsBytes), `"cpu_usage_percent":[94]`) ||
		!strings.Contains(string(metricsBytes), `"sample_size":1`) {
		t.Fatalf("fixture metrics missing: %s", metricsBytes)
	}

	terminateRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/fixtures/"+manifest.ManifestID+"/terminate", nil)
	addFixtureAuth(terminateRequest)
	terminateResponse, err := client.Do(terminateRequest)
	if err != nil {
		t.Fatal(err)
	}
	var termination apiResponse
	if err := json.NewDecoder(terminateResponse.Body).Decode(&termination); err != nil {
		t.Fatal(err)
	}
	_ = terminateResponse.Body.Close()
	if terminateResponse.StatusCode != http.StatusOK || !strings.Contains(string(termination.Data), `"status":"terminated"`) {
		t.Fatalf("terminate response = %d %+v", terminateResponse.StatusCode, termination)
	}
}
