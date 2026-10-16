package pigruntime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

var (
	// ErrArtifactDrift 表示固定工件摘要与注册发布不一致。
	ErrArtifactDrift = errors.New("pigruntime: pinned artifact digest drift")

	// ErrLaunchFailed 表示隔离进程无法启动。
	ErrLaunchFailed = errors.New("pigruntime: process launch failed")

	// ErrEventStreamInvalid 表示 PiG 输出无法归一为合法生命周期事件。
	ErrEventStreamInvalid = errors.New("pigruntime: process event stream is invalid")

	// ErrConcurrencyExhausted 表示运行时并发额度已耗尽。
	ErrConcurrencyExhausted = errors.New("pigruntime: runtime concurrency exhausted")
)

// ProcessDriver 通过固定可执行文件与 JSONL 事件流实现 AgentRuntimeAdapter。
// 它只负责进程隔离、事件归一、取消与终态兜底；能力授权、凭据、审批和
// 工具分发仍留在 Manager 的治理面。
type ProcessDriver struct {
	config     Config
	descriptor RuntimeDescriptor

	mu      sync.RWMutex
	active  map[string]*processExecution
	closing bool
}

var _ AgentRuntimeAdapter = (*ProcessDriver)(nil)

type processExecution struct {
	taskID        string
	cancel        context.CancelFunc
	command       *exec.Cmd
	events        chan LifecycleEvent
	lastHeartbeat time.Time
	terminal      bool
	cancelled     bool
}

// NewProcessDriver 在启动任何任务前校验配置、描述符和固定工件摘要。
// 构造成功不代表 PiG 获得授权，只代表驱动具备安全启动的基础条件。
func NewProcessDriver(config Config, descriptor RuntimeDescriptor) (*ProcessDriver, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if err := descriptor.Validate(); err != nil {
		return nil, err
	}
	if descriptor.Kind != RuntimeKindPiG {
		return nil, ErrInvalidContract
	}
	digest, err := HashArtifact(config.PinnedArtifact)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrArtifactDrift, err)
	}
	if digest != config.ArtifactDigest || descriptor.ReleaseDigest != config.ArtifactDigest {
		return nil, ErrArtifactDrift
	}
	return &ProcessDriver{
		config:     config,
		descriptor: descriptor,
		active:     make(map[string]*processExecution),
	}, nil
}

// Descriptor 返回固定运行时身份。
func (d *ProcessDriver) Descriptor() RuntimeDescriptor { return d.descriptor }

// Launch 启动一次隔离 PiG 进程并返回归一后的生命周期事件流。
func (d *ProcessDriver) Launch(ctx context.Context, request LaunchRequest) (<-chan LifecycleEvent, error) {
	if err := request.Semantics.Validate(); err != nil {
		return nil, err
	}
	if err := request.Execution.Validate(); err != nil {
		return nil, err
	}
	if request.Execution.RuntimeID != d.descriptor.RuntimeID ||
		request.Execution.PinnedArtifact != d.config.PinnedArtifact ||
		request.Execution.Release.ArtifactDigest != d.config.ArtifactDigest ||
		request.Execution.CapabilityDigest != d.descriptor.CapabilityDigest {
		return nil, ErrArtifactDrift
	}
	if request.Deadline.IsZero() || !request.Deadline.After(time.Now()) ||
		request.MaxEvents <= 0 || request.HeartbeatWindow <= 0 {
		return nil, ErrInvalidContract
	}

	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return nil, ErrLaunchFailed
	}
	if len(d.active) >= d.descriptor.MaxConcurrency {
		d.mu.Unlock()
		return nil, ErrConcurrencyExhausted
	}
	if _, exists := d.active[request.Semantics.TaskID]; exists {
		d.mu.Unlock()
		return nil, ErrInvalidContract
	}
	d.mu.Unlock()

	launchCtx, cancel := context.WithTimeout(ctx, time.Until(request.Deadline))
	args := append([]string(nil), d.config.LaunchArguments...)
	args = append(args,
		"--pig-runtime-id", d.descriptor.RuntimeID,
		"--pig-task-id", request.Semantics.TaskID,
	)
	command := exec.CommandContext(launchCtx, d.config.PinnedArtifact, args...)
	command.Env = d.processEnvironment(request)
	command.Stdout = nil
	command.Stderr = &bytes.Buffer{}

	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %v", ErrLaunchFailed, err)
	}
	if err := command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %v", ErrLaunchFailed, err)
	}

	events := make(chan LifecycleEvent, request.MaxEvents+1)
	execution := &processExecution{
		taskID:  request.Semantics.TaskID,
		cancel:  cancel,
		command: command,
		events:  events,
	}
	d.mu.Lock()
	if d.closing || len(d.active) >= d.descriptor.MaxConcurrency {
		d.mu.Unlock()
		_ = command.Process.Kill()
		cancel()
		_ = command.Wait()
		return nil, ErrLaunchFailed
	}
	d.active[execution.taskID] = execution
	d.mu.Unlock()

	go d.pump(execution, stdout, launchCtx, request)
	return events, nil
}

// Health 返回当前驱动级健康观察；没有活动进程时如实报告 Offline。
func (d *ProcessDriver) Health(context.Context) (HealthReport, error) {
	d.mu.RLock()
	active := len(d.active)
	var oldestHeartbeat time.Time
	for _, execution := range d.active {
		if execution.lastHeartbeat.IsZero() {
			continue
		}
		if oldestHeartbeat.IsZero() || execution.lastHeartbeat.Before(oldestHeartbeat) {
			oldestHeartbeat = execution.lastHeartbeat
		}
	}
	d.mu.RUnlock()

	if active == 0 {
		return HealthReport{Status: HealthOffline, Reason: "no active PiG process"}, nil
	}
	if oldestHeartbeat.IsZero() {
		return HealthReport{Status: HealthDegraded, ActiveExecutions: active, Reason: "registration or heartbeat not observed"}, nil
	}
	window := time.Duration(d.config.HeartbeatWindow) * time.Second
	overdue := time.Since(oldestHeartbeat) > window
	status := HealthHealthy
	reason := ""
	if overdue {
		status = HealthDegraded
		reason = "heartbeat overdue"
	}
	return HealthReport{
		Status:           status,
		LastHeartbeatAt:  oldestHeartbeat,
		ActiveExecutions: active,
		HeartbeatOverdue: overdue,
		Reason:           reason,
	}, nil
}

// Cancel 请求取消指定任务；Force 时直接终止进程，否则先撤销执行上下文。
func (d *ProcessDriver) Cancel(_ context.Context, request CancellationRequest) (CancellationResult, error) {
	if request.RuntimeID != d.descriptor.RuntimeID || request.TaskID == "" {
		return CancellationResult{}, ErrInvalidContract
	}
	d.mu.Lock()
	execution, exists := d.active[request.TaskID]
	if !exists {
		d.mu.Unlock()
		return CancellationResult{State: CancellationAlreadyTerminal, Reason: "execution already terminal"}, nil
	}
	if execution.terminal {
		d.mu.Unlock()
		return CancellationResult{State: CancellationAlreadyTerminal, Reason: "terminal event already received"}, nil
	}
	execution.cancelled = true
	if request.Force {
		_ = execution.command.Process.Kill()
	} else {
		execution.cancel()
	}
	d.mu.Unlock()
	return CancellationResult{State: CancellationTerminalPending, TerminatedProcess: request.Force, Reason: request.Reason}, nil
}

// Close 停止后续启动并撤销所有仍在执行的子进程。
func (d *ProcessDriver) Close() error {
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return nil
	}
	d.closing = true
	for _, execution := range d.active {
		if !execution.terminal {
			execution.cancelled = true
			execution.cancel()
		}
	}
	d.mu.Unlock()
	return nil
}

func (d *ProcessDriver) processEnvironment(request LaunchRequest) []string {
	values := []string{
		"PIG_RUNTIME_ID=" + d.descriptor.RuntimeID,
		"PIG_TENANT_ID=" + request.Semantics.TenantID,
		"PIG_INCIDENT_ID=" + request.Semantics.IncidentID,
		"PIG_TASK_ID=" + request.Semantics.TaskID,
		"PIG_ROLE=" + request.Semantics.Role,
		"PIG_READ_ONLY=true",
	}
	if request.Semantics.ConversationID != "" {
		values = append(values, "PIG_CONVERSATION_ID="+request.Semantics.ConversationID)
	}
	if request.Semantics.TeamID != "" {
		values = append(values, "PIG_TEAM_ID="+request.Semantics.TeamID)
	}
	if request.Semantics.TurnID != "" {
		values = append(values, "PIG_TURN_ID="+request.Semantics.TurnID)
	}
	return values
}

func (d *ProcessDriver) pump(execution *processExecution, stdout io.Reader, launchCtx context.Context, request LaunchRequest) {
	defer func() {
		_ = execution.command.Wait()
		d.mu.Lock()
		delete(d.active, execution.taskID)
		d.mu.Unlock()
		execution.cancel()
		close(execution.events)
	}()

	scanner := bufio.NewScanner(stdout)
	sequence := int64(0)
	for scanner.Scan() {
		var event LifecycleEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			d.emitTerminal(execution, launchCtx, request, LifecycleEventFailed, "invalid JSONL event")
			return
		}
		sequence++
		if event.Sequence <= 0 {
			event.Sequence = sequence
		}
		if event.Sequence < sequence {
			d.emitTerminal(execution, launchCtx, request, LifecycleEventFailed, "event sequence regressed")
			return
		}
		sequence = event.Sequence
		event.RuntimeID = d.descriptor.RuntimeID
		event.TenantID = request.Semantics.TenantID
		event.TaskID = request.Semantics.TaskID
		if event.OccurredAt.IsZero() {
			event.OccurredAt = time.Now().UTC()
		} else {
			event.OccurredAt = event.OccurredAt.UTC()
		}
		if err := event.Validate(); err != nil {
			d.emitTerminal(execution, launchCtx, request, LifecycleEventFailed, "event failed contract validation")
			return
		}
		if event.Type.Terminal() {
			d.mu.Lock()
			execution.terminal = true
			d.mu.Unlock()
		}
		if event.Type == LifecycleEventHeartbeat {
			d.mu.Lock()
			execution.lastHeartbeat = event.OccurredAt
			d.mu.Unlock()
		}
		select {
		case execution.events <- event:
		case <-launchCtx.Done():
			d.emitExitTerminal(execution, launchCtx, request)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		if launchCtx.Err() != nil {
			d.emitExitTerminal(execution, launchCtx, request)
			return
		}
		d.emitTerminal(execution, launchCtx, request, LifecycleEventFailed, "stdout stream failed")
		return
	}
	d.mu.RLock()
	terminal := execution.terminal
	d.mu.RUnlock()
	if !terminal {
		d.emitExitTerminal(execution, launchCtx, request)
	}
}

func (d *ProcessDriver) emitExitTerminal(
	execution *processExecution,
	launchCtx context.Context,
	request LaunchRequest,
) {
	if errors.Is(launchCtx.Err(), context.Canceled) {
		d.emitTerminal(execution, launchCtx, request, LifecycleEventCancelled, "PiG process cancelled")
		return
	}
	if launchCtx.Err() == nil {
		d.emitTerminal(execution, launchCtx, request, LifecycleEventFailed, "PiG process exited without terminal event")
		return
	}
	d.emitTerminal(execution, launchCtx, request, LifecycleEventFailed, "PiG execution deadline exceeded")
}

func (d *ProcessDriver) emitTerminal(
	execution *processExecution,
	launchCtx context.Context,
	request LaunchRequest,
	eventType LifecycleEventType,
	reason string,
) {
	terminal := TerminalResult{
		State:         TerminalFailed,
		FailureReason: reason,
		Sanitized:     true,
		CompletedAt:   time.Now().UTC(),
	}
	if eventType == LifecycleEventCancelled {
		terminal.State = TerminalCancelled
	}
	d.mu.Lock()
	alreadyTerminal := execution.terminal
	execution.terminal = true
	d.mu.Unlock()
	if alreadyTerminal {
		return
	}
	event := LifecycleEvent{
		Sequence:   time.Now().UnixNano(),
		RuntimeID:  d.descriptor.RuntimeID,
		TenantID:   request.Semantics.TenantID,
		TaskID:     request.Semantics.TaskID,
		Type:       eventType,
		OccurredAt: terminal.CompletedAt,
		Terminal:   &terminal,
	}
	select {
	case execution.events <- event:
	default:
	}
}

// HashArtifact 计算固定工件 SHA-256；驱动用它拒绝漂移或可变来源。
func HashArtifact(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
