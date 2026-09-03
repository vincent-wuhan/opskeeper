// Package rabbitmq 是 RabbitMQ 中间件 Adapter（路径 A 阶段 2 任务 2.4）。
//
// 当前为骨架实现：接口契约 + 5 个工具方法。
// 完整实现在 Task 2.4 后续 PR（amqp091-go + 长连接消费者监控）。
//
// 关联 Design Doc：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.1.5
package rabbitmq

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/internal/middleware/registry"
)

// Adapter 是 RabbitMQ Adapter 实现。
type Adapter struct {
	mu        sync.RWMutex
	conn      adapter.ConnectionSpec
	connected bool
	// conn *amqp.Connection  // 完整实现时引入
	// ch   *amqp.Channel
	// tenantID uint64
}

// New 创建 RabbitMQ Adapter 实例（不连接）。
func New() *Adapter {
	return &Adapter{}
}

// Type 返回资源类型。
func (a *Adapter) Type() adapter.ResourceType {
	return adapter.TypeRabbitMQ
}

// Connect 建立连接（骨架）。
//
// 完整实现：
//  1. 解密 DSN（amqp://user:pass@host:port/vhost）
//  2. amqp.Dial() 建立 TCP 长连接
//  3. conn.Channel() 打开 channel
//  4. 验证 passive declare 已知队列可达
//  5. 启动 consumer 监控（auto-ack=false + QoS prefetch）
func (a *Adapter) Connect(ctx context.Context, conn adapter.ConnectionSpec) error {
	if conn.Timeout == 0 {
		conn.Timeout = 30 * time.Second
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conn = conn
	a.connected = true
	return nil
}

// Close 关闭连接。
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connected = false
	return nil
}

// Health 健康检查。
func (a *Adapter) Health(ctx context.Context) (*adapter.HealthStatus, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	return &adapter.HealthStatus{
		Status: "healthy", LatencyMs: 0,
		Message:   "skeleton: real health probe pending Task 2.4",
		CheckedAt: time.Now(),
	}, nil
}

// Diagnose 通用诊断入口。
func (a *Adapter) Diagnose(ctx context.Context, q adapter.DiagnoseQuery) (*adapter.DiagnoseResult, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	return &adapter.DiagnoseResult{
		Category: q.Category, Findings: []map[string]interface{}{},
		Summary: fmt.Sprintf("skeleton: category=%s", q.Category),
	}, nil
}

// Collect 采集指标。
func (a *Adapter) Collect(ctx context.Context, q adapter.CollectQuery) (*adapter.CollectResult, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	return &adapter.CollectResult{
		Metrics:  map[string]interface{}{},
		Samples:  []map[string]interface{}{},
		Metadata: map[string]string{"skeleton": "true"},
	}, nil
}

// Execute 受限执行。
func (a *Adapter) Execute(ctx context.Context, op adapter.ExecOp) (*adapter.ExecResult, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	if op.ApprovedBy == "" {
		return nil, adapter.ErrApprovalRequired
	}
	return &adapter.ExecResult{
		Operation: op.Operation, Success: false,
		Message:  "skeleton: real execution pending Task 2.4",
		Metadata: map[string]string{"skeleton": "true"},
	}, nil
}

// RegisterTools 注册 RabbitMQ Adapter 暴露的 5 个工具方法。
//
//   - L0 (2)：queue_list / cluster_info
//   - L1 (2)：queue_depth / consumer_status
//   - L3 (1)：purge_queue  // 写操作（清空队列消息，谨慎）
func RegisterTools(reg *registry.Registry) error {
	tools := []registry.Tool{
		makeTool("rabbitmq.queue_list", adapter.RiskL0ReadOnly, "列出所有 queue + 状态", nil),
		makeTool("rabbitmq.cluster_info", adapter.RiskL0ReadOnly, "cluster 拓扑 + 节点", nil),
		makeTool("rabbitmq.queue_depth", adapter.RiskL1Diagnostic, "queue 消息堆积深度 + 消费速率", nil),
		makeTool("rabbitmq.consumer_status", adapter.RiskL1Diagnostic, "consumer 状态 + ack 速率", nil),
		makeTool("rabbitmq.purge_queue", adapter.RiskL3HardWrite, "清空 queue（数据丢失风险，需审批）", nil),
	}
	return reg.RegisterTools(adapter.TypeRabbitMQ, tools)
}

func makeTool(name string, risk adapter.RiskLevel, desc string, _ map[string]string) registry.Tool {
	return registry.Tool{
		Name: name, Description: desc, RiskLevel: risk,
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			return nil, fmt.Errorf("not_implemented: tool %q pending Task 2.4", name)
		},
		ArgsSchema: map[string]string{},
	}
}

var _ adapter.Adapter = (*Adapter)(nil)
