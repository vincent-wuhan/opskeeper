package chatruntime

import (
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/toolreplay"
	aiopsmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// history.go plans the conversation replay once and renders it for the kernel.
//
// The replay rules are not a formatting detail: which assistant turns are
// dropped, which tool results are hoisted next to their request, and which
// orphan rows are discarded all decide whether a strict provider accepts the
// transcript at all. They used to have to stay identical under two kernels —
// otherwise a kernel swap changed the conversation the model saw, and the
// symptom was one provider returning HTTP 400 on sessions that use a
// long-running tool.
//
// So the decision is made once, in planHistory, and buildKernelHistory is
// pure formatting over it. That is also what the file looked like with two
// renderings, and the split is kept rather than folded back in: a rule that
// decided a turn inside the renderer would then be invisible to the one
// place the transcript policy is reviewed.

// historyToolCall is one tool invocation an assistant requested, as the
// planner resolved it.
type historyToolCall struct {
	ID       string
	Name     string
	ArgsJSON string
}

// historyEntry is one replayed turn in kernel-neutral terms. Exactly one of
// the tool-call / tool-result slots is meaningful per role.
type historyEntry struct {
	// Role is "user" | "system" | "assistant" | "tool".
	Role    string
	Content string
	// ToolCalls is the assistant's request slot.
	ToolCalls []historyToolCall
	// ToolCallID and ToolName are the tool result's correlation slots.
	ToolCallID string
	ToolName   string
}

// planHistory turns persisted rows into the replay plan.
//
// Tool-call replay: tool-only assistant rows (Content=NULL, with hydrated
// ToolCalls) are emitted as {role:assistant, content:"", tool_calls:[...]}
// so the following role=tool messages remain paired. Strict providers
// (DeepSeek v4+) reject orphan tool messages with HTTP 400; OpenAI silently
// tolerated them. See toolreplay.Resolve for the LLM-call-id recovery rules
// (post-fix llm_call_id column + back-compat pair-by-order).
//
// The trailing role=user row is trimmed because the caller appends the live
// user turn separately; leaving it in would show the model the same turn
// twice.
func planHistory(rows []*aiopsmodel.Message) []historyEntry {
	if len(rows) == 0 {
		return nil
	}
	end := len(rows)
	if rows[end-1].Role == aiopsmodel.RoleUser {
		end--
	}
	// Resolve against the WHOLE history (not just rows[:end]) so the
	// trailing-user trim doesn't accidentally break pair-by-order
	// resolution for an assistant just before the trimmed user.
	callIDs := toolreplay.Resolve(rows)
	// Index role=tool rows by tool_call_id so an assistant with tool_calls
	// can hoist its responses to the immediate-next position regardless of
	// the persisted created_at order. Without this, long-running AgentTool
	// sub-agent spawns leave their parent assistant's tool_call slot
	// dangling — strict providers (DeepSeek v4+) reject with HTTP 400
	// "insufficient tool messages following tool_calls message".
	toolIdx := toolreplay.IndexToolMessagesByCallID(rows)
	// skipTool records the rows the hoist consumed. Nothing reads it while
	// un-hoisted tool rows are dropped unconditionally below, but it is
	// passed as the ledger it is: a future change that starts emitting tool
	// rows in natural order must consult it, and silently dropping the
	// bookkeeping here is exactly how that change would ship orphans.
	skipTool := make(map[int]bool)
	out := make([]historyEntry, 0, end)

	// emitToolByCallID appends the tool row matching callID and marks it as
	// consumed so a later reader does not double-emit it.
	emitToolByCallID := func(callID string) {
		j, ok := toolIdx[callID]
		if !ok || j >= end {
			// Tool result wasn't persisted (in flight) or sits in the
			// trimmed trailing user range. Skip silently — the assistant
			// turn that emitted this tool_call may need to be dropped
			// upstream; here we simply omit this slot.
			return
		}
		tm := rows[j]
		content := ""
		if tm.Content != nil {
			content = sanitizeToolReplayContent(*tm.Content)
		}
		tcID := ""
		if tm.ToolCallID != nil {
			tcID = *tm.ToolCallID
		}
		tname := ""
		if tm.ToolName != nil {
			tname = *tm.ToolName
		}
		out = append(out, historyEntry{
			Role:       aiopsmodel.RoleTool,
			Content:    content,
			ToolCallID: tcID,
			ToolName:   tname,
		})
		skipTool[j] = true
	}
	for i := 0; i < end; i++ {
		m := rows[i]
		switch m.Role {
		case aiopsmodel.RoleUser, aiopsmodel.RoleSystem:
			if m.Content == nil {
				continue
			}
			out = append(out, historyEntry{Role: m.Role, Content: *m.Content})
		case aiopsmodel.RoleAssistant:
			calls, ok := callIDs[m.ID]
			if len(m.ToolCalls) > 0 && !ok {
				toolreplay.MarkDependentToolsForSkip(rows, i, len(m.ToolCalls), skipTool)
				continue
			}
			// Precheck: every tool_call we'd emit must have a hoistable
			// response inside the current window. If any is missing
			// (parallel ToolsNode dropped an OnEnd → chat_tool_calls row
			// written but no role=tool chat_messages row), drop the whole
			// assistant turn + its dependent tools rather than send an
			// envelope strict providers reject with HTTP 400
			// "insufficient tool messages following tool_calls".
			if ok && len(calls) > 0 {
				complete := true
				for _, tc := range calls {
					if j, found := toolIdx[tc.ID]; !found || j >= end {
						complete = false
						break
					}
				}
				if !complete {
					toolreplay.MarkDependentToolsForSkip(rows, i, len(calls), skipTool)
					continue
				}
			}
			content := ""
			if m.Content != nil {
				content = *m.Content
			}
			entry := historyEntry{Role: aiopsmodel.RoleAssistant, Content: content}
			if ok {
				entry.ToolCalls = make([]historyToolCall, 0, len(calls))
				for _, tc := range calls {
					entry.ToolCalls = append(entry.ToolCalls, historyToolCall{ID: tc.ID, Name: tc.Name, ArgsJSON: tc.ArgsJSON})
				}
			}
			if content == "" && len(entry.ToolCalls) == 0 {
				// Polluted-data case: assistant with no replayable signal
				// AND no hydrated tool_calls. Drop assistant + dependent
				// tool rows so the LLM never sees orphans.
				toolreplay.MarkAllFollowingToolsForSkip(rows, i, skipTool)
				continue
			}
			out = append(out, entry)
			// HOIST: for every tool_call in this assistant's slot, look up
			// its response by tool_call_id and emit it RIGHT HERE so the
			// LLM API's "tool messages must immediately follow tool_calls"
			// invariant holds — even when the natural created_at order
			// interleaves another assistant (e.g. a long-running AgentTool
			// finishing after later short tools).
			for _, tc := range entry.ToolCalls {
				emitToolByCallID(tc.ID)
			}
		case aiopsmodel.RoleTool:
			// Tool messages are emitted ONLY via hoisting (emitToolByCallID,
			// right after their assistant's tool_calls). A role=tool row
			// reaching this branch un-hoisted is an ORPHAN: its
			// tool_call_id matches no preceding assistant tool_call. That
			// happens when an out-of-order parallel completion persisted the
			// response under a synthetic "<name>|einoToolAdapter" id. That
			// adapter name is historical — it comes from rows written by
			// the retired graph path and can still appear in replayed
			// sessions, so the matcher keeps accepting it (the
			// autoheal stub then filled the real id slot, so the assistant
			// turn survived the precheck and this real response is left
			// dangling). Emitting it bare yields the provider 400 "Messages
			// with role 'tool' must be a response to a preceding message
			// with 'tool_calls'". Drop it — the assistant's slot is already
			// satisfied by the hoisted row.
			continue
		}
	}
	return out
}

// buildKernelHistory renders the replay plan as the kernel-neutral message
// list the agent request carries.
func buildKernelHistory(rows []*aiopsmodel.Message) []ports.AgentMessage {
	plan := planHistory(rows)
	if len(plan) == 0 {
		return nil
	}
	out := make([]ports.AgentMessage, 0, len(plan))
	for _, e := range plan {
		msg := ports.AgentMessage{
			Role:       e.Role,
			Content:    e.Content,
			ToolCallID: e.ToolCallID,
			ToolName:   e.ToolName,
		}
		if len(e.ToolCalls) > 0 {
			msg.ToolCalls = make([]ports.AgentToolCall, 0, len(e.ToolCalls))
			for _, tc := range e.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, ports.AgentToolCall{
					ID:   tc.ID,
					Name: tc.Name,
					// The stored bytes are passed through unmodified:
					// re-marshalling would reorder keys and change float
					// formatting, and the provider compares the replayed
					// arguments with the ones it produced.
					Arguments: []byte(tc.ArgsJSON),
				})
			}
		}
		out = append(out, msg)
	}
	return out
}
