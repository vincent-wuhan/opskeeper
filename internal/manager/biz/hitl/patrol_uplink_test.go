package hitl

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 契约块与 walker/contracts/ops-patrol-report-v1.md 对齐。
func TestPatrolOnceContractShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/bad") {
			w.WriteHeader(502)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	yaml := PatrolOnce([]PatrolTarget{{Name: "multica", BaseURL: srv.URL}}, 14, srv.Client(), time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC))
	for _, want := range []string{
		"# ==== ops-patrol-report v1 ====",
		"schema: ops-patrol-report/1",
		"collector: opskeeper:managed-patrol",
		"target_ref: tgt:managed/multica",
		"self_check: pass",
		"layer: live",
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("patrol contract missing %q\n%s", want, yaml)
		}
	}
	if !strings.Contains(yaml, `result: pass`) {
		t.Errorf("healthy endpoint should pass\n%s", yaml)
	}
}

func TestParsePatrolTargets(t *testing.T) {
	if ParsePatrolTargets("") != nil {
		t.Fatal("empty → nil")
	}
	ts := ParsePatrolTargets("multica=https://opc.yueming.xin, bad, x=https://x.example/")
	if len(ts) != 2 || ts[0].Name != "multica" || ts[1].BaseURL != "https://x.example" {
		t.Fatalf("parse = %+v", ts)
	}
}
