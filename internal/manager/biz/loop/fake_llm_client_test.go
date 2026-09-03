package loop

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
)

// fakeChatReq 构造 {system, user} 双 message 的最小 ChatReq。
// LLMCaller 真实形态是 messages 交替 [system, user]；单测断言 prompt 时
// 复用此 helper 即可聚焦要看的内容。
func fakeChatReq(userPrompt, systemPrompt string) llm.ChatReq {
	return llm.ChatReq{
		Messages: []llm.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}
}

// 路径 1：SetResponse 预设后 Chat 返回 Content 字段（最基本 happy path）。
func TestFakeLLMClient_SetResponseReturnsContent(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	want := `{"foo":"bar"}`
	fc.SetResponse(0, want)

	resp, err := fc.Chat(context.Background(), fakeChatReq("hi", "sys"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatalf("resp is nil on success path")
	}
	if resp.Assistant.Content != want {
		t.Fatalf("Assistant.Content: want %q, got %q", want, resp.Assistant.Content)
	}
	if resp.Assistant.Role != "assistant" {
		t.Fatalf("Assistant.Role: want %q, got %q", "assistant", resp.Assistant.Role)
	}
}

// 路径 2：SetError 预设后 Chat 返回该 error 且 resp==nil。
func TestFakeLLMClient_SetErrorReturnsError(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	wantErr := errors.New("boom")
	fc.SetError(0, wantErr)

	resp, err := fc.Chat(context.Background(), fakeChatReq("hi", ""))
	if !errors.Is(err, wantErr) {
		t.Fatalf("err: want %v (errors.Is match), got %v", wantErr, err)
	}
	if resp != nil {
		t.Fatalf("resp must be nil on error path, got %+v", resp)
	}
}

// 路径 3：多次 SetResponse 按调用顺序消费（单测验"控制 LLM 返回序列"能力，
// 这是 LLMCaller 端到端测试的核心用法）。
func TestFakeLLMClient_SequentialResponsesAreConsumedInOrder(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	wantSeq := []string{"first", "second", "third"}
	for i, s := range wantSeq {
		fc.SetResponse(i, s)
	}

	ctx := context.Background()
	for i, want := range wantSeq {
		resp, err := fc.Chat(ctx, fakeChatReq("p"+string(rune('0'+i)), ""))
		if err != nil {
			t.Fatalf("call %d: unexpected err: %v", i, err)
		}
		if resp.Assistant.Content != want {
			t.Fatalf("call %d: want %q, got %q", i, want, resp.Assistant.Content)
		}
	}

	// 第 4 次未 SetResponse 时走 defaultContent（空）。
	resp, err := fc.Chat(ctx, fakeChatReq("after", ""))
	if err != nil {
		t.Fatalf("4th call: unexpected err: %v", err)
	}
	if resp.Assistant.Content != "" {
		t.Fatalf("unconsumed idx should fall back to empty, got %q", resp.Assistant.Content)
	}
}

// 路径 4：CallCount 跨成功 / 失败路径都精确计数。
func TestFakeLLMClient_CallCountCountsSuccessAndError(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	fc.SetResponse(0, "a")
	fc.SetError(1, errors.New("boom"))
	fc.SetResponse(2, "c")

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, _ = fc.Chat(ctx, fakeChatReq("p", ""))
	}
	if got := fc.CallCount(); got != 3 {
		t.Fatalf("CallCount: want 3, got %d", got)
	}

	// 未调用过的 FakeLLMClient 应返回 0 而不是 -1。
	if got := NewFakeLLMClient().CallCount(); got != 0 {
		t.Fatalf("fresh CallCount: want 0, got %d", got)
	}
}

// 路径 5：LastUserPrompt / LastSystemPrompt / LastSchema 在多次调用后
// 只反映"最近一次"的入参（覆盖语义 + 含 Tools）。
func TestFakeLLMClient_LastPromptsReflectMostRecentCall(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	fc.SetResponse(0, "ok")
	fc.SetResponse(1, "ok2")
	ctx := context.Background()

	// 第一次调用：带 system + user + 单个 Tool（含 JSON Schema）。
	_, err := fc.Chat(ctx, llm.ChatReq{
		Messages: []llm.Message{
			{Role: "system", Content: "first sys"},
			{Role: "user", Content: "first user"},
		},
		Tools: []llm.ToolSchema{
			{
				Name:       "emit_contract",
				Parameters: []byte(`{"type":"object","required":["alert_id"]}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("1st chat unexpected err: %v", err)
	}
	if got := fc.LastUserPrompt(); got != "first user" {
		t.Fatalf("1st: LastUserPrompt want %q, got %q", "first user", got)
	}
	if got := fc.LastSystemPrompt(); got != "first sys" {
		t.Fatalf("1st: LastSystemPrompt want %q, got %q", "first sys", got)
	}
	if got := fc.LastSchema(); !strings.Contains(got, `"required"`) || !strings.Contains(got, "alert_id") {
		t.Fatalf("1st: LastSchema missing required fields, got %q", got)
	}

	// 第二次调用：无 Tools / 改了 prompt；应覆盖上次。
	_, err = fc.Chat(ctx, llm.ChatReq{
		Messages: []llm.Message{
			{Role: "system", Content: "second sys"},
			{Role: "user", Content: "second user"},
		},
	})
	if err != nil {
		t.Fatalf("2nd chat unexpected err: %v", err)
	}
	if got := fc.LastUserPrompt(); got != "second user" {
		t.Fatalf("2nd: LastUserPrompt should overwrite prior, got %q", got)
	}
	if got := fc.LastSystemPrompt(); got != "second sys" {
		t.Fatalf("2nd: LastSystemPrompt should overwrite prior, got %q", got)
	}
	if got := fc.LastSchema(); !strings.Contains(got, `"required"`) {
		// Schema 应该保留上次的（未覆盖），便于跨多轮断言 schema 稳定。
		t.Fatalf("2nd: LastSchema should retain prior Tools[0], got %q", got)
	}
}

// 额外：SetDefault 兜底分支 —— 验证 SetResponse / SetError 都没覆盖的位置走 default。
func TestFakeLLMClient_SetDefaultFallback(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	fc.SetDefault("default-ok", nil)
	resp, err := fc.Chat(context.Background(), fakeChatReq("p", ""))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Assistant.Content != "default-ok" {
		t.Fatalf("default fallback want %q, got %q", "default-ok", resp.Assistant.Content)
	}

	// default error 也走兜底。
	fc2 := NewFakeLLMClient()
	wantErr := errors.New("default-boom")
	fc2.SetDefault("", wantErr)
	_, err = fc2.Chat(context.Background(), fakeChatReq("p", ""))
	if !errors.Is(err, wantErr) {
		t.Fatalf("default err want %v, got %v", wantErr, err)
	}
}

// 额外：-race 验证 —— 多个 goroutine 并发 Set + Chat 不得 data race。
func TestFakeLLMClient_ConcurrentAccessIsRaceFree(t *testing.T) {
	t.Parallel()
	fc := NewFakeLLMClient()
	for i := 0; i < 50; i++ {
		fc.SetResponse(i, "ok")
	}
	fc.SetDefault("fallback", nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				_, _ = fc.Chat(context.Background(), fakeChatReq("c", "s"))
				_ = fc.CallCount()
				_ = fc.LastUserPrompt()
				_ = fc.LastSystemPrompt()
				_ = fc.LastSchema()
			}
		}()
	}
	wg.Wait()
	if got := fc.CallCount(); got != 20*30 {
		t.Fatalf("CallCount under concurrency: want %d, got %d", 20*30, got)
	}
}
