// Package decorator 为 Adapter Tool 提供横切关注点装饰器。
//
// 路径 A 阶段 2 任务 2.1 装饰器层。
//
// 装饰器层与 opskeeper BaseTool 装饰器（manager/biz/aiops/tools/decorators/）
// 模式一致，但作用在 registry.Tool.Handler 上：
//
//   - AuditTool：所有 Tool 调用记审计日志（who / when / what / args / result / duration）
//   - TimeoutTool：每次 Tool 调用受 context.WithTimeout 约束
//   - RateLimitTool：每 (tool, tenant) 对限速
//   - MetricsTool：每次 Tool 调用记录 Prometheus 指标
//
// 装饰器按 Chain.Wrap() 顺序组合（参考 opskeeper BaseTool Wrap）。
package decorator

import (
	"context"
	"log/slog"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/registry"
)

// ToolHandler 是装饰器包装的目标函数签名。
//
// 与 registry.Tool.Handler 一致（func(ctx, args) (interface{}, error)）。
// 装饰器内部多次复用。
type ToolHandler = func(ctx context.Context, args map[string]interface{}) (interface{}, error)

// AuditSink 是审计日志接口（生产实现接 slog + 审计 DB）。
type AuditSink interface {
	Write(ctx context.Context, entry AuditEntry) error
}

// AuditEntry 是单次审计记录。
type AuditEntry struct {
	TenantID   uint64                 `json:"tenant_id"`
	Tool       string                 `json:"tool"`
	Args       map[string]interface{} `json:"args"`
	Result     interface{}            `json:"result,omitempty"`
	Error      string                 `json:"error,omitempty"`
	ApprovedBy string                 `json:"approved_by,omitempty"`
	DurationMs int64                  `json:"duration_ms"`
	StartedAt  time.Time              `json:"started_at"`
	FinishedAt time.Time              `json:"finished_at"`
}

// SlogAuditSink 默认实现：结构化日志（slog JSON handler）。
type SlogAuditSink struct {
	Logger *slog.Logger
}

// Write 写入审计日志。
func (s *SlogAuditSink) Write(_ context.Context, e AuditEntry) error {
	level := slog.LevelInfo
	if e.Error != "" {
		level = slog.LevelWarn
	}
	s.Logger.LogAttrs(context.Background(), level, "adapter_tool_call",
		slog.Uint64("tenant_id", e.TenantID),
		slog.String("tool", e.Tool),
		slog.Any("args", e.Args),
		slog.Int64("duration_ms", e.DurationMs),
		slog.String("error", e.Error),
		slog.String("approved_by", e.ApprovedBy),
	)
	return nil
}

// AuditDecorator 包装 inner 工具方法，调用前后写审计日志。
//
// 关键字段从 ctx 提取（中间件约定 key）：
//   - "tenant_id" → uint64
//   - "user_id" / "approved_by" → string
type AuditDecorator struct {
	Inner ToolHandler
	Sink  AuditSink
}

// Handle 执行并审计。
func (a *AuditDecorator) Handle(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	started := time.Now()
	tenantID, _ := ctx.Value(tenantCtxKey{}).(uint64)
	approvedBy, _ := ctx.Value(approvedByCtxKey{}).(string)

	result, err := a.Inner(ctx, args)
	finished := time.Now()

	entry := AuditEntry{
		TenantID:   tenantID,
		Tool:       toolNameFromArgs(args),
		Args:       args,
		DurationMs: finished.Sub(started).Milliseconds(),
		StartedAt:  started,
		FinishedAt: finished,
		ApprovedBy: approvedBy,
	}
	if err != nil {
		entry.Error = err.Error()
	} else {
		entry.Result = result
	}
	_ = a.Sink.Write(ctx, entry)
	return result, err
}

// tenantCtxKey / approvedByCtxKey 是 ctx 传递约定 key。
type tenantCtxKey struct{}
type approvedByCtxKey struct{}

// WithTenant 把 tenant_id 写入 ctx。
func WithTenant(ctx context.Context, tenantID uint64) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

// WithApprovedBy 把 approved_by 写入 ctx。
func WithApprovedBy(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, approvedByCtxKey{}, userID)
}

// toolNameFromArgs 从 args 中提取 tool 字段（约定调用方传入 args["__tool"] = name）。
func toolNameFromArgs(args map[string]interface{}) string {
	if args == nil {
		return ""
	}
	if name, ok := args["__tool"].(string); ok {
		return name
	}
	return ""
}

// WrapAudit 是装饰器工厂：返回包装后的 ToolHandler。
//
// 用法：
//
//	handler := decorator.WrapAudit(originalHandler, auditSink)
func WrapAudit(inner ToolHandler, sink AuditSink) ToolHandler {
	return (&AuditDecorator{Inner: inner, Sink: sink}).Handle
}

// 注册到 registry 工具的便捷函数（修改 Tool.Handler）。
func ApplyAudit(t registry.Tool, sink AuditSink) registry.Tool {
	t.Handler = WrapAudit(t.Handler, sink)
	return t
}
