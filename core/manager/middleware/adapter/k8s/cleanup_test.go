package k8s

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// cleanupHarness wires an adapter whose exec subresource records every
// command it was asked to run and answers with a scripted result.
type cleanupHarness struct {
	commands    []string
	stdout      string
	exitNonZero bool
}

func (h *cleanupHarness) mux(t *testing.T, execPath string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/test/persistentvolumeclaims/data-pvc", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, boundPVC("data-pvc", "test"))
	})
	mux.HandleFunc("/api/v1/namespaces/test/pods", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []any{runningPodMounting("app-1", "test", "data-pvc", "/data")},
		})
	})
	mux.HandleFunc(execPath, func(w http.ResponseWriter, r *http.Request) {
		h.commands = append(h.commands, strings.Join(r.URL.Query()["command"], " "))
		upgrader := websocket.Upgrader{Subprotocols: []string{"v4.channel.k8s.io"}}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(1, append([]byte{1}, []byte(h.stdout)...))
		if h.exitNonZero {
			_ = conn.WriteMessage(1, append([]byte{3}, []byte("find: unrecognized option: -printf")...))
			return
		}
		_ = conn.WriteMessage(1, []byte{3})
	})
	return mux
}

const execPathCleanup = "/api/v1/namespaces/test/pods/app-1/exec"

func runClean(t *testing.T, h *cleanupHarness, args map[string]any) (int, string, bool, error) {
	t.Helper()
	mux := h.mux(t, execPathCleanup)
	a, _ := newTestAdapter(t, mux)
	return runCleanupLogs(context.Background(), a, args)
}

func baseArgs(extra map[string]any) map[string]any {
	args := map[string]any{"pvc": "data-pvc", "namespace": "test", "path": "/data/logs"}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// The default is the safe one. A call that did not think about dry_run
// reports what it would reclaim and changes nothing — which is the whole
// reason the flag exists at L4.
func TestCleanupLogs_DryRunIsTheDefaultAndTruncatesNothing(t *testing.T) {
	h := &cleanupHarness{stdout: "52428800\t/data/logs/app.log.1\n10485760\t/data/logs/app.log.2\n"}
	impacted, message, _, err := runClean(t, h, baseArgs(nil))
	if err != nil {
		t.Fatalf("runCleanupLogs: %v", err)
	}
	if impacted != 0 {
		t.Errorf("impacted = %d, want 0 — a dry run changes nothing", impacted)
	}
	if !strings.Contains(message, "dry run") {
		t.Errorf("message = %q, want it to say this was a dry run", message)
	}
	for _, cmd := range h.commands {
		if strings.Contains(cmd, "truncate") {
			t.Fatalf("a dry run ran truncate: %q", cmd)
		}
	}
	if len(h.commands) != 1 {
		t.Errorf("made %d calls, want the listing only: %#v", len(h.commands), h.commands)
	}
}

// Asking for it explicitly is the only way bytes come back, and the report
// still says what it did rather than only what it would do.
func TestCleanupLogs_TruncatesWhenDryRunIsExplicitlyFalse(t *testing.T) {
	h := &cleanupHarness{stdout: "52428800\t/data/logs/app.log.1\n10485760\t/data/logs/app.log.2\n"}
	impacted, message, ok, err := runClean(t, h, baseArgs(map[string]any{"dry_run": false}))
	if err != nil {
		t.Fatalf("runCleanupLogs: %v", err)
	}
	if !ok || impacted != 2 {
		t.Errorf("impacted = %d ok = %v, want 2 files truncated", impacted, ok)
	}
	if !strings.Contains(message, "62914560") {
		t.Errorf("message = %q, want the bytes it held", message)
	}
	truncated := false
	for _, cmd := range h.commands {
		if strings.Contains(cmd, "truncate") {
			truncated = true
		}
	}
	if !truncated {
		t.Fatalf("no truncate ran: %#v", h.commands)
	}
	// The list is re-derived rather than reused. Two calls, the second one
	// the truncation, both running the same find.
	if len(h.commands) != 2 {
		t.Errorf("made %d calls, want a listing and a truncation: %#v", len(h.commands), h.commands)
	}
}

// A path outside the discovered mount is the dangerous one: truncating the
// container's own /var/log while reporting the volume's bytes as reclaimed.
func TestCleanupLogs_RefusesAPathOutsideTheVolume(t *testing.T) {
	for _, path := range []string{"/var/log/app", "/tmp/anything", "/data"} {
		t.Run(path, func(t *testing.T) {
			h := &cleanupHarness{stdout: "1048576\t/etc/passwd\n"}
			_, _, _, err := runClean(t, h, baseArgs(map[string]any{"path": path, "dry_run": false}))
			if err == nil {
				t.Fatalf("path %q was accepted", path)
			}
			if !strings.Contains(err.Error(), "not inside") && !strings.Contains(err.Error(), "mount root") {
				t.Errorf("error = %q, want it to say the path is not inside the mount", err)
			}
			if len(h.commands) != 0 {
				t.Fatalf("ran %v against a path outside the volume", h.commands)
			}
		})
	}
}

func TestCleanupLogs_RefusesSystemDirectories(t *testing.T) {
	for _, path := range []string{"/etc", "/var/lib/thing", "/usr/share", "/", "/data/../../etc"} {
		t.Run(path, func(t *testing.T) {
			if err := validateCleanupPath(path); err == nil {
				t.Fatalf("path %q was accepted", path)
			}
		})
	}
	if err := validateCleanupPath("/data/logs"); err != nil {
		t.Errorf("a log directory inside the volume must be accepted: %v", err)
	}
	if err := validateCleanupPath("data/logs"); err == nil {
		t.Error("a relative path must be refused; the container's working directory is not a known quantity")
	}
}

// Truncating a log something is still appending to destroys the lines
// written in the last few minutes, which is the window an operator is
// reading when they start this.
func TestCleanupLogs_RefusesAnAgeShortEnoughToCatchLiveLogs(t *testing.T) {
	h := &cleanupHarness{stdout: "1048576\t/data/logs/app.log\n"}
	_, _, _, err := runClean(t, h, baseArgs(map[string]any{"older_than_days": 0}))
	if err == nil {
		t.Fatal("an age of zero days must be refused")
	}
	if !strings.Contains(err.Error(), "older_than_days") {
		t.Errorf("error = %q, want it to name the argument", err)
	}
}

// Over the cap the call refuses. Truncating the first 500 of 5000 files and
// reporting success is the failure this bound exists to prevent.
func TestCleanupLogs_RefusesRatherThanClearingPartOfTheSet(t *testing.T) {
	var listing strings.Builder
	for i := 0; i < cleanupMaxFilesPerCall+1; i++ {
		listing.WriteString("1048576\t/data/logs/app.log." + strconv.Itoa(i) + "\n")
	}
	h := &cleanupHarness{stdout: listing.String()}
	_, _, _, err := runClean(t, h, baseArgs(map[string]any{"dry_run": false}))
	if err == nil {
		t.Fatal("more files than one call will touch must be refused")
	}
	if !strings.Contains(err.Error(), "rather than clearing part of the set") {
		t.Errorf("error = %q, want it to explain the refusal", err)
	}
	for _, cmd := range h.commands {
		if strings.Contains(cmd, "truncate") {
			t.Fatalf("it truncated anyway: %q", cmd)
		}
	}
}

// Nothing to reclaim is a successful no-op, not a failure and not a
// truncation of whatever happened to be there.
func TestCleanupLogs_NothingQualifyingIsASuccessfulNoOp(t *testing.T) {
	h := &cleanupHarness{stdout: ""}
	_, message, ok, err := runClean(t, h, baseArgs(map[string]any{"dry_run": false}))
	if err != nil || !ok {
		t.Fatalf("ok = %v err = %v, want a successful no-op", ok, err)
	}
	if !strings.Contains(message, "nothing was truncated") {
		t.Errorf("message = %q, want it to say nothing was touched", message)
	}
}

// An image whose find has no -printf cannot be listed, and cleaning
// something it could not list is exactly the thing not to do.
func TestCleanupLogs_ReportsAnImageWithoutGNUFind(t *testing.T) {
	h := &cleanupHarness{exitNonZero: true}
	_, _, _, err := runClean(t, h, baseArgs(map[string]any{"dry_run": false}))
	if err == nil {
		t.Fatal("a non-zero find must be an error")
	}
	if !strings.Contains(err.Error(), "GNU find") {
		t.Errorf("error = %q, want it to name the missing find feature", err)
	}
}

// A listing that cannot be read is not a partial answer. A total computed
// from some of the rows is a number the operator will believe.
func TestParseCleanupListing_RefusesAnUnreadableRow(t *testing.T) {
	cases := []struct {
		name string
		out  string
	}{
		{"no size separator", "1048576 /data/logs/app.log\n"},
		{"non-numeric size", "big\t/data/logs/app.log\n"},
		{"empty path", "1048576\t\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseCleanupListing(tc.out); err == nil {
				t.Fatal("a listing this adapter cannot read must be refused")
			}
		})
	}
	files, err := parseCleanupListing("1048576\t/data/logs/b.log\n2097152\t/data/logs/a.log\n")
	if err != nil {
		t.Fatalf("parseCleanupListing: %v", err)
	}
	if len(files) != 2 || files[0].path != "/data/logs/a.log" {
		t.Errorf("files = %#v, want both, ordered by path so the report is stable", files)
	}
	if files[0].size != 2097152 {
		t.Errorf("size = %d, want 2097152", files[0].size)
	}
}

func TestPathWithin(t *testing.T) {
	cases := []struct {
		base, child string
		want        bool
	}{
		{"/data", "/data", true},
		{"/data", "/data/logs", true},
		{"/data", "/database", false},
		{"/data", "/var/log", false},
		{"/", "/anything", true},
	}
	for _, tc := range cases {
		if got := pathWithin(tc.base, tc.child); got != tc.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", tc.base, tc.child, got, tc.want)
		}
	}
}
