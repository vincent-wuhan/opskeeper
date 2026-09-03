// Package adapter 定义 opskeeper 中间件 Adapter 接口契约。
//
// 路径 A 阶段 2 任务 2.1 启动 — 接口契约 + 注册机制。
//
// 6 类中间件资源（PG / Redis / RabbitMQ / Kafka / K8s Cluster / Git Repo）
// 实现 MiddlewareAdapter 接口后，由 Registry 注册到 opskeeper BaseTool
// 框架，复用装饰器 / ToolSearch / Casbin RBAC / cmdpolicy / Audit 体系。
//
// 关联 spec：openspec/changes/unified-platform-base-selection/specs/middleware-adapter/spec.md
// 关联 Design Doc：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.1
package adapter

import (
	"context"
	"errors"
	"time"
)

// ResourceType 标识中间件资源类型。
type ResourceType string

const (
	TypePostgres      ResourceType = "postgres"
	TypeRedis         ResourceType = "redis"
	TypeRabbitMQ      ResourceType = "rabbitmq"
	TypeKafka         ResourceType = "kafka"
	TypeK8sCluster    ResourceType = "k8s_cluster"
	TypeGitRepository ResourceType = "git_repository"
)

// ConnectionSpec 是 Adapter 连接的最小描述。
//
// DSN 必须经过 secrets 库加密；运行时由 pkg/credinject 解密并注入。
// 此结构不持久化明文。
type ConnectionSpec struct {
	DSN       string            // 加密存储的 DSN（运行时进程内解密）
	SecretRef string            // 引用 secrets 库的 key（如 "secret://dsn/<uuid>"）
	Labels    map[string]string // 资源标签
	Timeout   time.Duration     // 连接 / 查询超时（默认 30s）
	PoolSize  int               // 连接池大小（默认 10）
	TLSMode   string            // disable / require / verify-ca / verify-full
}

// HealthStatus 是 Health 检查结果。
type HealthStatus struct {
	Status    string // healthy / degraded / down
	LatencyMs int64  // 探测耗时
	Message   string // 错误信息（sanitized — 不含凭据）
	CheckedAt time.Time
}

// DiagnoseQuery 是 Diagnose 的输入。
type DiagnoseQuery struct {
	Category string                 // performance / locks / bloat / replication / ...
	Params   map[string]interface{} // 类型特定参数
	Limit    int                    // 结果限制
}

// DiagnoseResult 是 Diagnose 的输出。
type DiagnoseResult struct {
	Category    string                   // 与输入 Category 对应
	Findings    []map[string]interface{} // 结构化诊断发现
	Summary     string                   // 人类可读摘要
	Suggestions []string                 // 推荐的后续操作工具方法
	ElapsedMs   int64
}

// CollectQuery 是 Collect 的输入。
type CollectQuery struct {
	Resource  string     // 资源子集（all / specific table/key/queue）
	Metrics   []string   // 指标列表
	TimeRange *TimeRange // 时间范围（可选）
	Params    map[string]interface{}
}

// TimeRange 时间范围。
type TimeRange struct {
	Start time.Time
	End   time.Time
}

// CollectResult 是 Collect 的输出。
type CollectResult struct {
	Metrics  map[string]interface{}
	Samples  []map[string]interface{}
	Metadata map[string]string
}

// ExecOp 是 Execute 的输入（写操作）。
type ExecOp struct {
	Operation  string // kill_session / vacuum_table / config_set / ...
	Params     map[string]interface{}
	ApprovedBy string // 审批人 ID（写操作必填）
	Reason     string // 审批理由
}

// ExecResult 是 Execute 的输出。
type ExecResult struct {
	Operation string
	Success   bool
	Message   string
	Impacted  int // 受影响的行/键/资源数
	Metadata  map[string]string
}

// Adapter 是中间件资源适配器接口。
//
// 所有 6 类 Adapter 必须实现此接口。调用顺序建议：
//  1. Connect（一次）
//  2. Health（定期）
//  3. Diagnose / Collect（按需）
//  4. Execute（审批后）
//  5. Close（停止时）
type Adapter interface {
	// Type 返回资源类型。
	Type() ResourceType

	// Connect 建立长连接（pull-based 模型）。
	Connect(ctx context.Context, conn ConnectionSpec) error

	// Close 关闭连接池。
	Close(ctx context.Context) error

	// Health 健康检查（定期调用）。
	Health(ctx context.Context) (*HealthStatus, error)

	// Diagnose 通用诊断入口。
	Diagnose(ctx context.Context, q DiagnoseQuery) (*DiagnoseResult, error)

	// Collect 采集指标 / 日志 / 配置。
	Collect(ctx context.Context, q CollectQuery) (*CollectResult, error)

	// Execute 受限执行（受 cmdpolicy + Casbin 双重约束）。
	//
	// 任何 Adapter 实现 MUST 在执行前检查：
	//   1. cmdpolicy 9 类策略（cmdpolicy.CanExecute）
	//   2. Casbin RBAC（op 操作所需权限）
	//   3. ExecOp.ApprovedBy 非空
	Execute(ctx context.Context, op ExecOp) (*ExecResult, error)
}

// 公共错误（所有 Adapter 复用）
var (
	ErrNotConnected     = errors.New("adapter not connected")
	ErrApprovalRequired = errors.New("write operation requires approval")
	ErrInvalidOp        = errors.New("invalid operation")
	ErrTimeout          = errors.New("operation timed out")
)

// RiskLevel 写操作风险等级（cmdpolicy + Casbin 双重门控）。
type RiskLevel string

const (
	RiskL0ReadOnly    RiskLevel = "L0" // 只读，直接执行
	RiskL1Diagnostic  RiskLevel = "L1" // 诊断读取，直接执行
	RiskL2SoftWrite   RiskLevel = "L2" // 软写（如 analyze），Casbin 单层审批
	RiskL3HardWrite   RiskLevel = "L3" // 硬写（kill_session / drop 等），cmdpolicy + Casbin 双层
	RiskL4Destructive RiskLevel = "L4" // 破坏性（flushall / drop database），需双人审批
)

// OpRiskLevel 返回 op 的风险等级（Adapter 实现者提供映射表）。
type OpRiskLevel interface {
	OpRiskLevel(op string) RiskLevel
}

// 装饰器栈：与 opskeeper BaseTool 共享 Audit / Timeout / RateLimit / Metrics。
// 路径 A 阶段 2 任务 2.1 实现：
//   - internal/middleware/adapter/decorator/audit.go
//   - internal/middleware/adapter/decorator/timeout.go
//   - internal/middleware/adapter/decorator/metrics.go
