package scheduler

import (
	"sync"
	"testing"
	"time"
)

// --- MissedRunDetector ---

func TestMissedRunDetector_RecordForget(t *testing.T) {
	d := NewMissedRunDetector(1)
	d.Record("s1", time.Now())
	if d.TrackedCount() != 1 {
		t.Errorf("TrackedCount = %d, want 1", d.TrackedCount())
	}
	d.Forget("s1")
	if d.TrackedCount() != 0 {
		t.Errorf("after Forget: TrackedCount = %d, want 0", d.TrackedCount())
	}
}

func TestMissedRunDetector_Record_EmptyID_Ignored(t *testing.T) {
	d := NewMissedRunDetector(1)
	d.Record("", time.Now())
	if d.TrackedCount() != 0 {
		t.Errorf("empty id should be ignored")
	}
}

func TestMissedRunDetector_Detect_NoMiss(t *testing.T) {
	d := NewMissedRunDetector(1)
	now := time.Now()
	d.Record("s1", now)
	// 1 分钟前刚跑过，期望 5min 间隔 → 不算 miss
	m := d.Detect("s1", now.Add(time.Minute), 5*time.Minute)
	if m != nil {
		t.Errorf("expected no miss, got %+v", m)
	}
}

func TestMissedRunDetector_Detect_Miss(t *testing.T) {
	d := NewMissedRunDetector(1) // 容许 1 tick
	now := time.Now()
	d.Record("s1", now)
	// 期望 5min 间隔，已过 20min（远超 2*5=10min 容许）→ miss
	m := d.Detect("s1", now.Add(20*time.Minute), 5*time.Minute)
	if m == nil {
		t.Fatal("expected miss")
	}
	if m.MissedFires < 3 {
		t.Errorf("MissedFires = %d, want >= 3", m.MissedFires)
	}
	if m.Severity() != SeverityCritical {
		t.Errorf("Severity = %q, want critical (>=3 fires)", m.Severity())
	}
}

func TestMissedRunDetector_Detect_FirstRun_NotMiss(t *testing.T) {
	d := NewMissedRunDetector(1)
	// 未 Record 过的 schedule 视为 cold start，不算 miss
	m := d.Detect("s1", time.Now(), 5*time.Minute)
	if m != nil {
		t.Errorf("first run should not be miss, got %+v", m)
	}
}

func TestMissedRunDetector_Detect_EmptyArgs(t *testing.T) {
	d := NewMissedRunDetector(1)
	d.Record("s1", time.Now())
	if d.Detect("", time.Now(), time.Minute) != nil {
		t.Error("empty id should return nil")
	}
	if d.Detect("s1", time.Now(), 0) != nil {
		t.Error("zero interval should return nil")
	}
}

func TestMissedRunDetector_DetectAll(t *testing.T) {
	d := NewMissedRunDetector(1)
	now := time.Now()
	d.Record("s1", now)
	d.Record("s2", now)
	d.Record("s3", now) // 期望 1min 间隔
	intervals := map[string]time.Duration{
		"s1": 5 * time.Minute,  // 期望 5min；2min 后查 → ok
		"s2": 1 * time.Minute,  // 期望 1min；2min 后查 → miss
		"s3": 10 * time.Minute, // 期望 10min；2min 后查 → ok
		"s4": 1 * time.Minute,  // 未 tracked，cold start
	}
	future := now.Add(3 * time.Minute)
	misses := d.DetectAll(future, intervals)
	if len(misses) != 1 {
		t.Fatalf("len = %d, want 1 (only s2)", len(misses))
	}
	if misses[0].ScheduleID != "s2" {
		t.Errorf("ScheduleID = %q, want s2", misses[0].ScheduleID)
	}
}

func TestMissedRun_Severity(t *testing.T) {
	tests := []struct {
		fires    int
		expected Severity
	}{
		{0, SeverityInfo},
		{1, SeverityInfo},
		{2, SeverityWarning},
		{3, SeverityCritical},
		{10, SeverityCritical},
	}
	for _, tc := range tests {
		m := &MissedRun{MissedFires: tc.fires}
		if m.Severity() != tc.expected {
			t.Errorf("fires=%d: Severity=%q, want %q", tc.fires, m.Severity(), tc.expected)
		}
	}
}

func TestMissedRun_String(t *testing.T) {
	m := &MissedRun{
		ScheduleID:  "s1",
		LastFire:    time.Unix(100, 0).UTC(),
		ActualGap:   20 * time.Minute,
		ExpectedGap: 5 * time.Minute,
		MissedFires: 3,
	}
	got := m.String()
	if got == "" || got == "<nil>" {
		t.Errorf("String should produce non-empty output, got %q", got)
	}
	if m == nil {
		t.Errorf("String on nil should be safe")
	}
	var nilMr *MissedRun
	if nilMr.String() != "<nil>" {
		t.Error("nil MissedRun should stringify to <nil>")
	}
}

func TestMissedRunDetector_DefaultTolerance(t *testing.T) {
	d := NewMissedRunDetector(0) // 0 → 默认 1
	if d.tolerance != 1 {
		t.Errorf("tolerance = %d, want 1 (default for <= 0)", d.tolerance)
	}
	d = NewMissedRunDetector(-5)
	if d.tolerance != 1 {
		t.Errorf("tolerance = %d, want 1 (default for < 0)", d.tolerance)
	}
}

func TestMissedRunDetector_Concurrent(t *testing.T) {
	d := NewMissedRunDetector(1)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			d.Record("s", time.Now())
		}(i)
		go func() {
			defer wg.Done()
			_ = d.Detect("s", time.Now(), time.Minute)
		}()
	}
	wg.Wait()
}

// --- Deduplicator ---

func TestDeduplicator_ShouldSend_FirstTime(t *testing.T) {
	d := NewDeduplicator(5 * time.Minute)
	n := &Notification{Key: "k1", Subject: "x"}
	if !d.ShouldSend(n) {
		t.Error("first time should send")
	}
}

func TestDeduplicator_Send_SuppressesWithinWindow(t *testing.T) {
	d := NewDeduplicator(5 * time.Minute)
	now := time.Unix(100, 0)
	d.WithClock(func() time.Time { return now })
	n := &Notification{Key: "k1", Subject: "x"}
	if !d.Send(n) {
		t.Error("first send should succeed")
	}
	// 1 分钟后
	d.WithClock(func() time.Time { return now.Add(time.Minute) })
	if d.Send(n) {
		t.Error("within window: should be suppressed")
	}
	// 6 分钟后（过 5min 窗口）
	d.WithClock(func() time.Time { return now.Add(6 * time.Minute) })
	if !d.Send(n) {
		t.Error("after window: should send")
	}
}

func TestDeduplicator_PerKeyWindow(t *testing.T) {
	d := NewDeduplicator(5 * time.Minute)
	now := time.Unix(100, 0)
	d.WithClock(func() time.Time { return now })
	d.Send(&Notification{Key: "short", Subject: "x", Window: time.Minute})
	d.Send(&Notification{Key: "long", Subject: "x", Window: time.Hour})
	// 2 分钟后
	d.WithClock(func() time.Time { return now.Add(2 * time.Minute) })
	if !d.Send(&Notification{Key: "short", Subject: "x", Window: time.Minute}) {
		t.Error("short window (1min) should be re-sendable after 2min")
	}
	if d.Send(&Notification{Key: "long", Subject: "x", Window: time.Hour}) {
		t.Error("long window (1h) should still suppress at 2min")
	}
}

func TestDeduplicator_ShouldSend_NilSafe(t *testing.T) {
	d := NewDeduplicator(time.Minute)
	if !d.ShouldSend(nil) {
		t.Error("nil notification should send (fail open)")
	}
	if !d.ShouldSend(&Notification{Key: ""}) {
		t.Error("empty key should send")
	}
	d.MarkSent(nil)
	d.MarkSent(&Notification{Key: ""})
}

func TestDeduplicator_Reset(t *testing.T) {
	d := NewDeduplicator(time.Minute)
	now := time.Unix(100, 0)
	d.WithClock(func() time.Time { return now })
	d.Send(&Notification{Key: "k1"})
	d.Reset("k1")
	if !d.Send(&Notification{Key: "k1"}) {
		t.Error("after Reset: should send")
	}
}

func TestDeduplicator_ResetAll(t *testing.T) {
	d := NewDeduplicator(time.Minute)
	d.Send(&Notification{Key: "a"})
	d.Send(&Notification{Key: "b"})
	if d.TrackedCount() != 2 {
		t.Errorf("TrackedCount = %d", d.TrackedCount())
	}
	d.ResetAll()
	if d.TrackedCount() != 0 {
		t.Errorf("after ResetAll: TrackedCount = %d", d.TrackedCount())
	}
}

func TestDeduplicator_DefaultWindow(t *testing.T) {
	d := NewDeduplicator(0) // 0 → 5min
	if d.defaultWindow != 5*time.Minute {
		t.Errorf("defaultWindow = %v, want 5min", d.defaultWindow)
	}
	d = NewDeduplicator(-time.Hour)
	if d.defaultWindow != 5*time.Minute {
		t.Errorf("defaultWindow = %v, want 5min", d.defaultWindow)
	}
}

func TestDeduplicator_Stats(t *testing.T) {
	d := NewDeduplicator(time.Minute)
	d.Send(&Notification{Key: "a"})
	d.Send(&Notification{Key: "b"})
	d.Send(&Notification{Key: "a"}) // suppressed
	stats := d.Stats()
	if stats.Tracked != 2 {
		t.Errorf("Tracked = %d, want 2", stats.Tracked)
	}
}

func TestSanitizeKey(t *testing.T) {
	tests := []struct {
		in, out string
	}{
		{"simple", "simple"},
		{"with\x00null", "with_null"},
		{"with\nnewline", "with_newline"},
		{"with\x7Fdel", "with_del"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := SanitizeKey(tc.in); got != tc.out {
			t.Errorf("SanitizeKey(%q) = %q, want %q", tc.in, got, tc.out)
		}
	}
}

func TestSanitizeKey_LongString(t *testing.T) {
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'a'
	}
	got := SanitizeKey(string(long))
	if len(got) != 128 {
		t.Errorf("len = %d, want 128 (truncated)", len(got))
	}
}
