// chatdiagnose/loop/correlated_group_loader_adapter.go — CorrelatedGroupLoader
// narrow adapter，包一个 CorrelatedGroupReader（由 data/loop/store.ContractRepoDB
// 通过 ReadContractByID 满足）。
//
// 设计：
//   - loop 包定义 1 方法 narrow interface CorrelatedGroupReader
//   - loopstore.ContractRepoDB 新增 ReadContractByID(ctx, id) 方法满足该 interface
//   - main.go 把 ContractRepoDB 注入 Adapter，包图无环（loop 不 import loopstore）
//
// 行为：
//   - LoadCorrelatedGroup 按 contract ID 读 loop_contract 行
//   - 反序列化 Payload JSON → loop.CorrelatedGroup
//   - 任一失败 slog warn + 返回 (nil, nil)（KB 风格：不阻塞 investigated worker）
package loop

import (
	"context"
	"encoding/json"
	"log/slog"

	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

// CorrelatedGroupReader 是 loopstore.ContractRepoDB 需要满足的最小 narrow interface。
// 定义在消费方（loop 包）以避免 biz → data 直接 import（AGENTS.md §架构）。
//
// 多租户安全：tenantID 必传，⑥ 修复同 ApprovedContractReader。
type CorrelatedGroupReader interface {
	ReadContractByID(ctx context.Context, tenantID string, id int64) (*loopmodel.Contract, error)
}

// CorrelatedGroupLoaderAdapter 把 CorrelatedGroupReader 适配到
// loop.CorrelatedGroupLoader。
type CorrelatedGroupLoaderAdapter struct {
	reader CorrelatedGroupReader
	log    *slog.Logger
}

// NewCorrelatedGroupLoaderAdapter 构造。reader 不得为 nil。
func NewCorrelatedGroupLoaderAdapter(reader CorrelatedGroupReader, log *slog.Logger) *CorrelatedGroupLoaderAdapter {
	if reader == nil {
		panic("loop: NewCorrelatedGroupLoaderAdapter: reader is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	return &CorrelatedGroupLoaderAdapter{
		reader: reader,
		log:    log.With(slog.String("comp", "loop.correlated_group_loader_adapter")),
	}
}

// Compile-time interface satisfaction check.
var _ CorrelatedGroupLoader = (*CorrelatedGroupLoaderAdapter)(nil)

// LoadCorrelatedGroup 实现 CorrelatedGroupLoader 接口。
//
// 行为：
//   - contractID <= 0 → (nil, nil)（不触达 reader）
//   - reader 错误 → slog warn + (nil, nil)
//   - row == nil → (nil, nil)
//   - Payload 损坏 → slog warn + (nil, nil)
func (a *CorrelatedGroupLoaderAdapter) LoadCorrelatedGroup(ctx context.Context, tenantID string, contractID int64) (*CorrelatedGroup, error) {
	if contractID <= 0 {
		return nil, nil
	}
	if tenantID == "" {
		a.log.Warn("correlated_group_loader: tenantID required (skipping load)",
			slog.Int64("contract_id", contractID))
		return nil, nil
	}

	row, err := a.reader.ReadContractByID(ctx, tenantID, contractID)
	if err != nil {
		a.log.Warn("correlated_group_loader: ReadContractByID failed (non-fatal)",
			slog.Int64("contract_id", contractID),
			slog.Any("err", err))
		return nil, nil
	}
	if row == nil {
		return nil, nil
	}

	var group CorrelatedGroup
	if err := json.Unmarshal([]byte(row.Payload), &group); err != nil {
		a.log.Warn("correlated_group_loader: Payload unmarshal failed (non-fatal)",
			slog.Int64("contract_id", contractID),
			slog.Any("err", err))
		return nil, nil
	}

	if group.IncidentID == "" {
		a.log.Warn("correlated_group_loader: group.IncidentID empty after unmarshal (non-fatal)",
			slog.Int64("contract_id", contractID))
		return nil, nil
	}

	return &group, nil
}
