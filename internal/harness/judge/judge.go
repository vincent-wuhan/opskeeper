// Package judge 实现 Harness 评分模型。
//
// 路径 A 阶段 2 任务 2.7 — Harness judge（双模型取均值）。
//
// 设计要点：
//   - Judge 接口：Score(ctx, *Case, *AgentResponse) (*Score, error)
//   - 2 个实现：LLMJudge（双模型取均值，采纳 ADR-001）/ HeuristicJudge（规则打分，无 LLM 依赖）
//   - Score 包含 Overall + 4 个维度分 + Flagged + Reason
//   - 一致性检测：双模型差异 > 0.2 时 Flagged=true，进入 flagged_leaderboard 等待人工复评
//   - 缓存：同 (case_id, response_hash) 复用结果（followup PR）
//
// 关联 Design Doc：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.2
// 关联 ADR：docs/superpowers/decisions/2026-07-13-harness-judge-models.md
// 关联 spec：openspec/changes/unified-platform-base-selection/specs/harness-eval-platform/spec.md
package judge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"
)

// Score 是 judge 输出的评分结果。
type Score struct {
	Overall    float64            `json:"overall"`
	Dimensions map[string]float64 `json:"dimensions"` // rca_accuracy / time_efficiency / remediation_quality / collateral_safety
	Flagged    bool               `json:"flagged"`
	FlagReason string             `json:"flag_reason,omitempty"`
	Reasoning  string             `json:"reasoning,omitempty"`
	JudgesUsed []string           `json:"judges_used"` // ["claude-sonnet-4", "gpt-4o"]
	ComputedAt time.Time          `json:"computed_at"`
}

// IsValid 校验 Score 字段合法性（用于 judge 输出 sanity check）。
func (s *Score) IsValid() error {
	if math.IsNaN(s.Overall) || math.IsNaN(s.Overall) {
		return fmt.Errorf("overall is NaN")
	}
	if s.Overall < 0 || s.Overall > 1 {
		return fmt.Errorf("overall %f out of [0,1]", s.Overall)
	}
	for k, v := range s.Dimensions {
		if v < 0 || v > 1 {
			return fmt.Errorf("dimension %s = %f out of [0,1]", k, v)
		}
	}
	return nil
}

// AgentResponse 是 Agent 对 case 的响应（runner 产出）。
//
// 与 runner.RunResult 对齐；此处独立定义以避免 import 循环。
type AgentResponse struct {
	ToolCalls    []ToolCall `json:"tool_calls"`
	RootCause    []string   `json:"root_cause_matched"`
	Remediations []string   `json:"remediations_matched"`
	DetectMs     int64      `json:"detect_ms"`
	RemediateMs  int64      `json:"remediate_ms"`
	Errors       []string   `json:"errors,omitempty"`
	ResponseHash string     `json:"response_hash,omitempty"` // hash(tool_calls + root_cause + remediations)
}

// ToolCall is a single tool invocation by the agent.
type ToolCall struct {
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// Case 是 judge 需要的 case 字段（runner/schema.Case 的子集）。
type Case struct {
	ID                   string
	ExpectedRootCause    []string
	ExpectedRemediations []string
	ExpectedDetectSec    int
	ExpectedRemediateSec int
	RCAThreshold         float64
	NoCollateralDamage   bool
}

// ComputeResponseHash 计算 AgentResponse 哈希（缓存 key 的一部分）。
func ComputeResponseHash(r *AgentResponse) string {
	h := sha256.New()
	for _, t := range r.ToolCalls {
		h.Write([]byte(t.Name))
		h.Write(t.Args)
	}
	for _, rc := range r.RootCause {
		h.Write([]byte(rc))
	}
	for _, rm := range r.Remediations {
		h.Write([]byte(rm))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Judge 是评分模型接口。
type Judge interface {
	// Name 返回 judge 标识（如 "llm-duo"、"heuristic-v1"）。
	Name() string
	// Score 评分；返回的 Score.Overall ∈ [0,1]。
	Score(ctx context.Context, c *Case, r *AgentResponse) (*Score, error)
}

// Cache 缓存 (case_id, response_hash) → Score 的命中结果。
type Cache struct {
	mu     sync.RWMutex
	scores map[string]*Score
}

// NewCache 创建缓存。
func NewCache() *Cache {
	return &Cache{scores: make(map[string]*Score)}
}

func cacheKey(caseID, respHash string) string {
	return caseID + ":" + respHash
}

// Get 读取缓存（命中返回 *Score, true；未命中返回 nil, false）。
func (c *Cache) Get(caseID, respHash string) (*Score, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.scores[cacheKey(caseID, respHash)]
	return s, ok
}

// Put 写入缓存。
func (c *Cache) Put(caseID, respHash string, s *Score) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scores[cacheKey(caseID, respHash)] = s
}

// Size 返回缓存条目数。
func (c *Cache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.scores)
}

// MeanScore 计算两个 Score 的均值（ADR-001 双模型策略）。
//
// 若任一为 nil，返回另一个。
// 差异 > ConsistencyThreshold 时 flag 为 true。
func MeanScore(a, b *Score) *Score {
	if a == nil && b == nil {
		return nil
	}
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	merged := &Score{
		Overall:    (a.Overall + b.Overall) / 2,
		Dimensions: mergeDims(a.Dimensions, b.Dimensions),
		ComputedAt: time.Now(),
		JudgesUsed: append(append([]string{}, a.JudgesUsed...), b.JudgesUsed...),
	}
	// Reason 取第一个非空
	if a.Reasoning != "" {
		merged.Reasoning = a.Reasoning
	} else {
		merged.Reasoning = b.Reasoning
	}
	if math.Abs(a.Overall-b.Overall) > ConsistencyThreshold {
		merged.Flagged = true
		merged.FlagReason = fmt.Sprintf("双模型评分差异 %.2f > 阈值 %.2f，需人工复评",
			math.Abs(a.Overall-b.Overall), ConsistencyThreshold)
	}
	return merged
}

// ConsistencyThreshold 是双模型评分差异触发 flagged 的阈值（ADR-001）。
const ConsistencyThreshold = 0.2

func mergeDims(a, b map[string]float64) map[string]float64 {
	if a == nil && b == nil {
		return map[string]float64{}
	}
	out := make(map[string]float64)
	for k, va := range a {
		if vb, ok := b[k]; ok {
			out[k] = (va + vb) / 2
		} else {
			out[k] = va
		}
	}
	for k, vb := range b {
		if _, ok := out[k]; !ok {
			out[k] = vb
		}
	}
	return out
}
