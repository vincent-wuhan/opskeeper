// Package state 维护 AgentTeams 顶层事故闭环状态账本。
//
// state.json 只记录八阶段元状态；阶段内工具调用轨迹仍由 opskeeper
// loop_event_log 记录，避免形成两套执行状态机。
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

const SchemaVersion = "v1"

type Phase string

const (
	PhaseAlertDedup      Phase = "alert_dedup"
	PhaseRCA             Phase = "rca"
	PhaseCriticAudit     Phase = "critic_audit"
	PhaseReview          Phase = "review"
	PhaseHITLApproval    Phase = "hitl_approval"
	PhaseRepairExecution Phase = "repair_execution"
	PhaseVerification    Phase = "verification"
	PhasePostmortem      Phase = "postmortem"
)

type PhaseStatus string

const (
	PhasePending   PhaseStatus = "pending"
	PhaseRunning   PhaseStatus = "in_progress"
	PhaseCompleted PhaseStatus = "completed"
	PhaseFailed    PhaseStatus = "failed"
	PhaseSkipped   PhaseStatus = "skipped"
)

type TaskStatus string

const (
	TaskAssigned       TaskStatus = "assigned"
	TaskInProgress     TaskStatus = "in_progress"
	TaskAwaitingReview TaskStatus = "awaiting_review"
	TaskExecuting      TaskStatus = "executing"
	TaskVerifying      TaskStatus = "verifying"
	TaskCompleted      TaskStatus = "completed"
	TaskFailed         TaskStatus = "failed"
	TaskBlocked        TaskStatus = "blocked"
)

type EscalationStatus string

const (
	EscalationPending   EscalationStatus = "pending"
	EscalationDelivered EscalationStatus = "delivered"
	EscalationFailed    EscalationStatus = "failed"
)

type Decision string

const (
	DecisionApproved         Decision = "approved"
	DecisionRejected         Decision = "rejected"
	DecisionTimeoutEscalated Decision = "timeout_escalated"
)

var phaseOrder = []Phase{
	PhaseAlertDedup,
	PhaseRCA,
	PhaseCriticAudit,
	PhaseReview,
	PhaseHITLApproval,
	PhaseRepairExecution,
	PhaseVerification,
	PhasePostmortem,
}

var phaseStatuses = map[PhaseStatus]bool{
	PhasePending:   true,
	PhaseRunning:   true,
	PhaseCompleted: true,
	PhaseFailed:    true,
	PhaseSkipped:   true,
}

var taskStatuses = map[TaskStatus]bool{
	TaskAssigned:       true,
	TaskInProgress:     true,
	TaskAwaitingReview: true,
	TaskExecuting:      true,
	TaskVerifying:      true,
	TaskCompleted:      true,
	TaskFailed:         true,
	TaskBlocked:        true,
}

type Ledger struct {
	SchemaVersion string           `json:"schema_version"`
	Tasks         map[string]*Task `json:"tasks"`
}

type Task struct {
	IncidentID    string                `json:"incident_id"`
	ProjectID     string                `json:"project_id"`
	MatrixRoomID  string                `json:"matrix_room_id"`
	ProjectRoomID string                `json:"project_room_id,omitempty"`
	Type          string                `json:"type"`
	Status        TaskStatus            `json:"status"`
	AssignedTo    string                `json:"assigned_to"`
	CreatedAt     time.Time             `json:"created_at"`
	UpdatedAt     time.Time             `json:"updated_at"`
	Phases        map[Phase]*PhaseState `json:"phases"`
	HITLDecisions []HITLDecision        `json:"hitl_decisions"`
	AuditRefs     []AuditRef            `json:"audit_refs"`
	Escalation    *EscalationDelivery   `json:"escalation_delivery,omitempty"`
}

type PhaseState struct {
	AssignedTo        string      `json:"assigned_to"`
	Status            PhaseStatus `json:"status"`
	ApprovalRequestID string      `json:"approval_request_id,omitempty"`
	StartedAt         *time.Time  `json:"started_at"`
	CompletedAt       *time.Time  `json:"completed_at"`
}

type HITLDecision struct {
	Admin             string    `json:"admin"`
	Decision          Decision  `json:"decision"`
	DecidedAt         time.Time `json:"decided_at"`
	Reason            string    `json:"reason,omitempty"`
	ApprovalRequestID string    `json:"approval_request_id,omitempty"`
}

type AuditRef struct {
	AuditLogID string `json:"audit_log_id"`
	TraceID    string `json:"trace_id,omitempty"`
	EventID    string `json:"event_id,omitempty"`
}

type EscalationDelivery struct {
	IncidentID        string           `json:"incident_id"`
	MatrixRoomID      string           `json:"matrix_room_id"`
	ApprovalRequestID string           `json:"approval_request_id"`
	Admin             string           `json:"admin"`
	Reason            string           `json:"reason"`
	DecidedAt         time.Time        `json:"decided_at"`
	Status            EscalationStatus `json:"status"`
	Attempts          int              `json:"attempts"`
	LastError         string           `json:"last_error,omitempty"`
	UpdatedAt         time.Time        `json:"updated_at"`
}

type Manager struct {
	path string
	now  func() time.Time
}

type Option func(*Manager)

func WithClock(now func() time.Time) Option {
	return func(manager *Manager) {
		if now != nil {
			manager.now = now
		}
	}
}

func NewManager(path string, options ...Option) *Manager {
	manager := &Manager{path: path, now: func() time.Time { return time.Now().UTC() }}
	for _, option := range options {
		option(manager)
	}
	return manager
}

type InitRequest struct {
	TaskID        string
	IncidentID    string
	ProjectID     string
	MatrixRoomID  string
	ProjectRoomID string
}

func (manager *Manager) Initialize(ctx context.Context, request InitRequest) (*Task, error) {
	if err := validateInit(request); err != nil {
		return nil, err
	}
	var result *Task
	err := manager.update(ctx, func(ledger *Ledger) error {
		if _, exists := ledger.Tasks[request.TaskID]; exists {
			return fmt.Errorf("state: task %q already exists", request.TaskID)
		}
		for taskID, task := range ledger.Tasks {
			switch {
			case task.IncidentID == request.IncidentID:
				return fmt.Errorf("state: incident %q already tracked by task %q", request.IncidentID, taskID)
			case task.MatrixRoomID == request.MatrixRoomID:
				return fmt.Errorf("state: matrix room %q already used by task %q", request.MatrixRoomID, taskID)
			}
		}
		now := manager.now()
		started := now
		task := &Task{
			IncidentID:    request.IncidentID,
			ProjectID:     request.ProjectID,
			MatrixRoomID:  request.MatrixRoomID,
			ProjectRoomID: request.ProjectRoomID,
			Type:          "finite",
			Status:        TaskInProgress,
			AssignedTo:    "alerter",
			CreatedAt:     now,
			UpdatedAt:     now,
			HITLDecisions: make([]HITLDecision, 0),
			AuditRefs:     make([]AuditRef, 0),
			Phases: map[Phase]*PhaseState{
				PhaseAlertDedup:      {AssignedTo: "alerter", Status: PhaseRunning, StartedAt: &started},
				PhaseRCA:             {AssignedTo: "investigator", Status: PhasePending},
				PhaseCriticAudit:     {AssignedTo: "critic", Status: PhasePending},
				PhaseReview:          {AssignedTo: "reviewer", Status: PhasePending},
				PhaseHITLApproval:    {AssignedTo: "admin", Status: PhasePending},
				PhaseRepairExecution: {AssignedTo: "repairer", Status: PhasePending},
				PhaseVerification:    {AssignedTo: "verifier", Status: PhasePending},
				PhasePostmortem:      {AssignedTo: "reporter", Status: PhasePending},
			},
		}
		ledger.Tasks[request.TaskID] = task
		result = task
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (manager *Manager) Advance(ctx context.Context, taskID string, phase Phase, status PhaseStatus) (*Task, error) {
	if _, ok := phaseStatuses[status]; !ok || status == PhasePending {
		return nil, fmt.Errorf("state: unsupported advance status %q", status)
	}
	var result *Task
	err := manager.update(ctx, func(ledger *Ledger) error {
		task, err := ledger.task(taskID)
		if err != nil {
			return err
		}
		current, err := task.phase(phase)
		if err != nil {
			return err
		}
		if phase == PhaseHITLApproval {
			return errors.New("state: HITL phase can only transition through RecordHITL")
		}
		if phase == PhaseRepairExecution && current.Status == PhasePending && status == PhaseRunning {
			return errors.New("state: repair execution can only start after an approved HITL decision")
		}
		if current.Status == status {
			result = task
			return nil
		}
		now := manager.now()
		switch status {
		case PhaseRunning:
			current.Status = PhaseRunning
			current.StartedAt = &now
			current.CompletedAt = nil
		case PhaseCompleted, PhaseSkipped:
			current.Status = status
			if current.StartedAt == nil {
				current.StartedAt = &now
			}
			current.CompletedAt = &now
			if next, ok := nextPhase(phase); ok {
				nextState := task.Phases[next]
				if next == PhaseHITLApproval {
					nextState.Status = PhasePending
					nextState.StartedAt = nil
					nextState.CompletedAt = nil
				} else {
					nextState.Status = PhaseRunning
					nextState.StartedAt = &now
					nextState.CompletedAt = nil
				}
			}
		case PhaseFailed:
			current.Status = PhaseFailed
			if current.StartedAt == nil {
				current.StartedAt = &now
			}
			current.CompletedAt = &now
			task.Status = TaskFailed
		}
		if status != PhaseFailed {
			task.Status = deriveTaskStatus(task)
		}
		task.UpdatedAt = now
		result = task
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type HITLRequest struct {
	TaskID            string
	Admin             string
	Decision          Decision
	Reason            string
	ApprovalRequestID string
}

type StartHITLRequest struct {
	TaskID            string
	Admin             string
	Reason            string
	ApprovalRequestID string
}

func (manager *Manager) StartHITL(ctx context.Context, request StartHITLRequest) (*Task, error) {
	switch {
	case request.TaskID == "":
		return nil, errors.New("state: task_id is required")
	case request.Admin == "":
		return nil, errors.New("state: HITL admin is required")
	case request.ApprovalRequestID == "":
		return nil, errors.New("state: approval_request_id is required")
	}
	var result *Task
	err := manager.update(ctx, func(ledger *Ledger) error {
		task, err := ledger.task(request.TaskID)
		if err != nil {
			return err
		}
		review := task.Phases[PhaseReview]
		if review.Status != PhaseCompleted && review.Status != PhaseSkipped {
			return fmt.Errorf("state: review phase is %q, cannot start HITL", review.Status)
		}
		approval := task.Phases[PhaseHITLApproval]
		if approval.Status == PhaseRunning {
			if approval.AssignedTo != "admin" && approval.AssignedTo != request.Admin {
				return fmt.Errorf("state: HITL admin is %q, cannot reassign to %q", approval.AssignedTo, request.Admin)
			}
			if approval.ApprovalRequestID != request.ApprovalRequestID {
				return fmt.Errorf("state: HITL approval request is %q, cannot replace with %q", approval.ApprovalRequestID, request.ApprovalRequestID)
			}
			approval.AssignedTo = request.Admin
			task.Status = TaskAwaitingReview
			task.UpdatedAt = manager.now()
			result = task
			return nil
		}
		if approval.Status != PhasePending {
			return fmt.Errorf("state: HITL phase is %q, cannot start", approval.Status)
		}
		now := manager.now()
		approval.AssignedTo = request.Admin
		approval.ApprovalRequestID = request.ApprovalRequestID
		approval.Status = PhaseRunning
		approval.StartedAt = &now
		approval.CompletedAt = nil
		task.Status = TaskAwaitingReview
		task.UpdatedAt = now
		result = task
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (manager *Manager) RecordHITL(ctx context.Context, request HITLRequest) (*Task, error) {
	switch {
	case request.TaskID == "":
		return nil, errors.New("state: task_id is required")
	case request.Admin == "":
		return nil, errors.New("state: HITL admin is required")
	case request.Decision == DecisionRejected && request.Reason == "":
		return nil, errors.New("state: HITL rejection reason is required")
	}
	if request.Decision != DecisionApproved && request.Decision != DecisionRejected && request.Decision != DecisionTimeoutEscalated {
		return nil, fmt.Errorf("state: unsupported HITL decision %q", request.Decision)
	}
	var result *Task
	err := manager.update(ctx, func(ledger *Ledger) error {
		task, err := ledger.task(request.TaskID)
		if err != nil {
			return err
		}
		approval := task.Phases[PhaseHITLApproval]
		if approval.Status != PhaseRunning {
			return fmt.Errorf("state: HITL phase is %q, cannot record decision", approval.Status)
		}
		if request.Decision != DecisionTimeoutEscalated && approval.AssignedTo != request.Admin {
			return fmt.Errorf("state: HITL admin is %q, %q cannot decide", approval.AssignedTo, request.Admin)
		}
		if request.Decision != DecisionTimeoutEscalated {
			if request.ApprovalRequestID == "" {
				return errors.New("state: HITL approval_request_id is required")
			}
			if request.ApprovalRequestID != approval.ApprovalRequestID {
				return fmt.Errorf("state: HITL approval request is %q, %q cannot decide", approval.ApprovalRequestID, request.ApprovalRequestID)
			}
		} else {
			request.ApprovalRequestID = approval.ApprovalRequestID
		}
		now := manager.now()
		if approval.StartedAt == nil {
			approval.StartedAt = &now
		}
		approval.CompletedAt = &now
		repair := task.Phases[PhaseRepairExecution]
		switch request.Decision {
		case DecisionApproved:
			approval.Status = PhaseCompleted
			repair.Status = PhaseRunning
			repair.StartedAt = &now
			repair.CompletedAt = nil
			task.Status = TaskExecuting
		case DecisionRejected:
			approval.Status = PhaseCompleted
			repair.Status = PhaseFailed
			if repair.StartedAt == nil {
				repair.StartedAt = &now
			}
			repair.CompletedAt = &now
			task.Status = TaskBlocked
		case DecisionTimeoutEscalated:
			approval.Status = PhaseFailed
			task.Status = TaskBlocked
			task.Escalation = &EscalationDelivery{
				IncidentID:        task.IncidentID,
				MatrixRoomID:      task.MatrixRoomID,
				ApprovalRequestID: request.ApprovalRequestID,
				Admin:             request.Admin,
				Reason:            request.Reason,
				DecidedAt:         now,
				Status:            EscalationPending,
				UpdatedAt:         now,
			}
		}
		task.HITLDecisions = append(task.HITLDecisions, HITLDecision{
			Admin:             request.Admin,
			Decision:          request.Decision,
			DecidedAt:         now,
			Reason:            request.Reason,
			ApprovalRequestID: request.ApprovalRequestID,
		})
		task.UpdatedAt = now
		result = task
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type PendingEscalationTask struct {
	TaskID string
	Task   *Task
}

func (manager *Manager) PendingEscalations(ctx context.Context) ([]PendingEscalationTask, error) {
	results := make([]PendingEscalationTask, 0)
	err := manager.read(ctx, func(ledger *Ledger) error {
		for taskID, task := range ledger.Tasks {
			if task.Escalation != nil && task.Escalation.Status != EscalationDelivered {
				results = append(results, PendingEscalationTask{TaskID: taskID, Task: task})
			}
		}
		sort.Slice(results, func(left, right int) bool {
			return results[left].Task.Escalation.DecidedAt.Before(results[right].Task.Escalation.DecidedAt)
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (manager *Manager) CompleteEscalation(ctx context.Context, taskID string, deliveryErr error) (*Task, error) {
	var result *Task
	err := manager.update(ctx, func(ledger *Ledger) error {
		task, err := ledger.task(taskID)
		if err != nil {
			return err
		}
		if task.Escalation == nil {
			return fmt.Errorf("state: task %q has no escalation delivery", taskID)
		}
		now := manager.now()
		task.Escalation.Attempts++
		task.Escalation.UpdatedAt = now
		if deliveryErr == nil {
			task.Escalation.Status = EscalationDelivered
			task.Escalation.LastError = ""
		} else {
			task.Escalation.Status = EscalationFailed
			task.Escalation.LastError = deliveryErr.Error()
		}
		task.UpdatedAt = now
		result = task
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type DueHITLTask struct {
	TaskID string
	Task   *Task
}

func (manager *Manager) DueHITL(ctx context.Context, timeout time.Duration) ([]DueHITLTask, error) {
	if timeout <= 0 {
		return nil, errors.New("state: HITL timeout must be positive")
	}
	results := make([]DueHITLTask, 0)
	err := manager.read(ctx, func(ledger *Ledger) error {
		deadline := manager.now().Add(-timeout)
		for taskID, task := range ledger.Tasks {
			approval := task.Phases[PhaseHITLApproval]
			if approval.Status != PhaseRunning || approval.StartedAt == nil || approval.StartedAt.After(deadline) {
				continue
			}
			results = append(results, DueHITLTask{TaskID: taskID, Task: task})
		}
		sort.Slice(results, func(left, right int) bool {
			return results[left].Task.Phases[PhaseHITLApproval].StartedAt.Before(
				*results[right].Task.Phases[PhaseHITLApproval].StartedAt,
			)
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (manager *Manager) AddAudit(ctx context.Context, taskID string, ref AuditRef) (bool, error) {
	if ref.AuditLogID == "" {
		return false, errors.New("state: audit_log_id is required")
	}
	appended := false
	err := manager.update(ctx, func(ledger *Ledger) error {
		task, err := ledger.task(taskID)
		if err != nil {
			return err
		}
		for _, existing := range task.AuditRefs {
			if existing.AuditLogID == ref.AuditLogID {
				return nil
			}
		}
		task.AuditRefs = append(task.AuditRefs, ref)
		task.UpdatedAt = manager.now()
		appended = true
		return nil
	})
	return appended, err
}

func (manager *Manager) Get(ctx context.Context, taskID string) (*Task, error) {
	var result *Task
	err := manager.read(ctx, func(ledger *Ledger) error {
		task, err := ledger.task(taskID)
		if err != nil {
			return err
		}
		result = task
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (manager *Manager) LookupIncident(ctx context.Context, incidentID string) (*Task, error) {
	var result *Task
	err := manager.read(ctx, func(ledger *Ledger) error {
		for _, task := range ledger.Tasks {
			if task.IncidentID == incidentID {
				result = task
				return nil
			}
		}
		return fmt.Errorf("state: incident %q not found", incidentID)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (manager *Manager) LookupProject(ctx context.Context, projectID string) ([]*Task, error) {
	results := make([]*Task, 0)
	err := manager.read(ctx, func(ledger *Ledger) error {
		for _, task := range ledger.Tasks {
			if task.ProjectID == projectID {
				results = append(results, task)
			}
		}
		sort.Slice(results, func(left, right int) bool {
			if results[left].CreatedAt.Equal(results[right].CreatedAt) {
				return results[left].IncidentID < results[right].IncidentID
			}
			return results[left].CreatedAt.Before(results[right].CreatedAt)
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (manager *Manager) LookupRoom(ctx context.Context, roomID string) (*Task, error) {
	var result *Task
	err := manager.read(ctx, func(ledger *Ledger) error {
		for _, task := range ledger.Tasks {
			if task.MatrixRoomID == roomID {
				result = task
				return nil
			}
		}
		return fmt.Errorf("state: matrix room %q not found", roomID)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (manager *Manager) update(ctx context.Context, mutate func(*Ledger) error) error {
	return manager.withLock(ctx, func() error {
		ledger, exists, err := manager.load(ctx)
		if err != nil {
			return err
		}
		if !exists {
			ledger = &Ledger{SchemaVersion: SchemaVersion, Tasks: map[string]*Task{}}
		}
		if err := mutate(ledger); err != nil {
			return err
		}
		if err := validateLedger(ledger); err != nil {
			return err
		}
		return manager.save(ctx, ledger)
	})
}

func (manager *Manager) read(ctx context.Context, read func(*Ledger) error) error {
	return manager.withLock(ctx, func() error {
		ledger, exists, err := manager.load(ctx)
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("state: state.json not found")
		}
		return read(ledger)
	})
}

func (manager *Manager) load(ctx context.Context) (*Ledger, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	raw, err := os.ReadFile(manager.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("state: read %s: %w", manager.path, err)
	}
	ledger := &Ledger{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(ledger); err != nil {
		return nil, false, fmt.Errorf("state: decode %s: %w", manager.path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, false, fmt.Errorf("state: %s contains trailing JSON data", manager.path)
	}
	if err := validateLedger(ledger); err != nil {
		return nil, false, err
	}
	return ledger, true, nil
}

func (manager *Manager) save(ctx context.Context, ledger *Ledger) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encode ledger: %w", err)
	}
	raw = append(raw, '\n')
	directory := filepath.Dir(manager.path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("state: create state directory: %w", err)
	}
	temp, err := os.CreateTemp(directory, ".state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("state: create temp file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o644); err != nil {
		temp.Close()
		return fmt.Errorf("state: chmod temp file: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return fmt.Errorf("state: write temp file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("state: sync temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("state: close temp file: %w", err)
	}
	if err := os.Rename(tempName, manager.path); err != nil {
		return fmt.Errorf("state: replace state file: %w", err)
	}
	directoryFile, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("state: open state directory for sync: %w", err)
	}
	defer directoryFile.Close()
	if err := directoryFile.Sync(); err != nil {
		return fmt.Errorf("state: sync state directory: %w", err)
	}
	return nil
}

func (manager *Manager) withLock(ctx context.Context, operation func() error) error {
	if err := os.MkdirAll(filepath.Dir(manager.path), 0o755); err != nil {
		return fmt.Errorf("state: create state directory: %w", err)
	}
	lock, err := acquireLock(ctx, manager.path+".lock")
	if err != nil {
		return err
	}
	operationErr := operation()
	releaseErr := releaseLock(lock)
	if operationErr != nil {
		return operationErr
	}
	if releaseErr != nil {
		return releaseErr
	}
	return nil
}

type fileLock struct {
	file *os.File
}

func acquireLock(ctx context.Context, path string) (*fileLock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, fmt.Errorf("state: open lock file: %w", err)
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &fileLock{file: file}, nil
		}
		file.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("state: acquire file lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func releaseLock(lock *fileLock) error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return fmt.Errorf("state: release file lock: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("state: close lock file: %w", closeErr)
	}
	return nil
}

func (ledger *Ledger) task(taskID string) (*Task, error) {
	if taskID == "" {
		return nil, errors.New("state: task_id is required")
	}
	task, exists := ledger.Tasks[taskID]
	if !exists {
		return nil, fmt.Errorf("state: task %q not found", taskID)
	}
	return task, nil
}

func (task *Task) phase(phase Phase) (*PhaseState, error) {
	state, exists := task.Phases[phase]
	if !exists {
		return nil, fmt.Errorf("state: unknown phase %q", phase)
	}
	return state, nil
}

func nextPhase(phase Phase) (Phase, bool) {
	for index, current := range phaseOrder {
		if current == phase && index+1 < len(phaseOrder) {
			return phaseOrder[index+1], true
		}
	}
	return "", false
}

func deriveTaskStatus(task *Task) TaskStatus {
	switch {
	case task.Phases[PhasePostmortem].Status == PhaseCompleted:
		return TaskCompleted
	case task.Phases[PhaseVerification].Status == PhaseRunning:
		return TaskVerifying
	case task.Phases[PhaseRepairExecution].Status == PhaseRunning:
		return TaskExecuting
	case task.Phases[PhaseHITLApproval].Status == PhaseRunning:
		return TaskAwaitingReview
	case task.Phases[PhaseAlertDedup].Status == PhaseRunning ||
		task.Phases[PhaseRCA].Status == PhaseRunning ||
		task.Phases[PhaseCriticAudit].Status == PhaseRunning ||
		task.Phases[PhaseReview].Status == PhaseRunning:
		return TaskInProgress
	default:
		return TaskInProgress
	}
}

func validateInit(request InitRequest) error {
	switch {
	case request.TaskID == "":
		return errors.New("state: task_id is required")
	case request.IncidentID == "":
		return errors.New("state: incident_id is required")
	case request.ProjectID == "":
		return errors.New("state: project_id is required")
	case request.MatrixRoomID == "":
		return errors.New("state: matrix_room_id is required")
	}
	return nil
}

func validateLedger(ledger *Ledger) error {
	if ledger.SchemaVersion != SchemaVersion {
		return fmt.Errorf("state: schema_version must be %q", SchemaVersion)
	}
	if ledger.Tasks == nil {
		return errors.New("state: tasks must be an object")
	}
	incidents := map[string]string{}
	rooms := map[string]string{}
	for taskID, task := range ledger.Tasks {
		if taskID == "" || task == nil {
			return errors.New("state: tasks contains an empty or null task")
		}
		if task.Type != "finite" {
			return fmt.Errorf("state: task %q type must be finite", taskID)
		}
		if task.IncidentID == "" || task.ProjectID == "" || task.MatrixRoomID == "" || task.AssignedTo == "" {
			return fmt.Errorf("state: task %q has missing required mapping fields", taskID)
		}
		if !taskStatuses[task.Status] {
			return fmt.Errorf("state: task %q has unsupported status %q", taskID, task.Status)
		}
		if task.CreatedAt.IsZero() || task.UpdatedAt.IsZero() {
			return fmt.Errorf("state: task %q timestamps are required", taskID)
		}
		if len(task.Phases) != len(phaseOrder) {
			return fmt.Errorf("state: task %q must define exactly eight phases", taskID)
		}
		for _, phase := range phaseOrder {
			phaseState, exists := task.Phases[phase]
			if !exists || phaseState == nil {
				return fmt.Errorf("state: task %q is missing phase %q", taskID, phase)
			}
			if !phaseStatuses[phaseState.Status] || phaseState.AssignedTo == "" {
				return fmt.Errorf("state: task %q phase %q is invalid", taskID, phase)
			}
		}
		if previous, exists := incidents[task.IncidentID]; exists {
			return fmt.Errorf("state: incident %q is duplicated by tasks %q and %q", task.IncidentID, previous, taskID)
		}
		incidents[task.IncidentID] = taskID
		if previous, exists := rooms[task.MatrixRoomID]; exists {
			return fmt.Errorf("state: matrix room %q is duplicated by tasks %q and %q", task.MatrixRoomID, previous, taskID)
		}
		rooms[task.MatrixRoomID] = taskID
		auditIDs := make(map[string]bool, len(task.AuditRefs))
		for _, ref := range task.AuditRefs {
			if ref.AuditLogID == "" {
				return fmt.Errorf("state: task %q has an empty audit_log_id", taskID)
			}
			if auditIDs[ref.AuditLogID] {
				return fmt.Errorf("state: task %q has duplicate audit_log_id %q", taskID, ref.AuditLogID)
			}
			auditIDs[ref.AuditLogID] = true
		}
		for index, decision := range task.HITLDecisions {
			switch decision.Decision {
			case DecisionApproved, DecisionRejected, DecisionTimeoutEscalated:
			default:
				return fmt.Errorf("state: task %q HITL decision %d is unsupported", taskID, index)
			}
			if decision.Admin == "" || decision.DecidedAt.IsZero() {
				return fmt.Errorf("state: task %q HITL decision %d is incomplete", taskID, index)
			}
			if decision.Decision == DecisionRejected && decision.Reason == "" {
				return fmt.Errorf("state: task %q HITL rejection %d requires a reason", taskID, index)
			}
		}
	}
	return nil
}
