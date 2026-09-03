// chatdiagnose/chatruntime_adapter.go — adapter 实现 chatdiagnose.ChatRuntime
// interface，包装 *aiopschatruntime.Runtime。
//
// 类型转换：
//   - chatdiagnose.ChatRuntimeRequest (string user_id, string agent) ↔
//     chatruntime.Request (uint64 user_id, Mention[] mentions)
//   - chatdiagnose.ChatRuntimeResult (Reply string, ToolCalls []ToolCall,
//     RootCauseObject *loop.RootCauseObject, ...) ↔
//     chatruntime.Reply (*aiopsmodel.Message, []*aiopsmodel.ToolCall, ...)

package chatdiagnose

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	aiopschatruntime "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/chatruntime"
	aiopsmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
	"go.opentelemetry.io/otel/trace"
)

// ChatRuntimeAdapter 包装 *aiopschatruntime.Runtime 实现 ChatRuntime
// interface。nil 安全的 nil-receiver 方法（让单测可以传 nil）。
type ChatRuntimeAdapter struct {
	rt *aiopschatruntime.Runtime
}

// NewChatRuntimeAdapter 构造 adapter。rt 不能为 nil（生产 wire）。
func NewChatRuntimeAdapter(rt *aiopschatruntime.Runtime) *ChatRuntimeAdapter {
	return &ChatRuntimeAdapter{rt: rt}
}

// ReAct 实现 chatdiagnose.ChatRuntime。
func (a *ChatRuntimeAdapter) ReAct(ctx context.Context, req ChatRuntimeRequest) (*ChatRuntimeResult, error) {
	if a == nil || a.rt == nil {
		return nil, fmt.Errorf("chatdiagnose: ChatRuntimeAdapter: nil runtime")
	}
	// user id string → uint64（chatruntime 内部 SessionRepo 期望）
	var uid uint64
	if req.UserID != "" {
		if v, err := strconv.ParseUint(req.UserID, 10, 64); err == nil {
			uid = v
		}
	}
	// mentions from context refs
	mentions := make([]aiopschatruntime.Mention, 0, len(req.ContextRefs))
	for _, ref := range req.ContextRefs {
		t, id, _ := splitContextRef(ref)
		mentions = append(mentions, aiopschatruntime.Mention{Type: t, ID: id, Label: ref})
	}
	rtReq := &aiopschatruntime.Request{
		SessionID: req.ConversationID, // ConversationID 复用为 SessionID
		UserID:    uid,
		UserText:  req.Message,
		Mentions:  mentions,
		// Role / Provider / Model / Locale 走 chatruntime 默认值
	}
	reply, err := a.rt.Handle(ctx, rtReq)
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose: chatruntime handle: %w", err)
	}
	return convertReply(reply, ctx)
}

func convertReply(reply *aiopschatruntime.Reply, ctx context.Context) (*ChatRuntimeResult, error) {
	if reply == nil {
		return &ChatRuntimeResult{Reply: ""}, nil
	}
	out := &ChatRuntimeResult{}
	// 1) Reply text
	if reply.Message != nil && reply.Message.Content != nil {
		out.Reply = *reply.Message.Content
	}
	// 2) ToolCalls — 投影到 chatdiagnose.ToolCall
	for _, tc := range reply.ToolCalls {
		if tc == nil {
			continue
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			Name:        tc.ToolName,
			ArgsPreview: previewArgs(tc.ArgumentsJSON),
			Status:      tc.Status,
		})
	}
	// 3) TraceID
	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		out.TraceID = span.SpanContext().TraceID().String()
	}
	// 4) RootCauseObject / Confidence / Evidence / Remediation —
	//    chatruntime 当前 ReAct 不直接产出结构化 root cause。
	//    留 nil 让 service 的 RootCauseJSON 构造逻辑判定 "未收敛"。
	//    Day 10+ chatruntime-side 集成后从 reply.Metadata 解析（Q-α 决策点）。
	_ = aiopsmodel.Message{} // 保持 import；防止 unused
	return out, nil
}

// previewArgs 把 chatruntime 的 arguments_json 截断为 SPA chip 用的 preview。
func previewArgs(s string) string {
	const maxLen = 80
	if len(s) <= maxLen {
		return s
	}
	// 尝试 parse JSON 拿第一个 key=value 摘要
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err == nil {
		for k, v := range m {
			return fmt.Sprintf("%s=%v", k, v)
		}
	}
	return s[:maxLen] + "..."
}

// splitContextRef 把 "type:id" 拆成 type, id。
func splitContextRef(ref string) (string, string, error) {
	for i, c := range ref {
		if c == ':' {
			return ref[:i], ref[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("chatdiagnose: bad context_ref %q (want type:id)", ref)
}
