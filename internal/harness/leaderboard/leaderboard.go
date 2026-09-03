// Package leaderboard 实现 Harness 评分追踪与回归检测。
//
// 路径 A 阶段 2 任务 2.8 — Harness leaderboard + 回归基线。
//
// 核心概念：
//   - Entry: 单次 case 评分记录（case_id / score / branch / run_id / timestamp）
//   - Baseline: 已确立的基线评分（来自最近一次"good" run）
//   - Regression: 当前评分相对基线的下降幅度
//
// 回归检测（与 build plan 验收对齐）：
//   - 下降 ≥ 5%   → 告警（warn）
//   - 下降 ≥ 15%  → 阻断（block / release gate failure）
//
// 完整实现在 followup PR（DB 持久化 + REST API + 跨 run 趋势分析）。
//
// 关联 Design Doc：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.2
// 关联 spec：openspec/changes/unified-platform-base-selection/specs/harness-eval-platform/spec.md
package leaderboard

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// Entry 是单条评分记录。
type Entry struct {
	RunID      string    // 运行 ID（一次 run 的唯一标识）
	CaseID     string    // case 唯一 ID
	Branch     string    // 代码分支（main / PR-xxx）
	Score      float64   // 评分（0-1）
	Flagged    bool      // 是否被 judge 标记为需要复评
	FlagReason string    // 标记原因
	JudgesUsed []string  // 实际调用的 judge 模型
	Timestamp  time.Time // 评分时间
}

// Regression 是回归检测结果。
type Regression struct {
	CaseID        string
	BaselineScore float64
	CurrentScore  float64
	DropPercent   float64  // 下降百分比（0-100）
	Severity      Severity // warn / block
	Message       string
	DetectedAt    time.Time
}

// Severity 是回归严重等级。
type Severity string

const (
	SeverityNone  Severity = "none"  // 无回归
	SeverityWarn  Severity = "warn"  // 5% ≤ drop < 15%：告警
	SeverityBlock Severity = "block" // drop ≥ 15%：阻断
)

// 阈值常量（与 build plan 验收对齐）
const (
	WarnThreshold  = 0.05 // 5% 下降告警
	BlockThreshold = 0.15 // 15% 下降阻断
)

// Leaderboard 是评分追踪与回归检测中心。
type Leaderboard struct {
	mu sync.RWMutex
	// baseline[case_id] -> baseline_score（最近一次 "good" run 的分数）
	baseline map[string]float64
	// history[case_id] -> []*Entry（按时间倒序）
	history map[string][]*Entry
	// lastEntry[case_id] -> *Entry（最近一次评分）
	lastEntry map[string]*Entry
}

// NewLeaderboard 创建 Leaderboard。
func NewLeaderboard() *Leaderboard {
	return &Leaderboard{
		baseline:  make(map[string]float64),
		history:   make(map[string][]*Entry),
		lastEntry: make(map[string]*Entry),
	}
}

// Record 记录一条评分。
//
// 若 entry.Score >= baseline[caseID]（无回归），自动提升为新基线。
func (lb *Leaderboard) Record(entry *Entry) error {
	if entry == nil {
		return fmt.Errorf("entry required")
	}
	if entry.CaseID == "" {
		return fmt.Errorf("entry.CaseID required")
	}
	if math.IsNaN(entry.Score) || entry.Score < 0 || entry.Score > 1 {
		return fmt.Errorf("entry.Score %f out of [0,1]", entry.Score)
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.history[entry.CaseID] = append(lb.history[entry.CaseID], entry)
	lb.lastEntry[entry.CaseID] = entry
	// 自动提升基线：当前分数 >= 旧基线时替换
	if cur, ok := lb.baseline[entry.CaseID]; !ok || entry.Score >= cur {
		lb.baseline[entry.CaseID] = entry.Score
	}
	return nil
}

// SetBaseline 显式设置基线（用于人工 rollback / golden set 重置）。
func (lb *Leaderboard) SetBaseline(caseID string, score float64) error {
	if score < 0 || score > 1 {
		return fmt.Errorf("score %f out of [0,1]", score)
	}
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.baseline[caseID] = score
	return nil
}

// Baseline 返回指定 case 的基线（不存在返回 (0, false)）。
func (lb *Leaderboard) Baseline(caseID string) (float64, bool) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	s, ok := lb.baseline[caseID]
	return s, ok
}

// CheckRegression 检测当前分数相对基线的回归。
//
// score < baseline 时计算 drop%；>= 阈值触发对应 Severity。
func (lb *Leaderboard) CheckRegression(caseID string, currentScore float64) *Regression {
	lb.mu.RLock()
	base, ok := lb.baseline[caseID]
	lb.mu.RUnlock()
	if !ok {
		return &Regression{
			CaseID:       caseID,
			CurrentScore: currentScore,
			Severity:     SeverityNone,
			Message:      "no baseline established",
			DetectedAt:   time.Now(),
		}
	}
	if currentScore >= base {
		return &Regression{
			CaseID:        caseID,
			BaselineScore: base,
			CurrentScore:  currentScore,
			Severity:      SeverityNone,
			Message:       fmt.Sprintf("no regression (current %.3f >= baseline %.3f)", currentScore, base),
			DetectedAt:    time.Now(),
		}
	}
	dropPct := (base - currentScore) / base
	severity := SeverityNone
	switch {
	case dropPct >= BlockThreshold:
		severity = SeverityBlock
	case dropPct >= WarnThreshold:
		severity = SeverityWarn
	}
	return &Regression{
		CaseID:        caseID,
		BaselineScore: base,
		CurrentScore:  currentScore,
		DropPercent:   dropPct * 100,
		Severity:      severity,
		Message: fmt.Sprintf("%.1f%% drop (baseline %.3f → current %.3f)",
			dropPct*100, base, currentScore),
		DetectedAt: time.Now(),
	}
}

// History 返回指定 case 的历史评分（按时间正序）。
func (lb *Leaderboard) History(caseID string) []*Entry {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	src := lb.history[caseID]
	out := make([]*Entry, len(src))
	copy(out, src)
	return out
}

// LastEntry 返回指定 case 的最近一次评分（不存在返回 nil, false）。
func (lb *Leaderboard) LastEntry(caseID string) (*Entry, bool) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	e, ok := lb.lastEntry[caseID]
	return e, ok
}

// FlaggedEntries 返回所有 flag=true 的 entries（按时间倒序）。
func (lb *Leaderboard) FlaggedEntries() []*Entry {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	var out []*Entry
	for _, h := range lb.history {
		for _, e := range h {
			if e.Flagged {
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	return out
}

// Blockers 返回当前所有 Severity=Block 的回归（用于 release gate）。
func (lb *Leaderboard) Blockers() []*Regression {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	var out []*Regression
	for caseID, last := range lb.lastEntry {
		r := lb.computeRegressionLocked(caseID, last.Score)
		if r.Severity == SeverityBlock {
			out = append(out, r)
		}
	}
	return out
}

// Warns 返回当前所有 Severity=Warn 的回归。
func (lb *Leaderboard) Warns() []*Regression {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	var out []*Regression
	for caseID, last := range lb.lastEntry {
		r := lb.computeRegressionLocked(caseID, last.Score)
		if r.Severity == SeverityWarn {
			out = append(out, r)
		}
	}
	return out
}

func (lb *Leaderboard) computeRegressionLocked(caseID string, currentScore float64) *Regression {
	base, ok := lb.baseline[caseID]
	if !ok || currentScore >= base {
		return &Regression{CaseID: caseID, Severity: SeverityNone}
	}
	dropPct := (base - currentScore) / base
	severity := SeverityNone
	switch {
	case dropPct >= BlockThreshold:
		severity = SeverityBlock
	case dropPct >= WarnThreshold:
		severity = SeverityWarn
	}
	return &Regression{
		CaseID:        caseID,
		BaselineScore: base,
		CurrentScore:  currentScore,
		DropPercent:   dropPct * 100,
		Severity:      severity,
	}
}

// Size 返回追踪的 case 数。
func (lb *Leaderboard) Size() int {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	return len(lb.lastEntry)
}
