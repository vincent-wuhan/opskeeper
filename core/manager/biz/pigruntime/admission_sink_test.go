package pigruntime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// admissionSinkFixture 构造带对账配置的 sink：manifest 与 descriptor 的
// CapabilityDigest 同字节（NewLifecycleSinkWithAdmission 的装配约束），
// 单角色 investigator 放行两个只读工具。
func admissionSinkFixture(t *testing.T) (*LifecycleSink, *memorySinkStore) {
	t.Helper()
	manifest := admissionManifest()
	store := newMemorySinkStore()
	sink, err := NewLifecycleSinkWithAdmission(store, RuntimeDescriptor{
		RuntimeID:        "runtime-pig-1",
		Kind:             RuntimeKindPiG,
		Version:          "1.2.3",
		ReleaseDigest:    testDigest,
		CapabilityDigest: ComputeCapabilityDigest(manifest),
		MaxConcurrency:   2,
		SandboxProfile:   validSandbox(),
	}, AdmissionConfig{
		Manifest: manifest,
		Roles:    []RoleAllowlist{investigatorAllowlist()},
	})
	if err != nil {
		t.Fatalf("NewLifecycleSinkWithAdmission() error = %v", err)
	}
	return sink, store
}

func registeredEventWithCapabilities(observed []CapabilityObservation) LifecycleEvent {
	event := baseEvent(LifecycleEventRegistered)
	event.Capabilities = observed
	return event
}

// TestSinkReconcilesCleanCapabilitiesIntoExposedSet 证明对账从注册路径
// 落：观察与声明一致时暴露集写进 CapabilitiesJSON，健康保持 online。
func TestSinkReconcilesCleanCapabilitiesIntoExposedSet(t *testing.T) {
	sink, store := admissionSinkFixture(t)
	if _, err := sink.ApplyEvent(registeredEventWithCapabilities([]CapabilityObservation{
		observation("pig.host.status", "opskeeper.host.status", ToolClassReadOnly),
		observation("pig.logs.tail", "opskeeper.logs.tail", ToolClassReadOnly),
	})); err != nil {
		t.Fatalf("ApplyEvent(registered) error = %v", err)
	}
	rec := store.runtimes["runtime-pig-1"]
	caps := map[string]any{}
	if err := json.Unmarshal([]byte(rec.CapabilitiesJSON), &caps); err != nil {
		t.Fatalf("CapabilitiesJSON = %q: %v", rec.CapabilitiesJSON, err)
	}
	if len(caps) != 2 || caps["opskeeper.host.status"] != "read" {
		t.Fatalf("CapabilitiesJSON = %q, want both read-only tools in domain spelling", rec.CapabilitiesJSON)
	}
	if rec.Health != model.RuntimeHealthOnline || rec.HealthReason != "" {
		t.Fatalf("health = %q reason = %q, want online with no reason", rec.Health, rec.HealthReason)
	}
}

// TestSinkDegradesOnUndeclaredReadOnlyObservation 未声明 read_only 观察 →
// 注册行 degraded + HealthReason 含 drift 计数，事件仍被接受。
func TestSinkDegradesOnUndeclaredReadOnlyObservation(t *testing.T) {
	sink, store := admissionSinkFixture(t)
	if _, err := sink.ApplyEvent(registeredEventWithCapabilities([]CapabilityObservation{
		observation("pig.host.status", "opskeeper.host.status", ToolClassReadOnly),
		observation("pig.rogue.read", "opskeeper.rogue.read", ToolClassReadOnly),
	})); err != nil {
		t.Fatalf("ApplyEvent(registered) error = %v", err)
	}
	rec := store.runtimes["runtime-pig-1"]
	if rec.Health != model.RuntimeHealthDegraded {
		t.Fatalf("health = %q, want degraded", rec.Health)
	}
	if !strings.Contains(rec.HealthReason, "undeclared=1") {
		t.Fatalf("HealthReason = %q, want undeclared=1", rec.HealthReason)
	}
}

// TestSinkRejectsAdmissionOnUndeclaredSideEffect 未声明 write 观察 →
// 注册行 degraded 落库（写不丢），且 ApplyEvent 返回
// ErrAdmissionRejected 让消费方把后续准入 fail closed。
func TestSinkRejectsAdmissionOnUndeclaredSideEffect(t *testing.T) {
	sink, store := admissionSinkFixture(t)
	_, err := sink.ApplyEvent(registeredEventWithCapabilities([]CapabilityObservation{
		observation("pig.host.status", "opskeeper.host.status", ToolClassReadOnly),
		observation("pig.rogue.exec", "opskeeper.rogue.exec", ToolClassDestructive),
	}))
	if !errors.Is(err, ErrAdmissionRejected) {
		t.Fatalf("ApplyEvent error = %v, want ErrAdmissionRejected", err)
	}
	rec := store.runtimes["runtime-pig-1"]
	if rec.Health != model.RuntimeHealthDegraded {
		t.Fatalf("health = %q, want degraded", rec.Health)
	}
	if !strings.Contains(rec.HealthReason, "admission rejected") {
		t.Fatalf("HealthReason = %q, want admission rejected marker", rec.HealthReason)
	}
	// 拒绝裁决下暴露集为空：rogue 工具不进 CapabilitiesJSON。
	caps := map[string]any{}
	_ = json.Unmarshal([]byte(rec.CapabilitiesJSON), &caps)
	if _, exposed := caps["opskeeper.rogue.exec"]; exposed {
		t.Fatalf("rejected runtime exposed rogue tool: %q", rec.CapabilitiesJSON)
	}
}

// TestSinkHeartbeatWithoutCapabilitiesSkipsReconciliation 心跳不带
// Capabilities 时不触发对账——不把「没带」当「观察为空」重写证据。
func TestSinkHeartbeatWithoutCapabilitiesSkipsReconciliation(t *testing.T) {
	sink, store := admissionSinkFixture(t)
	if _, err := sink.ApplyEvent(registeredEventWithCapabilities([]CapabilityObservation{
		observation("pig.host.status", "opskeeper.host.status", ToolClassReadOnly),
	})); err != nil {
		t.Fatalf("ApplyEvent(registered) error = %v", err)
	}
	heartbeat := baseEvent(LifecycleEventHeartbeat)
	heartbeat.Sequence = 2
	heartbeat.OccurredAt = time.Date(2026, 11, 20, 12, 1, 0, 0, time.UTC)
	if _, err := sink.ApplyEvent(heartbeat); err != nil {
		t.Fatalf("ApplyEvent(heartbeat) error = %v", err)
	}
	rec := store.runtimes["runtime-pig-1"]
	if rec.HealthReason != "" || rec.Health == model.RuntimeHealthDegraded {
		t.Fatalf("capability-free heartbeat changed health: %q / %q", rec.Health, rec.HealthReason)
	}
}

// TestSinkHealthyHeartbeatDoesNotClearCapabilityDrift 钉住 drift 是持续
// 性异常：注册时记了 drift 降级，之后一条健康心跳不得把它抹回 online。
func TestSinkHealthyHeartbeatDoesNotClearCapabilityDrift(t *testing.T) {
	sink, store := admissionSinkFixture(t)
	if _, err := sink.ApplyEvent(registeredEventWithCapabilities([]CapabilityObservation{
		observation("pig.host.status", "opskeeper.host.status", ToolClassReadOnly),
		observation("pig.rogue.read", "opskeeper.rogue.read", ToolClassReadOnly),
	})); err != nil {
		t.Fatalf("ApplyEvent(registered) error = %v", err)
	}
	heartbeat := baseEvent(LifecycleEventHeartbeat)
	heartbeat.Sequence = 2
	heartbeat.OccurredAt = time.Date(2026, 11, 20, 12, 1, 0, 0, time.UTC)
	heartbeat.Health = &HealthReport{Status: HealthHealthy, ActiveExecutions: 1}
	if _, err := sink.ApplyEvent(heartbeat); err != nil {
		t.Fatalf("ApplyEvent(heartbeat) error = %v", err)
	}
	rec := store.runtimes["runtime-pig-1"]
	if rec.Health != model.RuntimeHealthDegraded || !strings.Contains(rec.HealthReason, "undeclared=1") {
		t.Fatalf("healthy heartbeat cleared capability drift: %q / %q", rec.Health, rec.HealthReason)
	}
}

// TestSinkCleanObservationClearsCapabilityDrift 干净观察负责清除 drift
// 降级（与心跳相对：恢复只能来自重算对账，不能来自时间流逝）。
func TestSinkCleanObservationClearsCapabilityDrift(t *testing.T) {
	sink, store := admissionSinkFixture(t)
	if _, err := sink.ApplyEvent(registeredEventWithCapabilities([]CapabilityObservation{
		observation("pig.rogue.read", "opskeeper.rogue.read", ToolClassReadOnly),
	})); err != nil {
		t.Fatalf("ApplyEvent(registered) error = %v", err)
	}
	if rec := store.runtimes["runtime-pig-1"]; rec.Health != model.RuntimeHealthDegraded {
		t.Fatalf("precondition: health = %q, want degraded", rec.Health)
	}
	heartbeat := baseEvent(LifecycleEventHeartbeat)
	heartbeat.Sequence = 2
	heartbeat.OccurredAt = time.Date(2026, 11, 20, 12, 1, 0, 0, time.UTC)
	heartbeat.Capabilities = []CapabilityObservation{
		observation("pig.host.status", "opskeeper.host.status", ToolClassReadOnly),
	}
	if _, err := sink.ApplyEvent(heartbeat); err != nil {
		t.Fatalf("ApplyEvent(heartbeat) error = %v", err)
	}
	rec := store.runtimes["runtime-pig-1"]
	if rec.Health != model.RuntimeHealthOnline || rec.HealthReason != "" {
		t.Fatalf("clean observation did not clear drift: %q / %q", rec.Health, rec.HealthReason)
	}
}

// TestSinkRejectsAdmissionConfigMismatch 装配约束：manifest 摘要与
// descriptor.CapabilityDigest 不一致 → 构造失败；角色重复 → 构造失败。
func TestSinkRejectsAdmissionConfigMismatch(t *testing.T) {
	store := newMemorySinkStore()
	manifest := admissionManifest()
	if _, err := NewLifecycleSinkWithAdmission(store, RuntimeDescriptor{
		RuntimeID:        "runtime-pig-1",
		Kind:             RuntimeKindPiG,
		Version:          "1.2.3",
		ReleaseDigest:    testDigest,
		CapabilityDigest: testDigest, // 与 manifest 摘要不同字节
		MaxConcurrency:   2,
		SandboxProfile:   validSandbox(),
	}, AdmissionConfig{Manifest: manifest, Roles: []RoleAllowlist{investigatorAllowlist()}}); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("digest mismatch error = %v, want ErrInvalidContract", err)
	}

	roles := []RoleAllowlist{investigatorAllowlist(), investigatorAllowlist()}
	if _, err := NewLifecycleSinkWithAdmission(store, RuntimeDescriptor{
		RuntimeID:        "runtime-pig-1",
		Kind:             RuntimeKindPiG,
		Version:          "1.2.3",
		ReleaseDigest:    testDigest,
		CapabilityDigest: ComputeCapabilityDigest(manifest),
		MaxConcurrency:   2,
		SandboxProfile:   validSandbox(),
	}, AdmissionConfig{Manifest: manifest, Roles: roles}); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("duplicate role error = %v, want ErrInvalidContract", err)
	}
}
