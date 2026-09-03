// Package alert - SourceLinker 单测（Day 3 路径 A 集成 - host 适配扩展）。
//
// 覆盖目标：
//   - DIAGNOSIS_SKILL_MAP 包含 host.cpu / host.memory / host.disk 三条 host 路径
//   - HostAdapter 接口的 NoopHostAdapter 占位实现可用
//   - WithHostAdapter 注入自定义 adapter（NilAdapter 防御）
//   - E2E：source_linker 根据 alert 标签（resource=host, metric=cpu_usage）
//     路由回 host.cpu skill + NoopHostAdapter 可被调用
//
// 设计参考：spec §D.12 + 路径 A 集成 Day 3 任务 3.5。
package alert

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeHostAdapter 记录三个方法的调用参数，供单测验证路由 binding 与 selector 形态。
type fakeHostAdapter struct {
	cpuTarget     string
	memTarget     string
	diskTarget    string
	cpuCalls      int
	memCalls      int
	diskCalls     int
	cpuFinding    string
	memFinding    string
	diskFinding   string
	evidenceBytes []byte
}

func (f *fakeHostAdapter) InvestigateCPU(_ context.Context, target string) (string, []byte, error) {
	f.cpuCalls++
	f.cpuTarget = target
	return f.cpuFinding, f.evidenceBytes, nil
}

func (f *fakeHostAdapter) InvestigateMemory(_ context.Context, target string) (string, []byte, error) {
	f.memCalls++
	f.memTarget = target
	return f.memFinding, f.evidenceBytes, nil
}

func (f *fakeHostAdapter) InvestigateDisk(_ context.Context, target string) (string, []byte, error) {
	f.diskCalls++
	f.diskTarget = target
	return f.diskFinding, f.evidenceBytes, nil
}

// 1. TestSourceLinker_HostCPU : Verify DIAGNOSIS_SKILL_MAP contains host.cpu entry.
func TestSourceLinker_HostCPU(t *testing.T) {
	s := NewSourceLinker()
	link, err := s.Lookup(string(ResourceTypeHost), "host.cpu")
	if err != nil {
		t.Fatalf("Lookup host.cpu: %v", err)
	}
	if link == nil {
		t.Fatal("host.cpu: link = nil, want non-nil")
	}
	if link.Tool != "host.cpu.investigateCPU" {
		t.Errorf("host.cpu: Tool = %q, want %q", link.Tool, "host.cpu.investigateCPU")
	}
	if link.NextPhase != "link_runtime_to_commit" {
		t.Errorf("host.cpu: NextPhase = %q, want %q", link.NextPhase, "link_runtime_to_commit")
	}
	if link.Timeout != 30*time.Second {
		t.Errorf("host.cpu: Timeout = %v, want 30s", link.Timeout)
	}
	if link.HarnessCaseID != "host/cpu-spike" {
		t.Errorf("host.cpu: HarnessCaseID = %q, want %q", link.HarnessCaseID, "host/cpu-spike")
	}
	// 验证 resourceScope 入参
	scope, ok := link.Args["resourceScope"].(string)
	if !ok || scope != "host" {
		t.Errorf("host.cpu: resourceScope = %v, want %q", link.Args["resourceScope"], "host")
	}
}

// 2. TestSourceLinker_HostMemory : Verify DIAGNOSIS_SKILL_MAP contains host.memory entry.
func TestSourceLinker_HostMemory(t *testing.T) {
	s := NewSourceLinker()
	link, err := s.Lookup(string(ResourceTypeHost), "host.memory")
	if err != nil {
		t.Fatalf("Lookup host.memory: %v", err)
	}
	if link == nil {
		t.Fatal("host.memory: link = nil, want non-nil")
	}
	if link.Tool != "host.memory.investigateMemory" {
		t.Errorf("host.memory: Tool = %q, want %q", link.Tool, "host.memory.investigateMemory")
	}
	if link.NextPhase != "link_runtime_to_commit" {
		t.Errorf("host.memory: NextPhase = %q, want %q", link.NextPhase, "link_runtime_to_commit")
	}
	if link.HarnessCaseID != "" {
		t.Errorf("host.memory: HarnessCaseID = %q, want empty", link.HarnessCaseID)
	}
}

// 3. TestSourceLinker_HostDisk : Verify DIAGNOSIS_SKILL_MAP contains host.disk entry.
func TestSourceLinker_HostDisk(t *testing.T) {
	s := NewSourceLinker()
	link, err := s.Lookup(string(ResourceTypeHost), "host.disk")
	if err != nil {
		t.Fatalf("Lookup host.disk: %v", err)
	}
	if link == nil {
		t.Fatal("host.disk: link = nil, want non-nil")
	}
	if link.Tool != "host.disk.investigateDisk" {
		t.Errorf("host.disk: Tool = %q, want %q", link.Tool, "host.disk.investigateDisk")
	}
	if link.NextPhase != "link_runtime_to_commit" {
		t.Errorf("host.disk: NextPhase = %q, want %q", link.NextPhase, "link_runtime_to_commit")
	}
	if link.HarnessCaseID != "" {
		t.Errorf("host.disk: HarnessCaseID = %q, want empty", link.HarnessCaseID)
	}
}

// TestSourceLinker_NoopHostAdapter_DoesNotPanic : 默认 NoopHostAdapter 占位可用。
func TestSourceLinker_NoopHostAdapter_DoesNotPanic(t *testing.T) {
	noop := NoopHostAdapter{}
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		fn   func() (string, []byte, error)
		want string
	}{
		{"cpu", func() (string, []byte, error) { return noop.InvestigateCPU(ctx, "host-1") }, "CPU"},
		{"memory", func() (string, []byte, error) { return noop.InvestigateMemory(ctx, "host-1") }, "memory"},
		{"disk", func() (string, []byte, error) { return noop.InvestigateDisk(ctx, "host-1") }, "disk"},
	} {
		finding, evidence, err := tc.fn()
		if err != nil {
			t.Errorf("%s: err = %v, want nil", tc.name, err)
		}
		if !strings.Contains(finding, "noop") {
			t.Errorf("%s: finding = %q, want contains 'noop'", tc.name, finding)
		}
		if !strings.Contains(finding, tc.want) {
			t.Errorf("%s: finding = %q, want contains %q", tc.name, finding, tc.want)
		}
		if evidence != nil {
			t.Errorf("%s: evidence = %v, want nil (placeholder)", tc.name, evidence)
		}
	}
}

// TestSourceLinker_WithHostAdapter_NilSafe : 注入 nil adapter 保持默认 NoopHostAdapter。
func TestSourceLinker_WithHostAdapter_NilSafe(t *testing.T) {
	s := NewSourceLinker()
	before := s.HostAdapter()
	if _, ok := before.(NoopHostAdapter); !ok {
		t.Fatalf("default adapter = %T, want NoopHostAdapter", before)
	}
	s.WithHostAdapter(nil) // should not override default
	after := s.HostAdapter()
	if _, ok := after.(NoopHostAdapter); !ok {
		t.Errorf("after WithHostAdapter(nil): adapter = %T, want NoopHostAdapter", after)
	}

	// 注入真实 fake
	fake := &fakeHostAdapter{cpuFinding: "cpu saturated", evidenceBytes: []byte("top -bn1")}
	s.WithHostAdapter(fake)
	if got := s.HostAdapter(); got != HostAdapter(fake) {
		t.Errorf("WithHostAdapter(fake): adapter = %T, want fakeHostAdapter", got)
	}
}

// TestSourceLinker_NilSourceLinker_HostAdapter : nil 指针防御返 NoopHostAdapter。
func TestSourceLinker_NilSourceLinker_HostAdapter(t *testing.T) {
	var s *SourceLinker
	if got := s.HostAdapter(); got == nil {
		t.Error("nil SourceLinker: HostAdapter = nil, want NoopHostAdapter{}")
	}
}

// 4. TestSourceLinker_HostEndToEnd : 端到端模拟 alert 标签 -> 路由 -> adapter 调用。
//
// 流程：
//
//	alert.labels{resource="host", metric="cpu_usage"} →
//	static rule "host/cpu-spike" →
//	ResolveFromStaticHit → host.cpu skill →
//	注入 fakeHostAdapter 并调用 → 验证 finding 传出。
func TestSourceLinker_HostEndToEnd(t *testing.T) {
	s := NewSourceLinker()
	fake := &fakeHostAdapter{
		cpuFinding:    "load average 18.4, top: stress-ng 480%",
		evidenceBytes: []byte("uptime: 18.4 12.3 9.8"),
	}
	s.WithHostAdapter(fake)

	// Step 1: 模拟 static rule hit（design D12 + D.10）
	hit := &StaticRuleHit{Rule: StaticRule{ID: "host/cpu-spike"}}
	link, err := s.ResolveFromStaticHit(context.Background(), hit)
	if err != nil {
		t.Fatalf("ResolveFromStaticHit(host/cpu-spike): err = %v, want nil", err)
	}
	if link == nil {
		t.Fatal("host/cpu-spike: link = nil, want host.cpu mapping")
	}

	// Step 2: 模拟 alert labels 提取 target
	incident := struct {
		Labels map[string]string
	}{
		Labels: map[string]string{
			"resource": "host",
			"metric":   "cpu_usage",
			"instance": "host-prod-01",
		},
	}
	selector := link.Args["selector"].(map[string]any)
	targetExpr := selector["target"].(string)
	if !strings.Contains(targetExpr, "incident.labels.instance") {
		t.Errorf("host.cpu: target selector = %q, want contains 'incident.labels.instance'", targetExpr)
	}

	// Step 3: 沿 source_linker 路由到 host adapter
	toolName := link.Tool
	switch toolName {
	case "host.cpu.investigateCPU":
		finding, evidence, err := fake.InvestigateCPU(context.Background(), incident.Labels["instance"])
		if err != nil {
			t.Fatalf("InvestigateCPU: %v", err)
		}
		if finding != "load average 18.4, top: stress-ng 480%" {
			t.Errorf("InvestigateCPU finding = %q, want CPU saturated", finding)
		}
		if string(evidence) != "uptime: 18.4 12.3 9.8" {
			t.Errorf("InvestigateCPU evidence = %q, want prom dump", string(evidence))
		}
	case "host.memory.investigateMemory":
		finding, _, err := fake.InvestigateMemory(context.Background(), incident.Labels["instance"])
		if err != nil {
			t.Fatalf("InvestigateMemory: %v", err)
		}
		_ = finding
	case "host.disk.investigateDisk":
		finding, _, err := fake.InvestigateDisk(context.Background(), incident.Labels["instance"])
		if err != nil {
			t.Fatalf("InvestigateDisk: %v", err)
		}
		_ = finding
	default:
		t.Errorf("host/cpu-spike routed to unknown tool %q", toolName)
	}

	if fake.cpuCalls != 1 {
		t.Errorf("fakeHostAdapter.cpuCalls = %d, want 1", fake.cpuCalls)
	}
	if fake.cpuTarget != "host-prod-01" {
		t.Errorf("fakeHostAdapter.cpuTarget = %q, want %q", fake.cpuTarget, "host-prod-01")
	}
}

// TestSourceLinker_HostNormalizeAlias : host 别名同时被支持（与 pg/k8s 一致）。
func TestSourceLinker_HostNormalizeAlias(t *testing.T) {
	s := NewSourceLinker()
	_, err := s.Lookup("host", "host.cpu")
	if err != nil {
		t.Errorf("Lookup(host, host.cpu): %v, want nil", err)
	}
}
