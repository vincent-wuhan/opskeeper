package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testPoolToken = "0123456789abcdef0123456789abcdef"

type fakeConnection struct {
	backendPID int
	released   bool
}

func (c *fakeConnection) BackendPID() int { return c.backendPID }
func (c *fakeConnection) Release() error {
	c.released = true
	return nil
}

type fakeRuntime struct {
	connections []*fakeConnection
	probeCount  int
	failFirst   int
	resizedTo   int
	closed      bool
}

func (r *fakeRuntime) Saturate(_ context.Context, capacity int) ([]PoolConnection, error) {
	connections := make([]PoolConnection, 0, capacity)
	for index := 0; index < capacity; index++ {
		connections = append(connections, &fakeConnection{backendPID: 7000 + index})
	}
	r.connections = make([]*fakeConnection, capacity)
	for index, connection := range connections {
		r.connections[index] = connection.(*fakeConnection)
	}
	return connections, nil
}

func (r *fakeRuntime) Probe(_ context.Context) (ProbeRecord, error) {
	r.probeCount++
	record := ProbeRecord{
		Status:     "success",
		StartedAt:  time.Now().UTC(),
		FinishedAt: time.Now().UTC(),
		BackendPID: 7100 + r.probeCount,
	}
	if r.probeCount <= r.failFirst {
		record.Status = "failed"
		record.ErrorCode = "pool_exhausted"
		return record, errors.New("connection pool exhausted")
	}
	return record, nil
}

func (r *fakeRuntime) ResizeAndRecycle(_ context.Context, connections []PoolConnection, capacity int) error {
	r.resizedTo = capacity
	for _, connection := range connections {
		if err := connection.Release(); err != nil {
			return err
		}
	}
	return nil
}

func (r *fakeRuntime) Close() error {
	r.closed = true
	return nil
}

func newTestController(t *testing.T) (*Controller, *fakeRuntime, string) {
	t.Helper()
	stateDir := t.TempDir()
	runtime := &fakeRuntime{failFirst: 1}
	controller, err := NewController(testPoolToken, stateDir, "postgres://fixture", func(context.Context, string) (PoolRuntime, error) {
		return runtime, nil
	}, nil)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	t.Cleanup(func() {
		if err := controller.Shutdown(); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	})
	return controller, runtime, stateDir
}

func addPoolAuth(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+testPoolToken)
	request.Header.Set("X-Opskeeper-Version", "v1")
}

func TestControllerRequiresFailedProbeThenRecovers(t *testing.T) {
	controller, runtime, stateDir := newTestController(t)
	started, err := controller.Start(context.Background(), StartRequest{
		CaseID: "pg-pool-exhaustion", IncidentID: "incident-live-001",
		InitialCapacity: 2, TargetCapacity: 4, TTLSeconds: 60,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.Status != poolStateRunning || started.ActiveConnections != 2 || started.Resource != "pg:pool-fixture" {
		t.Fatalf("started manifest = %+v", started)
	}

	if _, err := controller.Recover(context.Background(), started.ManifestID, RecoverRequest{Reason: "too early"}); !errors.Is(err, errProbeRequired) {
		t.Fatalf("recover without failed probe error = %v", err)
	}
	failed, err := controller.Probe(context.Background(), started.ManifestID, ProbeRequest{TimeoutMilliseconds: 250})
	if err == nil || failed.Status != "failed" || failed.ErrorCode != "pool_exhausted" {
		t.Fatalf("failed probe = %+v, err = %v", failed, err)
	}
	recovered, err := controller.Recover(context.Background(), started.ManifestID, RecoverRequest{Reason: "resize and recycle idle sessions"})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if recovered.Status != poolStateRecovered || recovered.ActiveConnections != 0 ||
		recovered.RecoveryProbe == nil || recovered.RecoveryProbe.Status != "success" {
		t.Fatalf("recovered manifest = %+v", recovered)
	}
	if runtime.resizedTo != 4 {
		t.Fatalf("target capacity = %d", runtime.resizedTo)
	}
	for _, connection := range runtime.connections {
		if !connection.released {
			t.Fatalf("owned connection was not released: %+v", connection)
		}
	}
	manifestData, err := os.ReadFile(filepath.Join(stateDir, started.ManifestID+".json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if strings.Contains(strings.ToLower(string(manifestData)), "pid") {
		t.Fatalf("manifest must not expose a backend PID: %s", manifestData)
	}
}

func TestHandlerProtectsAndExposesPoolLifecycle(t *testing.T) {
	controller, _, _ := newTestController(t)
	server := httptest.NewServer(NewHandler(controller))
	t.Cleanup(server.Close)
	client := &http.Client{Timeout: 5 * time.Second}

	unauthorized := httptest.NewRequest(http.MethodGet, server.URL+"/readyz", nil)
	recorder := httptest.NewRecorder()
	NewHandler(controller).ServeHTTP(recorder, unauthorized)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated readyz status = %d", recorder.Code)
	}

	body := []byte(`{"case_id":"pg-pool-exhaustion","incident_id":"incident-live-001","initial_capacity":2,"target_capacity":4,"ttl_seconds":60}`)
	createRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/pool-fixtures", bytes.NewReader(body))
	addPoolAuth(createRequest)
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
	var manifest PoolManifest
	if err := json.Unmarshal(created.Data, &manifest); err != nil {
		t.Fatal(err)
	}

	probeBody := []byte(`{"timeout_milliseconds":250}`)
	probeRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/pool-fixtures/"+manifest.ManifestID+"/probe", bytes.NewReader(probeBody))
	addPoolAuth(probeRequest)
	probeResponse, err := client.Do(probeRequest)
	if err != nil {
		t.Fatal(err)
	}
	probeBytes, _ := io.ReadAll(probeResponse.Body)
	_ = probeResponse.Body.Close()
	if probeResponse.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(probeBytes), `"error_code":"pool_exhausted"`) {
		t.Fatalf("probe response = %d %s", probeResponse.StatusCode, probeBytes)
	}

	metricsRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/pool-fixtures/"+manifest.ManifestID+"/metrics", nil)
	addPoolAuth(metricsRequest)
	metricsResponse, err := client.Do(metricsRequest)
	if err != nil {
		t.Fatal(err)
	}
	metricsBytes, _ := io.ReadAll(metricsResponse.Body)
	_ = metricsResponse.Body.Close()
	if !strings.Contains(string(metricsBytes), `"active_connections":2`) ||
		!strings.Contains(string(metricsBytes), `"utilization_percent":100`) ||
		!strings.Contains(string(metricsBytes), `"failed_probe_count":1`) {
		t.Fatalf("pool metrics missing: %s", metricsBytes)
	}
}
