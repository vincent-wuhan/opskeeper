// Package kafka 是 Kafka 中间件 Adapter（路径 A 阶段 2 任务 2.4）。
//
// 当前为骨架实现：接口契约 + 5 个工具方法。
// 完整实现在 Task 2.4 后续 PR（confluent-kafka-go + consumer group 监控）。
//
// 关联 Design Doc：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.1.5
package kafka

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/internal/middleware/registry"
)

// Adapter 是 Kafka Adapter 实现。
type Adapter struct {
	mu        sync.RWMutex
	conn      adapter.ConnectionSpec
	connected bool
	// producer/consumer *kafka.Conn  // confluent-kafka-go
	// adminClient        *kafka.Client
	// tenantID uint64
}

// New 创建 Kafka Adapter 实例。
func New() *Adapter {
	return &Adapter{}
}

// Type 返回资源类型。
func (a *Adapter) Type() adapter.ResourceType { return adapter.TypeKafka }

// Connect 建立连接（骨架）。
//
// 完整实现：
//  1. 解密 DSN（PLAINTEXT / SASL_SSL）
//  2. kafka.DialContext / kafka.NewConsumer
//  3. 验证 metadata fetch
//  4. 启动 consumer group lag 监控
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

func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connected = false
	return nil
}

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

// RegisterTools 注册 Kafka Adapter 暴露的 5 个工具方法。
//
//   - L0 (1)：topic_list
//   - L1 (3)：consumer_lag / partition_skew / broker_skew
//   - L3 (1)：rebalance_history  // 重平衡历史（含时间戳 + 影响 consumer group）
func RegisterTools(reg *registry.Registry) error {
	tools := []registry.Tool{
		makeTool("kafka.topic_list", adapter.RiskL0ReadOnly, "列出所有 topic", nil),
		makeTool("kafka.consumer_lag", adapter.RiskL1Diagnostic, "consumer group lag 监控", nil),
		makeTool("kafka.partition_skew", adapter.RiskL1Diagnostic, "partition 流量倾斜分析", nil),
		makeTool("kafka.broker_skew", adapter.RiskL1Diagnostic, "broker 流量 + leader 分布", nil),
		makeTool("kafka.rebalance_history", adapter.RiskL3HardWrite, "consumer group 重平衡历史（敏感数据，需审批）", nil),
	}
	return reg.RegisterTools(adapter.TypeKafka, tools)
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
