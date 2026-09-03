package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	aiopsmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
	approvalmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/approval"
	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
)

// MigrateResult 描述一次迁移的执行结果，cmd/opskeeper 在启动时打印并写指标。
type MigrateResult struct {
	ApprovalMigrated int64
	MutatingMigrated int64
	ApprovalSkipped  int64
	MutatingSkipped  int64
	ApprovalErrors   []string
	MutatingErrors   []string
	SampledApproval  *approvalmodel.Approval
	SampledMutating  *aiopsmodel.MutatingProposal
	Duration         time.Duration
}

// MigrateLegacy 把旧 approvals / chat_mutating_proposals 一次性迁移到 proposal 表。
//
// 行为约定：
//   - 幂等：重复执行不会重复插入（按 old id 生成 deterministic new id）
//   - 不触碰旧表：保留 3 个月仅读观察期
//   - legacy_kind 字段标记来源，便于回溯
//   - 7 天切换窗口期内，新写入走 proposal，读取走 proposal（带旧表 fallback 兜底）
//
// 设计见 openspec/changes/hitl-pause-resume/design.md §2.1。
func (r *Repo) MigrateLegacy(ctx context.Context) (*MigrateResult, error) {
	if r.db == nil {
		return nil, errors.New("hitl/store: nil db for MigrateLegacy")
	}
	start := time.Now()
	res := &MigrateResult{}

	// 第一步：从 approvals 迁
	approvalRows, err := r.loadAllApprovals(ctx)
	if err != nil {
		return res, fmt.Errorf("hitl/store: load approvals: %w", err)
	}
	for _, old := range approvalRows {
		newID := legacyToNewID("approval", old.ID)
		exists, err := r.proposalExists(ctx, newID)
		if err != nil {
			res.ApprovalErrors = append(res.ApprovalErrors, err.Error())
			continue
		}
		if exists {
			res.ApprovalSkipped++
			continue
		}
		new := approvalToProposal(old, newID)
		if err := r.db.WithContext(ctx).Create(new).Error; err != nil {
			res.ApprovalErrors = append(res.ApprovalErrors, err.Error())
			continue
		}
		res.ApprovalMigrated++
		if res.SampledApproval == nil {
			res.SampledApproval = old
		}
	}

	// 第二步：从 chat_mutating_proposals 迁
	mutatingRows, err := r.loadAllMutating(ctx)
	if err != nil {
		return res, fmt.Errorf("hitl/store: load mutating: %w", err)
	}
	for _, old := range mutatingRows {
		newID := legacyToNewID("mutating", old.ID)
		exists, err := r.proposalExists(ctx, newID)
		if err != nil {
			res.MutatingErrors = append(res.MutatingErrors, err.Error())
			continue
		}
		if exists {
			res.MutatingSkipped++
			continue
		}
		new := mutatingToProposal(old, newID)
		if err := r.db.WithContext(ctx).Create(new).Error; err != nil {
			res.MutatingErrors = append(res.MutatingErrors, err.Error())
			continue
		}
		res.MutatingMigrated++
		if res.SampledMutating == nil {
			res.SampledMutating = old
		}
	}

	res.Duration = time.Since(start)
	return res, nil
}

// loadAllApprovals 拉取所有旧 approval 行（仅一次性）。
func (r *Repo) loadAllApprovals(ctx context.Context) ([]*approvalmodel.Approval, error) {
	var rows []*approvalmodel.Approval
	if err := r.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// loadAllMutating 拉取所有旧 mutating_proposal 行。
func (r *Repo) loadAllMutating(ctx context.Context) ([]*aiopsmodel.MutatingProposal, error) {
	var rows []*aiopsmodel.MutatingProposal
	if err := r.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// proposalExists 检查 proposal 表是否已有该 id（避免重复插入）。
func (r *Repo) proposalExists(ctx context.Context, id string) (bool, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&model.Proposal{}).Where("id = ?", id).Count(&n).Error
	return n > 0, err
}

// approvalToProposal 把旧 Approval 映射到新 Proposal。
//
// 关键映射（见 design §2.1）：
//   - old.id → deterministic new.id = "approval:<old.id>"
//   - old.kind → new.kind（保持兼容，让执行器复用旧注册）
//   - old.status → new.state（pending/approved/rejected/executed/failed）
//   - old.payload_json → new.payload_json
//   - old.proposed_by / approved_by / source → 同名字段
//   - new.severity 默认为 mutating（旧表无 severity 字段）
//   - new.sensitivity 默认为 internal
//   - new.legacy_kind = "approval_legacy"
func approvalToProposal(old *approvalmodel.Approval, newID string) *model.Proposal {
	now := time.Now().UTC()
	return &model.Proposal{
		ID:          newID,
		Kind:        old.Kind,
		LegacyKind:  "approval_legacy",
		Title:       old.Title,
		Summary:     old.Summary,
		PayloadJSON: old.PayloadJSON,
		Source:      pickStr(old.Source, model.SourceAgent),
		SessionID:   old.SessionID,
		ProposedBy:  old.ProposedBy,
		ApprovedBy:  old.ApprovedBy,
		Reason:      old.Reason,
		ResultJSON:  old.ResultJSON,
		Severity:    model.SeverityMutating,
		Sensitivity: model.SensitivityInternal,
		State:       mapApprovalStatusToState(old.Status),
		CreatedAt:   old.CreatedAt,
		DecidedAt:   old.DecidedAt,
		ExecutedAt:  old.ExecutedAt,
		UpdatedAt:   now,
		DualWriteAt: &now,
	}
}

// mutatingToProposal 把旧 MutatingProposal 映射到新 Proposal。
//
// 关键映射：
//   - old.id → new.id = "mutating:<old.id>"
//   - old.tool_name → new.kind（ReviewGate 的 tool_class → severity 提升依据）
//   - tool_class write → mutating；destructive → dangerous；read → safe
//   - old.decision → new.state（pending → StatePending, approve → StateApproved 等）
//   - old.decision_reason → new.reason
//   - reviewer_agent / reviewer_task_id 写入 metadata JSON
func mutatingToProposal(old *aiopsmodel.MutatingProposal, newID string) *model.Proposal {
	now := time.Now().UTC()
	// 把 mutating 不直接映射的字段塞进 metadata JSON，便于 SPA 显示与回溯
	meta := map[string]any{
		"reviewer_agent":   old.ReviewerAgent,
		"reviewer_task_id": old.ReviewerTaskID,
		"tool_class":       old.ToolClass,
		"tool_call_id":     derefStr(old.ToolCallID),
		"message_id":       derefStr(old.MessageID),
	}
	metaJSON, _ := json.Marshal(meta)

	return &model.Proposal{
		ID:          newID,
		Kind:        old.ToolName,
		LegacyKind:  "mutating_legacy",
		Title:       buildMutatingTitle(old),
		Summary:     truncateSummary(derefStr(old.DecisionReason), 500),
		PayloadJSON: old.ArgsJSON,
		Source:      pickStr(model.SourceAgent, model.SourceAgent),
		SessionID:   old.SessionID,
		MessageID:   derefStr(old.MessageID),
		ProposedBy:  old.OperatorUserID,
		ApprovedBy:  old.ApproverUserID,
		Reason:      old.DecisionReason,
		Severity:    mutatingToolClassToSeverity(old.ToolClass),
		Sensitivity: model.SensitivityInternal,
		State:       mapMutatingDecisionToState(old.Decision),
		CreatedAt:   old.CreatedAt,
		DecidedAt:   old.DecidedAt,
		ExecutedAt:  old.ExecutedAt,
		UpdatedAt:   now,
		DualWriteAt: &now,
		ResultJSON:  strPtr(string(metaJSON)),
	}
}

// mapApprovalStatusToState 把 Approval.Status 转到 Proposal.State。
func mapApprovalStatusToState(s string) string {
	switch s {
	case approvalmodel.StatusPending:
		return model.StatePending
	case approvalmodel.StatusApproved:
		return model.StateApproved
	case approvalmodel.StatusRejected:
		return model.StateRejected
	case approvalmodel.StatusExecuted:
		return model.StateExecuted
	case approvalmodel.StatusFailed:
		return model.StateFailed
	}
	return model.StatePending
}

// mapMutatingDecisionToState 把 MutatingProposal.Decision 转到 Proposal.State。
func mapMutatingDecisionToState(d string) string {
	switch d {
	case aiopsmodel.DecisionPending:
		return model.StatePending
	case aiopsmodel.DecisionApprove:
		return model.StateApproved
	case aiopsmodel.DecisionReject:
		return model.StateRejected
	}
	return model.StatePending
}

// mutatingToolClassToSeverity 把 ReviewGate 观察到 tool_class 提升到 severity。
func mutatingToolClassToSeverity(cls string) string {
	switch cls {
	case "destructive":
		return model.SeverityDangerous
	case "write":
		return model.SeverityMutating
	case "read":
		return model.SeveritySafe
	}
	return model.SeverityMutating
}

// legacyToNewID 把旧 id 编码成 deterministic 新 id，永不冲突（双冒号作分隔符）。
func legacyToNewID(kind, oldID string) string {
	return "legacy-" + kind + "-" + oldID
}

// pickStr 第一个空时返回第二个，避免 source 为空字符串。
func pickStr(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

// derefStr 安全把 *string 解引用为 string（空时返回 ""）。
func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// strPtr 快速包成 *string。
func strPtr(s string) *string { return &s }

// truncateSummary 截取摘要，避免 Summary 列过大。
func truncateSummary(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// buildMutatingTitle 从 tool_name + tool_class 组装一行人类可读标题。
func buildMutatingTitle(p *aiopsmodel.MutatingProposal) string {
	if p == nil {
		return "mutating proposal"
	}
	return "mutating: " + p.ToolName + " (" + p.ToolClass + ")"
}
