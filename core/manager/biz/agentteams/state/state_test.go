package state

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestInitialize_CreatesEightPhasesAndMappings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	manager := NewManager(path, WithClock(func() time.Time {
		return time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	}))

	task, err := manager.Initialize(context.Background(), InitRequest{
		TaskID:        "task-1",
		IncidentID:    "incident-1",
		ProjectID:     "project-1",
		MatrixRoomID:  "!room:matrix.local",
		ProjectRoomID: "!project:matrix.local",
	})
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if len(task.Phases) != len(phaseOrder) {
		t.Fatalf("phase count = %d, want %d", len(task.Phases), len(phaseOrder))
	}
	for _, phase := range phaseOrder {
		phaseState := task.Phases[phase]
		if phaseState.AssignedTo == "" {
			t.Errorf("phase %s has empty assigned_to", phase)
		}
		if phase == PhaseAlertDedup {
			if phaseState.Status != PhaseRunning || phaseState.StartedAt == nil {
				t.Errorf("alert_dedup = %+v, want in_progress with started_at", phaseState)
			}
			continue
		}
		if phaseState.Status != PhasePending || phaseState.StartedAt != nil || phaseState.CompletedAt != nil {
			t.Errorf("phase %s = %+v, want pending with null timestamps", phase, phaseState)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state.json() error = %v", err)
	}
	document := struct {
		Tasks map[string]struct {
			Phases map[string]struct {
				StartedAt   *string `json:"started_at"`
				CompletedAt *string `json:"completed_at"`
			} `json:"phases"`
		} `json:"tasks"`
	}{}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode state.json() error = %v", err)
	}
	if document.Tasks["task-1"].Phases["rca"].StartedAt != nil {
		t.Error("rca.started_at must serialize as null before start")
	}

	found, err := manager.LookupIncident(context.Background(), "incident-1")
	if err != nil || found.MatrixRoomID != "!room:matrix.local" {
		t.Fatalf("LookupIncident() = %+v, %v", found, err)
	}
	rooms, err := manager.LookupProject(context.Background(), "project-1")
	if err != nil || len(rooms) != 1 || rooms[0].IncidentID != "incident-1" {
		t.Fatalf("LookupProject() = %+v, %v", rooms, err)
	}
}

func TestAdvance_CompletesPhaseAndStartsNext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	manager := NewManager(path)
	if _, err := manager.Initialize(context.Background(), initRequest("task-1")); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if _, err := manager.Advance(context.Background(), "task-1", PhaseAlertDedup, PhaseCompleted); err != nil {
		t.Fatalf("Advance(alert_dedup) error = %v", err)
	}
	task, err := manager.Advance(context.Background(), "task-1", PhaseRCA, PhaseCompleted)
	if err != nil {
		t.Fatalf("Advance(rca) error = %v", err)
	}
	if task.Phases[PhaseRCA].Status != PhaseCompleted || task.Phases[PhaseRCA].CompletedAt == nil {
		t.Fatalf("rca = %+v, want completed with timestamp", task.Phases[PhaseRCA])
	}
	if task.Phases[PhaseCriticAudit].Status != PhaseRunning || task.Phases[PhaseCriticAudit].StartedAt == nil {
		t.Fatalf("critic_audit = %+v, want in_progress with timestamp", task.Phases[PhaseCriticAudit])
	}
	if task.Status != TaskInProgress {
		t.Fatalf("task status = %s, want in_progress", task.Status)
	}
}

func TestRecordHITL_AllDecisionsDriveClosedLoopState(t *testing.T) {
	decisions := []struct {
		name     string
		decision Decision
		reason   string
		check    func(*testing.T, *Task)
	}{
		{
			name:     "approved",
			decision: DecisionApproved,
			check: func(t *testing.T, task *Task) {
				if task.Phases[PhaseHITLApproval].Status != PhaseCompleted || task.Phases[PhaseRepairExecution].Status != PhaseRunning || task.Status != TaskExecuting {
					t.Fatalf("approved task = %+v", task)
				}
			},
		},
		{
			name:     "rejected",
			decision: DecisionRejected,
			reason:   "too risky",
			check: func(t *testing.T, task *Task) {
				if task.Phases[PhaseRepairExecution].Status != PhaseFailed || task.Status != TaskBlocked {
					t.Fatalf("rejected task = %+v", task)
				}
			},
		},
		{
			name:     "timeout",
			decision: DecisionTimeoutEscalated,
			check: func(t *testing.T, task *Task) {
				if task.Phases[PhaseHITLApproval].Status != PhaseFailed || task.Status != TaskBlocked {
					t.Fatalf("timeout task = %+v", task)
				}
			},
		},
	}
	for _, test := range decisions {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			manager := NewManager(path)
			if _, err := manager.Initialize(context.Background(), initRequest("task-1")); err != nil {
				t.Fatalf("Initialize() error = %v", err)
			}
			completeThroughReview(t, manager, "task-1")
			if _, err := manager.StartHITL(context.Background(), StartHITLRequest{
				TaskID:            "task-1",
				Admin:             "@admin:matrix.local",
				ApprovalRequestID: "approval-1",
			}); err != nil {
				t.Fatalf("StartHITL() error = %v", err)
			}
			task, err := manager.RecordHITL(context.Background(), HITLRequest{
				TaskID:            "task-1",
				Admin:             "@admin:matrix.local",
				Decision:          test.decision,
				Reason:            test.reason,
				ApprovalRequestID: "approval-1",
			})
			if err != nil {
				t.Fatalf("RecordHITL() error = %v", err)
			}
			test.check(t, task)
			if len(task.HITLDecisions) != 1 || task.HITLDecisions[0].Decision != test.decision {
				t.Fatalf("HITL decisions = %+v", task.HITLDecisions)
			}
		})
	}
}

func TestStartHITL_MarksApprovalInProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	manager := NewManager(path)
	if _, err := manager.Initialize(context.Background(), initRequest("task-1")); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	completeThroughReview(t, manager, "task-1")
	task, err := manager.StartHITL(context.Background(), StartHITLRequest{
		TaskID:            "task-1",
		Admin:             "@admin:matrix.local",
		ApprovalRequestID: "approval-1",
	})
	if err != nil {
		t.Fatalf("StartHITL() error = %v", err)
	}
	if task.Phases[PhaseHITLApproval].Status != PhaseRunning || task.Phases[PhaseHITLApproval].AssignedTo != "@admin:matrix.local" || task.Status != TaskAwaitingReview {
		t.Fatalf("task after StartHITL() = %+v", task)
	}
}

func TestRecordHITL_RejectsPendingPhaseAndWrongAdmin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	manager := NewManager(path)
	if _, err := manager.Initialize(context.Background(), initRequest("task-1")); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	_, err := manager.RecordHITL(context.Background(), HITLRequest{
		TaskID: "task-1", Admin: "@admin:matrix.local", Decision: DecisionApproved,
	})
	if err == nil {
		t.Fatal("RecordHITL() accepted a pending approval phase")
	}
	completeThroughReview(t, manager, "task-1")
	if _, err := manager.StartHITL(context.Background(), StartHITLRequest{
		TaskID: "task-1", Admin: "@admin:matrix.local", ApprovalRequestID: "approval-1",
	}); err != nil {
		t.Fatalf("StartHITL() error = %v", err)
	}
	_, err = manager.RecordHITL(context.Background(), HITLRequest{
		TaskID: "task-1", Admin: "@other:matrix.local", Decision: DecisionApproved, ApprovalRequestID: "approval-1",
	})
	if err == nil {
		t.Fatal("RecordHITL() accepted the wrong admin")
	}
}

func TestRecordHITL_RejectsMismatchedApprovalRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	manager := NewManager(path)
	if _, err := manager.Initialize(context.Background(), initRequest("task-1")); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	completeThroughReview(t, manager, "task-1")
	if _, err := manager.StartHITL(context.Background(), StartHITLRequest{
		TaskID: "task-1", Admin: "@admin:matrix.local", ApprovalRequestID: "approval-1",
	}); err != nil {
		t.Fatalf("StartHITL() error = %v", err)
	}
	if _, err := manager.RecordHITL(context.Background(), HITLRequest{
		TaskID: "task-1", Admin: "@admin:matrix.local", Decision: DecisionApproved, ApprovalRequestID: "approval-2",
	}); err == nil {
		t.Fatal("RecordHITL() accepted a mismatched approval request")
	}
	if _, err := manager.RecordHITL(context.Background(), HITLRequest{
		TaskID: "task-1", Admin: "@admin:matrix.local", Decision: DecisionApproved,
	}); err == nil {
		t.Fatal("RecordHITL() accepted a missing approval request")
	}
}

func TestAdvance_CannotBypassHITLToStartRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	manager := NewManager(path)
	if _, err := manager.Initialize(context.Background(), initRequest("task-1")); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	completeThroughReview(t, manager, "task-1")
	if _, err := manager.Advance(context.Background(), "task-1", PhaseHITLApproval, PhaseCompleted); err == nil {
		t.Fatal("Advance() allowed HITL completion without a decision")
	}
	if _, err := manager.Advance(context.Background(), "task-1", PhaseHITLApproval, PhaseSkipped); err == nil {
		t.Fatal("Advance() allowed HITL skip without a decision")
	}
	if _, err := manager.Advance(context.Background(), "task-1", PhaseRepairExecution, PhaseRunning); err == nil {
		t.Fatal("Advance() allowed repair execution to start without approval")
	}
}

func TestDueHITL_ReturnsOnlyExpiredApprovals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 8, 19, 9, 30, 0, 0, time.UTC)
	manager := NewManager(path, WithClock(func() time.Time { return now }))
	if _, err := manager.Initialize(context.Background(), initRequest("task-1")); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	completeThroughReview(t, manager, "task-1")
	if _, err := manager.StartHITL(context.Background(), StartHITLRequest{
		TaskID:            "task-1",
		Admin:             "@admin:matrix.local",
		ApprovalRequestID: "approval-1",
	}); err != nil {
		t.Fatalf("StartHITL() error = %v", err)
	}
	now = now.Add(16 * time.Minute)
	due, err := manager.DueHITL(context.Background(), 15*time.Minute)
	if err != nil {
		t.Fatalf("DueHITL() error = %v", err)
	}
	if len(due) != 1 || due[0].TaskID != "task-1" || due[0].Task.IncidentID != "incident-task-1" {
		t.Fatalf("due HITL tasks = %+v", due)
	}
	if _, err := manager.RecordHITL(context.Background(), HITLRequest{
		TaskID:            "task-1",
		Admin:             "@admin:matrix.local",
		Decision:          DecisionApproved,
		ApprovalRequestID: "approval-1",
	}); err != nil {
		t.Fatalf("RecordHITL() error = %v", err)
	}
	due, err = manager.DueHITL(context.Background(), 15*time.Minute)
	if err != nil {
		t.Fatalf("DueHITL() after decision error = %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("due HITL tasks after decision = %+v", due)
	}
}

func completeThroughReview(t *testing.T, manager *Manager, taskID string) {
	t.Helper()
	for _, phase := range []Phase{PhaseAlertDedup, PhaseRCA, PhaseCriticAudit, PhaseReview} {
		if _, err := manager.Advance(context.Background(), taskID, phase, PhaseCompleted); err != nil {
			t.Fatalf("Advance(%s) error = %v", phase, err)
		}
	}
}

func TestAddAudit_IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	manager := NewManager(path)
	if _, err := manager.Initialize(context.Background(), initRequest("task-1")); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	ref := AuditRef{AuditLogID: "audit-1", TraceID: "trace-1", EventID: "event-1"}
	for call := 0; call < 2; call++ {
		appended, err := manager.AddAudit(context.Background(), "task-1", ref)
		if err != nil {
			t.Fatalf("AddAudit() call %d error = %v", call, err)
		}
		if appended != (call == 0) {
			t.Fatalf("AddAudit() call %d appended = %v", call, appended)
		}
	}
	task, err := manager.Get(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(task.AuditRefs) != 1 {
		t.Fatalf("audit refs = %+v, want one", task.AuditRefs)
	}
}

func TestManager_ConcurrentAuditWritesDoNotLoseUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	manager := NewManager(path)
	if _, err := manager.Initialize(context.Background(), initRequest("task-1")); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	const writes = 40
	var wait sync.WaitGroup
	for index := 0; index < writes; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := NewManager(path).AddAudit(context.Background(), "task-1", AuditRef{
				AuditLogID: fmt.Sprintf("audit-%03d", index),
			})
			if err != nil {
				t.Errorf("AddAudit(%d) error = %v", index, err)
			}
		}(index)
	}
	wait.Wait()
	task, err := manager.Get(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(task.AuditRefs) != writes {
		sort.Slice(task.AuditRefs, func(left, right int) bool { return task.AuditRefs[left].AuditLogID < task.AuditRefs[right].AuditLogID })
		t.Fatalf("audit count = %d, want %d: %+v", len(task.AuditRefs), writes, task.AuditRefs)
	}
}

func TestManager_InvalidOperationDoesNotReplaceExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	manager := NewManager(path)
	if _, err := manager.Initialize(context.Background(), initRequest("task-1")); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before() error = %v", err)
	}
	if _, err := manager.Advance(context.Background(), "missing", PhaseRCA, PhaseCompleted); err == nil {
		t.Fatal("Advance(missing) error = nil, want error")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after() error = %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("state.json changed after a rejected mutation")
	}
}

func TestManager_RejectsTrailingJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	manager := NewManager(path)
	if _, err := manager.Initialize(context.Background(), initRequest("task-1")); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state.json() error = %v", err)
	}
	if err := os.WriteFile(path, append(raw, []byte(`{"dirty":true}`)...), 0o644); err != nil {
		t.Fatalf("write dirty state.json() error = %v", err)
	}
	if _, err := manager.Get(context.Background(), "task-1"); err == nil {
		t.Fatal("Get() accepted trailing JSON")
	}
}

func initRequest(taskID string) InitRequest {
	return InitRequest{
		TaskID:       taskID,
		IncidentID:   "incident-" + taskID,
		ProjectID:    "project-1",
		MatrixRoomID: "!room-" + taskID + ":matrix.local",
	}
}
