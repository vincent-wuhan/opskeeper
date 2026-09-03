package judge

import (
	"context"
	"fmt"
	"math"
	"time"
)

// HeuristicJudge 是规则打分 Judge（无 LLM 依赖，骨架默认实现）。
//
// 评分维度：
//   - rca_accuracy:        匹配 case.ExpectedRootCause 的比例
//   - time_efficiency:     detect_ms / (expected_detect_sec * 1000) 比值
//   - remediation_quality: 匹配 case.ExpectedRemediations 的比例
//   - collateral_safety:   若 case.NoCollateralDamage && 有 errors → 0；否则 1.0
//   - overall:             加权均值（与 judgeSystemPrompt 一致：rca 0.4, time 0.2, remediation 0.3, safety 0.1）
//
// 用于：
//   - CI gate（不依赖外部 LLM，可快速验证骨架）
//   - LLM 不可用时的 degraded 兜底
//   - 黄金评分集回归基线对比
type HeuristicJudge struct{}

// NewHeuristicJudge 创建 HeuristicJudge。
func NewHeuristicJudge() *HeuristicJudge { return &HeuristicJudge{} }

// Name 返回 judge 标识。
func (j *HeuristicJudge) Name() string { return "heuristic-v1" }

// Score 规则打分。
func (j *HeuristicJudge) Score(ctx context.Context, c *Case, r *AgentResponse) (*Score, error) {
	if c == nil {
		return nil, fmt.Errorf("case required")
	}
	if r == nil {
		return nil, fmt.Errorf("response required")
	}
	rca := matchRatio(r.RootCause, c.ExpectedRootCause)
	rem := matchRatio(r.Remediations, c.ExpectedRemediations)
	tEff := timeEfficiency(r.DetectMs, c.ExpectedDetectSec)
	safety := 1.0
	if c.NoCollateralDamage && len(r.Errors) > 0 {
		safety = 0.0
	}
	overall := clamp(0.4*rca + 0.2*tEff + 0.3*rem + 0.1*safety)
	return &Score{
		Overall: overall,
		Dimensions: map[string]float64{
			"rca_accuracy":        rca,
			"time_efficiency":     tEff,
			"remediation_quality": rem,
			"collateral_safety":   safety,
		},
		JudgesUsed: []string{"heuristic-v1"},
		ComputedAt: time.Now(),
	}, nil
}

// matchRatio 计算 actual 中命中 expected 的比例（双向匹配）。
func matchRatio(actual, expected []string) float64 {
	if len(expected) == 0 {
		if len(actual) == 0 {
			return 1.0
		}
		return 0.5 // case 未要求但 agent 给出了部分 root cause：半对
	}
	hit := 0
	for _, e := range expected {
		for _, a := range actual {
			if e == a {
				hit++
				break
			}
		}
	}
	return float64(hit) / float64(len(expected))
}

// timeEfficiency 基于 expected 时间的比值打分。
// 1.0 if faster or equal, linear decay to 0 at 3x expected.
func timeEfficiency(actualMs int64, expectedSec int) float64 {
	if expectedSec <= 0 {
		return 1.0
	}
	actualSec := float64(actualMs) / 1000.0
	if actualSec <= float64(expectedSec) {
		return 1.0
	}
	ratio := actualSec / float64(expectedSec)
	if ratio >= 3.0 {
		return 0.0
	}
	return 1.0 - (ratio-1.0)/2.0
}

func clamp(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
