package runtimemgmt

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestNewIdempotencyContract_OK 覆盖标准变更型场景：契约指纹是
// (payloadDigest | targetSystem | scope) 的 SHA-256；相同输入必须得到相同
// 指纹，字段组合也必须通过 Matches 往返。
func TestNewIdempotencyContract_OK(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	c, err := NewIdempotencyContract(
		"task-1", "tenant-1",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"preview-pg", "scope:build:42",
		"external-1", "producer.v1", now,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if c.TaskID != "task-1" || c.TenantID != "tenant-1" {
		t.Errorf("ids not preserved: %+v", c)
	}
	want := RecomputeFingerprint(c.PayloadDigest, c.TargetSystem, c.Scope)
	if !c.Matches(want) {
		t.Errorf("Matches(%q) = false, want true", want)
	}
	if len(c.Fingerprint) != 64 {
		t.Errorf("Fingerprint len = %d, want 64", len(c.Fingerprint))
	}
	if !strings.HasPrefix(c.IssuedAt.Format(time.RFC3339), "2026-10-16T01:00:00") {
		t.Errorf("IssuedAt = %v, want 2026-10-16T01:00:00Z", c.IssuedAt)
	}
}

// TestNewIdempotencyContract_SameInputSameFingerprint 固定确定性要求：
// 重试必须能干净比较哈希。
func TestNewIdempotencyContract_SameInputSameFingerprint(t *testing.T) {
	a, err := NewIdempotencyContract("t", "tenant", "p", "sys", "scope", "", "p", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewIdempotencyContract("t", "tenant", "p", "sys", "scope", "", "p", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint != b.Fingerprint {
		t.Errorf("fp drift: %s vs %s", a.Fingerprint, b.Fingerprint)
	}
}

// TestNewIdempotencyContract_DiffInputDiffFingerprint 防止范围冲突：
// 载荷摘要相同但目标系统或范围不同，指纹必须不匹配。
func TestNewIdempotencyContract_DiffInputDiffFingerprint(t *testing.T) {
	a, _ := NewIdempotencyContract("t", "tenant", "p", "sys-a", "scope", "", "p", time.Now())
	b, _ := NewIdempotencyContract("t", "tenant", "p", "sys-b", "scope", "", "p", time.Now())
	if a.Fingerprint == b.Fingerprint {
		t.Errorf("different target systems produced same fingerprint: %s", a.Fingerprint)
	}
	c, _ := NewIdempotencyContract("t", "tenant", "p", "sys-a", "scope-x", "", "p", time.Now())
	if a.Fingerprint == c.Fingerprint {
		t.Errorf("different scopes produced same fingerprint: %s", a.Fingerprint)
	}
}

// TestNewIdempotencyContract_RejectsEmpty 覆盖必填字段闸门。
// task_id、tenant_id、载荷摘要、目标系统与范围均必填；空值返回
// ErrIdempotencyRequired，而不是生成空指纹记录。
func TestNewIdempotencyContract_RejectsEmpty(t *testing.T) {
	cases := []struct {
		name, taskID, tenantID, payload, sys, scope string
	}{
		{"empty task", "", "tenant", "p", "sys", "scope"},
		{"empty tenant", "t", "", "p", "sys", "scope"},
		{"empty payload", "t", "tenant", "", "sys", "scope"},
		{"empty system", "t", "tenant", "p", "", "scope"},
		{"empty scope", "t", "tenant", "p", "sys", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewIdempotencyContract(c.taskID, c.tenantID, c.payload, c.sys, c.scope, "", "p", time.Now())
			if !errors.Is(err, ErrIdempotencyRequired) {
				t.Errorf("err = %v, want ErrIdempotencyRequired", err)
			}
		})
	}
}

// TestMatches_Baseline 覆盖相等与空值负例，防止未来重构让 Matches
// 出现巧合相等。
func TestMatches_Baseline(t *testing.T) {
	c := IdempotencyContract{Fingerprint: "abc"}
	if c.Matches("") {
		t.Errorf("Matches(\"\") = true, want false")
	}
	if !c.Matches("abc") {
		t.Errorf("Matches(\"abc\") = false, want true")
	}
	if c.Matches("def") {
		t.Errorf("Matches(\"def\") = true, want false")
	}
}

// TestComputePayloadDigest 固定 Task 记录的标准哈希行为。
func TestComputePayloadDigest(t *testing.T) {
	if got := ComputePayloadDigest(nil); len(got) != 64 {
		t.Errorf("nil digest = %q, want 64-hex chars", got)
	}
	a := ComputePayloadDigest([]byte(`{"x":1}`))
	b := ComputePayloadDigest([]byte(`{"x":2}`))
	if a == b {
		t.Errorf("distinct payloads produced same digest: %s", a)
	}
	if ComputePayloadDigest([]byte(`{"x":1}`)) != a {
		t.Errorf("payload digest is non-deterministic")
	}
}

// TestNewTaskID 是 UUID 生成器冒烟测试；除非空且长度 36 外不固定格式，
// Go uuid 包自身已有完整测试。
func TestNewTaskID(t *testing.T) {
	a := NewTaskID()
	b := NewTaskID()
	if a == "" || b == "" {
		t.Fatalf("NewTaskID returned empty string")
	}
	if a == b {
		t.Errorf("NewTaskID returned duplicates: %s", a)
	}
	if len(a) != 36 || len(b) != 36 {
		t.Errorf("NewTaskID length: %q (%d) / %q (%d), want 36", a, len(a), b, len(b))
	}
}
