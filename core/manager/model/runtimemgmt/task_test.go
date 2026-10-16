package runtimemgmt

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestNewTask_OK 固定生产者侧标准构造：必填标识与初始 queued 状态写入，
// 时间取 now 的 UTC 值，载荷摘要等于 ComputePayloadDigest。
func TestNewTask_OK(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	payload := []byte(`{"k":"v"}`)
	task, err := NewTask(
		"tenant-1", "incident-1", "read_only.diagnostic", string(payload), payload,
		TaskSafetyReadOnly, "kind=edge", "trace-1", 5, now,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if task.State != TaskStateQueued {
		t.Errorf("State = %s, want %s", task.State, TaskStateQueued)
	}
	if task.PayloadDigest != ComputePayloadDigest(payload) {
		t.Errorf("PayloadDigest = %s, want canonical", task.PayloadDigest)
	}
	if task.TaskID == "" {
		t.Errorf("TaskID is empty")
	}
	if !task.CreatedAt.Equal(now) || !task.UpdatedAt.Equal(now) {
		t.Errorf("timestamps not stamped from now: created=%v updated=%v",
			task.CreatedAt, task.UpdatedAt)
	}
}

// TestNewTask_ZeroNowDefaultsToCurrent 固定便捷路径：传入 time.Time{}
// 时回退到 time.Now().UTC()，调用方不必始终携带时钟。
func TestNewTask_ZeroNowDefaultsToCurrent(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	task, err := NewTask(
		"t", "i", "k", "", nil,
		TaskSafetyReadOnly, "", "", 0, time.Time{},
	)
	after := time.Now().UTC().Add(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if task.CreatedAt.Before(before) || task.CreatedAt.After(after) {
		t.Errorf("CreatedAt = %v, want within [%v, %v]", task.CreatedAt, before, after)
	}
}

// TestNewTask_RejectsEmpty 覆盖生产者路径守卫：缺少租户或类型时，
// 必须在构造记录前失败。
func TestNewTask_RejectsEmpty(t *testing.T) {
	cases := []struct {
		name   string
		tenant string
		kind   string
		safety TaskSafety
	}{
		{"empty tenant", "", "k", TaskSafetyReadOnly},
		{"empty kind", "t", "", TaskSafetyReadOnly},
		{"bad safety", "t", "k", TaskSafety("harmful")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewTask(c.tenant, "i", c.kind, "", nil, c.safety, "", "", 0, time.Time{})
			if err == nil {
				t.Errorf("expected err")
			}
		})
	}
}

// TestTask_CanTransition_Allowed 覆盖允许的正常迁移路径。
func TestTask_CanTransition_Allowed(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		from TaskState
		to   TaskState
	}{
		{TaskStateQueued, TaskStateAdmitted},
		{TaskStateAdmitted, TaskStateClaimed},
		{TaskStateClaimed, TaskStatePreparing},
		{TaskStatePreparing, TaskStateRunning},
		{TaskStateRunning, TaskStateSucceeded},
		{TaskStateRunning, TaskStateFailed},
		{TaskStateRunning, TaskStateCancelled},
		{TaskStateClaimed, TaskStateCancelled},
	}
	for _, c := range cases {
		task := Task{State: c.from}
		if err := task.CanTransition(c.to); err != nil {
			t.Errorf("CanTransition(%s -> %s) = %v", c.from, c.to, err)
		}
		upd, err := task.WithState(c.to, now)
		if err != nil {
			t.Errorf("WithState(%s -> %s) = %v", c.from, c.to, err)
			continue
		}
		if upd.State != c.to {
			t.Errorf("WithState.State = %s, want %s", upd.State, c.to)
		}
		if !upd.UpdatedAt.Equal(now) {
			t.Errorf("WithState.UpdatedAt = %v, want %v", upd.UpdatedAt, now)
		}
	}
}

// TestTask_CanTransition_Forbidden 固定显式拒绝路径：被拒绝迁移必须返回
// Err* 哨兵，调用方可使用 errors.Is 分支。
func TestTask_CanTransition_Forbidden(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		from TaskState
		to   TaskState
		want error
	}{
		{TaskStateQueued, TaskStateClaimed, ErrInvalidTransition},   // skip admitted
		{TaskStateQueued, TaskStateRunning, ErrInvalidTransition},   // skip too many
		{TaskStateRunning, TaskStateAdmitted, ErrInvalidTransition}, // back-edge
		{TaskStateSucceeded, TaskStateFailed, ErrTerminalState},
		{TaskStateFailed, TaskStateRunning, ErrTerminalState},
		{TaskStateCancelled, TaskStateAdmitted, ErrTerminalState},
	}
	for _, c := range cases {
		task := Task{State: c.from}
		err := task.CanTransition(c.to)
		if !errors.Is(err, c.want) {
			t.Errorf("CanTransition(%s -> %s) = %v, want %v", c.from, c.to, err, c.want)
		}
		_, err = task.WithState(c.to, now)
		if !errors.Is(err, c.want) {
			t.Errorf("WithState(%s -> %s) = %v, want %v", c.from, c.to, err, c.want)
		}
	}
}

// TestTask_JSON_RoundTrip 固定持久化形状契约：所有导出字段必须经 JSON
// 编解码保持不变，让 API、Dashboard 与修复预览运行器无需专用转换器互通。
func TestTask_JSON_RoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	in := Task{
		TaskID:                 "t-1",
		TenantID:               "tenant-1",
		IncidentID:             "incident-7",
		Kind:                   "read_only.diagnostic",
		Safety:                 TaskSafetyReadOnly,
		State:                  TaskStateAdmitted,
		PayloadDigest:          "deadbeef",
		DesiredRuntimeSelector: "kind=edge tier=ha",
		RuntimeID:              "runtime:edge:abc",
		WorkspaceID:            "ws-42",
		Priority:               7,
		TraceID:                "trace-9",
		CreatedAt:              now,
		UpdatedAt:              now.Add(time.Minute),
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Task
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.TaskID != in.TaskID ||
		out.TenantID != in.TenantID ||
		out.IncidentID != in.IncidentID ||
		out.Kind != in.Kind ||
		out.Safety != in.Safety ||
		out.State != in.State ||
		out.PayloadDigest != in.PayloadDigest ||
		out.DesiredRuntimeSelector != in.DesiredRuntimeSelector ||
		out.RuntimeID != in.RuntimeID ||
		out.WorkspaceID != in.WorkspaceID ||
		out.Priority != in.Priority ||
		out.TraceID != in.TraceID {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
	if !out.CreatedAt.Equal(in.CreatedAt) || !out.UpdatedAt.Equal(in.UpdatedAt) {
		t.Errorf("timestamp drift: got created=%v updated=%v, want created=%v updated=%v",
			out.CreatedAt, out.UpdatedAt, in.CreatedAt, in.UpdatedAt)
	}
}

// TestTask_GORM_TableName 是持久化命名冒烟测试；未同步编排器读取路径的
// 表名变更会造成静默缺陷，本守卫负责拦截。
func TestTask_GORM_TableName(t *testing.T) {
	if got := (Task{}).TableName(); got != "runtimemgmt_tasks" {
		t.Errorf("Task.TableName() = %q, want runtimemgmt_tasks", got)
	}
	if got := (IdempotencyContract{}).TableName(); got != "runtimemgmt_idempotency_contracts" {
		t.Errorf("IdempotencyContract.TableName() = %q, want runtimemgmt_idempotency_contracts", got)
	}
	if got := (AdmissionDecision{}).TableName(); got != "runtimemgmt_admission_decisions" {
		t.Errorf("AdmissionDecision.TableName() = %q, want runtimemgmt_admission_decisions", got)
	}
	if got := (Claim{}).TableName(); got != "runtimemgmt_claims" {
		t.Errorf("Claim.TableName() = %q, want runtimemgmt_claims", got)
	}
	if got := (Lease{}).TableName(); got != "runtimemgmt_leases" {
		t.Errorf("Lease.TableName() = %q, want runtimemgmt_leases", got)
	}
	if got := (Checkpoint{}).TableName(); got != "runtimemgmt_checkpoints" {
		t.Errorf("Checkpoint.TableName() = %q, want runtimemgmt_checkpoints", got)
	}
	if got := (AttemptRecord{}).TableName(); got != "runtimemgmt_attempt_records" {
		t.Errorf("AttemptRecord.TableName() = %q, want runtimemgmt_attempt_records", got)
	}
	if got := (TaskResult{}).TableName(); got != "runtimemgmt_task_results" {
		t.Errorf("TaskResult.TableName() = %q, want runtimemgmt_task_results", got)
	}
}
