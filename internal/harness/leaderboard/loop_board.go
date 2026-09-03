// Package leaderboard — loop_board.go
//
// Day 6+ loop-mode leaderboard。聚合 harness/result/loop/<case>.json
// 中的 LoopResult，输出：
//
//  1. 排名（按 4 指标聚合）
//  2. NOT QUALIFIED 标：recovery_pass_rate < 0.5 的 case 不入榜
//  3. Markdown 报告落 harness/result/leaderboard-<date>.md
//
// 设计要点（与 path A day 7 任务 7.5 / 7.6 对齐）：
//   - 纯函数式（无全局状态），便于测试 + 多 repo 实例并存
//   - input: dir of LoopResult JSON files
//   - output: *Board (in-memory) + Render() string (markdown)
//   - 不写 DB；harness/result/ 目录是 single source of truth
package leaderboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LoopBoard aggregates LoopResult entries into a ranked board.
type LoopBoard struct {
	// Entries: one per case (or per run if multiple runs per case).
	Entries []*LoopBoardEntry
	// GeneratedAt: board build timestamp.
	GeneratedAt time.Time
	// RunDir: where the LoopResult JSONs came from.
	RunDir string
	// RecoveryPassRateThreshold: cases below this score are NOT QUALIFIED.
	RecoveryPassRateThreshold float64
}

// LoopBoardEntry is one row of the leaderboard.
type LoopBoardEntry struct {
	CaseID             string
	Passed             bool
	FinalPhase         string
	RCAAccuracy        *float64
	TimeToRemediate    string
	TimeToRemediateSec int
	ApprovalRate       *float64
	RecoveryPassRate   *float64
	KBHitRate          *float64
	FollowUpDepth      *int
	RubricIncomplete   bool
	Qualified          bool
	NotQualifiedReason string
}

// LoopResultJSON is the on-disk LoopResult shape we read.
// Mirrors runner.LoopResult JSON tags.
type LoopResultJSON struct {
	IncidentID string `json:"incident_id"`
	CaseID     string `json:"case_id"`
	Mode       string `json:"mode"`
	FinalPhase string `json:"final_phase"`
	Passed     bool   `json:"passed"`
	Rubric     struct {
		RCAAccuracy            *float64 `json:"rca_accuracy"`
		RCAAccuracyReason      string   `json:"rca_accuracy_reason"`
		TimeToRemediate        string   `json:"time_to_remediate"`
		TimeToRemediateSec     *int     `json:"time_to_remediate_sec"`
		TimeToRemediateReason  string   `json:"time_to_remediate_reason"`
		ApprovalRate           *float64 `json:"approval_rate"`
		ApprovalRateReason     string   `json:"approval_rate_reason"`
		RecoveryPassRate       *float64 `json:"recovery_pass_rate"`
		RecoveryPassRateReason string   `json:"recovery_pass_rate_reason"`
		KBHitRate              *float64 `json:"kb_hit_rate"`
		KBHitRateReason        string   `json:"kb_hit_rate_reason"`
		FollowUpDepth          *int     `json:"follow_up_depth"`
		FollowUpDepthReason    string   `json:"follow_up_depth_reason"`
		RubricIncomplete       bool     `json:"rubric_incomplete"`
	} `json:"rubric"`
}

// NewLoopBoard builds a board from LoopResult JSONs in dir.
func NewLoopBoard(dir string) (*LoopBoard, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("leaderboard: glob %s: %w", dir, err)
	}
	b := &LoopBoard{
		Entries:                   []*LoopBoardEntry{},
		GeneratedAt:               time.Now().UTC(),
		RunDir:                    dir,
		RecoveryPassRateThreshold: 0.5,
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("leaderboard: read %s: %w", f, err)
		}
		var lr LoopResultJSON
		if err := json.Unmarshal(raw, &lr); err != nil {
			return nil, fmt.Errorf("leaderboard: parse %s: %w", f, err)
		}
		b.Entries = append(b.Entries, loopResultToEntry(&lr, b.RecoveryPassRateThreshold))
	}
	// Stable sort: passed first, then by rca_accuracy desc.
	sort.SliceStable(b.Entries, func(i, j int) bool {
		if b.Entries[i].Passed != b.Entries[j].Passed {
			return b.Entries[i].Passed
		}
		return floatPtr(b.Entries[i].RCAAccuracy) > floatPtr(b.Entries[j].RCAAccuracy)
	})
	return b, nil
}

func loopResultToEntry(lr *LoopResultJSON, recoveryThreshold float64) *LoopBoardEntry {
	e := &LoopBoardEntry{
		CaseID:             lr.CaseID,
		Passed:             lr.Passed,
		FinalPhase:         lr.FinalPhase,
		RCAAccuracy:        lr.Rubric.RCAAccuracy,
		TimeToRemediate:    lr.Rubric.TimeToRemediate,
		TimeToRemediateSec: derefIntOr(lr.Rubric.TimeToRemediateSec, 0),
		ApprovalRate:       lr.Rubric.ApprovalRate,
		RecoveryPassRate:   lr.Rubric.RecoveryPassRate,
		KBHitRate:          lr.Rubric.KBHitRate,
		FollowUpDepth:      lr.Rubric.FollowUpDepth,
		RubricIncomplete:   lr.Rubric.RubricIncomplete,
	}
	// Qualification rule (spec loop-harness-rubric §"门槛不通过不入榜"):
	// recovery_pass_rate < threshold OR rubric_incomplete → NOT QUALIFIED.
	if e.RecoveryPassRate != nil && *e.RecoveryPassRate < recoveryThreshold {
		e.Qualified = false
		e.NotQualifiedReason = fmt.Sprintf("recovery_pass_rate %.3f < threshold %.3f", *e.RecoveryPassRate, recoveryThreshold)
	} else if e.RubricIncomplete {
		e.Qualified = false
		e.NotQualifiedReason = "rubric incomplete (4 metrics not all populated)"
	} else {
		e.Qualified = true
	}
	return e
}

func derefIntOr(p *int, dflt int) int {
	if p == nil {
		return dflt
	}
	return *p
}

func floatPtr(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// Render produces a Markdown report (spec loop-harness-rubric §"排行榜聚合").
func (b *LoopBoard) Render() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Harness Loop Leaderboard\n\n")
	fmt.Fprintf(&sb, "_Generated at %s from %s_\n\n", b.GeneratedAt.Format(time.RFC3339), b.RunDir)

	qualified := 0
	for _, e := range b.Entries {
		if e.Qualified {
			qualified++
		}
	}
	fmt.Fprintf(&sb, "**Total cases**: %d · **Qualified**: %d · **NOT QUALIFIED**: %d\n\n",
		len(b.Entries), qualified, len(b.Entries)-qualified)

	// Header
	sb.WriteString("| Rank | Case ID | Passed | rca_accuracy | time_to_remediate | approval_rate | recovery_pass_rate | kb_hit_rate | follow_up_depth | Status |\n")
	sb.WriteString("|------|---------|--------|--------------|-------------------|---------------|--------------------|-------------|------------------|--------|\n")
	rank := 0
	for _, e := range b.Entries {
		rank++
		status := "✅ QUALIFIED"
		if !e.Qualified {
			status = fmt.Sprintf("❌ NOT QUALIFIED (%s)", e.NotQualifiedReason)
		}
		fmt.Fprintf(&sb, "| %d | %s | %t | %s | %s | %s | %s | %s | %s | %s |\n",
			rank,
			e.CaseID,
			e.Passed,
			formatFloatPtr(e.RCAAccuracy),
			e.TimeToRemediate,
			formatFloatPtr(e.ApprovalRate),
			formatFloatPtrWithReason(e.RecoveryPassRate, thresholdMarker(*e.RecoveryPassRate, b.RecoveryPassRateThreshold)),
			formatFloatPtr(e.KBHitRate),
			formatIntPtr(e.FollowUpDepth),
			status,
		)
	}

	// Section: NOT QUALIFIED reasons
	hasNQ := false
	for _, e := range b.Entries {
		if !e.Qualified {
			if !hasNQ {
				sb.WriteString("\n## NOT QUALIFIED details\n\n")
				hasNQ = true
			}
			fmt.Fprintf(&sb, "- **%s**: %s\n", e.CaseID, e.NotQualifiedReason)
		}
	}

	// Section: chat-mode fields summary (for chat-mode boards).
	hasChat := false
	for _, e := range b.Entries {
		if e.KBHitRate != nil || e.FollowUpDepth != nil {
			hasChat = true
			break
		}
	}
	if hasChat {
		sb.WriteString("\n## Chat-mode rubric fields\n\n")
		sb.WriteString("- `kb_hit_rate`: KB 命中次数 / 调查阶段总查询次数 ([0,1], 0 次查询时为 null)\n")
		sb.WriteString("- `follow_up_depth`: 用户在同一 `diagnostic_conversation` 内追问轮数（首问不计）\n")
	}
	return sb.String()
}

// RenderToFile writes the Markdown report to path.
func (b *LoopBoard) RenderToFile(path string) error {
	return os.WriteFile(path, []byte(b.Render()), 0o644)
}

func formatFloatPtr(p *float64) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.3f", *p)
}

func formatFloatPtrWithReason(p *float64, marker string) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.3f %s", *p, marker)
}

func formatIntPtr(p *int) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%d", *p)
}

func thresholdMarker(v, threshold float64) string {
	if v < threshold {
		return "⚠️"
	}
	return ""
}
