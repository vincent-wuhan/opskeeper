package decorators

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/hitl"
	hitlmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
)

// PausePoint 是路径 A P1-2 阶段 2 任务 2.2 的工具执行前 PausePoint 装饰器。
//
// 与 ReviewGate 的关系：
//   - ReviewGate 拦截 mutating 类（write / destructive）并 spawn 二审 worker
//     或 deterministic policy，目前系统范围内 ReviewSpawner 为 nil，所以
//     ReviewGate 尚未在 chat / flow 路径生效
//   - PausePoint 拦截所有类（含 read），按 payload data_sensitivity
//     升级 + biz/hitl.PausePolicy 做统一判定；命中时把 Biz Action 写入
//     unified proposal 表，返回 ErrProposalPending 让调用方短路
//
// 链位置（chain.go Wrap）：
//
//	tenant_bind → pause_point → review_gate → timeout → audit → ratelimit → metric
//
// 为什么 pause_point 在 review_gate 之前：
//   - 一次工具调用只可能 pause 一次，pause_point 命中就直接返回，不再走
//     reviewer worker（节省 LLM round-trip）
//   - review_gate 仅在 pause_point 放行后才有机会拦截；mutating 类工具
//     通过 pause_point 后再走 reviewer，二者不冲突
//
// 为什么在 timeout / audit 之外：
//   - 命中 pause 后不执行 inner，没有 timeout 计时
//   - 命中 pause 后写到 proposal 表，不是 chat_tool_calls 执行审计；
//     proposal 表本身即 pause 的事实源

// PauseCoordinator 是 biz/hitl.Coordinator 对 decorators 的窄接口。
// *biz/hitl.Coordinator 隐式实现。
type PauseCoordinator interface {
	ShouldPause(ctx context.Context, action *hitl.Action) (*hitlmodel.Proposal, error)
}

// PausePoint 装饰器：在工具执行前调用 coordinator.ShouldPause，命中时
// 返回 ErrProposalPending 包装错误（含 proposal id）。
type PausePoint struct {
	inner basetool.BaseTool
	coord PauseCoordinator
}

// WithPausePoint 构造 PausePoint 装饰器。coord=nil 时退化为 noop（与
// ReviewSpawner=nil 不安装 ReviewGate 一致），保持向后兼容。
func WithPausePoint(inner basetool.BaseTool, coord PauseCoordinator) basetool.BaseTool {
	if inner == nil {
		return nil
	}
	if coord == nil {
		return inner
	}
	return &PausePoint{inner: inner, coord: coord}
}

// Info passes through unchanged.
func (p *PausePoint) Info(ctx context.Context) (*basetool.ToolInfo, error) {
	return p.inner.Info(ctx)
}

// InvokableRun 在 inner 执行前调用 coordinator.ShouldPause；命中 pause
// 时返回带 proposal id 的包装错误，让上层（agent loop / flow engine）
// 知道已创建 Proposal 并把 id 反馈到 SPA。
func (p *PausePoint) InvokableRun(ctx context.Context, argsJSON string, opts ...basetool.InvokeOption) (string, error) {
	info, err := p.inner.Info(ctx)
	if err != nil || info == nil {
		// Info 出错时退化为透传 — 跟 ReviewGate 同一保守策略，让 inner
		// 自己报 schema 错误而不是被 pause_point 屏蔽
		return p.inner.InvokableRun(ctx, argsJSON, opts...)
	}

	action := buildPauseAction(info, argsJSON)
	ref, err := p.coord.ShouldPause(ctx, action)
	if err != nil {
		// ErrProposalPending 是正常路径 — ref 非 nil
		if errors.Is(err, hitl.ErrProposalPending) && ref != nil {
			return "", fmt.Errorf("%w: proposal_id=%s severity=%s sensitivity=%s tool=%s",
				hitl.ErrProposalPending,
				ref.ID, ref.Severity, ref.Sensitivity, info.Name)
		}
		// 系统/数据错误：fail-fast，不重试
		return "", fmt.Errorf("hitl: pause_point: %w", err)
	}
	// ref==nil && err==nil → 放行
	return p.inner.InvokableRun(ctx, argsJSON, opts...)
}

// buildPauseAction 把 ToolInfo + argsJSON 转换成 biz/hitl.Action。
//
// 字段映射：
//   - Tool      ← info.Name
//   - RiskLevel ← info.Class（"read"→"read", "write"→"write",
//     "destructive"→"manage"）
//   - Resource  ← 从 argsJSON 提取 device_id / host / resource 字段；
//     取不到时用工具名兜底
//   - Payload   ← 完整 argsJSON 解析结果（map 形态）
func buildPauseAction(info *basetool.ToolInfo, argsJSON string) *hitl.Action {
	action := &hitl.Action{
		Tool:      info.Name,
		RiskLevel: classToRiskLevel(info.Class),
		Payload:   map[string]interface{}{},
	}
	if len(argsJSON) > 0 {
		var m map[string]interface{}
		if json.Unmarshal([]byte(argsJSON), &m) == nil {
			action.Payload = m
			for _, k := range []string{"device_id", "device_ids", "host", "resource", "resource_id"} {
				if v, ok := m[k]; ok {
					action.Resource = fmt.Sprintf("%v", v)
					break
				}
			}
		}
	}
	if action.Resource == "" {
		action.Resource = info.Name
	}
	return action
}

// classToRiskLevel 把 basetool.Class 映射到 hitl.Action.RiskLevel。
//
// 注意：hitl.PausePolicy.RiskLevelToSeverity 的合法值是 read / write /
// delete / manage，未识别值降级到 cfg.DefaultSeverity。
func classToRiskLevel(class string) string {
	switch class {
	case "read":
		return "read"
	case "write":
		return "write"
	case "destructive":
		return "manage"
	}
	return ""
}
