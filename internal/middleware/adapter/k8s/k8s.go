// Package k8s 是 Kubernetes Cluster 中间件 Adapter（路径 A 阶段 2 任务 2.3）。
//
// 当前为骨架实现：接口契约 + 13 个工具方法清单 + 错误处理。
// 完整实现在 Task 2.3 后续 PR（client-go + ServiceAccount auth + informers watch +
// Pod exec 审计增强）。
//
// 关联 Design Doc：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.1.6
package k8s

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/internal/middleware/registry"
)

// Adapter 是 Kubernetes Cluster Adapter 实现。
type Adapter struct {
	mu        sync.RWMutex
	conn      adapter.ConnectionSpec
	connected bool
	// clientset *kubernetes.Clientset  // 完整实现时引入 client-go
	// config    *rest.Config
	// tenantID  uint64
}

// New 创建 K8s Adapter 实例（不连接）。
func New() *Adapter {
	return &Adapter{}
}

// Type 返回资源类型。
func (a *Adapter) Type() adapter.ResourceType {
	return adapter.TypeK8sCluster
}

// Connect 建立连接（骨架：仅记录 spec，标记 connected）。
//
// 完整实现：
//  1. 解密 ConnectionSpec.DSN（pkg/credinject）
//     - 格式：base64(ServiceAccount token) 或 kubeconfig YAML
//  2. rest.InClusterConfig() 或 clientcmd.BuildConfigFromFlags()
//  3. kubernetes.NewForConfig() 创建 clientset
//  4. 验证 cluster-info 可达
//  5. 启动 informers watch（pod / node / deployment）
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

// Close 关闭连接（停止 informers）。
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connected = false
	return nil
}

// Health 健康检查（通过 clientset.Discovery().ServerVersion()）。
func (a *Adapter) Health(ctx context.Context) (*adapter.HealthStatus, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	return &adapter.HealthStatus{
		Status:    "healthy",
		LatencyMs: 0,
		Message:   "skeleton: real health probe pending Task 2.3",
		CheckedAt: time.Now(),
	}, nil
}

// Diagnose 通用诊断入口。
//
// 完整实现：基于 Deployment 状态 / Pod event / 资源配额 等多信号关联。
func (a *Adapter) Diagnose(ctx context.Context, q adapter.DiagnoseQuery) (*adapter.DiagnoseResult, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, adapter.ErrNotConnected
	}
	return &adapter.DiagnoseResult{
		Category:    q.Category,
		Findings:    []map[string]interface{}{},
		Summary:     fmt.Sprintf("skeleton: category=%s not yet implemented", q.Category),
		Suggestions: []string{},
		ElapsedMs:   0,
	}, nil
}

// Collect 采集指标 / 状态。
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

// Execute 受限执行（K8s 写操作必须审批）。
//
// 完整实现：
//   - exec_into_pod 走 cmdpolicy L3 + Casbin + 完整审计（命令+输出+会话）
//   - scale / rollout_undo / cordon / drain 走 Casbin + cmdpolicy
//   - resize_pvc 走 cmdpolicy（防意外容量变更）
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
		Operation: op.Operation,
		Success:   false,
		Message:   "skeleton: real execution pending Task 2.3",
		Impacted:  0,
		Metadata:  map[string]string{"skeleton": "true"},
	}, nil
}

// RegisterTools 注册 K8s Adapter 暴露的工具方法到 Registry。
//
// 13 个工具方法（按风险等级分组）：
//
//   - L0 (4)：cluster_info / node_list / pod_list / deployment_status
//   - L1 (4)：rollout_history / pod_logs / events / top_nodes
//   - L2 (1)：describe_pod  // 软写（生成诊断报告）
//   - L3 (3)：scale / rollout_undo / cordon
//   - L4 (1)：exec_into_pod  // 破坏性（生产容器 exec，需双层审批 + 全审计）
func RegisterTools(reg *registry.Registry) error {
	tools := []registry.Tool{
		// L0 只读
		makeTool("k8s.cluster_info", adapter.RiskL0ReadOnly, "集群版本 + 节点数", nil),
		makeTool("k8s.node_list", adapter.RiskL0ReadOnly, "列出所有 Node + 状态", nil),
		makeTool("k8s.pod_list", adapter.RiskL0ReadOnly, "列出 Pod（含 ns/deploy 过滤）", nil),
		makeTool("k8s.deployment_status", adapter.RiskL0ReadOnly, "Deployment 状态 + ReplicaSet", nil),
		// L1 诊断
		makeTool("k8s.rollout_history", adapter.RiskL1Diagnostic, "Deployment rollout 历史", nil),
		makeTool("k8s.pod_logs", adapter.RiskL1Diagnostic, "Pod 日志（tail N / since）", nil),
		makeTool("k8s.events", adapter.RiskL1Diagnostic, "K8s events（Warning / 调度失败）", nil),
		makeTool("k8s.top_nodes", adapter.RiskL1Diagnostic, "Node 资源使用（CPU/内存，需 metrics-server）", nil),
		// L2 软写（生成诊断报告，不变更资源）
		makeTool("k8s.describe_pod", adapter.RiskL2SoftWrite, "生成 Pod 详细诊断报告（Casbin 单层审批）", nil),
		// L3 写操作
		makeTool("k8s.scale", adapter.RiskL3HardWrite, "调整 Deployment 副本数", nil),
		makeTool("k8s.rollout_undo", adapter.RiskL3HardWrite, "回滚到上一版本", nil),
		makeTool("k8s.cordon", adapter.RiskL3HardWrite, "标记 Node 不可调度", nil),
		// L4 破坏性
		makeTool("k8s.exec_into_pod", adapter.RiskL4Destructive, "在 Pod 容器内执行命令（双层审批 + 全审计）", nil),
	}
	return reg.RegisterTools(adapter.TypeK8sCluster, tools)
}

func makeTool(name string, risk adapter.RiskLevel, desc string, _ map[string]string) registry.Tool {
	return registry.Tool{
		Name:        name,
		Description: desc,
		RiskLevel:   risk,
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			return nil, fmt.Errorf("not_implemented: tool %q pending Task 2.3", name)
		},
		ArgsSchema: map[string]string{},
	}
}

var _ adapter.Adapter = (*Adapter)(nil)
