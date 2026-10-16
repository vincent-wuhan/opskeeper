package pigruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePiGLaunch 生成固定 SHA-256 的 deterministic fake-PiG 工件。
// 使用临时脚本可避免测试框架输出混入运行时 JSONL 事件流。
func fakePiGLaunch(t *testing.T, mode string) Config {
	t.Helper()
	artifact := filepath.Join(t.TempDir(), "fake-pig-"+mode)
	var script string
	switch mode {
	case "completion":
		script = `#!/bin/sh
now=$(/bin/date -u +%Y-%m-%dT%H:%M:%SZ)
/bin/cat <<EOF
{"Sequence":1,"Type":"registered"}
{"Sequence":2,"Type":"heartbeat"}
{"Sequence":3,"Type":"progress"}
{"Sequence":4,"Type":"completed","Terminal":{"State":"succeeded","OutputRef":"oss://results/task-1","OutputDigest":"` + testDigest + `","Sanitized":true,"CompletedAt":"$now"}}
EOF
`
	case "cancellation":
		script = `#!/bin/sh
/bin/echo '{"Sequence":1,"Type":"registered"}'
exec /bin/sleep 30
`
	case "crash":
		script = `#!/bin/sh
/bin/echo '{"Sequence":1,"Type":"registered"}'
exit 42
`
	case "invalid-json":
		script = `#!/bin/sh
/bin/echo '{not-json'
`
	default:
		t.Fatalf("未知 fake-PiG 模式: %q", mode)
	}
	if err := os.WriteFile(artifact, []byte(script), 0o755); err != nil {
		t.Fatalf("写入 fake-PiG 工件失败: %v", err)
	}
	digest, err := HashArtifact(artifact)
	if err != nil {
		t.Fatalf("HashArtifact() error = %v", err)
	}
	return Config{
		Enabled:         true,
		Mode:            ModeExecutable,
		PinnedArtifact:  artifact,
		ArtifactDigest:  digest,
		HeartbeatWindow: 5,
		LaunchTimeoutMS: 1000,
	}
}

func processDriverFixture(t *testing.T, mode string) (*ProcessDriver, LaunchRequest) {
	t.Helper()
	config := fakePiGLaunch(t, mode)
	release := validRelease()
	release.ArtifactDigest = config.ArtifactDigest
	snapshot := ExecutionSnapshot{
		RuntimeID:        "runtime-pig-1",
		Release:          release,
		CapabilityDigest: ComputeCapabilityDigest(release.Manifest),
		PinnedArtifact:   config.PinnedArtifact,
	}
	descriptor := RuntimeDescriptor{
		RuntimeID:        "runtime-pig-1",
		Kind:             RuntimeKindPiG,
		Version:          "1.2.3",
		ReleaseDigest:    config.ArtifactDigest,
		CapabilityDigest: snapshot.CapabilityDigest,
		MaxConcurrency:   2,
		SandboxProfile:   validSandbox(),
	}
	driver, err := NewProcessDriver(config, descriptor)
	if err != nil {
		t.Fatalf("NewProcessDriver() error = %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	return driver, LaunchRequest{
		Semantics: FromChatRuntimeSemantics(
			"tenant-1", "incident-1", "investigator", "conversation-1", "task-1", true,
		),
		Execution:       snapshot,
		Deadline:        time.Now().Add(30 * time.Second),
		MaxEvents:       16,
		AllowedTools:    []string{"opskeeper.host.status"},
		HeartbeatWindow: time.Second,
	}
}

func collectEvents(t *testing.T, events <-chan LifecycleEvent, count int) []LifecycleEvent {
	t.Helper()
	result := make([]LifecycleEvent, 0, count)
	for len(result) < count {
		select {
		case event, ok := <-events:
			if !ok {
				return result
			}
			result = append(result, event)
		case <-time.After(10 * time.Second):
			t.Fatalf("等待事件超时，已收到 %+v", result)
		}
	}
	return result
}

// TestProcessDriverEnvironmentIsPIGOnly 证明进程环境不继承宿主凭据或
// Provider 配置，只保留 PiG 身份与只读语义。
func TestProcessDriverEnvironmentIsPIGOnly(t *testing.T) {
	driver, request := processDriverFixture(t, "completion")
	for _, item := range driver.processEnvironment(request) {
		if !strings.HasPrefix(item, "PIG_") {
			t.Fatalf("子进程环境越界: %q", item)
		}
	}
}

// TestProcessDriverCompletesNormalizedStream 覆盖启动、注册、心跳、进度、
// 完成与健康探测的完整正常路径。
func TestProcessDriverCompletesNormalizedStream(t *testing.T) {
	driver, request := processDriverFixture(t, "completion")
	health, err := driver.Health(t.Context())
	if err != nil || health.Status != HealthOffline {
		t.Fatalf("启动前 Health() = %+v, %v", health, err)
	}
	events, err := driver.Launch(t.Context(), request)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	received := collectEvents(t, events, 4)
	want := []LifecycleEventType{
		LifecycleEventRegistered,
		LifecycleEventHeartbeat,
		LifecycleEventProgress,
		LifecycleEventCompleted,
	}
	for index, event := range received {
		if event.Type != want[index] {
			t.Fatalf("event[%d].Type = %s, want %s", index, event.Type, want[index])
		}
		if event.RuntimeID != request.Execution.RuntimeID ||
			event.TenantID != request.Semantics.TenantID ||
			event.TaskID != request.Semantics.TaskID {
			t.Fatalf("event[%d] identity was not normalized: %+v", index, event)
		}
	}
}

// TestProcessDriverCancelsIsolatedProcess 证明非强杀取消也会撤销子进程，
// 并由驱动补发取消终态，避免事件流悬空。
func TestProcessDriverCancelsIsolatedProcess(t *testing.T) {
	driver, request := processDriverFixture(t, "cancellation")
	events, err := driver.Launch(t.Context(), request)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	received := collectEvents(t, events, 1)
	if received[0].Type != LifecycleEventRegistered {
		t.Fatalf("cancellation pre-events = %+v", received)
	}
	result, err := driver.Cancel(t.Context(), CancellationRequest{
		RuntimeID: request.Execution.RuntimeID,
		TaskID:    request.Semantics.TaskID,
		Reason:    "operator requested cancellation",
	})
	if err != nil || result.State != CancellationTerminalPending {
		t.Fatalf("Cancel() = %+v, %v", result, err)
	}
	received = append(received, collectEvents(t, events, 1)...)
	last := received[len(received)-1]
	if last.Type != LifecycleEventCancelled || last.Terminal == nil ||
		last.Terminal.State != TerminalCancelled {
		t.Fatalf("terminal event = %+v, want cancelled", last)
	}
}

// TestProcessDriverEmitsTerminalWhenParentContextCancelsWhileStreaming 覆盖
// 事件通道拥塞时父上下文取消的终态兜底路径。
func TestProcessDriverEmitsTerminalWhenParentContextCancelsWhileStreaming(t *testing.T) {
	driver, request := processDriverFixture(t, "completion")
	request.MaxEvents = 1
	ctx, cancel := context.WithCancel(t.Context())
	events, err := driver.Launch(ctx, request)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	cancel()
	received := collectEvents(t, events, 2)
	if received[len(received)-1].Type != LifecycleEventCancelled {
		t.Fatalf("context cancellation terminal = %+v", received[len(received)-1])
	}
}

// TestProcessDriverConvertsCrashToFailedTerminal 证明非法退出码会被归一为
// 失败终态，而不是让 Manager 等待一个已经消失的进程。
func TestProcessDriverConvertsCrashToFailedTerminal(t *testing.T) {
	driver, request := processDriverFixture(t, "crash")
	events, err := driver.Launch(t.Context(), request)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	received := collectEvents(t, events, 2)
	last := received[len(received)-1]
	if last.Type != LifecycleEventFailed || last.Terminal == nil ||
		last.Terminal.State != TerminalFailed {
		t.Fatalf("crash terminal = %+v, want failed", last)
	}
}

// TestProcessDriverRejectsInvalidJSONL 证明协议错误会 fail closed 并补发
// 已脱敏的失败终态。
func TestProcessDriverRejectsInvalidJSONL(t *testing.T) {
	driver, request := processDriverFixture(t, "invalid-json")
	events, err := driver.Launch(t.Context(), request)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	received := collectEvents(t, events, 1)
	if received[0].Type != LifecycleEventFailed ||
		received[0].Terminal.FailureReason != "invalid JSONL event" {
		t.Fatalf("invalid JSON terminal = %+v", received[0])
	}
}

// TestProcessDriverRejectsArtifactDrift 在启动前校验固定工件摘要。
func TestProcessDriverRejectsArtifactDrift(t *testing.T) {
	config := fakePiGLaunch(t, "completion")
	config.ArtifactDigest = strings.Repeat("b", 64)
	descriptor := RuntimeDescriptor{
		RuntimeID:        "runtime-pig-1",
		Kind:             RuntimeKindPiG,
		Version:          "1.2.3",
		ReleaseDigest:    config.ArtifactDigest,
		CapabilityDigest: testDigest,
		MaxConcurrency:   1,
		SandboxProfile:   validSandbox(),
	}
	if _, err := NewProcessDriver(config, descriptor); !errors.Is(err, ErrArtifactDrift) {
		t.Fatalf("NewProcessDriver() error = %v, want ErrArtifactDrift", err)
	}
}
