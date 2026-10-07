package main

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/llm"
	agent "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/agent"
	managerbizimbridge "github.com/vincent-wuhan/opskeeper/core/manager/biz/imbridge"
)

type fakeDefaults struct {
	providers []llm.ProviderConfig
	defID     string
	err       error
}

func (f *fakeDefaults) ResolveProviders(_ context.Context) ([]llm.ProviderConfig, string, error) {
	return f.providers, f.defID, f.err
}

// Every event name the kernel can emit reaches the bridge unchanged, and
// every frame that is not an assistant turn arrives with no text. The
// second half matters as much as the first: an event that carries text
// while claiming not to be an assistant turn is a frame the bridge would
// silently swallow, and one that loses its name is a frame it would
// swallow under a different reason.
func TestEveryKernelFrameReachesTheBridgeUnchanged(t *testing.T) {
	cases := []struct {
		name     string
		in       agent.Event
		wantType string
		wantText string
	}{
		{"assistant with text", agent.Event{Type: agent.EventAssistant, Assistant: &agent.AssistantEvent{Content: "重启完成"}}, "assistant", "重启完成"},
		{"assistant with empty text", agent.Event{Type: agent.EventAssistant, Assistant: &agent.AssistantEvent{}}, "assistant", ""},
		{"terminal", agent.Event{Type: agent.EventDone, Done: &agent.Reply{}}, "done", ""},
		{"tool start", agent.Event{Type: agent.EventToolStart, Tool: &agent.ToolEvent{}}, "tool_start", ""},
		{"tool end", agent.Event{Type: agent.EventToolEnd, Tool: &agent.ToolEvent{}}, "tool_end", ""},
		{"task notification", agent.Event{Type: agent.EventTaskNotification, Notification: &agent.TaskNotificationEvent{TaskID: "t1"}}, "task_notification", ""},
		{"approval pending", agent.Event{Type: agent.EventApprovalPending, Approval: &agent.ApprovalPendingEvent{ApprovalID: "a1"}}, "approval_pending", ""},
		{"a type the kernel has not written yet", agent.Event{Type: agent.EventType("telemetry_tick")}, "telemetry_tick", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := streamEventFrom(c.in)
			if got.Type != c.wantType {
				t.Errorf("Type: got %q, want %q", got.Type, c.wantType)
			}
			if got.Assistant != c.wantText {
				t.Errorf("Assistant: got %q, want %q", got.Assistant, c.wantText)
			}
		})
	}
}

// The bridge switches on the names above, so a translation that renamed
// one would produce a bridge that streams nothing and reports no error.
// Pinning them here as well as in the bridge's own package is deliberate:
// this is the code that could get it wrong, and that is the side the
// bridge's package cannot see.
func TestTheTranslatedNamesAreTheOnesTheBridgeSwitchesOn(t *testing.T) {
	got := streamEventFrom(agent.Event{Type: agent.EventAssistant, Assistant: &agent.AssistantEvent{Content: "x"}})
	if got.Type != managerbizimbridge.EventAssistant {
		t.Errorf("assistant: got %q, want %q", got.Type, managerbizimbridge.EventAssistant)
	}
	got = streamEventFrom(agent.Event{Type: agent.EventDone})
	if got.Type != managerbizimbridge.EventDone {
		t.Errorf("done: got %q, want %q", got.Type, managerbizimbridge.EventDone)
	}
}

// TestAdapter_runOptions_picksResolverDefault covers the v0.7.169 fix:
// IM bridge must thread the cluster default_provider + <provider>_default_model
// into the run options instead of leaving them empty (which made the
// kernel fall back to the hard-coded "gpt-5.4" model and end users saw
// "助手执行失败" in Lark/Slack/Telegram).
func TestAdapter_runOptions_picksResolverDefault(t *testing.T) {
	tests := []struct {
		name         string
		defaults     LLMDefaultProvider
		wantProvider string
		wantModel    string
	}{
		{
			name: "configured default points at a registered provider",
			defaults: &fakeDefaults{
				providers: []llm.ProviderConfig{
					{ID: "custom", Model: "claude-opus-4-7"},
					{ID: "zhipu", Model: "glm-4.7-flash"},
				},
				defID: "zhipu",
			},
			wantProvider: "zhipu",
			wantModel:    "glm-4.7-flash",
		},
		{
			name: "empty default falls back to the first provider in catalog",
			defaults: &fakeDefaults{
				providers: []llm.ProviderConfig{
					{ID: "custom", Model: "claude-opus-4-7"},
					{ID: "zhipu", Model: "glm-4.7-flash"},
				},
				defID: "",
			},
			wantProvider: "custom",
			wantModel:    "claude-opus-4-7",
		},
		{
			name: "default points at a provider that's no longer in the catalog falls back to first",
			defaults: &fakeDefaults{
				providers: []llm.ProviderConfig{
					{ID: "zhipu", Model: "glm-4.7-flash"},
				},
				defID: "openai",
			},
			wantProvider: "zhipu",
			wantModel:    "glm-4.7-flash",
		},
		{
			name:         "resolver error → zero options (kernel fallback applies)",
			defaults:     &fakeDefaults{err: errors.New("transient DB error")},
			wantProvider: "",
			wantModel:    "",
		},
		{
			name:         "empty catalog → zero options (no provider configured at all)",
			defaults:     &fakeDefaults{providers: nil},
			wantProvider: "",
			wantModel:    "",
		},
		{
			name:         "nil resolver → zero options (caller didn't wire it)",
			defaults:     nil,
			wantProvider: "",
			wantModel:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newImbridgeAgentAdapter(nil, 1, tt.defaults, nil)
			opts := a.runOptions(context.Background())
			if opts.Provider != tt.wantProvider {
				t.Errorf("provider = %q, want %q", opts.Provider, tt.wantProvider)
			}
			if opts.Model != tt.wantModel {
				t.Errorf("model = %q, want %q", opts.Model, tt.wantModel)
			}
		})
	}
}
