package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/llm"
	managerbizaiopsagent "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/agent"
	managerbizimbridge "github.com/vincent-wuhan/opskeeper/core/manager/biz/imbridge"
	svcaiops "github.com/vincent-wuhan/opskeeper/core/manager/service/aiops"
)

// LLMDefaultProvider returns the cluster-wide default LLM provider id +
// model that web/SPA chats would pick. The IM path uses this so it
// stays in sync with whatever the operator selected in the UI picker;
// otherwise agent.New's internal "" → "gpt-5.4" fallback would route IM
// traffic to a model that's not in the catalog and the LLM router
// rejects it ("this model has beta-limitations…"), which surfaces in
// chat apps as "助手执行失败".
//
// Implemented by managerbizsetting.LLMSettingsResolver — the same
// resolver wired into the multi-provider LLM router (see main.go
// llmSettingsResolver) so the IM and HTTP paths converge on one source
// of truth for "default model".
type LLMDefaultProvider interface {
	ResolveProviders(ctx context.Context) ([]llm.ProviderConfig, string, error)
}

// imbridgeAgentAdapter wires the IM bridge's AgentSession port to the
// agent kernel (decision 280). It used to live in the imbridge package
// next to the bridge, which meant the bridge's own package imported the
// kernel — and since that was the kernel's only inbound edge, it was the
// one thing standing between a 41,000-line domain and being provably
// independently shippable. Everything below this comment is the reason
// that import existed, and none of it is the bridge's business: which
// service method to call, which options struct to build, how to pick a
// provider out of a catalog.
type imbridgeAgentAdapter struct {
	svc           *svcaiops.Service
	serviceUserID uint64
	defaults      LLMDefaultProvider
	log           *slog.Logger
}

var _ managerbizimbridge.AgentSession = imbridgeAgentAdapter{}

// newImbridgeAgentAdapter wires the adapter. defaults is optional — when
// nil the adapter falls back to agent.RunOptions{} (which puts
// agent.New's hard-coded model default in charge). Pass the
// LLMSettingsResolver so the IM path picks up the same default the SPA
// picker writes.
func newImbridgeAgentAdapter(svc *svcaiops.Service, serviceUserID uint64, defaults LLMDefaultProvider, log *slog.Logger) imbridgeAgentAdapter {
	if log == nil {
		log = slog.Default()
	}
	return imbridgeAgentAdapter{
		svc:           svc,
		serviceUserID: serviceUserID,
		defaults:      defaults,
		log:           log.With(slog.String("comp", "imbridge.adapter")),
	}
}

func (a imbridgeAgentAdapter) caller() svcaiops.Caller {
	// Role left blank — backend uses caller.UserID for ownership
	// checks; admin gating doesn't apply on the IM path.
	return svcaiops.Caller{UserID: a.serviceUserID}
}

// EnsureSession just creates a fresh session per inbound thread. We
// don't yet dedupe by label because the bridge already memoises via
// the im_threads table — duplicate calls only happen the first time
// after manager restart, which is acceptable for now.
func (a imbridgeAgentAdapter) EnsureSession(ctx context.Context, ownerUserID uint64, label string) (string, error) {
	caller := a.caller()
	if ownerUserID != 0 {
		caller.UserID = ownerUserID
	}
	sess, err := a.svc.CreateSession(ctx, caller, svcaiops.CreateSessionInput{
		Title: label,
	})
	if err != nil {
		return "", fmt.Errorf("imbridge adapter: create session: %w", err)
	}
	return sess.ID, nil
}

// StreamMessage posts user content to the session and forwards each
// frame to emit, translated. The agent loop runs synchronously on the
// caller's goroutine — the bridge calls this from its own goroutine
// (the webhook handler returns 200 immediately).
func (a imbridgeAgentAdapter) StreamMessage(ctx context.Context, sessionID string, userContent string, emit managerbizimbridge.Emit) error {
	opts := a.runOptions(ctx)
	_, err := a.svc.PostMessageStreamWithOpts(ctx, a.caller(), sessionID, userContent,
		func(e managerbizaiopsagent.Event) { emit(streamEventFrom(e)) }, opts)
	return err
}

// streamEventFrom translates one kernel frame into the bridge's view.
//
// The type name is carried through unchanged rather than mapped onto a
// three-value enum, so a kernel that grows an event type reaches the
// bridge's switch as its own name instead of collapsing into the same
// value as every other unhandled type. Only the assistant text crosses;
// the tool, task and approval payloads are dropped by the bridge on
// purpose (see streamEditor.OnEvent) and copying them across a domain
// boundary would make them look available when they are not.
func streamEventFrom(e managerbizaiopsagent.Event) managerbizimbridge.StreamEvent {
	out := managerbizimbridge.StreamEvent{Type: string(e.Type)}
	if e.Assistant != nil {
		out.Assistant = e.Assistant.Content
	}
	return out
}

// runOptions resolves the cluster default provider+model so IM traffic
// uses the same LLM the SPA picker writes. On any resolver error or
// missing catalog, returns zero RunOptions and lets the agent layer
// apply its own fallback (which currently lands on gpt-5.4 — see
// agent.New).
func (a imbridgeAgentAdapter) runOptions(ctx context.Context) managerbizaiopsagent.RunOptions {
	if a.defaults == nil {
		return managerbizaiopsagent.RunOptions{}
	}
	providers, defID, err := a.defaults.ResolveProviders(ctx)
	if err != nil || len(providers) == 0 {
		if err != nil {
			a.log.Warn("imbridge: llm resolver failed; using agent fallback model",
				slog.Any("err", err))
		}
		return managerbizaiopsagent.RunOptions{}
	}
	// If a default provider is set, look it up; otherwise take the
	// first catalog entry (alphabetical by id — same order the SPA
	// shows). Both paths fall back to the first provider when the
	// configured default points at a provider that was removed.
	var pick *llm.ProviderConfig
	if defID != "" {
		for i := range providers {
			if providers[i].ID == defID {
				pick = &providers[i]
				break
			}
		}
	}
	if pick == nil {
		pick = &providers[0]
	}
	return managerbizaiopsagent.RunOptions{
		Provider: pick.ID,
		Model:    pick.Model,
	}
}
