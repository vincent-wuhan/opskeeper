package qdrantx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSetPayloadByFilter(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{"operation_id":1,"status":"completed"}}`))
	}))
	defer server.Close()

	client := New(server.URL, nil)
	payload := map[string]any{"tenant_scopes": []string{"global"}}
	must := map[string]any{"source_type": "vault"}
	if err := client.SetPayloadByFilter(context.Background(), "knowledge", payload, must); err != nil {
		t.Fatalf("SetPayloadByFilter() error = %v", err)
	}
	if gotPath != "/collections/knowledge/points/payload" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["wait"] != true {
		t.Fatalf("wait = %#v, want true", gotBody["wait"])
	}
	scopes, _ := gotBody["payload"].(map[string]any)["tenant_scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != "global" {
		t.Fatalf("payload scopes = %#v", gotBody["payload"])
	}
	filter, _ := gotBody["filter"].(map[string]any)
	if filter == nil {
		t.Fatalf("filter missing: %#v", gotBody)
	}
}
