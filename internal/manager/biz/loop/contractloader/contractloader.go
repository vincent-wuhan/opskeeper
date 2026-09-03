// Package contractloader 提供 loop.UpstreamContractLoader 的 narrow adapter，
// 包装 internal/manager/data/loop/store.ContractRepoDB。
//
// 设计动机：
//   - loop 包（biz 层）不能直接 import data/loop/store，否则形成
//     loop → loopstore → loop 的 import cycle（loopstore.event_repo 实现 loop.EventRepo/ContractRepo）
//   - 通过把 adapter 放在独立子包 loop/contractloader，包图为：
//     loop/contractloader → loopstore → loop
//     无环 ✅
//
// 行为：
//   - LoadPostmortemInputs 顺序读 3 个 contract：
//     1. RootCauseJSON  — PhaseInvestigated + contract_type="root_cause_json"
//     2. CritiqueScore — PhaseCritiqued    + contract_type="critique_score"
//     3. VerifiedDelta — PhaseRecovered    + contract_type="verified_delta"
//   - 每条反序列化 JSON Payload → typed struct
//   - 任一 contract 缺失 → 对应字段 nil（spec 容忍）
//   - 失败 slog warn + 继续（KB 风格：contract load 失败不阻塞 postmortem）
//
// 已知 limitation：
//   - contract_type 命名约定需与 orchestrator 写入路径 v2 对齐（Day 5+）
package contractloader

import (
	"context"
	"encoding/json"
	"log/slog"

	loopstore "github.com/vincent-wuhan/opskeeper/internal/manager/data/loop/store"

	loop "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
)

// Adapter 把 loopstore.ContractRepoDB 适配到 loop.UpstreamContractLoader 接口。
type Adapter struct {
	repo     *loopstore.ContractRepoDB
	tenantID string
	log      *slog.Logger
}

// NewAdapter 构造。repo 不得为 nil；log 为 nil 时回退 slog.Default()。
func NewAdapter(repo *loopstore.ContractRepoDB, tenantID string, log *slog.Logger) *Adapter {
	if repo == nil {
		panic("contractloader: NewAdapter: repo is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Adapter{
		repo:     repo,
		tenantID: tenantID,
		log:      log.With(slog.String("comp", "loop.contract_loader_adapter")),
	}
}

// Compile-time interface satisfaction check.
var _ loop.UpstreamContractLoader = (*Adapter)(nil)

// LoadPostmortemInputs 实现 loop.UpstreamContractLoader 接口。
//
// 行为：
//   - 读 3 个 contract，任一失败 slog warn + 继续（spec §"PostmortemInputs nil fields 容忍"）
//   - 任一 contract 缺失 → 对应字段 nil（spec 已声明）
//   - incidentID 为空时直接返回 (nil, nil)（与 NoopUpstreamContractLoader 语义一致）
func (a *Adapter) LoadPostmortemInputs(ctx context.Context, incidentID string) (*loop.PostmortemInputs, error) {
	if incidentID == "" {
		return nil, nil
	}

	out := &loop.PostmortemInputs{}

	if rc, err := a.loadRootCause(ctx, incidentID); err != nil {
		a.log.Warn("contract_loader: root_cause load failed (non-fatal)",
			slog.String("incident_id", incidentID), slog.Any("err", err))
	} else if rc != nil {
		out.RootCause = rc
	}

	if cs, err := a.loadCritique(ctx, incidentID); err != nil {
		a.log.Warn("contract_loader: critique load failed (non-fatal)",
			slog.String("incident_id", incidentID), slog.Any("err", err))
	} else if cs != nil {
		out.Critique = cs
	}

	if vd, err := a.loadVerified(ctx, incidentID); err != nil {
		a.log.Warn("contract_loader: verified load failed (non-fatal)",
			slog.String("incident_id", incidentID), slog.Any("err", err))
	} else if vd != nil {
		out.Verified = vd
	}

	return out, nil
}

func (a *Adapter) loadRootCause(ctx context.Context, incidentID string) (*loop.RootCauseJSON, error) {
	row, err := a.repo.ReadContract(ctx, a.tenantID, incidentID, loop.PhaseInvestigated, "root_cause_json")
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	var rc loop.RootCauseJSON
	if err := json.Unmarshal([]byte(row.Payload), &rc); err != nil {
		return nil, err
	}
	return &rc, nil
}

func (a *Adapter) loadCritique(ctx context.Context, incidentID string) (*loop.CritiqueScore, error) {
	row, err := a.repo.ReadContract(ctx, a.tenantID, incidentID, loop.PhaseCritiqued, "critique_score")
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	var cs loop.CritiqueScore
	if err := json.Unmarshal([]byte(row.Payload), &cs); err != nil {
		return nil, err
	}
	return &cs, nil
}

func (a *Adapter) loadVerified(ctx context.Context, incidentID string) (*loop.VerifiedDelta, error) {
	row, err := a.repo.ReadContract(ctx, a.tenantID, incidentID, loop.PhaseRecovered, "verified_delta")
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	var vd loop.VerifiedDelta
	if err := json.Unmarshal([]byte(row.Payload), &vd); err != nil {
		return nil, err
	}
	return &vd, nil
}
