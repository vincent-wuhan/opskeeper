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
	releaseErr error
}

func TestAggregatePrometheusMetricsExposeOnlyLatestManifest(t *testing.T) {
	controller, _, _ := newTestController(t)
	first, err := controller.Start(context.Background(), StartRequest{
		CaseID:          "pg-pool-exhaustion",
		IncidentID:      "incident-live-001",
		InitialCapacity: 2,
		TargetCapacity:  4,
		TTLSeconds:      60,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = controller.Start(context.Background(), StartRequest{
		CaseID:          "pg-pool-exhaustion",
		IncidentID:      "incident-live-002",
		InitialCapacity: 3,
		TargetCapacity:  6,
		TTLSeconds:      60,
	})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	addPoolAuth(request)
	recorder := httptest.NewRecorder()
	NewHandler(controller).ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("aggregate metrics status = %d", recorder.Code)
	}
	expectedSeries := []string{
		`opskeeper_pool_fixture_active_connections{target="pg:pool-fixture",pool_manifest_id="live"} 3`,
		`opskeeper_pool_fixture_capacity{target="pg:pool-fixture",pool_manifest_id="live"} 3`,
	}
	for _, expected := range expectedSeries {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing %s in:\n%s", expected, body)
		}
	}
	if strings.Contains(body, first.ManifestID) {
		t.Fatalf("historical manifest leaked into metrics:\n%s", body)
	}
}

func TestAggregateMetricsExposeNewManifestWithoutRestart(t *testing.T) {
	controller, _, _ := newTestController(t)
	server := httptest.NewServer(NewHandler(controller))
	t.Cleanup(server.Close)
	client := &http.Client{Timeout: 5 * time.Second}

	initialRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/metrics", nil)
	addPoolAuth(initialRequest)
	initialResponse, err := client.Do(initialRequest)
	if err != nil {
		t.Fatal(err)
	}
	initialBody, err := io.ReadAll(initialResponse.Body)
	_ = initialResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if initialResponse.StatusCode != http.StatusOK || len(initialBody) != 0 {
		t.Fatalf("initial metrics response = %d %q", initialResponse.StatusCode, initialBody)
	}

	createRequest, _ := http.NewRequest(
		http.MethodPost,
		server.URL+"/v1/pool-fixtures",
		strings.NewReader(`{"case_id":"pg-pool-exhaustion","incident_id":"incident-live-new","initial_capacity":2,"target_capacity":4,"ttl_seconds":60}`),
	)
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
	if createResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create response = %d", createResponse.StatusCode)
	}
	var manifest PoolManifest
	if err := json.Unmarshal(created.Data, &manifest); err != nil {
		t.Fatal(err)
	}

	metricsRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/metrics", nil)
	addPoolAuth(metricsRequest)
	metricsResponse, err := client.Do(metricsRequest)
	if err != nil {
		t.Fatal(err)
	}
	metricsBody, err := io.ReadAll(metricsResponse.Body)
	_ = metricsResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if metricsResponse.StatusCode != http.StatusOK {
		t.Fatalf("metrics response = %d", metricsResponse.StatusCode)
	}
	expected := `opskeeper_pool_fixture_active_connections{target="pg:pool-fixture",pool_manifest_id="live"} 2`
	if !strings.Contains(string(metricsBody), expected) {
		t.Fatalf("missing %s in:\n%s", expected, metricsBody)
	}
}

func (c *fakeConnection) BackendPID() int { return c.backendPID }
func (c *fakeConnection) Release(context.Context) error {
	c.released = true
	return c.releaseErr
}

type fakeRuntime struct {
	connections     []*fakeConnection
	probeCount      int
	failFirst       int
	resizedTo       int
	closed          bool
	businessSection BusinessSection
	businessErr     error
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
		if err := connection.Release(context.Background()); err != nil {
			return err
		}
	}
	return nil
}

func (r *fakeRuntime) Close() error {
	r.closed = true
	return nil
}

func (r *fakeRuntime) BusinessSnapshot(_ context.Context, section BusinessSection) (BusinessSnapshot, error) {
	r.businessSection = section
	if r.businessErr != nil {
		return BusinessSnapshot{}, r.businessErr
	}
	return BusinessSnapshot{
		Section:     string(section),
		Value:       "fixture",
		Detail:      "deterministic fixture snapshot",
		LatencyMS:   1,
		GeneratedAt: time.Unix(0, 0).UTC(),
	}, nil
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
	if !runtime.closed {
		t.Fatal("recovered pool runtime was not closed")
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

func TestReleaseConnectionsContinuesAfterStaleConnection(t *testing.T) {
	connections := []PoolConnection{
		&fakeConnection{releaseErr: errors.New("driver: bad connection")},
		&fakeConnection{},
	}

	if err := releaseConnections(context.Background(), connections); err == nil {
		t.Fatal("expected stale connection release error")
	}

	for index, connection := range connections {
		fake, ok := connection.(*fakeConnection)
		if !ok || !fake.released {
			t.Fatalf("connection %d was not recycled: %+v", index, connection)
		}
	}
}

func TestBusinessSnapshotUsesSharedSaturatedPool(t *testing.T) {
	controller, runtime, _ := newTestController(t)
	runtime.businessErr = context.DeadlineExceeded
	manifest, err := controller.Start(context.Background(), StartRequest{
		CaseID: "pg-pool-exhaustion", IncidentID: "incident-business",
		InitialCapacity: 2, TargetCapacity: 4, TTLSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != poolStateRunning {
		t.Fatalf("business snapshot fixture status = %s", manifest.Status)
	}
	if _, err := controller.BusinessSnapshot(context.Background(), manifest.ManifestID, BusinessSectionOrders); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected saturated-pool error, got %v", err)
	}
	if runtime.businessSection != BusinessSectionOrders {
		t.Fatalf("unexpected section %q", runtime.businessSection)
	}
}

func TestBusinessSnapshotHandlerCoversAllSections(t *testing.T) {
	controller, _, _ := newTestController(t)
	manifest, err := controller.Start(context.Background(), StartRequest{
		CaseID: "pg-pool-exhaustion", IncidentID: "incident-business-http",
		InitialCapacity: 2, TargetCapacity: 4, TTLSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != poolStateRunning {
		t.Fatalf("business snapshot fixture status = %s", manifest.Status)
	}
	server := httptest.NewServer(NewHandler(controller))
	defer server.Close()
	for _, section := range []string{"orders", "inventory", "audit"} {
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/business-snapshots/"+section, nil)
		addPoolAuth(request)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		responseBody, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d body = %s", section, response.StatusCode, responseBody)
		}
		if cacheControl := response.Header.Get("Cache-Control"); cacheControl != "no-store" {
			t.Fatalf("%s cache-control = %q", section, cacheControl)
		}
	}
}

func TestBusinessSnapshotHandlerUsesBaselineWithoutActiveManifest(t *testing.T) {
	controller, runtime, _ := newTestController(t)
	server := httptest.NewServer(NewHandler(controller))
	defer server.Close()

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/business-snapshots/orders", nil)
	addPoolAuth(request)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), `"section":"orders"`) ||
		response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("baseline response = %d %s", response.StatusCode, responseBody)
	}
	if runtime.businessSection != BusinessSectionOrders {
		t.Fatalf("baseline section = %q", runtime.businessSection)
	}
}

func TestBusinessSnapshotHandlerBoundsErrors(t *testing.T) {
	controller, runtime, _ := newTestController(t)
	_, err := controller.Start(context.Background(), StartRequest{
		CaseID: "pg-pool-exhaustion", IncidentID: "incident-business-errors",
		InitialCapacity: 2, TargetCapacity: 4, TTLSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewHandler(controller))
	defer server.Close()

	runtime.businessErr = context.DeadlineExceeded
	timeoutRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/business-snapshots/orders", nil)
	addPoolAuth(timeoutRequest)
	timeoutResponse, err := server.Client().Do(timeoutRequest)
	if err != nil {
		t.Fatal(err)
	}
	timeoutBody, _ := io.ReadAll(timeoutResponse.Body)
	_ = timeoutResponse.Body.Close()
	if timeoutResponse.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(timeoutBody), `"error_code":"pool_exhausted"`) {
		t.Fatalf("timeout response = %d %s", timeoutResponse.StatusCode, timeoutBody)
	}
	if len(timeoutBody) > 1024 {
		t.Fatalf("timeout response is unbounded: %d bytes", len(timeoutBody))
	}

	invalidRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/business-snapshots/unknown", nil)
	addPoolAuth(invalidRequest)
	invalidResponse, err := server.Client().Do(invalidRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = invalidResponse.Body.Close()
	if invalidResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid section status = %d", invalidResponse.StatusCode)
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
