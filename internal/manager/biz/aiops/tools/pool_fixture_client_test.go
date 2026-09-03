package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPoolFixtureClientResolvesAndRecoversByManifest(t *testing.T) {
	var seenStatusPath, seenRecoverPath, seenVersion, seenMethod string
	var seenBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seenVersion = request.Header.Get("X-Opskeeper-Version")
		if request.URL.Path == "/v1/pool-fixtures/p4b1c0a19d3e5f7a" {
			seenStatusPath = request.URL.Path
			seenMethod = request.Method
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"code":200,"message":"success","data":{"manifest_id":"p4b1c0a19d3e5f7a","incident_id":"pg-pool","resource":"pg:pool-fixture","status":"running","failed_probe":{"status":"failed"}}}`))
			return
		}
		if request.URL.Path == "/v1/pool-fixtures/p4b1c0a19d3e5f7a/recover" {
			seenRecoverPath = request.URL.Path
			rawBody, err := io.ReadAll(request.Body)
			if err != nil || json.Unmarshal(rawBody, &seenBody) != nil {
				t.Errorf("read body err=%v body=%s", err, rawBody)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"code":200,"message":"success","data":{"status":"recovered","recovery_probe":{"status":"success"}}}`))
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	client := NewPoolFixtureClient(server.URL, "runtime-token-0123456789abcdef")
	request := PoolRecoveryRequest{
		IncidentID: "pg-pool", PoolManifestID: "p4b1c0a19d3e5f7a", Reason: "approved resize",
	}
	status, err := client.Status(context.Background(), request)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if seenStatusPath != "/v1/pool-fixtures/p4b1c0a19d3e5f7a" || seenMethod != http.MethodGet ||
		status.Resource != "pg:pool-fixture" || status.FailedProbe.Status != "failed" {
		t.Fatalf("status wire/result = path=%q method=%q status=%+v", seenStatusPath, seenMethod, status)
	}
	result, err := client.Recover(context.Background(), request)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if seenRecoverPath != "/v1/pool-fixtures/p4b1c0a19d3e5f7a/recover" || seenVersion != "v1" ||
		seenBody["reason"] != "approved resize" || !json.Valid(result) {
		t.Fatalf("recover wire/result = path=%q version=%q body=%v result=%s", seenRecoverPath, seenVersion, seenBody, result)
	}
}
