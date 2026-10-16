package runtimemgmt

import (
	"errors"
	"testing"
	"time"
)

// TestNewPrepareDeadline 覆盖准备窗口构造。TTL 为零或负数必须得到零值，
// 让只读任务可以显式跳过准备窗口。
func TestNewPrepareDeadline(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	if got := NewPrepareDeadline(now, 0); !got.IsZero() {
		t.Errorf("ttl=0 -> %v, want zero", got)
	}
	if got := NewPrepareDeadline(now, -time.Second); !got.IsZero() {
		t.Errorf("ttl<0 -> %v, want zero", got)
	}
	if got := NewPrepareDeadline(time.Time{}, time.Minute); !got.IsZero() {
		t.Errorf("zero claimTime -> %v, want zero", got)
	}
	got := NewPrepareDeadline(now, 30*time.Second)
	want := now.Add(30 * time.Second)
	if !got.Equal(want) {
		t.Errorf("ttl=30s -> %v, want %v", got, want)
	}
}

func TestNewLivenessDeadline(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	if got := NewLivenessDeadline(now, 0); !got.IsZero() {
		t.Errorf("ttl=0 -> %v, want zero", got)
	}
	if got := NewLivenessDeadline(now, -time.Second); !got.IsZero() {
		t.Errorf("ttl<0 -> %v, want zero", got)
	}
	got := NewLivenessDeadline(now, time.Minute)
	if !got.Equal(now.Add(time.Minute)) {
		t.Errorf("ttl=1m -> %v, want %v", got, now.Add(time.Minute))
	}
}

// TestLease_HasPrepare_Liveness 覆盖期限尚未分配的语义。租约记录在认领时
// 已存在，但期限只有在运行时进入阶段后才有意义；零值表示不可操作，
// 防止调用方误使未开始租约过期。
func TestLease_HasPrepare_Liveness(t *testing.T) {
	l := Lease{}
	if l.HasPrepare() {
		t.Errorf("zero PrepareDeadline should report not set")
	}
	if l.HasLiveness() {
		t.Errorf("zero LivenessDeadline should report not set")
	}
	now := time.Now().UTC()
	l.PrepareDeadline = now.Add(time.Minute)
	l.LivenessDeadline = now.Add(time.Minute)
	if !l.HasPrepare() || !l.HasLiveness() {
		t.Errorf("non-zero deadlines should be reported as set")
	}
}

// TestLease_Expired 覆盖过期检测。零期限不得报告过期；调用方必须先用
// HasPrepare / HasLiveness 区分状态。
func TestLease_Expired(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	l := Lease{}
	if l.PrepareExpired(now) {
		t.Errorf("zero PrepareDeadline should NOT report expired")
	}
	if l.LivenessExpired(now) {
		t.Errorf("zero LivenessDeadline should NOT report expired")
	}
	l.PrepareDeadline = now.Add(-time.Minute)
	if !l.PrepareExpired(now) {
		t.Errorf("past PrepareDeadline should report expired")
	}
	l2 := Lease{LivenessDeadline: now.Add(time.Minute)}
	if l2.LivenessExpired(now) {
		t.Errorf("future LivenessDeadline should NOT report expired")
	}
}

// TestLease_ExtendLiveness 覆盖滚动租约正常路径：期限按 ttl 前移、
// Extensions 递增并设置 ExtendedAt。
func TestLease_ExtendLiveness(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	l := Lease{LivenessDeadline: now.Add(time.Minute)}
	adv, err := l.ExtendLiveness(now, 2*time.Minute)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !adv.LivenessDeadline.Equal(now.Add(2 * time.Minute)) {
		t.Errorf("LivenessDeadline = %v, want %v", adv.LivenessDeadline, now.Add(2*time.Minute))
	}
	if adv.Extensions != 1 {
		t.Errorf("Extensions = %d, want 1", adv.Extensions)
	}
	if adv.ExtendedAt.IsZero() {
		t.Errorf("ExtendedAt = zero, want %v", now)
	}
}

// TestLease_ExtendLiveness_RejectExpired 覆盖安全分支：扩展已过期租约
// 必须返回 ErrLeaseExpired，而不是静默复活记录。
func TestLease_ExtendLiveness_RejectExpired(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	l := Lease{LivenessDeadline: now.Add(-time.Minute)}
	if _, err := l.ExtendLiveness(now, time.Minute); !errors.Is(err, ErrLeaseExpired) {
		t.Errorf("expired extend err = %v, want ErrLeaseExpired", err)
	}
}

// TestLease_ExtendLiveness_RejectInvalid 覆盖 ttl <= 0 与零期限拒绝路径。
func TestLease_ExtendLiveness_RejectInvalid(t *testing.T) {
	now := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	l := Lease{LivenessDeadline: now.Add(time.Minute)}
	if _, err := l.ExtendLiveness(now, 0); !errors.Is(err, ErrLeaseExpired) {
		t.Errorf("ttl=0 err = %v, want ErrLeaseExpired", err)
	}
	if _, err := l.ExtendLiveness(now, -time.Second); !errors.Is(err, ErrLeaseExpired) {
		t.Errorf("ttl<0 err = %v, want ErrLeaseExpired", err)
	}
	zero := Lease{}
	if _, err := zero.ExtendLiveness(now, time.Minute); !errors.Is(err, ErrLeaseExpired) {
		t.Errorf("zero deadline err = %v, want ErrLeaseExpired", err)
	}
}
