package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
)

// LLMJudge 是真实 LLM 评分 Judge（路径 A / llm-worker-integration，Design Doc §7）。
//
// 设计要点：
//   - 只对 rca_accuracy 一个维度走 LLM 评分（Design Doc §7.2 / Open Q1 答案）
//   - 其他 3 维度（time_to_remediate / approval_rate / recovery_pass_rate）由调用方公式计算，
//     本 judge 不输出
//   - LLM 调用失败（timeout / API 5xx / schema-invalid / 越界）→ fallback 到
//     HeuristicJudge，JudgesUsed 标记为 "llm-fallback-heuristic"
//   - 复用 internal/pkg/llm.Client 抽象层；不直接 import eino
//   - 单一 ScoreInput 形态：Judge 接口规定的 (Case, AgentResponse)
type LLMJudge struct {
	llmClient llm.Client
	fallback  Judge
	log       *slog.Logger
}

// NewLLMJudge 创建 LLMJudge。fallback 不能为 nil（用于 LLM 不可用时降级）；
// log 允许为 nil（默认 slog.Default()）。
func NewLLMJudge(llmClient llm.Client, fallback Judge, log *slog.Logger) *LLMJudge {
	if log == nil {
		log = slog.Default()
	}
	if fallback == nil {
		// 安全网：fallback 为 nil 时用 HeuristicJudge，避免 nil pointer
		fallback = NewHeuristicJudge()
	}
	return &LLMJudge{
		llmClient: llmClient,
		fallback:  fallback,
		log:       log,
	}
}

// Name 返回 judge 标识。
func (j *LLMJudge) Name() string { return "llm-judge" }

// Score 评分主入口。LLM 失败时自动 fallback 到 HeuristicJudge。
func (j *LLMJudge) Score(ctx context.Context, c *Case, r *AgentResponse) (*Score, error) {
	if c == nil {
		return nil, fmt.Errorf("case required")
	}
	if r == nil {
		return nil, fmt.Errorf("response required")
	}
	if j.llmClient == nil {
		j.log.WarnContext(ctx, "llm judge: llmClient is nil, fallback to heuristic")
		return j.fallbackAndMark(ctx, c, r, "llmClient is nil")
	}

	prompt := buildLLMJudgePrompt(c, r)
	resp, err := j.llmClient.Chat(ctx, llm.ChatReq{
		Messages: []llm.Message{
			{Role: "system", Content: llmJudgeSystemPrompt},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		j.log.WarnContext(ctx, "llm judge: chat call failed, fallback to heuristic",
			slog.String("error", err.Error()))
		return j.fallbackAndMark(ctx, c, r, "llm chat failed: "+err.Error())
	}
	if resp == nil {
		j.log.WarnContext(ctx, "llm judge: nil response, fallback to heuristic")
		return j.fallbackAndMark(ctx, c, r, "llm chat returned nil response")
	}

	acc, perr := parseLLMJudgeAccuracy(resp.Assistant.Content)
	if perr != nil {
		j.log.WarnContext(ctx, "llm judge: parse failed, fallback to heuristic",
			slog.String("error", perr.Error()))
		return j.fallbackAndMark(ctx, c, r, "schema-invalid: "+perr.Error())
	}

	return &Score{
		Overall: acc,
		Dimensions: map[string]float64{
			"rca_accuracy": acc,
		},
		JudgesUsed: []string{"llm"},
		Reasoning:  fmt.Sprintf("LLM judge scored rca_accuracy=%.3f", acc),
		ComputedAt: time.Now(),
	}, nil
}

// fallbackAndMark 调用 fallback judge 并把 JudgesUsed 改成 "llm-fallback-heuristic"。
//
// 调用方拿到的 Score 结构与 fallback 自己产出的等价，仅 JudgesUsed 替换；
// 失败原因写入 FlagReason 以便 leaderboard 统计失败率。
func (j *LLMJudge) fallbackAndMark(ctx context.Context, c *Case, r *AgentResponse, reason string) (*Score, error) {
	if j.fallback == nil {
		return nil, fmt.Errorf("fallback judge is nil and llm judge failed: %s", reason)
	}
	score, err := j.fallback.Score(ctx, c, r)
	if err != nil {
		return nil, fmt.Errorf("llm judge failed (%s) and fallback also failed: %w", reason, err)
	}
	score.JudgesUsed = []string{"llm-fallback-heuristic"}
	if score.Dimensions == nil {
		score.Dimensions = map[string]float64{}
	}
	// fallback 路径必须 flag，便于 leaderboard 统计 LLM 失败率
	score.Flagged = true
	if _, ok := score.Dimensions["rca_accuracy"]; !ok {
		score.Dimensions["rca_accuracy"] = 0
	}
	if existing := score.FlagReason; existing != "" {
		score.FlagReason = existing + "; " + reason
	} else {
		score.FlagReason = reason
	}
	return score, nil
}

// llmJudgeSystemPrompt 是 judge 的 system prompt（Design Doc §7.3）。
const llmJudgeSystemPrompt = "You are an SRE postmortem reviewer. Compare the actual root cause to the " +
	"expected root cause and output a JSON object with a single field \"rca_accuracy\" " +
	"in [0, 1]. 0 = completely unrelated, 1 = identical. Only output the JSON, no other text."

// buildLLMJudgePrompt 拼接 user prompt（Design Doc §7.3 模板）。
//
// 占位符映射：
//   - {expected_root_cause}             ← c.ExpectedRootCause
//   - {actual_root_cause_object}        ← r.RootCause（agent 报告的根因集）
//   - {actual_root_cause_detail}        ← r.ToolCalls（agent 实际触发的工具调用）
//   - {actual_evidence_chain}           ← r.ToolCalls name 列表 + r.Errors
func buildLLMJudgePrompt(c *Case, r *AgentResponse) string {
	var b strings.Builder
	b.WriteString("Expected root cause:\n")
	if len(c.ExpectedRootCause) == 0 {
		b.WriteString("  (none specified)\n")
	} else {
		for _, rc := range c.ExpectedRootCause {
			fmt.Fprintf(&b, "  - %s\n", rc)
		}
	}
	b.WriteString("\nActual root cause (from RootCauseJSON):\n")
	if len(r.RootCause) == 0 {
		b.WriteString("  (agent did not identify any root cause)\n")
	} else {
		for _, rc := range r.RootCause {
			fmt.Fprintf(&b, "  - %s\n", rc)
		}
	}
	b.WriteString("\nActual root cause detail (agent tool calls):\n")
	if len(r.ToolCalls) == 0 {
		b.WriteString("  (no tool calls recorded)\n")
	} else {
		for _, tc := range r.ToolCalls {
			fmt.Fprintf(&b, "  - %s\n", tc.Name)
		}
	}
	b.WriteString("\nEvidence chain:\n")
	if len(r.ToolCalls) == 0 && len(r.Errors) == 0 {
		b.WriteString("  (no evidence recorded)\n")
	} else {
		for _, tc := range r.ToolCalls {
			fmt.Fprintf(&b, "  - call: %s\n", tc.Name)
		}
		for _, e := range r.Errors {
			fmt.Fprintf(&b, "  - error: %s\n", e)
		}
	}
	b.WriteString("\nOutput JSON:\n")
	b.WriteString("{\"rca_accuracy\": <float in [0, 1]>}\n")
	return b.String()
}

// parseLLMJudgeAccuracy 从 LLM 输出提取 rca_accuracy ∈ [0,1]。
//
// 容错策略：
//   - 剥 ```json / ``` markdown fence
//   - 找首尾 { } 截取 JSON 片段
//   - 缺 rca_accuracy 字段 / 越界 / NaN → 返回 error（触发 fallback）
func parseLLMJudgeAccuracy(content string) (float64, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return 0, errors.New("empty llm response")
	}
	// 剥 markdown fence
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return 0, fmt.Errorf("no JSON object found in llm response: %q", truncate(content, 200))
	}
	content = content[start : end+1]

	var raw struct {
		RCAAccuracy *float64 `json:"rca_accuracy"`
	}
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return 0, fmt.Errorf("json unmarshal failed: %w (content=%q)", err, truncate(content, 200))
	}
	if raw.RCAAccuracy == nil {
		return 0, errors.New("missing required field rca_accuracy")
	}
	v := *raw.RCAAccuracy
	if v < 0 || v > 1 {
		return 0, fmt.Errorf("rca_accuracy %f out of [0,1]", v)
	}
	return v, nil
}

// truncate 把字符串截到 n 字节（用于错误日志，避免超长内容爆炸）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
