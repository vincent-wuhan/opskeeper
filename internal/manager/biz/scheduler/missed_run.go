// Package scheduler 提供调度器增强组件（路径 A 阶段 2 任务 2.11）。
//
// 子模块：
//   - MissedRunDetector：检测 schedule 错过运行（用于 missed_run alert）
//   - NotificationDeduplicator：通知去重（避免告警风暴）
//
// 设计原则：
//   - 与具体 scheduler（report / alert）解耦，通过接口接入
//   - 内存存储（sync.RWMutex + map），生产环境可替换为 Redis
//   - 不引入外部依赖（避免 go.mod 改动）
//
// 关联 build plan：docs/superpowers/plans/2026-07-13-unified-platform-path-a.md
package scheduler

import (
	"fmt"
	"sync"
	"time"
)

// MissedRun 表示一次"错过运行"事件。
type MissedRun struct {
	ScheduleID   string        // 调度 ID
	LastFire     time.Time     // 上次成功触发时间（零值 = 首次运行）
	DetectedAt   time.Time     // 检测时间
	ExpectedGap  time.Duration // 期望间隔
	ActualGap    time.Duration // 实际间隔
	MissedFires  int           // 错过的触发数（估算）
	MissedWindow time.Duration // 累积错过时长
}

// Severity 是 missed run 的严重程度。
type Severity string

const (
	SeverityInfo     Severity = "info"     // 单次错过（抖动）
	SeverityWarning  Severity = "warning"  // 2-3 次错过
	SeverityCritical Severity = "critical" // 3+ 次错过
)

// Severity 返回错过的严重程度（基于 MissedFires）。
func (m *MissedRun) Severity() Severity {
	switch {
	case m.MissedFires >= 3:
		return SeverityCritical
	case m.MissedFires >= 2:
		return SeverityWarning
	default:
		return SeverityInfo
	}
}

// MissedRunDetector 跟踪每个 schedule 的上次触发时间，检测"错过运行"。
//
// 使用方式：
//  1. scheduler 成功 fire → Record(scheduleID, now)
//  2. 任意时刻检查错过 → Detect(scheduleID, now, expectedInterval)
type MissedRunDetector struct {
	mu        sync.RWMutex
	lastFire  map[string]time.Time // schedule_id -> last fire time
	tolerance int                  // 容许的 tick 次数（避免边界抖动）
}

// NewMissedRunDetector 创建检测器。
// tolerance 是允许的"延迟触发"次数：默认 1（一次 tick 没跑视为正常）。
func NewMissedRunDetector(tolerance int) *MissedRunDetector {
	if tolerance <= 0 {
		tolerance = 1
	}
	return &MissedRunDetector{
		lastFire:  make(map[string]time.Time),
		tolerance: tolerance,
	}
}

// Record 记录 schedule 成功触发的时间。
func (d *MissedRunDetector) Record(scheduleID string, fireTime time.Time) {
	if scheduleID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastFire[scheduleID] = fireTime
}

// Forget 移除 schedule 跟踪（删除 schedule 时调用）。
func (d *MissedRunDetector) Forget(scheduleID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.lastFire, scheduleID)
}

// Detect 检测错过运行。
//
//   - scheduleID: 调度 ID
//   - now: 当前时间
//   - expectedInterval: 期望触发间隔（schedule.cron 解析得出）
//
// 返回 nil 表示未错过；否则返回 MissedRun 详情。
func (d *MissedRunDetector) Detect(scheduleID string, now time.Time, expectedInterval time.Duration) *MissedRun {
	if scheduleID == "" || expectedInterval <= 0 {
		return nil
	}
	d.mu.RLock()
	last, ok := d.lastFire[scheduleID]
	d.mu.RUnlock()
	if !ok {
		// 首次运行不视为错过（cold start）
		return nil
	}
	actualGap := now.Sub(last)
	// 容许的正常 gap = expectedInterval * (tolerance + 1)
	maxAcceptable := expectedInterval * time.Duration(d.tolerance+1)
	if actualGap <= maxAcceptable {
		return nil
	}
	missed := actualGap - expectedInterval
	missedFires := int(missed / expectedInterval)
	if missedFires < 1 {
		missedFires = 1
	}
	return &MissedRun{
		ScheduleID:   scheduleID,
		LastFire:     last,
		DetectedAt:   now,
		ExpectedGap:  expectedInterval,
		ActualGap:    actualGap,
		MissedFires:  missedFires,
		MissedWindow: missed,
	}
}

// DetectAll 批量检测所有 tracked schedules。
//
// expectedIntervals 提供每个 schedule 的期望间隔（key = schedule_id）。
// 返回所有错过运行（按 DetectedAt 倒序）。
func (d *MissedRunDetector) DetectAll(now time.Time, expectedIntervals map[string]time.Duration) []*MissedRun {
	d.mu.RLock()
	ids := make([]string, 0, len(d.lastFire))
	for id := range d.lastFire {
		ids = append(ids, id)
	}
	d.mu.RUnlock()

	var out []*MissedRun
	for _, id := range ids {
		interval, ok := expectedIntervals[id]
		if !ok {
			continue
		}
		if m := d.Detect(id, now, interval); m != nil {
			out = append(out, m)
		}
	}
	return out
}

// TrackedCount 返回被跟踪的 schedule 数。
func (d *MissedRunDetector) TrackedCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.lastFire)
}

// --- NotificationDeduplicator ---

// Notification 是待去重通知的最小描述。
type Notification struct {
	Key       string        // 去重 key（如 "alert-123" / "report-456-missed"）
	Subject   string        // 主题
	Body      string        // 正文
	Timestamp time.Time     // 发送时间
	Window    time.Duration // 去重时间窗（默认 5min）
}

// Deduplicator 跟踪每个 key 的最近发送时间，抑制重复通知。
type Deduplicator struct {
	mu            sync.RWMutex
	lastSent      map[string]time.Time // key -> last sent time
	defaultWindow time.Duration
	now           func() time.Time // 时钟注入（测试用）
}

// NewDeduplicator 创建去重器。
// defaultWindow 是默认去重时间窗（key 特定 window 为空时使用）；默认 5min。
func NewDeduplicator(defaultWindow time.Duration) *Deduplicator {
	if defaultWindow <= 0 {
		defaultWindow = 5 * time.Minute
	}
	return &Deduplicator{
		lastSent:      make(map[string]time.Time),
		defaultWindow: defaultWindow,
		now:           time.Now,
	}
}

// WithClock 注入时钟（测试用）。
func (d *Deduplicator) WithClock(now func() time.Time) *Deduplicator {
	d.now = now
	return d
}

// ShouldSend 判断是否应该发送（不修改状态）。
//
// true = 应该发送（首次 / 已过窗口）；false = 应抑制。
func (d *Deduplicator) ShouldSend(n *Notification) bool {
	if n == nil || n.Key == "" {
		return true
	}
	window := n.Window
	if window <= 0 {
		window = d.defaultWindow
	}
	d.mu.RLock()
	last, ok := d.lastSent[n.Key]
	d.mu.RUnlock()
	if !ok {
		return true
	}
	return d.now().Sub(last) >= window
}

// MarkSent 标记已发送（更新 lastSent[key] = now）。
func (d *Deduplicator) MarkSent(n *Notification) {
	if n == nil || n.Key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastSent[n.Key] = d.now()
}

// Send 一站式：ShouldSend + MarkSent（仅在 ShouldSend 为 true 时 MarkSent）。
//
// 返回 true 表示已发送；false 表示被去重抑制。
func (d *Deduplicator) Send(n *Notification) bool {
	if !d.ShouldSend(n) {
		return false
	}
	d.MarkSent(n)
	return true
}

// Reset 移除 key 的去重状态（强制下次发送）。
func (d *Deduplicator) Reset(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.lastSent, key)
}

// ResetAll 清空所有去重状态。
func (d *Deduplicator) ResetAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastSent = make(map[string]time.Time)
}

// TrackedCount 返回被跟踪的 key 数。
func (d *Deduplicator) TrackedCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.lastSent)
}

// Stats 返回去重统计。
type DedupStats struct {
	Tracked    int
	Suppressed uint64 // 累计抑制次数（进程生命周期）
}

// StatsSnapshot 是当前统计快照。
type StatsSnapshot struct {
	Tracked int
}

// Stats returns current dedup stats.
func (d *Deduplicator) Stats() StatsSnapshot {
	return StatsSnapshot{Tracked: d.TrackedCount()}
}

// SanitizeKey 清理 key（防止 key 注入 / 超长）。
//
// 限制：max 128 字符，去除控制字符。
func SanitizeKey(key string) string {
	if len(key) > 128 {
		key = key[:128]
	}
	out := make([]byte, 0, len(key))
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c < 0x20 || c == 0x7F {
			out = append(out, '_')
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}

// String 返回 MissedRun 的可读字符串（用于日志）。
func (m *MissedRun) String() string {
	if m == nil {
		return "<nil>"
	}
	return fmt.Sprintf("missed_run{schedule=%s, last=%s, gap=%s/%s, fires=%d, severity=%s}",
		m.ScheduleID, m.LastFire.Format(time.RFC3339), m.ActualGap, m.ExpectedGap, m.MissedFires, m.Severity())
}
