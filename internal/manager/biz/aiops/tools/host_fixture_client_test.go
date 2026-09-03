package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostFixtureClientResolvesAndTerminatesByManifest(t *testing.T) {
	var seenStatusPath, seenTerminatePath, seenVersion, seenMethod string
	seenBody := map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seenVersion = request.Header.Get("X-Opskeeper-Version")
		if request.URL.Path == "/v1/fixtures/f4b1c0a19d3e5f7a" {
			seenStatusPath = request.URL.Path
			seenMethod = request.Method
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"code":200,"message":"success","data":{"manifest_id":"f4b1c0a19d3e5f7a","incident_id":"incident-real-001","resource":"host:fixture","status":"running","process_count":2}}`))
			return
		}
		if request.URL.Path == "/v1/fixtures/f4b1c0a19d3e5f7a/terminate" {
			seenTerminatePath = request.URL.Path
			rawBody, err := io.ReadAll(request.Body)
			if err != nil || (len(rawBody) != 0 && json.Unmarshal(rawBody, &seenBody) != nil) {
				t.Errorf("read body err=%v body=%s", err, rawBody)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"code":200,"message":"success","data":{"manifest_id":"f4b1c0a19d3e5f7a","status":"terminated","owned_process_count":0}}`))
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	client := NewHostFixtureClient(server.URL, "runtime-token-0123456789abcdef")
	request := HostProcessTerminationRequest{IncidentID: "incident-real-001", FixtureManifestID: "f4b1c0a19d3e5f7a"}
	status, err := client.Status(context.Background(), request)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if seenStatusPath != "/v1/fixtures/f4b1c0a19d3e5f7a" || seenMethod != http.MethodGet ||
		status.IncidentID != request.IncidentID || status.Status != "running" {
		t.Fatalf("status wire/result = path=%q method=%q status=%+v", seenStatusPath, seenMethod, status)
	}
	result, err := client.Terminate(context.Background(), request)
	if err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if seenTerminatePath != "/v1/fixtures/f4b1c0a19d3e5f7a/terminate" || seenVersion != "v1" || len(seenBody) != 0 {
		t.Fatalf("terminate wire path=%q version=%q body=%v", seenTerminatePath, seenVersion, seenBody)
	}
	if !json.Valid(result) {
		t.Fatalf("invalid termination result: %s", result)
	}
}
