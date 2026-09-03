package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
)

func TestAdapter_Type(t *testing.T) {
	a := New()
	if got := a.Type(); got != adapter.TypeK8sCluster {
		t.Errorf("Type() = %s, want k8s_cluster", got)
	}
}

func TestAdapter_Health_NotConnected(t *testing.T) {
	a := New()
	_, err := a.Health(context.Background())
	if !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("expected ErrNotConnected, got %v", err)
	}
}

func TestAdapter_Execute_RequiresApproval(t *testing.T) {
	a := New()
	_ = a.Connect(context.Background(), adapter.ConnectionSpec{})
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation: "exec_into_pod",
		// 缺 ApprovedBy
	})
	if !errors.Is(err, adapter.ErrApprovalRequired) {
		t.Errorf("expected ErrApprovalRequired, got %v", err)
	}
}

func TestAdapter_RegisterTools_Count(t *testing.T) {
	// 期望 13 个 K8s 工具（4 L0 + 4 L1 + 1 L2 + 3 L3 + 1 L4 = 13）
	expectedTools := []string{
		// L0 (4)
		"k8s.cluster_info", "k8s.node_list", "k8s.pod_list", "k8s.deployment_status",
		// L1 (4)
		"k8s.rollout_history", "k8s.pod_logs", "k8s.events", "k8s.top_nodes",
		// L2 (1)
		"k8s.describe_pod",
		// L3 (3)
		"k8s.scale", "k8s.rollout_undo", "k8s.cordon",
		// L4 (1)
		"k8s.exec_into_pod",
	}
	if len(expectedTools) != 13 {
		t.Errorf("expected 13 K8s tools, got %d", len(expectedTools))
	}
	t.Logf("K8s tools: %d total (4 L0 + 4 L1 + 1 L2 + 3 L3 + 1 L4)", len(expectedTools))
}

func TestAdapter_Connect(t *testing.T) {
	a := New()
	if err := a.Connect(context.Background(), adapter.ConnectionSpec{}); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("Health failed: %v", err)
	}
	if !strings.Contains(h.Message, "skeleton") {
		t.Errorf("expected skeleton marker: %s", h.Message)
	}
}
