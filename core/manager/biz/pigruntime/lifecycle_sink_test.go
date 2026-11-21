package pigruntime

import (
	"errors"
	"fmt"
	"testing"
	"time"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// memorySinkStore 是 RuntimeMgmtStore 的内存 fake：按本文件断言需要记录
// 注册行与终态结果，并支持注入 GetRuntime 故障。
type memorySinkStore struct {
	runtimes map[string]model.RuntimeRecord
	results  []model.TaskResult
	getErr   error
}

func newMemorySinkStore() *memorySinkStore {
	return &memorySinkStore{runtimes: map[string]model.RuntimeRecord{}}
}

func (s *memorySinkStore) GetRuntime(id string) (model.RuntimeRecord, error) {
	if s.getErr != nil {
		return model.RuntimeRecord{}, s.getErr
	}
	rec, ok := s.runtimes[id]
	if !ok {
		return model.RuntimeRecord{}, ErrRuntimeRecordMissing
	}
	return rec, nil
}

func (s *memorySinkStore) UpsertRuntime(rec model.RuntimeRecord) error {
	s.runtimes[rec.RuntimeID] = rec
	return nil
}

func (s *memorySinkStore) SaveTerminalResult(result model.TaskResult) error {
	s.results = append(s.results, result)
	return nil
}

func sinkFixture(t *testing.T) (*LifecycleSink, *memorySinkStore) {
	t.Helper()
	store := newMemorySinkStore()
	sink, err := NewLifecycleSink(store, RuntimeDescriptor{
		RuntimeID:        "runtime-pig-1",
		Kind:             RuntimeKindPiG,
		Version:          "1.2.3",
		ReleaseDigest:    testDigest,
		CapabilityDigest: testDigest,
		MaxConcurrency:   2,
		SandboxProfile:   validSandbox(),
	})
	if err != nil {
		t.Fatalf("NewLifecycleSink() error = %v", err)
	}
	return sink, store
}

func baseEvent(eventType LifecycleEventType) LifecycleEvent {
	return LifecycleEvent{
		Sequence:   1,
		RuntimeID:  "runtime-pig-1",
		TenantID:   "tenant-1",
		TaskID:     "task-1",
		Type:       eventType,
		OccurredAt: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
	}
}

// TestLifecycleSinkRegistersRuntimeFromDescriptor 钉住注册行只写 Manager
// 侧钉住的事实：digest/版本/并发来自 descriptor，不来自子进程自报。
func TestLifecycleSinkRegistersRuntimeFromDescriptor(t *testing.T) {
	sink, store := sinkFixture(t)
	write, err := sink.ApplyEvent(baseEvent(LifecycleEventRegistered))
	if err != nil {
		t.Fatalf("ApplyEvent(registered) error = %v", err)
	}
	if !write.RuntimeTouched || !write.Heartbeat {
		t.Fatalf("registered write = %+v, want runtime+heartbeat", write)
	}
	rec, ok := store.runtimes["runtime-pig-1"]
	if !ok {
		t.Fatal("registered event did not create a runtime record")
	}
	if rec.Kind != model.RuntimeKindPiG {
		t.Fatalf("record kind = %q, want pig", rec.Kind)
	}
	if rec.ArtifactDigest != testDigest || rec.CapabilityDigest != testDigest {
		t.Fatalf("record digests = %q/%q, want descriptor digests", rec.ArtifactDigest, rec.CapabilityDigest)
	}
	if rec.VersionSource != model.VersionSourceReleaseMetadata || rec.ObservedVersion != "1.2.3" {
		t.Fatalf("record version = %q via %q, want 1.2.3 via release_metadata", rec.ObservedVersion, rec.VersionSource)
	}
	if rec.TotalSlots != 2 {
		t.Fatalf("record TotalSlots = %d, want MaxConcurrency 2", rec.TotalSlots)
	}
	if rec.LastHeartbeatAt == nil || !rec.LastHeartbeatAt.Equal(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("record LastHeartbeatAt = %v, want event time", rec.LastHeartbeatAt)
	}
	if got := rec.EvaluateHealth(rec.LastHeartbeatAt.Add(time.Second)); got != model.RuntimeHealthOnline {
		t.Fatalf("EvaluateHealth after registration = %q, want online", got)
	}
}

// TestLifecycleSinkHeartbeatRefreshesLiveness 证明心跳推进存活时戳并采纳
// 事件 Health 的活跃数与超期降级。
func TestLifecycleSinkHeartbeatRefreshesLiveness(t *testing.T) {
	sink, store := sinkFixture(t)
	if _, err := sink.ApplyEvent(baseEvent(LifecycleEventRegistered)); err != nil {
		t.Fatalf("registered error = %v", err)
	}
	beat := baseEvent(LifecycleEventHeartbeat)
	beat.Sequence = 2
	beat.OccurredAt = time.Date(2026, 11, 20, 12, 0, 30, 0, time.UTC)
	beat.Health = &HealthReport{Status: HealthHealthy, ActiveExecutions: 1}
	write, err := sink.ApplyEvent(beat)
	if err != nil {
		t.Fatalf("ApplyEvent(heartbeat) error = %v", err)
	}
	if !write.Heartbeat {
		t.Fatalf("heartbeat write = %+v", write)
	}
	rec := store.runtimes["runtime-pig-1"]
	if rec.ActiveTasks != 1 {
		t.Fatalf("ActiveTasks = %d, want 1", rec.ActiveTasks)
	}
	if rec.LastHeartbeatAt == nil || !rec.LastHeartbeatAt.Equal(beat.OccurredAt) {
		t.Fatalf("LastHeartbeatAt = %v, want %v", rec.LastHeartbeatAt, beat.OccurredAt)
	}

	// 超期心跳把行降级为 degraded，恢复心跳把降级解释除。
	late := baseEvent(LifecycleEventHeartbeat)
	late.Sequence = 3
	late.OccurredAt = beat.OccurredAt.Add(time.Minute)
	late.Health = &HealthReport{Status: HealthDegraded, ActiveExecutions: 1, HeartbeatOverdue: true, Reason: "slow beat"}
	if _, err := sink.ApplyEvent(late); err != nil {
		t.Fatalf("overdue heartbeat error = %v", err)
	}
	if got := store.runtimes["runtime-pig-1"].Health; got != model.RuntimeHealthDegraded {
		t.Fatalf("overdue heartbeat health = %q, want degraded", got)
	}
	recovered := late
	recovered.Sequence = 4
	recovered.OccurredAt = late.OccurredAt.Add(time.Second)
	recovered.Health = &HealthReport{Status: HealthHealthy, ActiveExecutions: 0}
	if _, err := sink.ApplyEvent(recovered); err != nil {
		t.Fatalf("recovered heartbeat error = %v", err)
	}
	rec = store.runtimes["runtime-pig-1"]
	if rec.Health != model.RuntimeHealthOnline || rec.HealthReason != "" {
		t.Fatalf("recovered health = %q (%q), want online", rec.Health, rec.HealthReason)
	}
}

// TestLifecycleSinkRejectsHeartbeatBeforeRegistration 钉住乱序拒绝：心跳
// 先于注册到达时不得自造注册行。
func TestLifecycleSinkRejectsHeartbeatBeforeRegistration(t *testing.T) {
	sink, store := sinkFixture(t)
	if _, err := sink.ApplyEvent(baseEvent(LifecycleEventHeartbeat)); err == nil {
		t.Fatal("heartbeat before registration was accepted")
	}
	if len(store.runtimes) != 0 {
		t.Fatalf("heartbeat before registration created rows: %+v", store.runtimes)
	}
}

// TestLifecycleSinkRejectsRegressedHeartbeat 钉住心跳时戳回退拒绝。
func TestLifecycleSinkRejectsRegressedHeartbeat(t *testing.T) {
	sink, _ := sinkFixture(t)
	if _, err := sink.ApplyEvent(baseEvent(LifecycleEventRegistered)); err != nil {
		t.Fatalf("registered error = %v", err)
	}
	regressed := baseEvent(LifecycleEventHeartbeat)
	regressed.Sequence = 2
	regressed.OccurredAt = time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)
	if _, err := sink.ApplyEvent(regressed); !errors.Is(err, ErrEventStreamInvalid) {
		t.Fatalf("regressed heartbeat error = %v, want ErrEventStreamInvalid", err)
	}
}

// TestLifecycleSinkProgressWritesNothing 钉住进度事件只校验不落库。
func TestLifecycleSinkProgressWritesNothing(t *testing.T) {
	sink, store := sinkFixture(t)
	write, err := sink.ApplyEvent(baseEvent(LifecycleEventProgress))
	if err != nil {
		t.Fatalf("ApplyEvent(progress) error = %v", err)
	}
	if write != (SinkWrite{}) {
		t.Fatalf("progress write = %+v, want zero", write)
	}
	if len(store.runtimes) != 0 || len(store.results) != 0 {
		t.Fatalf("progress wrote rows: %+v %+v", store.runtimes, store.results)
	}
}

// TestLifecycleSinkSanitizerThreeStateMapping 是 issue 2.2 的核心断言：
// TerminalResult 脱敏布尔对 → TaskResult.SanitizerStatus 的保守三态映射。
func TestLifecycleSinkSanitizerThreeStateMapping(t *testing.T) {
	sink, store := sinkFixture(t)
	cases := []struct {
		name        string
		sanitized   bool
		skipped     bool
		wantStatus  model.SanitizerStatus
		wantPersist bool
		wantNotes   bool
	}{
		{
			name:        "sanitized maps to applied",
			sanitized:   true,
			wantStatus:  model.SanitizerStatusApplied,
			wantPersist: true,
		},
		{
			name:        "explicit skip maps to skipped with notes",
			skipped:     true,
			wantStatus:  model.SanitizerStatusSkipped,
			wantPersist: true,
			wantNotes:   true,
		},
		{
			name:        "no decision is rejected",
			wantPersist: false,
		}, // 事件先在 LifecycleEvent.Validate（驱动同款闸门）被拒为
		// ErrInvalidContract；归一层自身的 ErrTerminalRejected 对
		// bypass 驱动的输入兜底，两个哨兵都 fail closed。
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(store.results)
			event := baseEvent(LifecycleEventCompleted)
			terminal := validTerminal()
			terminal.Sanitized = tc.sanitized
			terminal.SkippedSanitizer = tc.skipped
			event.Terminal = &terminal
			write, err := sink.ApplyEvent(event)
			if !tc.wantPersist {
				if !errors.Is(err, ErrTerminalRejected) && !errors.Is(err, ErrInvalidContract) {
					t.Fatalf("ApplyEvent() error = %v, want a fail-closed sentinel", err)
				}
				if len(store.results) != before {
					t.Fatalf("rejected terminal persisted a result: %+v", store.results)
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplyEvent() error = %v", err)
			}
			if !write.TaskResult {
				t.Fatalf("write = %+v, want TaskResult", write)
			}
			got := store.results[len(store.results)-1]
			if got.SanitizerStatus != tc.wantStatus {
				t.Fatalf("SanitizerStatus = %q, want %q", got.SanitizerStatus, tc.wantStatus)
			}
			if tc.wantNotes && got.SanitizerNotes == "" {
				t.Fatal("skipped status persisted without notes")
			}
		})
	}
}

// TestLifecycleSinkRejectsConflictingSanitizerFlags 钉住布尔互斥：同时
// 声明已脱敏与跳过即拒绝。
func TestLifecycleSinkRejectsConflictingSanitizerFlags(t *testing.T) {
	sink, store := sinkFixture(t)
	event := baseEvent(LifecycleEventCompleted)
	terminal := validTerminal()
	terminal.Sanitized = true
	terminal.SkippedSanitizer = true
	event.Terminal = &terminal
	if _, err := sink.ApplyEvent(event); !errors.Is(err, ErrTerminalRejected) {
		t.Fatalf("conflicting flags error = %v, want ErrTerminalRejected", err)
	}
	if len(store.results) != 0 {
		t.Fatalf("conflicting flags persisted: %+v", store.results)
	}
}

// TestLifecycleSinkTerminalStateMapping 钉住三类终态事件 → TaskState 与
// 失败分类。
func TestLifecycleSinkTerminalStateMapping(t *testing.T) {
	sink, store := sinkFixture(t)
	cases := []struct {
		eventType  LifecycleEventType
		terminal   TerminalState
		wantStatus model.TaskState
		wantFailed bool
	}{
		{LifecycleEventCompleted, TerminalSucceeded, model.TaskStateSucceeded, false},
		{LifecycleEventCancelled, TerminalCancelled, model.TaskStateCancelled, false},
		{LifecycleEventFailed, TerminalFailed, model.TaskStateFailed, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.eventType), func(t *testing.T) {
			event := baseEvent(tc.eventType)
			terminal := validTerminal()
			terminal.State = tc.terminal
			if tc.terminal == TerminalFailed {
				terminal.FailureReason = "pig worker crashed"
			}
			event.Terminal = &terminal
			if _, err := sink.ApplyEvent(event); err != nil {
				t.Fatalf("ApplyEvent(%s) error = %v", tc.eventType, err)
			}
			got := store.results[len(store.results)-1]
			if got.Status != tc.wantStatus {
				t.Fatalf("Status = %q, want %q", got.Status, tc.wantStatus)
			}
			if tc.wantFailed && got.FailureClass != model.FailureClassWorkerError {
				t.Fatalf("FailureClass = %q, want worker_error", got.FailureClass)
			}
			if got.CompletedBy != "runtime-pig-1" || got.TenantID != "tenant-1" || got.TaskID != "task-1" {
				t.Fatalf("terminal identity = %+v", got)
			}
		})
	}
}

// TestLifecycleSinkRejectsMissingTerminal 钉住缺终态载荷的终态事件被拒。
func TestLifecycleSinkRejectsMissingTerminal(t *testing.T) {
	sink, store := sinkFixture(t)
	if _, err := sink.ApplyEvent(baseEvent(LifecycleEventCompleted)); !errors.Is(err, ErrTerminalRequired) {
		t.Fatalf("missing terminal error = %v, want ErrTerminalRequired", err)
	}
	if len(store.results) != 0 {
		t.Fatalf("missing terminal persisted: %+v", store.results)
	}
}

// TestLifecycleSinkRejectsForeignRuntimeEvent 钉住事件身份必须与 descriptor
// 一致：别的 runtime 的事件不得写进本注册行。
func TestLifecycleSinkRejectsForeignRuntimeEvent(t *testing.T) {
	sink, store := sinkFixture(t)
	foreign := baseEvent(LifecycleEventRegistered)
	foreign.RuntimeID = "runtime-other"
	if _, err := sink.ApplyEvent(foreign); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("foreign runtime error = %v, want ErrInvalidContract", err)
	}
	if len(store.runtimes) != 0 {
		t.Fatalf("foreign runtime wrote rows: %+v", store.runtimes)
	}
}

// TestLifecycleSinkRejectsInvalidEvent 钉住契约校验失败的事件被拒。
func TestLifecycleSinkRejectsInvalidEvent(t *testing.T) {
	sink, _ := sinkFixture(t)
	bad := baseEvent(LifecycleEventRegistered)
	bad.Sequence = 0
	if _, err := sink.ApplyEvent(bad); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("invalid event error = %v, want ErrInvalidContract", err)
	}
}

// TestLifecycleSinkRequiresPiGDescriptor 钉住归一层只接受 pig 描述符。
func TestLifecycleSinkRequiresPiGDescriptor(t *testing.T) {
	descriptor := RuntimeDescriptor{
		RuntimeID:        "runtime-chat-1",
		Kind:             RuntimeKindChat,
		Version:          "1.2.3",
		ReleaseDigest:    testDigest,
		CapabilityDigest: testDigest,
		MaxConcurrency:   2,
		SandboxProfile:   validSandbox(),
	}
	if _, err := NewLifecycleSink(newMemorySinkStore(), descriptor); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("chat descriptor error = %v, want ErrInvalidContract", err)
	}
}

// TestLifecycleSinkSurfacesStoreFailure 钉住底层存储错误不被吞掉。
func TestLifecycleSinkSurfacesStoreFailure(t *testing.T) {
	store := newMemorySinkStore()
	store.getErr = fmt.Errorf("db down")
	sink, err := NewLifecycleSink(store, RuntimeDescriptor{
		RuntimeID:        "runtime-pig-1",
		Kind:             RuntimeKindPiG,
		Version:          "1.2.3",
		ReleaseDigest:    testDigest,
		CapabilityDigest: testDigest,
		MaxConcurrency:   2,
		SandboxProfile:   validSandbox(),
	})
	if err != nil {
		t.Fatalf("NewLifecycleSink() error = %v", err)
	}
	if _, err := sink.ApplyEvent(baseEvent(LifecycleEventRegistered)); err == nil ||
		errors.Is(err, ErrRuntimeRecordMissing) {
		t.Fatalf("store failure error = %v, want propagated db error", err)
	}
}
