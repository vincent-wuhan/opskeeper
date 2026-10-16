package pigruntime

import (
	"context"
	"errors"
	"testing"
	"time"
)

var testDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func validSandbox() SandboxProfile {
	return SandboxProfile{
		NoAmbientDiscovery:  true,
		NoProviderOverride:  true,
		NoHostWrite:         true,
		NoDirectCredential:  true,
		ReadOnly:            true,
		AllowedNetworkHosts: []string{"example.internal"},
	}
}

type recordingAdapter struct {
	descriptor RuntimeDescriptor
	health     HealthReport
	cancel     CancellationResult
}

func (a recordingAdapter) Descriptor() RuntimeDescriptor { return a.descriptor }

func (a recordingAdapter) Launch(context.Context, LaunchRequest) (<-chan LifecycleEvent, error) {
	events := make(chan LifecycleEvent, 1)
	events <- LifecycleEvent{}
	return events, nil
}

func (a recordingAdapter) Health(context.Context) (HealthReport, error) {
	return a.health, nil
}

func (a recordingAdapter) Cancel(context.Context, CancellationRequest) (CancellationResult, error) {
	return a.cancel, nil
}

// TestAgentRuntimeAdapterPortAcceptsFakeImplementation 证明端口只依赖
// 生命周期契约，测试可以 fake，不需要启动 PiG 进程。
func TestAgentRuntimeAdapterPortAcceptsFakeImplementation(t *testing.T) {
	var adapter AgentRuntimeAdapter = recordingAdapter{
		descriptor: RuntimeDescriptor{
			RuntimeID:        "runtime-pig-1",
			Kind:             RuntimeKindPiG,
			Version:          "1.2.3",
			ReleaseDigest:    testDigest,
			CapabilityDigest: testDigest,
			MaxConcurrency:   2,
			SandboxProfile:   validSandbox(),
		},
		health: HealthReport{Status: HealthHealthy, LastHeartbeatAt: time.Now()},
		cancel: CancellationResult{State: CancellationAccepted},
	}
	if err := adapter.Descriptor().Validate(); err != nil {
		t.Fatalf("Descriptor().Validate() = %v", err)
	}
	if _, err := adapter.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	cancel, err := adapter.Cancel(context.Background(), CancellationRequest{})
	if err != nil || cancel.State != CancellationAccepted {
		t.Fatalf("Cancel() = %+v, %v", cancel, err)
	}
}

// TestRuntimeSemanticsMapsBothExistingRuntimes 固定 ChatRuntime 与
// AgentTeams 只映射治理语义，不改变现有运行时行为。
func TestRuntimeSemanticsMapsBothExistingRuntimes(t *testing.T) {
	chat := FromChatRuntimeSemantics("tenant-1", "incident-1", "investigator", "conversation-1", "task-1", true)
	if err := chat.Validate(); err != nil {
		t.Fatalf("chat semantics Validate() = %v", err)
	}
	if chat.TeamID != "" || chat.TurnID != "" {
		t.Fatalf("chat semantics unexpectedly carries AgentTeams identity: %+v", chat)
	}

	team := FromAgentTeamsSemantics("tenant-1", "incident-1", "investigator", "team-1", "turn-1", "task-2", true)
	if err := team.Validate(); err != nil {
		t.Fatalf("team semantics Validate() = %v", err)
	}
	if team.ConversationID != "" {
		t.Fatalf("team semantics unexpectedly carries chat identity: %+v", team)
	}
}

func validTerminal() TerminalResult {
	return TerminalResult{
		State:        TerminalSucceeded,
		Sanitized:    true,
		OutputRef:    "oss://results/1",
		OutputDigest: testDigest,
		CompletedAt:  time.Now(),
	}
}

// TestLifecycleEventValidateRequiresTerminalResult 固定终态事件必须有
// 完整结果，避免事件流静默停止。
func TestLifecycleEventValidateRequiresTerminalResult(t *testing.T) {
	base := LifecycleEvent{
		Sequence:   1,
		RuntimeID:  "runtime-pig-1",
		TenantID:   "tenant-1",
		TaskID:     "task-1",
		Type:       LifecycleEventCompleted,
		OccurredAt: time.Now(),
	}
	event := base
	event.Terminal = &TerminalResult{}
	if !errors.Is(event.Validate(), ErrInvalidContract) {
		t.Fatalf("empty terminal Validate() = %v, want ErrInvalidContract", event.Validate())
	}
	event.Terminal = nil
	if !errors.Is(event.Validate(), ErrTerminalRequired) {
		t.Fatalf("missing terminal Validate() = %v, want ErrTerminalRequired", event.Validate())
	}
	terminal := validTerminal()
	event.Terminal = &terminal
	if err := event.Validate(); err != nil {
		t.Fatalf("valid terminal Validate() = %v", err)
	}
}

// TestTerminalResultRequiresSanitizerDecision 固定输出必须声明已脱敏
// 或显式跳过，避免凭据进入证据链。
func TestTerminalResultRequiresSanitizerDecision(t *testing.T) {
	result := validTerminal()
	result.Sanitized = false
	if !errors.Is(result.Validate(), ErrInvalidContract) {
		t.Fatalf("unclassified result Validate() = %v", result.Validate())
	}
	result.SkippedSanitizer = true
	if err := result.Validate(); err != nil {
		t.Fatalf("explicit skip Validate() = %v", err)
	}
}

// TestHealthReportRequiresReasonForUnhealthy 固定降级或离线必须说明原因。
func TestHealthReportRequiresReasonForUnhealthy(t *testing.T) {
	report := HealthReport{Status: HealthDegraded}
	if !errors.Is(report.Validate(), ErrInvalidContract) {
		t.Fatalf("degraded without reason Validate() = %v", report.Validate())
	}
	report.Reason = "heartbeat overdue"
	if err := report.Validate(); err != nil {
		t.Fatalf("degraded with reason Validate() = %v", err)
	}
}
