package k8s

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// podMetrics registers the two reads k8s.top_pods makes: the metrics API and
// the pod list it joins against.
func podMetrics(t *testing.T, mux *http.ServeMux, metrics, pods any) {
	t.Helper()
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/pods", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, metrics)
	})
	mux.HandleFunc("/api/v1/pods", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, pods)
	})
}

func podSpec(name, namespace string, limit, request string, status map[string]any) map[string]any {
	resources := map[string]any{"requests": map[string]any{"memory": request}}
	if limit != "" {
		resources["limits"] = map[string]any{"cpu": "1", "memory": limit}
	}
	container := map[string]any{"name": "app", "resources": resources}
	pod := map[string]any{
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec":     map[string]any{"containers": []any{container}},
	}
	if status != nil {
		pod["status"] = map[string]any{"containerStatuses": []any{status}}
	}
	return pod
}

// The number this tool exists for is use against the container's own limit,
// not against the node: a container at 100% of a 512Mi limit is about to be
// killed while the box it runs on may be nearly idle.
func TestTopPods_ReportsUseAgainstTheContainersOwnLimit(t *testing.T) {
	mux := http.NewServeMux()
	podMetrics(t, mux,
		map[string]any{"items": []any{map[string]any{
			"metadata":  map[string]any{"name": "api-1", "namespace": "test"},
			"timestamp": "2026-01-01T00:00:00Z",
			"containers": []any{map[string]any{
				"name":  "app",
				"usage": map[string]any{"cpu": "250m", "memory": "512Mi"},
			}},
		}}},
		map[string]any{"items": []any{podSpec("api-1", "test", "512Mi", "256Mi", map[string]any{
			"name":         "app",
			"restartCount": 3,
			"ready":        true,
			"state":        map[string]any{"running": map[string]any{}},
			"lastState":    map[string]any{"terminated": map[string]any{"reason": "OOMKilled", "exitCode": 137}},
		})}},
	)
	a, _ := newTestAdapter(t, mux)
	rows, summary, err := runTopPods(context.Background(), a, map[string]any{})
	if err != nil {
		t.Fatalf("runTopPods: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %#v", len(rows), rows)
	}
	row := rows[0]
	if row["measured"] != true {
		t.Fatalf("row is not marked measured: %#v", row)
	}
	if pct, _ := row["mem_limit_pct"].(float64); pct != 100 {
		t.Errorf("mem_limit_pct = %v, want 100 (usage is exactly the limit)", row["mem_limit_pct"])
	}
	if pct, _ := row["cpu_limit_pct"].(float64); pct != 25 {
		t.Errorf("cpu_limit_pct = %v, want 25", row["cpu_limit_pct"])
	}
	if row["mem_request"] != "256Mi" {
		t.Errorf("mem_request = %v, want the request kept separate from the limit", row["mem_request"])
	}
	// The interesting OOM evidence is the previous state: by the time anyone
	// looks, the terminated current state is usually already gone.
	if row["oom_killed"] != true || row["last_reason"] != "OOMKilled" {
		t.Errorf("lastState OOMKilled was not reported: %#v", row)
	}
	if row["restart_count"] != float64(3) {
		t.Errorf("restart_count = %v, want 3", row["restart_count"])
	}
	if !strings.Contains(summary, "OOM-killed") {
		t.Errorf("summary = %q, want the OOM kill named", summary)
	}
}

// A container with no memory limit is not at 0% of anything. Reporting a
// percentage here would rank the least constrained container as the safest.
func TestTopPods_NoLimitIsAFindingNotZeroPercent(t *testing.T) {
	mux := http.NewServeMux()
	podMetrics(t, mux,
		map[string]any{"items": []any{map[string]any{
			"metadata": map[string]any{"name": "api-1", "namespace": "test"},
			"containers": []any{map[string]any{
				"name":  "app",
				"usage": map[string]any{"memory": "1Gi"},
			}},
		}}},
		map[string]any{"items": []any{podSpec("api-1", "test", "", "", nil)}},
	)
	a, _ := newTestAdapter(t, mux)
	rows, summary, err := runTopPods(context.Background(), a, map[string]any{})
	if err != nil {
		t.Fatalf("runTopPods: %v", err)
	}
	row := rows[0]
	if _, present := row["mem_limit_pct"]; present {
		t.Fatal("a container with no limit must not be given a percentage against a limit it does not have")
	}
	if row["mem_limit_pct_unlimited"] != true {
		t.Errorf("the missing limit must be flagged explicitly: %#v", row)
	}
	if row["mem_limit"] != "" {
		t.Errorf("mem_limit = %v, want empty", row["mem_limit"])
	}
	if !strings.Contains(summary, "NO memory limit") {
		t.Errorf("summary = %q, want it to say the limit is missing and that this is not safety", summary)
	}
}

// An unknown limit is not a safe limit, so it must not sort to the top of a
// list whose whole purpose is "what is closest to being killed".
func TestTopPods_UnknownLimitsSortAfterMeasuredOnes(t *testing.T) {
	mux := http.NewServeMux()
	podMetrics(t, mux,
		map[string]any{"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "unlimited", "namespace": "test"},
				"containers": []any{map[string]any{
					"name": "app", "usage": map[string]any{"memory": "2Gi"},
				}},
			},
			map[string]any{
				"metadata": map[string]any{"name": "nearly-out", "namespace": "test"},
				"containers": []any{map[string]any{
					"name": "app", "usage": map[string]any{"memory": "512Mi"},
				}},
			},
		}},
		map[string]any{"items": []any{
			podSpec("unlimited", "test", "", "", nil),
			podSpec("nearly-out", "test", "512Mi", "128Mi", nil),
		}},
	)
	a, _ := newTestAdapter(t, mux)
	rows, _, err := runTopPods(context.Background(), a, map[string]any{})
	if err != nil {
		t.Fatalf("runTopPods: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0]["pod"] != "nearly-out" {
		t.Errorf("first row is %v; a container at its limit must outrank one whose limit is unknown", rows[0]["pod"])
	}
}

// A metrics entry with no per-container breakdown still describes a real pod
// using real memory. Dropping it would make a heavy pod invisible.
func TestTopPods_MetricsWithoutContainerBreakdownIsKept(t *testing.T) {
	mux := http.NewServeMux()
	podMetrics(t, mux,
		map[string]any{"items": []any{map[string]any{
			"metadata": map[string]any{"name": "api-1", "namespace": "test"},
			"usage":    map[string]any{"cpu": "100m", "memory": "256Mi"},
		}}},
		map[string]any{"items": []any{podSpec("api-1", "test", "1Gi", "", nil)}},
	)
	a, _ := newTestAdapter(t, mux)
	rows, _, err := runTopPods(context.Background(), a, map[string]any{})
	if err != nil {
		t.Fatalf("runTopPods: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("a pod-level metrics entry was dropped: %#v", rows)
	}
	if rows[0]["measured"] != true {
		t.Errorf("pod-level usage is still a measurement: %#v", rows[0])
	}
	if pct, _ := rows[0]["mem_limit_pct"].(float64); pct != 25 {
		t.Errorf("mem_limit_pct = %v, want 25 (256Mi against the pod's 1Gi limit)", rows[0]["mem_limit_pct"])
	}
}

// metrics-server is an add-on. When it is missing the answer is "this API is
// not there", never an empty list, which reads as "nothing is near its limit".
func TestTopPods_MissingMetricsServerIsAnErrorNotAnEmptyList(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/apis/metrics.k8s.io/v1beta1/pods", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"kind": "Status", "reason": "NotFound"})
	})
	a, _ := newTestAdapter(t, mux)
	rows, _, err := runTopPods(context.Background(), a, map[string]any{})
	if err == nil {
		t.Fatalf("a missing metrics API returned %d rows and no error", len(rows))
	}
	if !strings.Contains(err.Error(), "metrics-server") {
		t.Errorf("error = %v, want it to name the likely cause", err)
	}
}
