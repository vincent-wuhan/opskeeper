// Package loop — db_approved_decision_loader.go
//
// approved → recovered phase transition 的 ApprovalDecision loader
// （real DB-backed 实现）。同 correlated_group_loader_adapter 的设计：
//
//   - loop 包定义 1 方法 narrow interface ApprovedContractReader
//   - loopstore.ContractRepoDB 通过 ReadContractByID 满足该 interface
//   - main.go 把 ContractRepoDB 注入 Adapter，包图无环（loop 不 import loopstore）
//
// 行为：
//   - contractID <= 0 → (nil, nil)（与 Planner "无上游合同用 default" 语义对齐）
//   - row == nil → (nil, nil)：上游客约不存在时 Planner 走 default
//   - row.Type != "ApprovalDecision" → error：契约类型错乱必须暴露
//   - reader 错误 / Payload 损坏 → error：DB 不可达或合同被破坏需 fail loudly
//
// 历史：之前是 NoopApprovedDecisionLoader 永远返回 (nil, nil)，让 Planner
// 永远走 default metrics + default tolerance —— 把"上游客约不存在"和
// "上游客约加载失败"混在一起，是 silent misconfig。DB loader 区分两者：
// 合同真的不存在返回 (nil, nil)（用 default），DB 错误返回 error（阻止
// phase 推进，避免基于过期 default metrics 误判 verify_recovery）。
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/prom"
)

// ApprovedContractReader 是 loopstore.ContractRepoDB 需要满足的
// 最小 narrow interface（按 ID + tenant 读 loop_contract 行）。
//
// 与 CorrelatedGroupReader 同形（同一 ReadContractByID 方法）但语义不同：
// 这里消费方只关心 type="ApprovalDecision" 的行。
//
// 多租户安全：tenantID 必传，阻止跨租户读 —— ⑥ 号差距的修复。
// caller 必须已经从上游 contract 拿到该 row 所属 tenant（如 planInput.TenantID），
// 不能拿 incident_id 反查或假设 default。
type ApprovedContractReader interface {
	ReadContractByID(ctx context.Context, tenantID string, id int64) (*loopmodel.Contract, error)
}

// DBApprovedDecisionLoader 是 ApprovedDecisionLoader 的 real 实现。
//
// 构造：
//   - reader：loopstore.ContractRepoDB（满足 ApprovedContractReader）
//   - log：   slog.Logger（nil 用 slog.Default()）
type DBApprovedDecisionLoader struct {
	reader ApprovedContractReader
	log    *slog.Logger
}

// NewDBApprovedDecisionLoader 构造 DBApprovedDecisionLoader。
// reader 不得为 nil（fail-fast 同 NewRecoveryStateStoreDB 风格）。
func NewDBApprovedDecisionLoader(reader ApprovedContractReader, log *slog.Logger) *DBApprovedDecisionLoader {
	if reader == nil {
		panic("loop: NewDBApprovedDecisionLoader: reader is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	return &DBApprovedDecisionLoader{
		reader: reader,
		log:    log.With(slog.String("comp", "loop.db_approved_decision_loader")),
	}
}

// Compile-time interface satisfaction check.
var _ ApprovedDecisionLoader = (*DBApprovedDecisionLoader)(nil)

// LoadApprovedDecision 实现 ApprovedDecisionLoader。
//
// contractID <= 0：返回 (nil, nil) —— Planner 视为"无上游合同"，用 default。
//
// contractID > 0：
//   - reader 错误 → 返回 error（fail loudly：DB 不通 = 必须停 phase）
//   - row == nil → (nil, nil)（合同真的不存在 —— Planner 用 default）
//   - row.Type != "ApprovalDecision" → 返回 error
//     （contract 表里同 ID 不应承载不同类型；schema 错乱必须暴露）
//   - Payload 损坏 → 返回 error
//   - 成功 → 返回 &ApprovalDecision{}
//
// 多租户安全：tenantID 必传，传递给 reader。⑥ 号差距的修复——
// 之前 ReadContractByID 不带 tenant 过滤，攻击者拿到一个跨租户
// 合同 ID 就能读到。修复后即使知道 ID，跨租户也被 WHERE 阻断。
//
// 副作用：每次调用 emit 一条 prom.IncLoopDBApprovedDecisionLookup(result)
// 让 opskeeper self-health 能看到 DB-backed loader 实际被调用次数 + 各分支
// 分布（loaded / not_found / type_mismatch / payload_corrupted / db_error /
// tenant_mismatch）。
func (l *DBApprovedDecisionLoader) LoadApprovedDecision(ctx context.Context, tenantID string, contractID int64) (*ApprovalDecision, error) {
	if contractID <= 0 {
		prom.IncLoopDBApprovedDecisionLookup("skipped")
		return nil, nil
	}
	if tenantID == "" {
		// 与 reader 拒绝空 tenant 一致；早返防止 caller 失误
		prom.IncLoopDBApprovedDecisionLookup("tenant_mismatch")
		return nil, errors.New("loop: load ApprovalDecision: tenantID required")
	}
	row, err := l.reader.ReadContractByID(ctx, tenantID, contractID)
	if err != nil {
		prom.IncLoopDBApprovedDecisionLookup("db_error")
		return nil, fmt.Errorf("loop: load ApprovalDecision (tenant=%s, contract_id=%d): %w", tenantID, contractID, err)
	}
	if row == nil {
		// 行不存在 OR 不属于该 tenant —— 对 caller 都是 "no upstream contract"。
		// 不区分两种 case 是有意为之：避免泄漏 "ID 存在但属于别的 tenant" 的存在性信号。
		prom.IncLoopDBApprovedDecisionLookup("not_found")
		return nil, nil
	}
	if row.Type != "ApprovalDecision" {
		prom.IncLoopDBApprovedDecisionLookup("type_mismatch")
		return nil, fmt.Errorf("loop: contract id=%d has type=%q, expected %q: %w",
			contractID, row.Type, "ApprovalDecision", errors.New("contract type mismatch"))
	}
	var decision ApprovalDecision
	if err := json.Unmarshal([]byte(row.Payload), &decision); err != nil {
		prom.IncLoopDBApprovedDecisionLookup("payload_corrupted")
		return nil, fmt.Errorf("loop: ApprovalDecision (tenant=%s, contract_id=%d) payload unmarshal: %w", tenantID, contractID, err)
	}
	prom.IncLoopDBApprovedDecisionLookup("loaded")
	return &decision, nil
}
