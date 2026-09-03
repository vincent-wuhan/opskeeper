// Package loop — fake_llm_client.go
//
// FakeLLMClient 是 llm.Client 的 mock 实现，供 5 phase worker +
// LLMCaller + LLMJudge 单测共用（路径 A 集成期：
// docs/superpowers/specs/2026-08-12-llm-worker-integration-design.md §9.1）。
//
// 选 llm.Client.Chat 作为被 stub 的接口有两个原因：
//  1. LLMJudge 已经直接依赖 llm.Client.Chat（internal/harness/judge/llm_judge.go）
//  2. internal/pkg/llm.Client 是当前 LLM 抽象的"最底层 + 最稳定"接口，
//     LLMCaller / 5 phase worker 通过构造注入拿到它，单测只需替换 Client。
//
// 用法：
//
//	fc := NewFakeLLMClient()
//	fc.SetResponse(0, `{"alert_id":"a1","severity":"critical"}`)
//	resp, err := fc.Chat(ctx, llm.ChatReq{...})
//	_ = fc.CallCount()           // == 1
//	_ = fc.LastUserPrompt()      // == user prompt from last call
//	_ = fc.LastSystemPrompt()    // == system prompt from last call
//	_ = fc.LastSchema()          // == req.Tools[0].Parameters（如有）
//
// 红线：
//   - 不模拟 OpenAI token usage / network / stream，只回 PresetResponse。
//   - 调用计数从 0 起，按调用顺序消费 responses/errors 序列；未 Set 的位置
//     走 SetDefault 或默认空字符串。
//   - error 优先级 > response：同 idx 上同时有 SetResponse + SetError 时
//     返回 error（与 Go 多返回值的"错误即短路"约定一致）。
//   - 线程安全：所有读写都加锁；测试 -race 下不会报 data race。
package loop

import (
	"context"
	"sync"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
)

// FakeLLMClient 是 llm.Client 的 mock 实现。
type FakeLLMClient struct {
	mu sync.Mutex
	// responses[idx] 预设第 idx 次 Chat 调用的 Content；空字符串
	// 表示未预设，走 defaultContent。
	responses []string
	// errors[idx] 预设第 idx 次 Chat 调用的 error；非 nil 时短路返回。
	errors         []error
	defaultContent string // 未通过 SetResponse 覆盖的位置默认返回此 Content
	defaultError   error  // 未通过 SetError 覆盖且未配置 defaultContent 时返回

	callCount        int
	lastUserPrompt   string
	lastSystemPrompt string
	lastSchema       string
	lastTools        []llm.ToolSchema
}

// 编译期保证 FakeLLMClient 实现 llm.Client 接口；任何签名漂移会在这里炸。
var _ llm.Client = (*FakeLLMClient)(nil)

// NewFakeLLMClient 返回一个空的 FakeLLMClient。空 Mock 的 Chat 返回
// （*ChatResp{Assistant: Message{Content: ""}}, nil）。
func NewFakeLLMClient() *FakeLLMClient {
	return &FakeLLMClient{}
}

// SetResponse 预设第 seq 次 Chat 的 Content；后续 Set(seq, ...) 会覆盖。
// 多次按从小到大顺序消费；未预设的 idx 走 defaultContent 或空字符串。
func (f *FakeLLMClient) SetResponse(seq int, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.responses) <= seq {
		f.responses = append(f.responses, "")
	}
	f.responses[seq] = content
}

// SetError 预设第 seq 次 Chat 的 error；非 nil 时短路返回错误响应（resp=nil）。
// 与 SetResponse 同 idx 时优先返回 error。
func (f *FakeLLMClient) SetError(seq int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.errors) <= seq {
		f.errors = append(f.errors, nil)
	}
	f.errors[seq] = err
}

// SetDefault 设置未通过 SetResponse/SetError 覆盖的位置的兜底响应。
// 调用约定：error 优先；若 defaultError == nil 则用 defaultContent 返回。
func (f *FakeLLMClient) SetDefault(content string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultContent = content
	f.defaultError = err
}

// CallCount 返回已被调用过的 Chat 次数（含返回 error 的）。
// 单测断言："跑了 N 轮 worker，LLM 必被调用恰好 N 次"。
func (f *FakeLLMClient) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCount
}

// LastUserPrompt 返回最后一次 Chat 调用里 Role=="user" 的 Message.Content。
// LLMCaller 通常构造 {system, user} 双 message，因此 LastUserPrompt 取
// 末次 user 内容；从未调用过返回空串。
func (f *FakeLLMClient) LastUserPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastUserPrompt
}

// LastSystemPrompt 返回最后一次 Chat 调用里 Role=="system" 的 Message.Content。
// 未传 system 时返回空串（这是合法用法：部分 worker 可能只发 user prompt）。
func (f *FakeLLMClient) LastSystemPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSystemPrompt
}

// LastSchema 返回最后一次 Chat 调用里的 Tools[0].Parameters（JSON Schema 字节）
// 作为 string 返回，方便测试用 Contains 等做模糊断言。Tools 为空时返回空串。
func (f *FakeLLMClient) LastSchema() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSchema
}

// Chat 实现 llm.Client 接口。
//
// 行为：
//  1. 把最后一次调用的 user / system / Tools 摘到 last* 字段（断言用）。
//  2. 按 errors[idx] / responses[idx] / default* 顺序决定返回：
//     error 非 nil → 直接返回该 err（resp == nil）
//     否则按 responses[idx] → defaultError → defaultContent → 空字符串 fallback。
//  3. callCount 自增（即使 error 路径也计数，方便"被调用次数"断言）。
func (f *FakeLLMClient) Chat(ctx context.Context, req llm.ChatReq) (*llm.ChatResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := f.callCount

	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			f.lastUserPrompt = m.Content
		case "system":
			f.lastSystemPrompt = m.Content
		}
	}
	if len(req.Tools) > 0 {
		f.lastTools = append([]llm.ToolSchema(nil), req.Tools...)
		f.lastSchema = string(req.Tools[0].Parameters)
	}

	var (
		content string
		err     error
	)
	if idx < len(f.errors) && f.errors[idx] != nil {
		err = f.errors[idx]
	}
	if err == nil {
		switch {
		case idx < len(f.responses) && f.responses[idx] != "":
			content = f.responses[idx]
		case f.defaultError != nil:
			err = f.defaultError
		default:
			content = f.defaultContent
		}
	}
	f.callCount = idx + 1

	if err != nil {
		return nil, err
	}
	return &llm.ChatResp{
		Assistant: llm.Message{
			Role:    "assistant",
			Content: content,
		},
	}, nil
}
