package systemhealth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
)

type fixedSource struct{ inv kernel.Inventory }

func (f fixedSource) Inventory() kernel.Inventory { return f.inv }

func do(t *testing.T, h *CapabilitiesHandler) (int, map[string]any) {
	t.Helper()
	r := chi.NewRouter()
	h.Register(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/plugin-capabilities", nil))
	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
		}
	}
	return rec.Code, body
}

func TestListReturnsTheInventory(t *testing.T) {
	h := NewCapabilitiesHandler(fixedSource{inv: kernel.Inventory{
		Target:     "manager",
		Plugins:    []string{"opskeeper-datasource-loki"},
		KindCounts: map[string]int{"datasource": 2},
		Entries: []kernel.InventoryEntry{
			{Kind: kernel.KindDatasource, Name: "loki", Plugin: "opskeeper-datasource-loki", Impl: "*logquery.Client"},
			{Kind: kernel.KindDatasource, Name: "tempo", Plugin: "opskeeper-datasource-tempo", Impl: "*tracequery.Client"},
		},
	}})
	code, body := do(t, h)
	if code != http.StatusOK {
		t.Fatalf("code = %d; want 200", code)
	}
	if body["target"] != "manager" {
		t.Fatalf("target = %v; want manager", body["target"])
	}
	if got := body["total"].(float64); got != 2 {
		t.Fatalf("total = %v; want 2", got)
	}
	entries, ok := body["entries"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("entries = %v; want 2 items", body["entries"])
	}
	// The implementation is reported by type, never by value.
	first := entries[0].(map[string]any)
	if first["impl"] != "*logquery.Client" {
		t.Fatalf("impl = %v; want the implementation's type name", first["impl"])
	}
}

func TestListEmitsEmptyCollectionsNotNull(t *testing.T) {
	// A caller that indexes into a null array gets nothing useful, so an
	// empty inventory must still be a well-shaped document.
	h := NewCapabilitiesHandler(fixedSource{inv: kernel.Inventory{Target: "manager"}})
	code, body := do(t, h)
	if code != http.StatusOK {
		t.Fatalf("code = %d; want 200", code)
	}
	for _, key := range []string{"plugins", "entries", "kind_counts"} {
		v, present := body[key]
		if !present {
			t.Fatalf("body is missing %q; got %v", key, body)
		}
		if v == nil {
			t.Fatalf("%q must not be null", key)
		}
	}
	if got := body["total"].(float64); got != 0 {
		t.Fatalf("total = %v; want 0", got)
	}
}

func TestListWithoutASourceIsUnavailable(t *testing.T) {
	h := NewCapabilitiesHandler(nil)
	code, body := do(t, h)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d; want 503", code)
	}
	if body["code"] != "no_source" {
		t.Fatalf("code = %v; want no_source", body["code"])
	}
}
