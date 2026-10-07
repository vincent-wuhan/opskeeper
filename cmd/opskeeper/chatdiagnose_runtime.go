package main

// This file is where the chatdiagnose <-> aiops boundary is actually drawn.
//
// The boundary itself was drawn correctly and long ago: biz/chatdiagnose
// declares `ChatRuntime` in its own words — `ReAct(ctx, ChatRuntimeRequest)
// (*ChatRuntimeResult, error)` — with its own request and result types, and
// nothing in the domain's service code names the agent kernel. That is the
// shape decisions 114 and 253 both arrived at independently, and it is why
// the only thing this cut had to move was a file.
//
// What it moved is this: the adapter that implements that port lived *inside*
// the consumer, in biz/chatdiagnose/chatruntime_adapter.go, and it imported
// chatruntime for three types and model/aiops for one line that said
// `_ = aiopsmodel.Message{}` — an import held open by an expression that
// computes nothing. So the domain had a correct port and still carried a
// package-shaped dependency underneath it, and the reason is mundane enough to
// be worth writing down: an adapter is a bridge, and bridges get built next to
// the thing they connect rather than next to the things that own them.
//
// Three types cross, and the consumer reads four of Request's fields, three of
// Mention's and two of Reply's:
//
//	Request   SessionID UserID UserText Mentions
//	Mention   Type ID Label
//	Reply     Message ToolCalls
//
// Reply also carries Usage and Iterations, and Request also carries Role,
// Provider, Model and Locale. Those belong to a coordinator session that this
// domain does not run: chatdiagnose drives a diagnostic conversation, not the
// user's chat, so it takes the runtime's defaults — which is why the old
// adapter left them zero rather than passing anything.
//
// The runtime dependency is an interface declared here, not the concrete
// *chatruntime.Runtime the old adapter held. That is not a style preference: a
// concrete field means no test can construct the adapter, and the adapter had
// no test at all — the translation between two vocabularies, which is the one
// place a silent field mix-up costs an operator their diagnosis, was the one
// piece of this boundary nothing could reach.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	aiopschatruntime "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/chatruntime"
	managerbizchatdiagnose "github.com/vincent-wuhan/opskeeper/core/manager/biz/chatdiagnose"
	"go.opentelemetry.io/otel/trace"
)

// chatDiagnoseRuntime is the one method this translation needs. Declared here
// so the adapter is constructible from a fake, and so a reader can see the
// boundary's whole width on one screen: a context, a request in the runtime's
// words, a reply in the runtime's words.
type chatDiagnoseRuntime interface {
	Handle(ctx context.Context, req *aiopschatruntime.Request) (*aiopschatruntime.Reply, error)
}

// chatDiagnoseReAct adapts the agent kernel's chat runtime to
// chatdiagnose.ChatRuntime.
type chatDiagnoseReAct struct {
	rt chatDiagnoseRuntime
}

// ReAct is nil-runtime-safe like the adapter it replaces: the chatdiagnose
// service is constructed on every boot, including the ones where the LLM
// runtime failed to build and there is nothing to delegate to.
//
// The check is `a.rt == nil` and not "is the runtime pointer nil", and that
// distinction is the whole reason the wiring below is three lines instead of
// one. A nil *chatruntime.Runtime stored in a non-nil interface compares
// unequal to nil, so a one-line `chatDiagnoseReAct{rt: chatRT}` would sail
// past this guard and panic on the first ReAct of a boot where the LLM was not
// configured. The old adapter held a concrete pointer and could not hit it.
// The compiler will not catch this one, which is why the guard and the wiring
// are written the way they are rather than the way that reads shortest.
func (a chatDiagnoseReAct) ReAct(
	ctx context.Context,
	req managerbizchatdiagnose.ChatRuntimeRequest,
) (*managerbizchatdiagnose.ChatRuntimeResult, error) {
	if a.rt == nil {
		return nil, fmt.Errorf("chatdiagnose: chatDiagnoseReAct: nil runtime")
	}
	mentions := make([]aiopschatruntime.Mention, 0, len(req.ContextRefs))
	for _, ref := range req.ContextRefs {
		// A malformed ref is not fatal: it drops to an empty type and the
		// runtime renders the raw string. This is the old behaviour, kept
		// deliberately — the alternative is failing a whole diagnostic turn
		// because one @-mention was malformed, and the diagnostic is the
		// expensive part.
		t, id := splitChatContextRef(ref)
		mentions = append(mentions, aiopschatruntime.Mention{Type: t, ID: id, Label: ref})
	}
	reply, err := a.rt.Handle(ctx, &aiopschatruntime.Request{
		SessionID: req.ConversationID, // ConversationID is reused as SessionID
		UserID:    parseUintLoose(req.UserID),
		UserText:  req.Message,
		Mentions:  mentions,
		// Role / Provider / Model / Locale take the runtime's defaults: this
		// domain runs a diagnostic conversation, not the operator's chat.
	})
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose: chatruntime handle: %w", err)
	}
	return convertChatDiagnoseReply(reply, ctx)
}

// splitChatContextRef splits a "type:id" reference.
//
// It is not strings.Cut, which returns the whole string as `before` when the
// separator is absent — so a malformed ref would arrive at the model as a
// mention whose type is the whole string, which is worse than no type at all.
// This returns two empty strings instead, and the raw text still reaches the
// runtime as the label. A first draft of this file used strings.Cut and the
// test that now pins this function is the only reason the difference was
// noticed before a malformed @-mention reached a model as a bogus type.
func splitChatContextRef(ref string) (string, string) {
	if i := strings.Index(ref, ":"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return "", ""
}

// parseUintLoose turns a string user id into the uint64 the runtime's session
// store wants. An unparseable id becomes 0, which is how the runtime spells
// "system-owned" — the old adapter did the same and dropped the error, and
// failing a diagnostic because a user id was not numeric would be worse than
// attributing the session to nobody.
func parseUintLoose(s string) uint64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func convertChatDiagnoseReply(reply *aiopschatruntime.Reply, ctx context.Context) (*managerbizchatdiagnose.ChatRuntimeResult, error) {
	if reply == nil {
		return &managerbizchatdiagnose.ChatRuntimeResult{Reply: ""}, nil
	}
	out := &managerbizchatdiagnose.ChatRuntimeResult{}
	if reply.Message != nil && reply.Message.Content != nil {
		out.Reply = *reply.Message.Content
	}
	for _, tc := range reply.ToolCalls {
		if tc == nil {
			continue
		}
		out.ToolCalls = append(out.ToolCalls, managerbizchatdiagnose.ToolCall{
			Name:        tc.ToolName,
			ArgsPreview: previewArgs(tc.ArgumentsJSON),
			Status:      tc.Status,
		})
	}
	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		out.TraceID = span.SpanContext().TraceID().String()
	}
	// RootCauseObject / Confidence / Evidence / Remediation stay nil: the
	// runtime's ReAct path does not produce a structured root cause, and the
	// service's RootCauseJSON builder is what decides "not converged" from a
	// nil. Parsing them out of reply metadata is a later change and should be
	// a change to this file, not to the domain.
	return out, nil
}

// previewArgs truncates the runtime's arguments_json into the chip the SPA
// shows under a tool call.
func previewArgs(s string) string {
	const maxLen = 80
	if len(s) <= maxLen {
		return s
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err == nil {
		for k, v := range m {
			return fmt.Sprintf("%s=%v", k, v)
		}
	}
	return s[:maxLen] + "..."
}
