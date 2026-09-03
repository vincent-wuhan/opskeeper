// Package tools — recovery_execute_basetool.go
//
// recovery.execute 是 zero-manual-ops-loop Day 4 落地的闭环修复
// BaseTool，由 orchestrator 的 recovered phase 调用（见
// internal/manager/biz/loop/recovery.go）。它是 verify_recovery 之后的
// 动作执行环节：闭环已经拿到 investigator 给出的 skill_id / target /
// resource_type，并且 verify_recovery 已经判定需要修复；本工具负责把
// mutating proposal gate 与底层 host_restart_service 桥接起来。
//
// 设计依据：
//   - OpenSpec spec: recovery-execute（事实源）
//   - docs/superpowers/specs/2026-08-10-zero-manual-ops-recovery-postmortem-design.md §B
//
// 关键约束：
//   - 在执行前必须先验证 caller 已经写入一条 mutating proposal（audit
//     防越权 —— 没有 proposal 就代表 reviewer 没批）；AgentTeams caller
//     禁止绕过，skipAudit 仅保留给非 AgentTeams 离线演练。
//   - Class="write"，与 host_restart_service 一致 —— ReviewGate 装饰
//     器同样会拦截本工具，但 audit 校验放在工具内部做（不解码 argsJSON
//     两遍）。
//   - parameters.command 决定分发："restart_service" 委托给底层
//     host_restart_service；"kill_process" 只调用 case-owned host fixture；
//     "noop" 用于演练 / 单元测试。其它 command 直接拒绝。
//   - 返回 envelope 严格使用 schema_version=v1，与 verify_recovery 同
//     shape；LLM 与 orchestrator 反向校验都共用同一个 RevDelta 解析。
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
	hitlmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

// ToolNameRecoveryExecute is the stable wire name the LLM sees.
// Mirrors the spec key (recovery-execute) but with dot-form for
// opskeeper's namespace taxonomy (recovery.* are loop-phase tools).
const ToolNameRecoveryExecute = "recovery.execute"

// RecoveryExecuteDescription is the one-line blurb the LLM reads when
// picking tools. Keep it terse — verify_recovery already explained
// "what is the recovery loop", this just says "what this tool does".
const RecoveryExecuteDescription = "Execute a closed-loop recovery action for an incident after verify_recovery returned a " +
	"passed=false verdict. MUTATING — atomically reserves an exact approved HITL proposal before dispatching " +
	"to host_restart_service or the case-owned host fixture controller."

// recoveryExecuteWhenToUse is the routing hint shown under the
// "When to use" header. Mirrors verify_recovery's NOT-for list so a
// runaway agent doesn't reach for execute before diagnose.
const recoveryExecuteWhenToUse = "Use ONLY inside the orchestrator's recovered phase when verify_recovery returned " +
	"passed=false. Do NOT propose restart without an incident_id and skill_id (orchestrator supplies them). " +
	"This tool is MUTATING: it requires an exact proposal_id and atomically reserves its approved HITL proposal; " +
	"if the proposal is missing or mismatched the tool returns a proposal reservation error " +
	"without touching the edge. AgentTeams callers must not set parameters.skip_audit."

// RecoveryExecuteSchema is the JSON Schema of the tool's argument
// object. Mirrors verify_recovery's shape (incident_id / skill_id /
// target / resource_type / baseline_window / compare_window /
// tolerance) plus parameters.command so the orchestrator can switch
// between restart_service, kill_process, resize_pool, and noop.
var RecoveryExecuteSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "incident_id":      {"type": "string", "minLength": 1, "description": "Incident id the recovery action targets. Used as the proposal session binding."},
    "proposal_id":      {"type": "string", "pattern": "^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$", "description": "Exact approved HITL proposal reserved for this action."},
    "skill_id":         {"type": "string", "minLength": 1, "description": "Skill id selected by investigator (matches verify_recovery.skill_id)."},
    "target":           {"type": "string", "minLength": 1, "description": "Resource locator (host-1 / pg-cluster-x / k8s-deploy-y). Echoed into the audit envelope."},
    "resource_type":    {"type": "string", "enum": ["host","pg","redis","k8s","app"], "description": "Target resource type — host dispatches to restart_service or the case-owned fixture; other types are drill-only."},
    "baseline_window":  {"type": "string", "pattern": "^[1-9][0-9]*(m|h|s)$", "description": "Echo from verify_recovery's baseline_window; not re-validated here (verify already ran the gate)."},
    "compare_window":   {"type": "string", "pattern": "^[1-9][0-9]*(m|h|s)$", "description": "Echo from verify_recovery's compare_window."},
    "tolerance":        {"type": "number", "minimum": 0, "maximum": 1, "description": "Echo from verify_recovery's tolerance."},
    "parameters": {
      "type": "object",
      "additionalProperties": false,
      "description": "Action-specific payload. restart_service binds device_id/service; kill_process binds incident_id/fixture_manifest_id; resize_pool binds incident_id/pool_manifest_id; skip_audit is forbidden for AgentTeams.",
      "properties": {
        "command":        {"type": "string", "enum": ["restart_service", "kill_process", "resize_pool", "noop"]},
        "device_id":      {"type": "integer", "minimum": 1},
        "service":        {"type": "string", "minLength": 1, "maxLength": 255},
        "incident_id":        {"type": "string", "minLength": 1, "maxLength": 64},
        "fixture_manifest_id":{"type": "string", "minLength": 8, "maxLength": 128},
        "pool_manifest_id":   {"type": "string", "minLength": 8, "maxLength": 128},
        "reason":         {"type": "string", "minLength": 1, "maxLength": 512},
        "skip_audit":  {"type": "boolean", "default": false}
      }
    }
  },
  "required": ["incident_id","proposal_id","skill_id","target","resource_type","parameters"],
  "additionalProperties": false
}`)

// RecoveryExecuteArgs is the typed form of RecoveryExecuteSchema.
// Mirrors verify_recovery's VerifyRecoveryArgs (skill_id / target /
// resource_type / windows / tolerance) plus an embedded Parameters
// blob that the dispatcher parses.
type RecoveryExecuteArgs struct {
	IncidentID     string          `json:"incident_id"`
	ProposalID     string          `json:"proposal_id"`
	SkillID        string          `json:"skill_id"`
	Target         string          `json:"target"`
	ResourceType   string          `json:"resource_type"`
	BaselineWindow string          `json:"baseline_window,omitempty"`
	CompareWindow  string          `json:"compare_window,omitempty"`
	Tolerance      float64         `json:"tolerance,omitempty"`
	Parameters     json.RawMessage `json:"parameters"`
}

// recoveryExecuteParameters is the typed form of parameters.
// Command is the dispatcher switch; the remaining fields are
// forwarded to host_restart_service when command=="restart_service".
type recoveryExecuteParameters struct {
	Command           string `json:"command"`
	DeviceID          uint64 `json:"device_id,omitempty"`
	Service           string `json:"service,omitempty"`
	Reason            string `json:"reason,omitempty"`
	IncidentID        string `json:"incident_id,omitempty"`
	FixtureManifestID string `json:"fixture_manifest_id,omitempty"`
	PoolManifestID    string `json:"pool_manifest_id,omitempty"`
	SkipAudit         bool   `json:"skip_audit,omitempty"`
}

var errRecoveryProposalRequired = fmt.Errorf("%s: approved proposal_id required", ToolNameRecoveryExecute)

// recoveryExecuteEnvelope is the output shape LLM + orchestrator see.
// schema_version=v1 matches verify_recovery's VerifiedDelta shape so
// the orchestrator's ValidateVerifiedDelta can be reused. result_json
// is set to host_restart_service's raw envelope when command =
// "restart_service"; nil on noop.
type recoveryExecuteEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	IncidentID    string          `json:"incident_id"`
	Executed      bool            `json:"executed"`
	Command       string          `json:"command"`
	RestartedAt   time.Time       `json:"restarted_at"`
	ResultJSON    json.RawMessage `json:"result,omitempty"`
}

// MutatingProposalAuditRepo is the narrow seam the recovery.execute
// tool uses to reserve and complete one exact approved HITL proposal.
// Production binding validates proposal_id/session_id/kind/state/
// expiry/action/resource, transitions approved→executing before the
// mutation, and then transitions executing→executed/failed.
//
// Why an interface (vs importing the repo): same boundary reasoning
// as decorators.MutatingProposalSink — keeps this tool free of biz
// repo coupling and lets the unit tests stand up a fake without a
// GORM DB. Nil audit repo is treated as "audit disabled" so
// production wiring in cmd/opskeeper/main.go can choose to omit the
// tool entirely when the repo isn't ready.
type MutatingProposalAuditRepo interface {
	ReserveApprovedProposal(ctx context.Context, request RecoveryProposalRequest) error
	CompleteReservedProposal(ctx context.Context, proposalID string, success bool, resultJSON string) error
}

type RecoveryProposalRequest struct {
	ProposalID string
	SessionID  string
	Kind       string
	Action     string
	Resource   string
	Execution  hitlmodel.RecoveryExecutionParameters
}

// HostProcessTerminator is the only execution seam allowed for kill_process.
// Implementations resolve the incident-owned process group server-side and
// must never accept a client-provided PID.
type HostProcessTerminator interface {
	Status(ctx context.Context, request HostProcessTerminationRequest) (HostFixtureStatus, error)
	Terminate(ctx context.Context, request HostProcessTerminationRequest) (json.RawMessage, error)
}

type HostProcessTerminationRequest struct {
	IncidentID        string
	FixtureManifestID string
}

// PoolRecoveryExecutor is the only execution seam allowed for resize_pool.
// Implementations resolve the incident-owned PostgreSQL pool server-side and
// prove recovery with a new database probe.
type PoolRecoveryExecutor interface {
	Status(ctx context.Context, request PoolRecoveryRequest) (PoolFixtureStatus, error)
	Recover(ctx context.Context, request PoolRecoveryRequest) (json.RawMessage, error)
}

// RecoveryExecuteTool is the BaseTool form of recovery.execute.
// dispatcher is the inner tool that actually performs the mutation
// (today: host_restart_service). It is held by interface so tests can
// inject a stub without pulling in the real RestartServiceTool.
type RecoveryExecuteTool struct {
	dispatcher    basetool.BaseTool
	terminator    HostProcessTerminator
	poolRecoverer PoolRecoveryExecutor
	auditRepo     MutatingProposalAuditRepo
	log           *slog.Logger
}

// NewRecoveryExecuteTool builds the BaseTool. dispatcher must be the
// host_restart_service BaseTool (or a compatible inner) — recovery.
// execute delegates to it for command=="restart_service". auditRepo
// MAY be nil (test-time only); production wiring should pass a real
// repo and gate the tool's registration on it being non-nil.
func NewRecoveryExecuteTool(dispatcher basetool.BaseTool, terminator HostProcessTerminator, auditRepo MutatingProposalAuditRepo, log *slog.Logger) *RecoveryExecuteTool {
	if log == nil {
		log = slog.Default()
	}
	return &RecoveryExecuteTool{
		dispatcher:    dispatcher,
		terminator:    terminator,
		poolRecoverer: nil,
		auditRepo:     auditRepo,
		log:           log,
	}
}

func (t *RecoveryExecuteTool) SetPoolRecoveryExecutor(recoverer PoolRecoveryExecutor) {
	t.poolRecoverer = recoverer
}

// Info returns the tool metadata. Class="write" — the call mutates
// edge state when command=="restart_service". The ReviewGate decorator
// (chain.go's Wrap) intercepts calls whose Info.Class is "write" or
// "destructive" and spawns a reviewer worker before dispatch; the
// audit check inside InvokableRun is the **inner** guard (catches
// bypass paths the decorator might miss).
func (t *RecoveryExecuteTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        ToolNameRecoveryExecute,
		Description: RecoveryExecuteDescription,
		WhenToUse:   recoveryExecuteWhenToUse,
		Parameters:  RecoveryExecuteSchema,
		Class:       "write",
	}, nil
}

// InvokableRun is the recovery.execute dispatch loop. Flow:
//  1. Unmarshal args; reject missing required fields with a clear error.
//  2. Non-AgentTeams callers may use parameters.skip_audit for offline
//     drills. AgentTeams callers always require auditRepo and an approved
//     proposal, regardless of skip_audit.
//  3. Switch on parameters.command:
//     - "restart_service": build host_restart_service argsJSON from
//     parameters.{device_id,service,reason}, call dispatcher, embed
//     the raw response into the envelope.
//     - "kill_process": call the case-owned fixture terminator with
//     parameters.{incident_id,fixture_manifest_id}; never accept a PID.
//     - "noop": skip dispatch, return executed=true with command="noop".
//     - else: error.
func (t *RecoveryExecuteTool) InvokableRun(ctx context.Context, argsJSON string, opts ...basetool.InvokeOption) (string, error) {
	var in RecoveryExecuteArgs
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("%s: bad args: %w", ToolNameRecoveryExecute, err)
	}
	if in.IncidentID == "" {
		return "", fmt.Errorf("%s: incident_id required", ToolNameRecoveryExecute)
	}
	if in.SkillID == "" {
		return "", fmt.Errorf("%s: skill_id required", ToolNameRecoveryExecute)
	}
	if in.Target == "" {
		return "", fmt.Errorf("%s: target required", ToolNameRecoveryExecute)
	}
	if in.ResourceType == "" {
		return "", fmt.Errorf("%s: resource_type required", ToolNameRecoveryExecute)
	}
	if len(in.Parameters) == 0 {
		return "", fmt.Errorf("%s: parameters required", ToolNameRecoveryExecute)
	}
	var params recoveryExecuteParameters
	parameterDecoder := json.NewDecoder(bytes.NewReader(in.Parameters))
	parameterDecoder.DisallowUnknownFields()
	if err := parameterDecoder.Decode(&params); err != nil {
		return "", fmt.Errorf("%s: bad parameters: %w", ToolNameRecoveryExecute, err)
	}
	if parameterDecoder.Decode(&struct{}{}) != io.EOF {
		return "", fmt.Errorf("%s: bad parameters: expected exactly one JSON object", ToolNameRecoveryExecute)
	}
	if params.Command == "" {
		return "", fmt.Errorf("%s: parameters.command required", ToolNameRecoveryExecute)
	}
	caller, hasCaller := tenantctx.From(ctx)
	agentTeamsCaller := hasCaller && caller.AgentTeams != nil
	if params.SkipAudit && (agentTeamsCaller ||
		params.Command == hitlmodel.RecoveryActionKillProcess ||
		params.Command == hitlmodel.RecoveryActionResizePool) {
		return "", fmt.Errorf("%s: skip_audit is forbidden for this caller and command", ToolNameRecoveryExecute)
	}
	switch params.Command {
	case "restart_service", "kill_process", "resize_pool", "noop":
		// recognised; validated below.
	default:
		return "", fmt.Errorf("%s: unknown parameters.command %q (want restart_service|kill_process|resize_pool|noop)",
			ToolNameRecoveryExecute, params.Command)
	}
	switch params.Command {
	case hitlmodel.RecoveryActionRestartService:
		if params.DeviceID == 0 || params.Service == "" || params.Reason == "" ||
			params.IncidentID != "" || params.FixtureManifestID != "" || params.PoolManifestID != "" {
			return "", fmt.Errorf("%s: restart_service requires device_id, service, and reason only", ToolNameRecoveryExecute)
		}
		if t.dispatcher == nil {
			return "", fmt.Errorf("%s: inner dispatcher (host_restart_service) not wired", ToolNameRecoveryExecute)
		}
	case hitlmodel.RecoveryActionKillProcess:
		if params.IncidentID == "" || params.FixtureManifestID == "" || params.Reason == "" ||
			params.DeviceID != 0 || params.Service != "" || params.PoolManifestID != "" {
			return "", fmt.Errorf("%s: kill_process requires incident_id, fixture_manifest_id, and reason only", ToolNameRecoveryExecute)
		}
		if t.terminator == nil {
			return "", fmt.Errorf("%s: host fixture terminator not wired", ToolNameRecoveryExecute)
		}
	case hitlmodel.RecoveryActionResizePool:
		if params.IncidentID == "" || params.PoolManifestID == "" || params.Reason == "" ||
			params.DeviceID != 0 || params.Service != "" || params.FixtureManifestID != "" {
			return "", fmt.Errorf("%s: resize_pool requires incident_id, pool_manifest_id, and reason only", ToolNameRecoveryExecute)
		}
		if t.poolRecoverer == nil {
			return "", fmt.Errorf("%s: pool fixture recovery executor not wired", ToolNameRecoveryExecute)
		}
	case "noop":
		if agentTeamsCaller {
			return "", fmt.Errorf("%s: agentteams callers support only restart_service|kill_process|resize_pool", ToolNameRecoveryExecute)
		}
	}

	// Audit gate: AgentTeams callers cannot bypass the approved-proposal
	// check. Offline callers retain the existing explicit drill escape hatch.
	gated := (!params.SkipAudit && t.auditRepo != nil) || agentTeamsCaller ||
		params.Command == hitlmodel.RecoveryActionKillProcess ||
		params.Command == hitlmodel.RecoveryActionResizePool
	if gated && in.ProposalID == "" {
		return "", errRecoveryProposalRequired
	}
	if gated {
		if t.auditRepo == nil {
			return "", fmt.Errorf("%s: mutating proposal audit repository required for agentteams callers", ToolNameRecoveryExecute)
		}
		fixtureRequest := HostProcessTerminationRequest{
			IncidentID:        in.IncidentID,
			FixtureManifestID: params.FixtureManifestID,
		}
		if params.Command == hitlmodel.RecoveryActionKillProcess {
			fixtureStatus, err := t.terminator.Status(ctx, fixtureRequest)
			if err != nil {
				return "", fmt.Errorf("%s: resolve case-owned fixture: %w", ToolNameRecoveryExecute, err)
			}
			if in.ResourceType != "host" || in.Target != fixtureStatus.Resource ||
				fixtureStatus.ManifestID != params.FixtureManifestID ||
				fixtureStatus.IncidentID != in.IncidentID || params.IncidentID != in.IncidentID ||
				fixtureStatus.Status != "running" {
				return "", fmt.Errorf("%s: recovery target does not exactly match the incident-owned fixture", ToolNameRecoveryExecute)
			}
		}
		if params.Command == hitlmodel.RecoveryActionResizePool {
			poolStatus, err := t.poolRecoverer.Status(ctx, PoolRecoveryRequest{
				IncidentID:     in.IncidentID,
				PoolManifestID: params.PoolManifestID,
			})
			if err != nil {
				return "", fmt.Errorf("%s: resolve incident-owned PostgreSQL pool: %w", ToolNameRecoveryExecute, err)
			}
			if in.ResourceType != "pg" || in.Target != poolStatus.Resource ||
				poolStatus.ManifestID != params.PoolManifestID ||
				poolStatus.IncidentID != in.IncidentID || params.IncidentID != in.IncidentID ||
				poolStatus.Status != "running" || poolStatus.FailedProbe.Status != "failed" {
				return "", fmt.Errorf("%s: recovery target does not exactly match an exhausted incident-owned pool", ToolNameRecoveryExecute)
			}
		}
		err := t.auditRepo.ReserveApprovedProposal(ctx, RecoveryProposalRequest{
			ProposalID: in.ProposalID,
			SessionID:  in.IncidentID,
			Kind:       hitlmodel.KindAgentTeams,
			Action:     params.Command,
			Resource:   in.Target,
			Execution: hitlmodel.RecoveryExecutionParameters{
				Command:           params.Command,
				DeviceID:          params.DeviceID,
				Service:           params.Service,
				Reason:            params.Reason,
				IncidentID:        params.IncidentID,
				FixtureManifestID: params.FixtureManifestID,
				PoolManifestID:    params.PoolManifestID,
			},
		})
		if err != nil {
			return "", fmt.Errorf("%s: reserve approved proposal: %w", ToolNameRecoveryExecute, err)
		}
	}

	env := recoveryExecuteEnvelope{
		SchemaVersion: "v1",
		IncidentID:    in.IncidentID,
		Executed:      true,
		Command:       params.Command,
		RestartedAt:   time.Now().UTC(),
	}

	if params.Command == "noop" {
		out, err := json.Marshal(env)
		if err != nil {
			err = fmt.Errorf("%s: marshal response: %w", ToolNameRecoveryExecute, err)
			return "", t.finishReservedProposal(ctx, in.ProposalID, false, "", err)
		}
		if err := t.completeReservedProposal(ctx, in.ProposalID, true, string(out)); err != nil {
			return "", err
		}
		return string(out), nil
	}

	if params.Command == hitlmodel.RecoveryActionKillProcess {
		terminationResult, err := t.terminator.Terminate(ctx, HostProcessTerminationRequest{
			IncidentID:        in.IncidentID,
			FixtureManifestID: params.FixtureManifestID,
		})
		if err != nil {
			err = fmt.Errorf("%s: terminate case-owned fixture process: %w", ToolNameRecoveryExecute, err)
			return "", t.finishReservedProposal(ctx, in.ProposalID, false, err.Error(), err)
		}
		env.ResultJSON = terminationResult
		out, err := json.Marshal(env)
		if err != nil {
			err = fmt.Errorf("%s: marshal response: %w", ToolNameRecoveryExecute, err)
			return "", t.finishReservedProposal(ctx, in.ProposalID, false, "", err)
		}
		if err := t.completeReservedProposal(ctx, in.ProposalID, true, string(out)); err != nil {
			return "", err
		}
		return string(out), nil
	}

	if params.Command == hitlmodel.RecoveryActionResizePool {
		recoveryResult, err := t.poolRecoverer.Recover(ctx, PoolRecoveryRequest{
			IncidentID:     in.IncidentID,
			PoolManifestID: params.PoolManifestID,
			Reason:         params.Reason,
		})
		if err != nil {
			err = fmt.Errorf("%s: recover incident-owned PostgreSQL pool: %w", ToolNameRecoveryExecute, err)
			return "", t.finishReservedProposal(ctx, in.ProposalID, false, err.Error(), err)
		}
		env.ResultJSON = recoveryResult
		out, err := json.Marshal(env)
		if err != nil {
			err = fmt.Errorf("%s: marshal response: %w", ToolNameRecoveryExecute, err)
			return "", t.finishReservedProposal(ctx, in.ProposalID, false, "", err)
		}
		if err := t.completeReservedProposal(ctx, in.ProposalID, true, string(out)); err != nil {
			return "", err
		}
		return string(out), nil
	}

	// command == "restart_service". Dispatch via the inner tool.
	innerArgs, err := json.Marshal(map[string]any{
		"device_id": params.DeviceID,
		"service":   params.Service,
		"reason":    params.Reason,
	})
	if err != nil {
		err = fmt.Errorf("%s: marshal inner args: %w", ToolNameRecoveryExecute, err)
		return "", t.finishReservedProposal(ctx, in.ProposalID, false, "", err)
	}
	innerOut, err := t.dispatcher.InvokableRun(ctx, string(innerArgs), opts...)
	if err != nil {
		err = fmt.Errorf("%s: dispatch: %w", ToolNameRecoveryExecute, err)
		return "", t.finishReservedProposal(ctx, in.ProposalID, false, err.Error(), err)
	}
	if json.Valid([]byte(innerOut)) {
		env.ResultJSON = json.RawMessage(innerOut)
	}
	out, err := json.Marshal(env)
	if err != nil {
		err = fmt.Errorf("%s: marshal response: %w", ToolNameRecoveryExecute, err)
		return "", t.finishReservedProposal(ctx, in.ProposalID, false, "", err)
	}
	if err := t.completeReservedProposal(ctx, in.ProposalID, true, string(out)); err != nil {
		return "", err
	}
	return string(out), nil
}

func (t *RecoveryExecuteTool) completeReservedProposal(ctx context.Context, proposalID string, success bool, resultJSON string) error {
	if proposalID == "" || t.auditRepo == nil {
		return nil
	}
	if err := t.auditRepo.CompleteReservedProposal(ctx, proposalID, success, resultJSON); err != nil {
		return fmt.Errorf("%s: complete proposal reservation: %w", ToolNameRecoveryExecute, err)
	}
	return nil
}

func (t *RecoveryExecuteTool) finishReservedProposal(ctx context.Context, proposalID string, success bool, resultJSON string, cause error) error {
	if err := t.completeReservedProposal(ctx, proposalID, success, resultJSON); err != nil {
		if cause == nil {
			return err
		}
		return fmt.Errorf("%v; %w", cause, err)
	}
	return cause
}

// AppendRecoveryExecuteTool registers the recovery.execute BaseTool
// onto the provided slice when the dependency triple is wired. The
// dispatcher is expected to be the host_restart_service tool (or a
// compatible wrapper); nil dispatcher / nil auditRepo both keep the
// slice unchanged so a deployment without the seam can boot.
//
// The caller is responsible for wrapping the returned tool in the
// decorator chain (chain.go's Wrap), which automatically applies the
// ReviewGate decorator when Class="write"|"destructive".
func AppendRecoveryExecuteTool(out []basetool.BaseTool, dispatcher basetool.BaseTool, terminator HostProcessTerminator, auditRepo MutatingProposalAuditRepo, log *slog.Logger) []basetool.BaseTool {
	if dispatcher == nil || auditRepo == nil {
		return out
	}
	if log == nil {
		log = slog.Default()
	}
	return append(out, NewRecoveryExecuteTool(dispatcher, terminator, auditRepo, log))
}
