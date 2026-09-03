// Command agentteams-state 维护 AgentTeams state.json 顶层闭环状态。
//
// 该 CLI 是跨语言集成边界：AgentTeams Manager 可通过 manage-state.sh 调用，
// 不需要 import Go 包，也不需要理解 opskeeper 内部模型。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	state "github.com/vincent-wuhan/opskeeper/internal/manager/biz/agentteams/state"
)

const protocolVersion = "v1"

type hitlNotification struct {
	SchemaVersion     string     `json:"schema_version"`
	IncidentID        string     `json:"incident_id"`
	MatrixRoomID      string     `json:"matrix_room_id"`
	ApprovalRequestID string     `json:"approval_request_id"`
	Decision          string     `json:"decision,omitempty"`
	Admin             string     `json:"admin,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	DecidedAt         *time.Time `json:"decided_at,omitempty"`
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agentteams-state: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	switch args[0] {
	case "init":
		return cmdInit(ctx, args[1:])
	case "advance":
		return cmdAdvance(ctx, args[1:])
	case "start-hitl":
		return cmdStartHITL(ctx, args[1:])
	case "hitl":
		return cmdHITL(ctx, args[1:])
	case "expire-hitl":
		return cmdExpireHITL(ctx, args[1:])
	case "retry-escalation":
		return cmdRetryEscalation(ctx, args[1:])
	case "audit":
		return cmdAudit(ctx, args[1:])
	case "get", "lookup":
		return cmdLookup(ctx, args[1:])
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown action %q", args[0])
	}
}

func cmdInit(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	statePath := flags.String("state", "state.json", "state.json path")
	taskID := flags.String("task-id", "", "AgentTeams task key")
	incidentID := flags.String("incident-id", "", "incident ID")
	projectID := flags.String("project-id", "", "Matrix project room ID")
	roomID := flags.String("room-id", "", "Matrix incident room ID")
	projectRoomID := flags.String("project-room-id", "", "optional Matrix project room ID override")
	if err := flags.Parse(args); err != nil {
		return err
	}
	task, err := NewManager(*statePath).Initialize(ctx, state.InitRequest{
		TaskID:        *taskID,
		IncidentID:    *incidentID,
		ProjectID:     *projectID,
		MatrixRoomID:  *roomID,
		ProjectRoomID: *projectRoomID,
	})
	return writeJSON(task, err)
}

func cmdAdvance(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("advance", flag.ContinueOnError)
	statePath := flags.String("state", "state.json", "state.json path")
	taskID := flags.String("task-id", "", "AgentTeams task key")
	phase := flags.String("phase", "", "phase name")
	status := flags.String("status", "", "in_progress/completed/failed/skipped")
	if err := flags.Parse(args); err != nil {
		return err
	}
	task, err := NewManager(*statePath).Advance(ctx, *taskID, state.Phase(*phase), state.PhaseStatus(*status))
	return writeJSON(task, err)
}

func cmdStartHITL(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("start-hitl", flag.ContinueOnError)
	statePath := flags.String("state", "state.json", "state.json path")
	taskID := flags.String("task-id", "", "AgentTeams task key")
	approvalID := flags.String("approval-request-id", "", "HITL approval request ID")
	reason := flags.String("reason", "", "why admin approval is required")
	admin := flags.String("admin", "@admin:matrix.local", "admin Matrix ID")
	notifyURL := flags.String("notify-url", "", "Matrix bridge webhook URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *notifyURL == "" {
		return errors.New("--notify-url is required for Matrix @admin notification")
	}
	task, err := NewManager(*statePath).StartHITL(ctx, state.StartHITLRequest{
		TaskID:            *taskID,
		ApprovalRequestID: *approvalID,
		Admin:             *admin,
		Reason:            *reason,
	})
	if err != nil {
		return err
	}
	notification := hitlNotification{
		SchemaVersion:     protocolVersion,
		IncidentID:        task.IncidentID,
		MatrixRoomID:      task.MatrixRoomID,
		ApprovalRequestID: *approvalID,
		Admin:             *admin,
		Reason:            *reason,
	}
	if err := postHook(ctx, *notifyURL, notification); err != nil {
		return err
	}
	return writeJSON(task, nil)
}

func cmdHITL(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("hitl", flag.ContinueOnError)
	statePath := flags.String("state", "state.json", "state.json path")
	taskID := flags.String("task-id", "", "AgentTeams task key")
	admin := flags.String("admin", "", "admin Matrix ID")
	decision := flags.String("decision", "", "approved/rejected/timeout_escalated")
	reason := flags.String("reason", "", "decision or escalation reason")
	approvalID := flags.String("approval-request-id", "", "HITL approval request ID")
	escalationURL := flags.String("escalation-url", "", "PagerDuty/Feishu escalation webhook URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if state.Decision(*decision) == state.DecisionTimeoutEscalated && *escalationURL == "" {
		return errors.New("--escalation-url is required for timeout_escalated")
	}
	task, err := NewManager(*statePath).RecordHITL(ctx, state.HITLRequest{
		TaskID:            *taskID,
		Admin:             *admin,
		Decision:          state.Decision(*decision),
		Reason:            *reason,
		ApprovalRequestID: *approvalID,
	})
	if err != nil {
		return err
	}
	if state.Decision(*decision) != state.DecisionTimeoutEscalated {
		return writeJSON(task, nil)
	}
	notification := hitlNotification{
		SchemaVersion:     protocolVersion,
		IncidentID:        task.IncidentID,
		MatrixRoomID:      task.MatrixRoomID,
		ApprovalRequestID: *approvalID,
		Decision:          *decision,
		Admin:             *admin,
		Reason:            *reason,
	}
	if len(task.HITLDecisions) > 0 {
		notification.DecidedAt = &task.HITLDecisions[len(task.HITLDecisions)-1].DecidedAt
	}
	if err := postHook(ctx, *escalationURL, notification); err != nil {
		if _, completeErr := NewManager(*statePath).CompleteEscalation(ctx, *taskID, err); completeErr != nil {
			return errors.Join(fmt.Errorf("state saved but escalation failed: %w", err), completeErr)
		}
		return fmt.Errorf("state saved but escalation failed: %w", err)
	}
	completed, err := NewManager(*statePath).CompleteEscalation(ctx, *taskID, nil)
	return writeJSON(completed, err)
}

func cmdExpireHITL(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("expire-hitl", flag.ContinueOnError)
	statePath := flags.String("state", "state.json", "state.json path")
	timeout := flags.Duration("timeout", 15*time.Minute, "HITL timeout")
	admin := flags.String("admin", "hitl-timeout", "actor recorded for timeout escalation")
	reason := flags.String("reason", "no admin response before timeout", "escalation reason")
	escalationURL := flags.String("escalation-url", "", "PagerDuty/Feishu escalation webhook URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *escalationURL == "" {
		return errors.New("--escalation-url is required for timeout escalation")
	}
	manager := NewManager(*statePath)
	dueTasks, err := manager.DueHITL(ctx, *timeout)
	if err != nil {
		return err
	}
	escalated := make([]*state.Task, 0, len(dueTasks))
	for _, dueTask := range dueTasks {
		_, recordErr := manager.RecordHITL(ctx, state.HITLRequest{
			TaskID:   dueTask.TaskID,
			Admin:    *admin,
			Decision: state.DecisionTimeoutEscalated,
			Reason:   *reason,
		})
		if recordErr != nil {
			return recordErr
		}
	}
	pending, err := manager.PendingEscalations(ctx)
	if err != nil {
		return err
	}
	delivered, deliveryErr := deliverEscalations(ctx, manager, *escalationURL, pending)
	escalated = append(escalated, delivered...)
	if deliveryErr != nil {
		return deliveryErr
	}
	return writeJSON(struct {
		Tasks []*state.Task `json:"tasks"`
	}{Tasks: escalated}, nil)
}

func cmdRetryEscalation(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("retry-escalation", flag.ContinueOnError)
	statePath := flags.String("state", "state.json", "state.json path")
	escalationURL := flags.String("escalation-url", "", "PagerDuty/Feishu escalation webhook URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *escalationURL == "" {
		return errors.New("--escalation-url is required for escalation retry")
	}
	manager := NewManager(*statePath)
	pending, err := manager.PendingEscalations(ctx)
	if err != nil {
		return err
	}
	delivered, deliveryErr := deliverEscalations(ctx, manager, *escalationURL, pending)
	if deliveryErr != nil {
		return deliveryErr
	}
	return writeJSON(struct {
		Tasks []*state.Task `json:"tasks"`
	}{Tasks: delivered}, nil)
}

func deliverEscalations(ctx context.Context, manager *state.Manager, escalationURL string, pending []state.PendingEscalationTask) ([]*state.Task, error) {
	delivered := make([]*state.Task, 0, len(pending))
	var deliveryErrors []error
	for _, item := range pending {
		if item.Task == nil || item.Task.Escalation == nil {
			continue
		}
		err := postHook(ctx, escalationURL, escalationNotification(item.Task))
		completed, completeErr := manager.CompleteEscalation(ctx, item.TaskID, err)
		if completeErr != nil {
			deliveryErrors = append(deliveryErrors, completeErr)
			continue
		}
		if err != nil {
			deliveryErrors = append(deliveryErrors, fmt.Errorf("escalation for %s failed: %w", item.Task.IncidentID, err))
			continue
		}
		delivered = append(delivered, completed)
	}
	return delivered, errors.Join(deliveryErrors...)
}

func escalationNotification(task *state.Task) hitlNotification {
	escalation := task.Escalation
	return hitlNotification{
		SchemaVersion:     protocolVersion,
		IncidentID:        escalation.IncidentID,
		MatrixRoomID:      escalation.MatrixRoomID,
		ApprovalRequestID: escalation.ApprovalRequestID,
		Decision:          string(state.DecisionTimeoutEscalated),
		Admin:             escalation.Admin,
		Reason:            escalation.Reason,
		DecidedAt:         &escalation.DecidedAt,
	}
}

func cmdAudit(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("audit", flag.ContinueOnError)
	statePath := flags.String("state", "state.json", "state.json path")
	taskID := flags.String("task-id", "", "AgentTeams task key")
	auditLogID := flags.String("audit-log-id", "", "opskeeper audit log ID")
	traceID := flags.String("trace-id", "", "optional trace ID")
	eventID := flags.String("event-id", "", "optional event ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	appended, err := NewManager(*statePath).AddAudit(ctx, *taskID, state.AuditRef{
		AuditLogID: *auditLogID,
		TraceID:    *traceID,
		EventID:    *eventID,
	})
	if err != nil {
		return err
	}
	return writeJSON(struct {
		Appended bool `json:"appended"`
	}{Appended: appended}, nil)
}

func cmdLookup(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("lookup", flag.ContinueOnError)
	statePath := flags.String("state", "state.json", "state.json path")
	by := flags.String("by", "task", "task/incident/project/room")
	id := flags.String("id", "", "lookup ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manager := NewManager(*statePath)
	var tasks []*state.Task
	var err error
	switch strings.ToLower(*by) {
	case "task":
		var task *state.Task
		task, err = manager.Get(ctx, *id)
		tasks = []*state.Task{task}
	case "incident":
		var task *state.Task
		task, err = manager.LookupIncident(ctx, *id)
		tasks = []*state.Task{task}
	case "project":
		tasks, err = manager.LookupProject(ctx, *id)
	case "room":
		var task *state.Task
		task, err = manager.LookupRoom(ctx, *id)
		tasks = []*state.Task{task}
	default:
		return fmt.Errorf("unsupported lookup key %q", *by)
	}
	if err != nil {
		return err
	}
	return writeJSON(struct {
		Tasks []*state.Task `json:"tasks"`
	}{Tasks: tasks}, nil)
}

func NewManager(path string) *state.Manager {
	return state.NewManager(path)
}

func postHook(ctx context.Context, rawURL string, payload hitlNotification) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode webhook payload: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Opskeeper-Version", protocolVersion)
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("post webhook: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("webhook returned %s: %s", response.Status, strings.TrimSpace(string(detail)))
	}
	return nil
}

func writeJSON(value any, operationErr error) error {
	if operationErr != nil {
		return operationErr
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	return nil
}

func usage() {
	fmt.Print(`agentteams-state — AgentTeams state.json manager

USAGE:
  agentteams-state <action> [flags]

ACTIONS:
  init         Initialize the eight-phase incident state and mappings
  advance      Atomically advance a phase and start the next phase
  start-hitl   Mark HITL in progress and notify the Matrix admin bridge
  hitl         Record approved/rejected/timeout_escalated and update repair state
  expire-hitl  Record and escalate HITL requests older than --timeout
  retry-escalation Retry pending or failed HITL escalation deliveries
  audit        Idempotently append an opskeeper audit_log pointer
  lookup       Look up state by task/incident/project/room

All HTTP webhook calls send X-Opskeeper-Version: v1.
`)
}
