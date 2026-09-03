package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	state "github.com/vincent-wuhan/opskeeper/internal/manager/biz/agentteams/state"
)

func TestCLI_HITLNotifyAndTimeoutEscalation(t *testing.T) {
	var mutex sync.Mutex
	notifications := make([]hitlNotification, 0, 2)
	headers := make(map[string]string)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		notification := hitlNotification{}
		if err := json.NewDecoder(request.Body).Decode(&notification); err != nil {
			t.Errorf("decode webhook() error = %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		mutex.Lock()
		notifications = append(notifications, notification)
		headers["X-Opskeeper-Version"] = request.Header.Get("X-Opskeeper-Version")
		mutex.Unlock()
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()
	if err := run(ctx, []string{"init", "--state", path,
		"--task-id", "task-1", "--incident-id", "incident-1",
		"--project-id", "project-1", "--room-id", "!room:matrix.local",
	}); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	for _, phase := range []state.Phase{state.PhaseAlertDedup, state.PhaseRCA, state.PhaseCriticAudit, state.PhaseReview} {
		if err := run(ctx, []string{"advance", "--state", path, "--task-id", "task-1",
			"--phase", string(phase), "--status", "completed",
		}); err != nil {
			t.Fatalf("advance(%s) error = %v", phase, err)
		}
	}
	if err := run(ctx, []string{"start-hitl", "--state", path,
		"--task-id", "task-1", "--approval-request-id", "approval-1",
		"--reason", "cluster blast radius", "--admin", "@admin:matrix.local",
		"--notify-url", server.URL,
	}); err != nil {
		t.Fatalf("start-hitl() error = %v", err)
	}
	if err := run(ctx, []string{"hitl", "--state", path,
		"--task-id", "task-1", "--admin", "@admin:matrix.local",
		"--decision", "timeout_escalated", "--reason", "no response in 15m",
		"--approval-request-id", "approval-1", "--escalation-url", server.URL,
	}); err != nil {
		t.Fatalf("hitl() error = %v", err)
	}

	task, err := state.NewManager(path).Get(ctx, "task-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if task.Status != state.TaskBlocked || task.Phases[state.PhaseHITLApproval].Status != state.PhaseFailed {
		t.Fatalf("task after timeout = status %s, hitl %+v", task.Status, task.Phases[state.PhaseHITLApproval])
	}
	if len(task.HITLDecisions) != 1 || task.HITLDecisions[0].Decision != state.DecisionTimeoutEscalated {
		t.Fatalf("HITL decisions = %+v", task.HITLDecisions)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(notifications) != 2 {
		t.Fatalf("webhook calls = %d, want 2", len(notifications))
	}
	if headers["X-Opskeeper-Version"] != protocolVersion {
		t.Fatalf("webhook version header = %q", headers["X-Opskeeper-Version"])
	}
	if notifications[1].Decision != string(state.DecisionTimeoutEscalated) || notifications[1].DecidedAt == nil {
		t.Fatalf("escalation payload = %+v", notifications[1])
	}
}

func TestUsage_WritesActions(t *testing.T) {
	original := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	os.Stdout = write
	usage()
	os.Stdout = original
	if err := write.Close(); err != nil {
		t.Fatalf("close pipe() error = %v", err)
	}
	output, err := io.ReadAll(read)
	if err != nil {
		t.Fatalf("read usage() error = %v", err)
	}
	if !strings.Contains(string(output), "start-hitl") || !strings.Contains(string(output), "timeout_escalated") {
		t.Fatalf("usage does not document HITL actions: %s", output)
	}
}

func TestCLI_ExpireHITL_EscalatesOnlyDueApprovals(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()
	args := []string{"init", "--state", path, "--task-id", "task-1",
		"--incident-id", "incident-1", "--project-id", "project-1",
		"--room-id", "!room:matrix.local"}
	if err := run(ctx, args); err != nil {
		t.Fatalf("init() error = %v", err)
	}
	for _, phase := range []state.Phase{state.PhaseAlertDedup, state.PhaseRCA, state.PhaseCriticAudit, state.PhaseReview} {
		if err := run(ctx, []string{"advance", "--state", path, "--task-id", "task-1",
			"--phase", string(phase), "--status", "completed"}); err != nil {
			t.Fatalf("advance(%s) error = %v", phase, err)
		}
	}
	if err := run(ctx, []string{"start-hitl", "--state", path, "--task-id", "task-1",
		"--approval-request-id", "approval-1", "--admin", "@admin:matrix.local",
		"--notify-url", server.URL}); err != nil {
		t.Fatalf("start-hitl() error = %v", err)
	}
	if err := run(ctx, []string{"expire-hitl", "--state", path, "--timeout", "1ns",
		"--escalation-url", server.URL}); err != nil {
		t.Fatalf("expire-hitl() error = %v", err)
	}
	task, err := state.NewManager(path).Get(ctx, "task-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if task.Status != state.TaskBlocked || task.Phases[state.PhaseHITLApproval].Status != state.PhaseFailed {
		t.Fatalf("task after expire = status %s, hitl %+v", task.Status, task.Phases[state.PhaseHITLApproval])
	}
	if calls != 2 {
		t.Fatalf("webhook calls = %d, want request + escalation", calls)
	}
	if err := run(ctx, []string{"expire-hitl", "--state", path, "--timeout", "1ns",
		"--escalation-url", server.URL}); err != nil {
		t.Fatalf("expire-hitl() retry error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("webhook calls after retry = %d, want no duplicate escalation", calls)
	}
}

func TestCLI_EscalationFailureCanBeRetried(t *testing.T) {
	var calls int
	var mutex sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		mutex.Lock()
		calls++
		currentCall := calls
		mutex.Unlock()
		if currentCall == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()
	manager := state.NewManager(path)
	if _, err := manager.Initialize(ctx, state.InitRequest{
		TaskID: "task-1", IncidentID: "incident-1", ProjectID: "project-1", MatrixRoomID: "!room:matrix.local",
	}); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []state.Phase{state.PhaseAlertDedup, state.PhaseRCA, state.PhaseCriticAudit, state.PhaseReview} {
		if _, err := manager.Advance(ctx, "task-1", phase, state.PhaseCompleted); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.StartHITL(ctx, state.StartHITLRequest{
		TaskID: "task-1", Admin: "@admin:matrix.local", ApprovalRequestID: "approval-1",
	}); err != nil {
		t.Fatal(err)
	}

	firstErr := run(ctx, []string{"hitl", "--state", path, "--task-id", "task-1",
		"--admin", "hitl-timeout", "--decision", "timeout_escalated", "--reason", "no response",
		"--approval-request-id", "approval-1", "--escalation-url", server.URL})
	if firstErr == nil {
		t.Fatal("hitl timeout succeeded despite webhook failure")
	}
	task, err := manager.Get(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Escalation == nil || task.Escalation.Status != state.EscalationFailed || task.Escalation.Attempts != 1 {
		t.Fatalf("failed escalation = %+v", task.Escalation)
	}
	if err := run(ctx, []string{"retry-escalation", "--state", path, "--escalation-url", server.URL}); err != nil {
		t.Fatalf("retry-escalation() error = %v", err)
	}
	task, err = manager.Get(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Escalation.Status != state.EscalationDelivered || task.Escalation.Attempts != 2 || task.Escalation.LastError != "" {
		t.Fatalf("delivered escalation = %+v", task.Escalation)
	}
}
