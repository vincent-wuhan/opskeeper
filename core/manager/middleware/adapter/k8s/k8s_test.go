package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// newTestAdapter wires an adapter against a stub API server.
//
// The stub is the real HTTP path — the same client method, the same patch
// body, the same status decoding — because the bugs worth catching here live
// in the request this adapter builds and in how it reads the answer, not in
// the JSON it happens to unmarshal.
func newTestAdapter(t *testing.T, mux *http.ServeMux) (*Adapter, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewWithClient(&kubeClient{
		base:    srv.URL,
		http:    srv.Client(),
		timeout: 5 * time.Second,
	}), srv
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func TestAdapter_Type(t *testing.T) {
	if got := New().Type(); got != adapter.TypeK8sCluster {
		t.Errorf("Type() = %s, want k8s_cluster", got)
	}
}

func TestAdapter_Health_NotConnected(t *testing.T) {
	_, err := New().Health(context.Background())
	if !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("expected ErrNotConnected, got %v", err)
	}
}

func TestAdapter_Execute_RequiresApprovalBeforeConnecting(t *testing.T) {
	// The order matters: an unapproved write is refused whether or not a
	// cluster is configured, so the error must be the approval error and not
	// "not connected".
	_, err := New().Execute(context.Background(), adapter.ExecOp{Operation: "scale"})
	if !errors.Is(err, adapter.ErrApprovalRequired) {
		t.Errorf("expected ErrApprovalRequired, got %v", err)
	}
}

func TestAdapter_Health_ProbesVersion(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"gitVersion": "v1.31.4", "platform": "linux/amd64"})
	})
	a, _ := newTestAdapter(t, mux)
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Status != "healthy" || !strings.Contains(h.Message, "v1.31.4") {
		t.Errorf("Health = %+v, want healthy with the version in the message", h)
	}
}

func TestAdapter_Health_ReportsDownRatherThanErroring(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	a, _ := newTestAdapter(t, mux)
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("Health returned an error for a reachable but unauthorised server: %v", err)
	}
	if h.Status != "down" {
		t.Errorf("Status = %q, want down", h.Status)
	}
}

// ── registration ───────────────────────────────────────────────────────

func TestRegisterTools_ExposesEveryLoopActionAndRequiredArgs(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	names := reg.ListTools("k8s.")
	want := []string{
		"k8s.cluster_info", "k8s.node_list", "k8s.pod_list", "k8s.deployment_status", "k8s.rollout_status",
		"k8s.rollout_history", "k8s.pod_logs", "k8s.events", "k8s.top_nodes", "k8s.top_pods",
		"k8s.pvc_usage", "k8s.pvc_list",
		"k8s.describe_pod",
		"k8s.scale", "k8s.rollout_undo", "k8s.rolling_restart", "k8s.cordon", "k8s.uncordon",
		"k8s.drain", "k8s.evict_pod", "k8s.resize_pvc",
		"k8s.exec_into_pod", "k8s.cleanup_logs",
	}
	present := map[string]bool{}
	for _, n := range names {
		present[n] = true
	}
	for _, n := range want {
		if !present[n] {
			t.Errorf("tool %s is not registered", n)
		}
	}
	// The three loop actions this adapter exists to serve must be registered
	// under exactly the names the investigator writes into a RootCauseJSON.
	// A near-miss here is a remediation the platform proposes and cannot run.
	for _, action := range []string{"k8s.evict_pod", "k8s.rolling_restart", "k8s.rollout_status"} {
		if _, ok := reg.LookupTool(action); !ok {
			t.Errorf("loop action %s does not resolve to a registered tool", action)
		}
	}
	// k8s.rollout_status is the one k8s action the closed loop runs
	// unattended, and it has to be answerable from a resource locator alone:
	// with no deployment named it reports every rollout in scope.
	if spec, ok := reg.LookupTool("k8s.rollout_status"); !ok || len(spec.RequiredArgs) != 0 {
		t.Errorf("k8s.rollout_status RequiredArgs = %v, want none so the loop can dispatch it", spec.RequiredArgs)
	}
	// A tool that acts on one named object must declare the argument, so a
	// dispatcher that has only a resource locator refuses instead of guessing.
	cases := map[string][]string{
		"k8s.evict_pod":       {"pod"},
		"k8s.rolling_restart": {"deployment"},
		"k8s.scale":           {"deployment", "replicas"},
		"k8s.pod_logs":        {"pod"},
	}
	for tool, required := range cases {
		spec, ok := reg.LookupTool(tool)
		if !ok {
			t.Fatalf("tool %s is not registered", tool)
		}
		for _, arg := range required {
			found := false
			for _, r := range spec.RequiredArgs {
				if r == arg {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: %s is not declared required (declared: %v)", tool, arg, spec.RequiredArgs)
			}
		}
	}
}

func TestWriteTools_RefuseWithoutAnApprover(t *testing.T) {
	mux := http.NewServeMux()
	a, _ := newTestAdapter(t, mux)
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, a); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	_, err := reg.CallTool(context.Background(), "k8s.evict_pod", map[string]any{"pod": "api-1"})
	if !errors.Is(err, adapter.ErrApprovalRequired) {
		t.Errorf("evict without approved_by: got %v, want ErrApprovalRequired", err)
	}
}

// ── reads ──────────────────────────────────────────────────────────────

func TestRunPodList_FiltersUnhealthy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/pods", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "healthy", "namespace": "prod", "ownerReferences": []any{map[string]any{"kind": "ReplicaSet"}}},
				"spec":     map[string]any{"nodeName": "n1"},
				"status": map[string]any{
					"phase":             "Running",
					"containerStatuses": []any{map[string]any{"ready": true, "restartCount": 0}},
				},
			},
			map[string]any{
				"metadata": map[string]any{"name": "crashloop", "namespace": "prod"},
				"spec":     map[string]any{"nodeName": "n2"},
				"status": map[string]any{
					"phase": "Running",
					"containerStatuses": []any{map[string]any{
						"ready":        false,
						"restartCount": 7,
						"state":        map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}},
					}},
				},
			},
		}})
	})
	a, _ := newTestAdapter(t, mux)
	rows, _, err := runPodList(context.Background(), a, map[string]any{"unhealthy": true})
	if err != nil {
		t.Fatalf("runPodList: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want only the unhealthy pod", len(rows))
	}
	row := rows[0]
	if row["name"] != "crashloop" {
		t.Errorf("kept %v, want crashloop", row["name"])
	}
	if row["waiting_reason"] != "CrashLoopBackOff" || row["restarts"] != 7 || row["ready"] != "0/1" {
		t.Errorf("row = %v, want the waiting reason, restart count and readiness", row)
	}
	if row["owner"] != "none" {
		t.Errorf("owner = %v, want none for a bare pod", row["owner"])
	}
}

func TestRunNodeList_RolesComeFromLabelKeys(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{
			map[string]any{
				"metadata": map[string]any{
					"name":   "cp-1",
					"labels": map[string]any{"node-role.kubernetes.io/control-plane": ""},
				},
				"spec": map[string]any{"unschedulable": true},
				"status": map[string]any{
					"conditions":  []any{map[string]any{"type": "Ready", "status": "True"}},
					"capacity":    map[string]any{"cpu": "16", "memory": "64Gi", "pods": "110"},
					"allocatable": map[string]any{"cpu": "15800m", "memory": "62Gi"},
					"nodeInfo":    map[string]any{"kubeletVersion": "v1.31.4"},
				},
			},
		}})
	})
	a, _ := newTestAdapter(t, mux)
	rows, _, err := runNodeList(context.Background(), a, nil)
	if err != nil {
		t.Fatalf("runNodeList: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row["roles"] != "control-plane" {
		t.Errorf("roles = %v, want control-plane", row["roles"])
	}
	if row["unschedulable"] != true {
		t.Errorf("unschedulable = %v, want true", row["unschedulable"])
	}
	if row["memory_capacity_human"] != "64.0Gi" {
		t.Errorf("memory_capacity_human = %v, want 64.0Gi", row["memory_capacity_human"])
	}
	if row["status"] != "Ready" {
		t.Errorf("status = %v, want Ready", row["status"])
	}
}

func TestResolveNamespace_RefusesAmbiguity(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/pods", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{
			map[string]any{"metadata": map[string]any{"name": "api-1", "namespace": "prod"}},
			map[string]any{"metadata": map[string]any{"name": "api-1", "namespace": "staging"}},
		}})
	})
	a, _ := newTestAdapter(t, mux)
	client, err := a.handle()
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.resolveNamespace(context.Background(), colPods, "api-1", "")
	if err == nil || !strings.Contains(err.Error(), "prod") || !strings.Contains(err.Error(), "staging") {
		t.Errorf("expected an ambiguity error naming both namespaces, got %v", err)
	}
}

func TestRunPodLogs_ReadsPlainText(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/prod/pods/api-1/log", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("tailLines"); got != "50" {
			t.Errorf("tailLines = %q, want 50", got)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("line one\nline two\n"))
	})
	a, _ := newTestAdapter(t, mux)
	rows, _, err := runPodLogsTool(context.Background(), a, map[string]any{
		"pod": "api-1", "namespace": "prod", "tail_lines": 50,
	})
	if err != nil {
		t.Fatalf("runPodLogsTool: %v", err)
	}
	if len(rows) != 1 || rows[0]["log"] != "line one\nline two\n" {
		t.Fatalf("rows = %v, want the log text", rows)
	}
	if rows[0]["lines"] != 2 {
		t.Errorf("lines = %v, want 2", rows[0]["lines"])
	}
}

func TestRunEvents_NewestFirstAndBoundedByLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("fieldSelector"); got != "type=Warning" {
			t.Errorf("fieldSelector = %q, want type=Warning", got)
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{
			map[string]any{"metadata": map[string]any{"namespace": "prod"}, "type": "Warning", "reason": "BackOff", "lastTimestamp": "2026-09-30T01:00:00Z", "count": 3, "involvedObject": map[string]any{"kind": "Pod", "name": "api-1"}},
			map[string]any{"metadata": map[string]any{"namespace": "prod"}, "type": "Warning", "reason": "FailedScheduling", "lastTimestamp": "2026-09-30T02:00:00Z", "count": 9, "involvedObject": map[string]any{"kind": "Pod", "name": "api-2"}},
		}})
	})
	a, _ := newTestAdapter(t, mux)
	rows, _, err := runEvents(context.Background(), a, map[string]any{"warnings_only": true, "limit": 1})
	if err != nil {
		t.Fatalf("runEvents: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want the limit to apply", len(rows))
	}
	if rows[0]["reason"] != "FailedScheduling" {
		t.Errorf("kept %v, want the newest event", rows[0]["reason"])
	}
}

// ── writes ─────────────────────────────────────────────────────────────

func TestScale_PatchesReplicasAndReportsTheDelta(t *testing.T) {
	var patched map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/apis/apps/v1/namespaces/prod/deployments/api", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{
				"metadata": map[string]any{"name": "api", "namespace": "prod"},
				"spec":     map[string]any{"replicas": 3},
			})
		case http.MethodPatch:
			if ct := r.Header.Get("Content-Type"); ct != "application/merge-patch+json" {
				t.Errorf("Content-Type = %q, want merge-patch", ct)
			}
			if err := json.NewDecoder(r.Body).Decode(&patched); err != nil {
				t.Fatalf("decode patch: %v", err)
			}
			writeJSON(w, http.StatusOK, map[string]any{"metadata": map[string]any{"name": "api"}})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	})
	a, _ := newTestAdapter(t, mux)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "scale",
		Params:     map[string]any{"deployment": "api", "namespace": "prod", "replicas": 6},
		ApprovedBy: "op-7",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Success || res.Impacted != 3 {
		t.Errorf("result = %+v, want success with 3 replicas changed", res)
	}
	spec, _ := patched["spec"].(map[string]any)
	if spec["replicas"] != float64(6) {
		t.Errorf("patch spec = %v, want replicas 6", patched)
	}
}

func TestScale_RefusesToGuessWhenTheNameIsMissing(t *testing.T) {
	a, _ := newTestAdapter(t, http.NewServeMux())
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "scale",
		Params:     map[string]any{"namespace": "prod", "replicas": 2},
		ApprovedBy: "op-7",
	})
	if err == nil || !strings.Contains(err.Error(), "deployment is required") {
		t.Errorf("got %v, want a refusal naming the missing argument", err)
	}
}

func TestEvictPod_PodDisruptionBudgetRefusalIsNotAnError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/prod/pods/api-1/eviction", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"kind": "Status", "reason": "TooManyRequests",
			"message": "Cannot evict pod as it would violate the pod's disruption budget.",
		})
	})
	a, _ := newTestAdapter(t, mux)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "evict_pod",
		Params:     map[string]any{"pod": "api-1", "namespace": "prod"},
		ApprovedBy: "op-7",
	})
	if err != nil {
		t.Fatalf("Execute returned an error for a budget refusal: %v", err)
	}
	if res.Success {
		t.Error("a refused eviction must not report success")
	}
	if !strings.Contains(res.Message, "PodDisruptionBudget") {
		t.Errorf("message = %q, want it to name the budget rather than a generic failure", res.Message)
	}
}

func TestEvictPod_AlreadyGoneIsSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/prod/pods/api-1/eviction", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"kind": "Status", "reason": "NotFound", "message": "pods \"api-1\" not found"})
	})
	a, _ := newTestAdapter(t, mux)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "evict_pod",
		Params:     map[string]any{"pod": "api-1", "namespace": "prod"},
		ApprovedBy: "op-7",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Success {
		t.Errorf("a deleted pod is the outcome eviction wanted: %+v", res)
	}
}

func TestRollingRestart_SetsTheRestartedAtAnnotation(t *testing.T) {
	var patched map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/apis/apps/v1/namespaces/prod/deployments/api", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]any{
				"metadata": map[string]any{"name": "api", "namespace": "prod"},
				"spec":     map[string]any{"replicas": 4},
			})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&patched)
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	a, _ := newTestAdapter(t, mux)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "rolling_restart",
		Params:     map[string]any{"deployment": "api", "namespace": "prod"},
		ApprovedBy: "op-7",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Success || res.Impacted != 4 {
		t.Errorf("result = %+v, want success over 4 replicas", res)
	}
	annotations := nested(nested(patched, "spec", "template", "metadata"), "annotations")
	if _, ok := annotations["kubectl.kubernetes.io/restartedAt"]; !ok {
		t.Errorf("patch = %v, want a restartedAt annotation", patched)
	}
}

func TestResizePVC_RefusesToShrink(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/prod/persistentvolumeclaims/data", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"metadata": map[string]any{"name": "data", "namespace": "prod"},
			"spec": map[string]any{
				"storageClassName": "gp3",
				"resources":        map[string]any{"requests": map[string]any{"storage": "100Gi"}},
			},
		})
	})
	a, _ := newTestAdapter(t, mux)
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "resize_pvc",
		Params:     map[string]any{"pvc": "data", "namespace": "prod", "size": "50Gi"},
		ApprovedBy: "op-7",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be shrunk") {
		t.Errorf("got %v, want a refusal naming the direction that is allowed", err)
	}
}

func TestDrain_RefusesDaemonSetPodsUnlessIgnored(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/pods", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("fieldSelector"); got != "spec.nodeName=n1" {
			t.Errorf("fieldSelector = %q, want the node filter", got)
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "fluent-bit-1", "namespace": "logging", "ownerReferences": []any{map[string]any{"kind": "DaemonSet"}}},
				"spec":     map[string]any{"nodeName": "n1"},
				"status":   map[string]any{"phase": "Running"},
			},
		}})
	})
	a, _ := newTestAdapter(t, mux)
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "drain",
		Params:     map[string]any{"node": "n1"},
		ApprovedBy: "op-7",
	})
	if err == nil || !strings.Contains(err.Error(), "ignore_daemonsets") {
		t.Errorf("got %v, want a refusal that names the flag", err)
	}
}

func TestCordon_SaysWhatItDidNotDo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/nodes/n1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	mux.HandleFunc("/api/v1/pods", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{
			map[string]any{"metadata": map[string]any{"name": "a", "namespace": "prod"}},
			map[string]any{"metadata": map[string]any{"name": "b", "namespace": "prod"}},
		}})
	})
	a, _ := newTestAdapter(t, mux)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "cordon",
		Params:     map[string]any{"node": "n1"},
		ApprovedBy: "op-7",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Message, "2 pod(s) remain running") {
		t.Errorf("message = %q, want it to say cordon does not evict", res.Message)
	}
}

func TestRolloutStatus_WithoutADeploymentReportsEveryRollout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/apis/apps/v1/deployments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "done", "namespace": "prod", "generation": 2},
				"spec":     map[string]any{"replicas": 2},
				"status":   map[string]any{"readyReplicas": 2, "updatedReplicas": 2, "availableReplicas": 2, "observedGeneration": 2},
			},
			map[string]any{
				"metadata": map[string]any{"name": "stuck", "namespace": "prod", "generation": 5},
				"spec":     map[string]any{"replicas": 3},
				"status":   map[string]any{"readyReplicas": 1, "updatedReplicas": 3, "availableReplicas": 1, "unavailableReplicas": 2, "observedGeneration": 4},
			},
		}})
	})
	a, _ := newTestAdapter(t, mux)
	rows, summary, err := runRolloutStatusTool(context.Background(), a, map[string]any{})
	if err != nil {
		t.Fatalf("runRolloutStatusTool: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want every deployment", len(rows))
	}
	if rows[0]["name"] != "stuck" {
		t.Errorf("first row = %v, want the incomplete rollout first", rows[0]["name"])
	}
	if rows[0]["rollout_complete"] != false || rows[1]["rollout_complete"] != true {
		t.Errorf("rollout_complete = %v/%v, want false then true", rows[0]["rollout_complete"], rows[1]["rollout_complete"])
	}
	if !strings.Contains(summary, "1 not fully rolled out") {
		t.Errorf("summary = %q", summary)
	}
}

func TestRolloutStatus_ExplainsWhyTheRolloutIsStuck(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/apis/apps/v1/namespaces/prod/deployments/api", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"metadata": map[string]any{
				"name": "api", "namespace": "prod", "generation": 9,
				"annotations": map[string]any{"deployment.kubernetes.io/revision": "9"},
			},
			"spec": map[string]any{
				"replicas": 3,
				"selector": map[string]any{"matchLabels": map[string]any{"app": "api"}},
				"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"image": "api:v9"}}}},
			},
			"status": map[string]any{
				"readyReplicas": 1, "updatedReplicas": 3, "availableReplicas": 1, "unavailableReplicas": 2,
				"observedGeneration": 8,
				"conditions":         []any{map[string]any{"type": "Progressing", "status": "True", "reason": "ReplicaSetUpdated"}},
			},
		})
	})
	mux.HandleFunc("/apis/apps/v1/namespaces/prod/replicasets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "api-9", "namespace": "prod", "annotations": map[string]any{"deployment.kubernetes.io/revision": "9"}},
				"spec":     map[string]any{"replicas": 3, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"image": "api:v9"}}}}},
				"status": map[string]any{
					"readyReplicas": 1, "availableReplicas": 1,
					"conditions": []any{map[string]any{"type": "ReplicaFailure", "status": "True", "message": "pods \"api-9-abc\" is forbidden: exceeded quota"}},
				},
			},
		}})
	})
	a, _ := newTestAdapter(t, mux)
	rows, summary, err := runRolloutStatusTool(context.Background(), a, map[string]any{"deployment": "api", "namespace": "prod"})
	if err != nil {
		t.Fatalf("runRolloutStatusTool: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0]["rollout_complete"] != false {
		t.Errorf("rollout_complete = %v, want false", rows[0]["rollout_complete"])
	}
	if got := rows[0]["newest_rs_condition"]; got != "pods \"api-9-abc\" is forbidden: exceeded quota" {
		t.Errorf("newest_rs_condition = %v, want the controller's message", got)
	}
	if !strings.Contains(summary, "rollout_complete=false") {
		t.Errorf("summary = %q", summary)
	}
}

func TestExec_FramesOutputAndReportsNonZeroExit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/prod/pods/api-1/exec", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query()["command"]; len(got) != 2 || got[0] != "ls" || got[1] != "-l" {
			t.Errorf("command = %v, want argv ls -l", got)
		}
		upgrader := websocket.Upgrader{Subprotocols: []string{"v4.channel.k8s.io"}}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(1, append([]byte{1}, []byte("total 0\n")...))
		// Channel 3 carries the API server's exit summary. A non-zero exit
		// arrives here, and reading only stdout would report success.
		_ = conn.WriteMessage(1, append([]byte{3}, []byte("command terminated with exit code 1")...))
	})
	a, _ := newTestAdapter(t, mux)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "exec_into_pod",
		Params:     map[string]any{"pod": "api-1", "namespace": "prod", "command": "ls -l"},
		ApprovedBy: "op-7",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Success {
		t.Error("a non-zero exit must not report success")
	}
	if !strings.Contains(res.Message, "total 0") || !strings.Contains(res.Message, "exit code 1") {
		t.Errorf("message = %q, want stdout and the exit summary", res.Message)
	}
}

// ── DSN parsing ────────────────────────────────────────────────────────

func TestNewKubeClient_UnsupportedForm(t *testing.T) {
	_, err := newKubeClient("postgres://localhost", time.Second)
	if err == nil || !strings.Contains(err.Error(), "unsupported DSN form") {
		t.Errorf("got %v, want an error naming the accepted forms", err)
	}
}

func TestNewKubeClient_KubeconfigRefusesExecPlugin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	cfg := `
apiVersion: v1
kind: Config
current-context: prod
clusters:
- name: c1
  cluster:
    server: https://api.example.com
contexts:
- name: prod
  context:
    cluster: c1
    user: sso
users:
- name: sso
  user:
    exec:
      command: aws
      args: ["eks", "get-token"]
`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := newKubeClient("kubeconfig://"+path, time.Second)
	if err == nil || !strings.Contains(err.Error(), "exec") {
		t.Errorf("got %v, want a refusal that names the exec plugin", err)
	}
}

func TestNewKubeClient_KubeconfigResolvesRelativePathsAgainstItsOwnDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `
apiVersion: v1
kind: Config
current-context: prod
clusters:
- name: c1
  cluster:
    server: https://api.example.com
    certificate-authority: ca.crt
contexts:
- name: prod
  context:
    cluster: c1
    user: u1
users:
- name: u1
  user:
    token: abc
`
	_, err := kubeClientFromKubeconfig([]byte(cfg), dir, time.Second)
	if err == nil || !strings.Contains(err.Error(), "ca.crt") {
		t.Errorf("got %v, want the relative CA to be resolved and rejected for its contents", err)
	}
}

func TestQuantityBytes(t *testing.T) {
	cases := map[string]float64{
		"1":    1,
		"100m": 0.1,
		"1Ki":  1024,
		"1Mi":  1 << 20,
		"2Gi":  2 << 30,
		"1K":   1000,
	}
	for in, want := range cases {
		got, ok := quantityBytes(in)
		if !ok || got != want {
			t.Errorf("quantityBytes(%q) = %v, %v; want %v, true", in, got, ok, want)
		}
	}
	if _, ok := quantityBytes("not-a-quantity"); ok {
		t.Error("a non-quantity must not be silently parsed")
	}
}
