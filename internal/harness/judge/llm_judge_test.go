package judge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
)

// fakeLLMClient 是测试用 llm.Client stub。
//
//   - responses: 按调用顺序返回的 assistant content
//   - errOnCall: 指定调用序号的错误（覆盖 responses）
//   - recorded: 收集实际收到的 ChatReq（断言 prompt 内容）
type fakeLLMClient struct {
	responses []string
	calls     int
	errOnCall map[int]error
	recorded  []llm.ChatReq
}

func (f *fakeLLMClient) Chat(_ context.Context, req llm.ChatReq) (*llm.ChatResp, error) {
	idx := f.calls
	f.calls++
	f.recorded = append(f.recorded, req)
	if err, ok := f.errOnCall[idx]; ok {
		return nil, err
	}
	if idx >= len(f.responses) {
		return nil, errors.New("fake: no more responses")
	}
	return &llm.ChatResp{
		Assistant: llm.Message{Role: "assistant", Content: f.responses[idx]},
	}, nil
}

func TestLLMJudge_Name(t *testing.T) {
	j := NewLLMJudge(&fakeLLMClient{}, NewHeuristicJudge(), nil)
	if j.Name() != "llm-judge" {
		t.Errorf("Name = %q, want llm-judge", j.Name())
	}
}

func TestLLMJudge_NilCase(t *testing.T) {
	j := NewLLMJudge(&fakeLLMClient{}, NewHeuristicJudge(), nil)
	if _, err := j.Score(context.Background(), nil, &AgentResponse{}); err == nil {
		t.Errorf("expected error on nil case")
	}
}

func TestLLMJudge_NilResponse(t *testing.T) {
	j := NewLLMJudge(&fakeLLMClient{}, NewHeuristicJudge(), nil)
	if _, err := j.Score(context.Background(), &Case{}, nil); err == nil {
		t.Errorf("expected error on nil response")
	}
}

// --- Path 1: LLM 成功 -----------------------------------------------------

func TestLLMJudge_Score_LLMSuccess(t *testing.T) {
	fake := &fakeLLMClient{responses: []string{`{"rca_accuracy": 0.85}`}}
	j := NewLLMJudge(fake, NewHeuristicJudge(), nil)

	c := &Case{
		ID:                "pg/long-running-tx",
		ExpectedRootCause: []string{"pg.lock_waits"},
	}
	r := &AgentResponse{
		RootCause: []string{"pg.lock_waits"},
		ToolCalls: []ToolCall{{Name: "pg.lock_waits"}},
	}

	score, err := j.Score(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.Dimensions["rca_accuracy"] != 0.85 {
		t.Errorf("rca_accuracy = %f, want 0.85", score.Dimensions["rca_accuracy"])
	}
	if score.Overall != 0.85 {
		t.Errorf("Overall = %f, want 0.85 (single dim)", score.Overall)
	}
	if len(score.JudgesUsed) != 1 || score.JudgesUsed[0] != "llm" {
		t.Errorf("JudgesUsed = %v, want [\"llm\"]", score.JudgesUsed)
	}
	if score.Flagged {
		t.Errorf("success path should not flag; FlagReason=%q", score.FlagReason)
	}
	if score.ComputedAt.IsZero() {
		t.Errorf("ComputedAt should be set")
	}
}

// 验证 prompt 渲染包含 expected / actual / evidence（Design Doc §7.3 模板）。
func TestLLMJudge_Score_LLMSuccess_PromptContent(t *testing.T) {
	fake := &fakeLLMClient{responses: []string{`{"rca_accuracy": 0.9}`}}
	j := NewLLMJudge(fake, NewHeuristicJudge(), nil)

	c := &Case{
		ID:                "pg/long-running-tx",
		ExpectedRootCause: []string{"pg.lock_waits"},
	}
	r := &AgentResponse{
		RootCause: []string{"pg.lock_waits"},
		ToolCalls: []ToolCall{{Name: "pg.lock_waits"}, {Name: "pg.active_sessions"}},
		Errors:    []string{"timeout reading"},
	}

	if _, err := j.Score(context.Background(), c, r); err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(fake.recorded) != 1 {
		t.Fatalf("expected 1 Chat call, got %d", len(fake.recorded))
	}
	req := fake.recorded[0]
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Errorf("messages = %+v, want [system, user]", req.Messages)
	}
	prompt := req.Messages[1].Content
	for _, want := range []string{
		"Expected root cause",
		"pg.lock_waits",
		"Actual root cause",
		"Evidence chain",
		"call: pg.lock_waits",
		"error: timeout reading",
		`"rca_accuracy"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, prompt)
		}
	}
}

// 验证 markdown fence 容错：```json ... ``` 能正确解析。
func TestLLMJudge_Score_LLMSuccess_StripsMarkdownFence(t *testing.T) {
	fake := &fakeLLMClient{
		responses: []string{"```json\n{\"rca_accuracy\": 0.7}\n```"},
	}
	j := NewLLMJudge(fake, NewHeuristicJudge(), nil)

	c := &Case{ID: "test", ExpectedRootCause: []string{"x"}}
	r := &AgentResponse{RootCause: []string{"x"}}

	score, err := j.Score(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.Dimensions["rca_accuracy"] != 0.7 {
		t.Errorf("rca_accuracy = %f, want 0.7", score.Dimensions["rca_accuracy"])
	}
}

// --- Path 2: LLM 失败 fallback --------------------------------------------

func TestLLMJudge_Score_LLMTimeoutFallback(t *testing.T) {
	fake := &fakeLLMClient{
		errOnCall: map[int]error{0: errors.New("context deadline exceeded")},
	}
	heuristic := NewHeuristicJudge()
	j := NewLLMJudge(fake, heuristic, nil)

	c := &Case{
		ID:                "pg/long-running-tx",
		ExpectedRootCause: []string{"pg.lock_waits"},
	}
	r := &AgentResponse{
		RootCause:    []string{"pg.lock_waits"},
		Remediations: []string{"pg.kill_session"},
	}

	score, err := j.Score(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(score.JudgesUsed) != 1 || score.JudgesUsed[0] != "llm-fallback-heuristic" {
		t.Errorf("JudgesUsed = %v, want [\"llm-fallback-heuristic\"]", score.JudgesUsed)
	}
	// fallback 应当产出 HeuristicJudge 的完整维度集
	if _, ok := score.Dimensions["rca_accuracy"]; !ok {
		t.Errorf("fallback score should carry rca_accuracy (synthesized 0 by fallbackAndMark)")
	}
	if _, ok := score.Dimensions["remediation_quality"]; !ok {
		t.Errorf("fallback score should carry heuristic dimensions; got %v", score.Dimensions)
	}
	if !score.Flagged {
		t.Errorf("fallback path should be flagged")
	}
	if !strings.Contains(score.FlagReason, "context deadline exceeded") {
		t.Errorf("FlagReason = %q, want to contain timeout cause", score.FlagReason)
	}
}

// 验证 5xx 类错误（网络层）也触发 fallback。
func TestLLMJudge_Score_LLM5xxFallback(t *testing.T) {
	fake := &fakeLLMClient{
		errOnCall: map[int]error{0: errors.New("upstream returned 503")},
	}
	j := NewLLMJudge(fake, NewHeuristicJudge(), nil)

	c := &Case{ID: "test", ExpectedRootCause: []string{"a"}}
	r := &AgentResponse{RootCause: []string{"a"}}

	score, err := j.Score(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.JudgesUsed[0] != "llm-fallback-heuristic" {
		t.Errorf("JudgesUsed = %v, want fallback marker", score.JudgesUsed)
	}
}

// --- Path 3: LLM schema-invalid fallback ----------------------------------

func TestLLMJudge_Score_LLMSchemaInvalidFallback(t *testing.T) {
	fake := &fakeLLMClient{
		responses: []string{`{"score": 0.5}`}, // 缺 rca_accuracy 字段
	}
	j := NewLLMJudge(fake, NewHeuristicJudge(), nil)

	c := &Case{ID: "test", ExpectedRootCause: []string{"a"}}
	r := &AgentResponse{RootCause: []string{"a"}}

	score, err := j.Score(context.Background(), c, r)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(score.JudgesUsed) != 1 || score.JudgesUsed[0] != "llm-fallback-heuristic" {
		t.Errorf("JudgesUsed = %v, want [\"llm-fallback-heuristic\"]", score.JudgesUsed)
	}
	if !score.Flagged {
		t.Errorf("schema-invalid fallback should flag")
	}
	if !strings.Contains(score.FlagReason, "schema-invalid") &&
		!strings.Contains(score.FlagReason, "rca_accuracy") {
		t.Errorf("FlagReason = %q, want to mention schema/rca_accuracy", score.FlagReason)
	}
}

// 越界值也走 fallback（与 schema-invalid 同路径）。
func TestLLMJudge_Score_LLMOutOfRangeFallback(t *testing.T) {
	fake := &fakeLLMClient{
		responses: []string{`{"rca_accuracy": 1.5}`}, // 越界
	}
	j := NewLLMJudge(fake, NewHeuristicJudge(), nil)

	score, err := j.Score(context.Background(),
		&Case{ID: "t"}, &AgentResponse{})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.JudgesUsed[0] != "llm-fallback-heuristic" {
		t.Errorf("JudgesUsed = %v, want fallback", score.JudgesUsed)
	}
}

// 空响应也走 fallback。
func TestLLMJudge_Score_LLMEmptyResponseFallback(t *testing.T) {
	fake := &fakeLLMClient{responses: []string{""}}
	j := NewLLMJudge(fake, NewHeuristicJudge(), nil)

	score, err := j.Score(context.Background(),
		&Case{ID: "t"}, &AgentResponse{})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.JudgesUsed[0] != "llm-fallback-heuristic" {
		t.Errorf("JudgesUsed = %v, want fallback", score.JudgesUsed)
	}
}

// --- Edge cases -----------------------------------------------------------

// llmClient 为 nil 时直接走 fallback。
func TestLLMJudge_Score_NilLLMClientFallback(t *testing.T) {
	j := NewLLMJudge(nil, NewHeuristicJudge(), nil)

	score, err := j.Score(context.Background(),
		&Case{ID: "t", ExpectedRootCause: []string{"x"}},
		&AgentResponse{RootCause: []string{"x"}})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.JudgesUsed[0] != "llm-fallback-heuristic" {
		t.Errorf("JudgesUsed = %v, want fallback", score.JudgesUsed)
	}
}

// fallback 也失败时返回 error（避免静默丢分）。
func TestLLMJudge_Score_LLMAndFallbackBothFail(t *testing.T) {
	fake := &fakeLLMClient{errOnCall: map[int]error{0: errors.New("net down")}}
	// 用一个永远报错的 fallback
	badFallback := &alwaysFailJudge{}
	j := NewLLMJudge(fake, badFallback, nil)

	_, err := j.Score(context.Background(), &Case{ID: "t"}, &AgentResponse{})
	if err == nil {
		t.Errorf("expected error when both LLM and fallback fail")
	}
	if !strings.Contains(err.Error(), "fallback also failed") {
		t.Errorf("err = %v, want to mention fallback failure", err)
	}
}

// fallback 为 nil 时自动用 HeuristicJudge（构造器安全网）。
func TestLLMJudge_NilFallback_UsesHeuristic(t *testing.T) {
	fake := &fakeLLMClient{errOnCall: map[int]error{0: errors.New("net down")}}
	j := NewLLMJudge(fake, nil, nil) // fallback=nil 应自动用 heuristic

	score, err := j.Score(context.Background(), &Case{ID: "t"}, &AgentResponse{})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.JudgesUsed[0] != "llm-fallback-heuristic" {
		t.Errorf("JudgesUsed = %v, want fallback", score.JudgesUsed)
	}
}

// --- parseLLMJudgeAccuracy 直接单测 --------------------------------------

func TestParseLLMJudgeAccuracy_Valid(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    float64
	}{
		{"plain", `{"rca_accuracy":0.5}`, 0.5},
		{"with_spaces", `  {"rca_accuracy" : 0.42 }  `, 0.42},
		{"fenced", "```json\n{\"rca_accuracy\":0.9}\n```", 0.9},
		{"with_other_text", `thinking... {"rca_accuracy":0.33} done`, 0.33},
		{"zero", `{"rca_accuracy":0}`, 0.0},
		{"one", `{"rca_accuracy":1}`, 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLLMJudgeAccuracy(tc.content)
			if err != nil {
				t.Fatalf("parseLLMJudgeAccuracy: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %f, want %f", got, tc.want)
			}
		})
	}
}

func TestParseLLMJudgeAccuracy_Invalid(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"no_json", "no braces at all"},
		{"missing_field", `{"score":0.5}`},
		{"out_of_range_high", `{"rca_accuracy":1.5}`},
		{"out_of_range_low", `{"rca_accuracy":-0.1}`},
		{"null_value", `{"rca_accuracy":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseLLMJudgeAccuracy(tc.content)
			if err == nil {
				t.Errorf("expected error for %q", tc.content)
			}
		})
	}
}

// --- buildLLMJudgePrompt 直接单测 -----------------------------------------

func TestBuildLLMJudgePrompt_AllFieldsPresent(t *testing.T) {
	c := &Case{
		ID:                "pg/long-running-tx",
		ExpectedRootCause: []string{"pg.lock_waits"},
	}
	r := &AgentResponse{
		RootCause: []string{"pg.lock_waits"},
		ToolCalls: []ToolCall{{Name: "pg.lock_waits"}},
		Errors:    []string{"err1"},
	}
	prompt := buildLLMJudgePrompt(c, r)

	for _, want := range []string{
		"Expected root cause",
		"pg.lock_waits",
		"Actual root cause",
		"Evidence chain",
		"call: pg.lock_waits",
		"error: err1",
		"rca_accuracy",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestBuildLLMJudgePrompt_EmptyInputsRenderGracefully(t *testing.T) {
	c := &Case{ID: "t"}
	r := &AgentResponse{}
	prompt := buildLLMJudgePrompt(c, r)

	for _, want := range []string{
		"(none specified)",
		"(agent did not identify any root cause)",
		"(no tool calls recorded)",
		"(no evidence recorded)",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing fallback marker %q", want)
		}
	}
}

// alwaysFailJudge 用于测试 fallback 也失败的场景。
type alwaysFailJudge struct{}

func (a *alwaysFailJudge) Name() string { return "always-fail" }
func (a *alwaysFailJudge) Score(_ context.Context, _ *Case, _ *AgentResponse) (*Score, error) {
	return nil, errors.New("fallback always fails")
}
