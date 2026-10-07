package k8s

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// execStdout registers one exec subresource that prints the given text on
// the stdout channel and then reports a clean exit.
//
// It registers on the mux it is given. An earlier version built its own mux
// and discarded it, which made every measurement test fail on a 404 that
// looked exactly like a broken path computation — a stub that silently
// tests nothing is worse than no stub.
func execStdout(t *testing.T, mux *http.ServeMux, path, out string, commandSeen *[]string) {
	t.Helper()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if commandSeen != nil {
			*commandSeen = append(*commandSeen, strings.Join(r.URL.Query()["command"], " "))
		}
		upgrader := websocket.Upgrader{Subprotocols: []string{"v4.channel.k8s.io"}}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(1, append([]byte{1}, []byte(out)...))
		// Channel 3 carries the error stream, not an "exit code" field: a
		// successful exec sends the frame with NO payload, and anything
		// written into it is the API server's summary of a failure. Sending
		// "exit status 0" here would be a command that failed with that
		// message, which is what the first version of this stub did.
		_ = conn.WriteMessage(1, []byte{3})
	})
}

const dfLine = "Filesystem     1024-blocks       Used  Available Capacity Mounted on\n" +
	"/dev/sdb         104857600    94371840   10485760      90% /data\n"

func boundPVC(name, namespace string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{
			"resources":    map[string]any{"requests": map[string]any{"storage": "100Gi"}},
			"accessModes":  []any{"ReadWriteOnce"},
			"storageClass": nil,
			"volumeName":   "pv-1",
		},
		"status": map[string]any{
			"phase":    "Bound",
			"capacity": map[string]any{"storage": "100Gi"},
		},
	}
}

func runningPodMounting(name, namespace, pvc, mountPath string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{
			"volumes": []any{
				map[string]any{"name": "vol", "persistentVolumeClaim": map[string]any{"claimName": pvc}},
			},
			"containers": []any{
				map[string]any{
					"name":         "app",
					"volumeMounts": []any{map[string]any{"name": "vol", "mountPath": mountPath}},
				},
			},
		},
		"status": map[string]any{"phase": "Running"},
	}
}

// The measurement is the whole point of the second tool, so the happy path
// has to actually parse df's POSIX output and report a percentage.
func TestPVCUsage_MeasuresTheFilesystemThePodHasMounted(t *testing.T) {
	var command []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/test/persistentvolumeclaims/data-pvc", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, boundPVC("data-pvc", "test"))
	})
	mux.HandleFunc("/api/v1/namespaces/test/pods", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []any{runningPodMounting("app-1", "test", "data-pvc", "/data")},
		})
	})
	execStdout(t, mux, "/api/v1/namespaces/test/pods/app-1/exec", dfLine, &command)

	a, _ := newTestAdapter(t, mux)
	rows, _, err := runPVCUsage(context.Background(), a, map[string]any{"pvc": "data-pvc", "namespace": "test"})
	if err != nil {
		t.Fatalf("runPVCUsage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %#v", len(rows), rows)
	}
	row := rows[0]
	if row["measured"] != true {
		t.Fatalf("row is not marked measured: %#v", row)
	}
	if pct, _ := row["used_percent"].(float64); pct != 90.0 {
		t.Errorf("used_percent = %v, want 90", row["used_percent"])
	}
	// 94371840 KiB * 1024.
	if used, _ := row["used_bytes"].(float64); used != 94371840*1024 {
		t.Errorf("used_bytes = %v, want %d", row["used_bytes"], int64(94371840)*1024)
	}
	// The command is this adapter's, not the caller's.
	if len(command) != 1 || !strings.HasPrefix(command[0], "df ") {
		t.Errorf("ran %v, want a df", command)
	}
}

// An unbound claim has no filesystem. The row must say so rather than
// reporting the requested size as though it were the used size.
func TestPVCUsage_AnUnboundClaimIsUnknownNotEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/test/persistentvolumeclaims/pending-pvc", func(w http.ResponseWriter, r *http.Request) {
		claim := boundPVC("pending-pvc", "test")
		claim["status"] = map[string]any{"phase": "Pending"}
		writeJSON(w, http.StatusOK, claim)
	})
	a, _ := newTestAdapter(t, mux)
	rows, summary, err := runPVCUsage(context.Background(), a, map[string]any{"pvc": "pending-pvc", "namespace": "test"})
	if err != nil {
		t.Fatalf("runPVCUsage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0]["measured"] != false {
		t.Fatal("a claim that never bound must be reported as unmeasured")
	}
	if _, present := rows[0]["used_percent"]; present {
		t.Fatal("an unmeasured claim must carry no used_percent at all, not a zero")
	}
	if !strings.Contains(summary, "unknown rather than zero") {
		t.Errorf("summary = %q, want it to say the usage is unknown", summary)
	}
}

// No running pod means nothing has the claim mounted, and the honest answer
// is that there is no filesystem to read.
func TestPVCUsage_NoRunningPodIsUnknownNotEmpty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/test/persistentvolumeclaims/data-pvc", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, boundPVC("data-pvc", "test"))
	})
	mux.HandleFunc("/api/v1/namespaces/test/pods", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
	})
	a, _ := newTestAdapter(t, mux)
	rows, _, err := runPVCUsage(context.Background(), a, map[string]any{"pvc": "data-pvc", "namespace": "test"})
	if err != nil {
		t.Fatalf("runPVCUsage: %v", err)
	}
	if rows[0]["measured"] != false {
		t.Fatal("with no pod mounted there is no filesystem, which is unknown and not zero")
	}
	if !strings.Contains(rows[0]["unmeasured_reason"].(string), "no running pod") {
		t.Errorf("reason = %v, want it to name the missing pod", rows[0]["unmeasured_reason"])
	}
}

// A claim mounted as a raw block device has no path to run df on, and
// answering with a number from some other filesystem would be the worst
// possible outcome.
func TestPVCUsage_BlockDeviceIsUnknown(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/test/persistentvolumeclaims/data-pvc", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, boundPVC("data-pvc", "test"))
	})
	mux.HandleFunc("/api/v1/namespaces/test/pods", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []any{map[string]any{
				"metadata": map[string]any{"name": "app-1", "namespace": "test"},
				"spec": map[string]any{
					"volumes":    []any{map[string]any{"name": "vol", "persistentVolumeClaim": map[string]any{"claimName": "data-pvc"}}},
					"containers": []any{map[string]any{"name": "app", "volumeDevices": []any{map[string]any{"name": "vol", "devicePath": "/dev/xvdb"}}}},
				},
				"status": map[string]any{"phase": "Running"},
			}},
		})
	})
	a, _ := newTestAdapter(t, mux)
	rows, _, err := runPVCUsage(context.Background(), a, map[string]any{"pvc": "data-pvc", "namespace": "test"})
	if err != nil {
		t.Fatalf("runPVCUsage: %v", err)
	}
	if rows[0]["measured"] != false {
		t.Fatal("a block device has no filesystem path and cannot be measured")
	}
	if !strings.Contains(rows[0]["unmeasured_reason"].(string), "block device") {
		t.Errorf("reason = %v, want it to say the claim is a raw block device", rows[0]["unmeasured_reason"])
	}
}

// One pod mounting the claim at two paths is a fact about an application,
// and which one the operator means is not something to decide for them.
func TestPVCUsage_RefusesWhenThePodMountsTheClaimAtTwoPaths(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/test/persistentvolumeclaims/data-pvc", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, boundPVC("data-pvc", "test"))
	})
	mux.HandleFunc("/api/v1/namespaces/test/pods", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []any{map[string]any{
				"metadata": map[string]any{"name": "app-1", "namespace": "test"},
				"spec": map[string]any{
					"volumes": []any{map[string]any{"name": "vol", "persistentVolumeClaim": map[string]any{"claimName": "data-pvc"}}},
					"containers": []any{map[string]any{
						"name":         "app",
						"volumeMounts": []any{map[string]any{"name": "vol", "mountPath": "/data"}},
					}, map[string]any{
						"name":         "sidecar",
						"volumeMounts": []any{map[string]any{"name": "vol", "mountPath": "/scratch"}},
					}},
				},
				"status": map[string]any{"phase": "Running"},
			}},
		})
	})
	a, _ := newTestAdapter(t, mux)
	rows, _, err := runPVCUsage(context.Background(), a, map[string]any{"pvc": "data-pvc", "namespace": "test"})
	if err != nil {
		t.Fatalf("runPVCUsage: %v", err)
	}
	if rows[0]["measured"] != false {
		t.Fatal("two mount paths must not have one chosen for the caller")
	}
	reason := rows[0]["unmeasured_reason"].(string)
	if !strings.Contains(reason, "/data") || !strings.Contains(reason, "/scratch") {
		t.Errorf("reason = %q, want both paths named", reason)
	}
}

// Several replicas on one claim see the SAME filesystem, so the first in a
// stable order is a correct sample rather than a guess — and the row says
// which pod it came from.
func TestPVCUsage_SeveralPodsOnOneClaimMeasureTheSameFilesystem(t *testing.T) {
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/test/persistentvolumeclaims/data-pvc", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, boundPVC("data-pvc", "test"))
	})
	mux.HandleFunc("/api/v1/namespaces/test/pods", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []any{
				runningPodMounting("app-2", "test", "data-pvc", "/data"),
				runningPodMounting("app-1", "test", "data-pvc", "/data"),
			},
		})
	})
	execStdout(t, mux, "/api/v1/namespaces/test/pods/app-1/exec", dfLine, &seen)
	execStdout(t, mux, "/api/v1/namespaces/test/pods/app-2/exec", dfLine, nil)

	a, _ := newTestAdapter(t, mux)
	rows, _, err := runPVCUsage(context.Background(), a, map[string]any{"pvc": "data-pvc", "namespace": "test"})
	if err != nil {
		t.Fatalf("runPVCUsage: %v", err)
	}
	if rows[0]["measured"] != true {
		t.Fatal("replicas share a filesystem, so one measurement answers the question")
	}
	// The order is by name, not by whatever the API returned first, so the
	// same investigation reaches the same pod every run.
	if !strings.Contains(rows[0]["measured_from"].(string), "app-1") {
		t.Errorf("measured_from = %v, want the lowest-named pod for stability", rows[0]["measured_from"])
	}
}

// A pod that cannot supply the name is a measurement of nothing, and the
// caller must say which claim it is asking about.
func TestPVCUsage_RefusesWithoutAClaimName(t *testing.T) {
	mux := http.NewServeMux()
	a, _ := newTestAdapter(t, mux)
	if _, _, err := runPVCUsage(context.Background(), a, map[string]any{}); err == nil {
		t.Fatal("measuring every claim in a namespace is not what this tool does; it must be named")
	}
}

// ── df parsing ─────────────────────────────────────────────────────────

func TestParseDF(t *testing.T) {
	cases := []struct {
		name       string
		out        string
		wantPct    float64
		wantReason string
	}{
		{name: "normal", out: dfLine, wantPct: 90.0},
		{name: "the percentage column is recomputed, not trusted", out: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sdb 1000 500 500 99% /data\n", wantPct: 50.0},
		{name: "no rows", out: "", wantReason: "no filesystem lines"},
		{name: "a header and nothing else", out: "Filesystem 1024-blocks Used Available Capacity Mounted on\n", wantReason: "no filesystem lines"},
		{name: "too few columns", out: "/dev/sdb 1000 500\n", wantReason: "expected at least 5"},
		{name: "non-numeric counts", out: "/dev/sdb abc 500 500 50% /data\n", wantReason: "non-numeric block counts"},
		{name: "zero-sized filesystem", out: "/dev/sdb 0 0 0 0% /data\n", wantReason: "cannot be divided"},
		{name: "a wrapped device name is refused, not half-read", out: "/dev/mapper/a-very-long-volume-name-that-wrapped\n                1000      500      500      50% /data\n", wantReason: "wrapped its output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := parseDF(tc.out)
			if tc.wantReason != "" {
				if !strings.Contains(reason, tc.wantReason) {
					t.Fatalf("reason = %q, want it to contain %q", reason, tc.wantReason)
				}
				return
			}
			if reason != "" {
				t.Fatalf("unexpected refusal: %s", reason)
			}
			if got.usedPercent != tc.wantPct {
				t.Errorf("usedPercent = %v, want %v", got.usedPercent, tc.wantPct)
			}
		})
	}
}

// pvc_list is API data only. The row must never imply a fullness it cannot
// know, which is why the note is asserted rather than assumed.
func TestPVCList_ReportsTheRequestAndSaysUsageIsNotAmongThem(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/persistentvolumeclaims", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{boundPVC("data-pvc", "test")}})
	})
	a, _ := newTestAdapter(t, mux)
	rows, summary, err := runPVCList(context.Background(), a, map[string]any{})
	if err != nil {
		t.Fatalf("runPVCList: %v", err)
	}
	if len(rows) != 1 || rows[0]["requested_storage"] != "100Gi" {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0]["used_bytes_known"] != false {
		t.Error("the row must say the usage is not known, because the API does not report it")
	}
	if !strings.Contains(summary, "k8s.pvc_usage") {
		t.Errorf("summary = %q, want it to point at the tool that does measure", summary)
	}
}
