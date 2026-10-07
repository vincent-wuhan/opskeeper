// Package pigcontract is the compiled contract between OpsKeeper and PiG.
//
// PiG is a pre-stable 0.x dependency: its API moves, and so does its
// dependency graph. The module split exists so that a PiG change is a change
// to core/pig and to nothing else (decisions 57 and 62), and this package is
// what turns that promise into something the build checks rather than
// something a reviewer remembers.
//
// It is not a test and it has no behaviour. It contains exactly two things:
//
//  1. a reference to every PiG symbol OpsKeeper actually uses, pinned to the
//     shape OpsKeeper uses it in — so a removed function, a renamed field, a
//     changed parameter or a widened return type stops `go build ./...` at
//     the line that names what moved, instead of surfacing later as a runtime
//     surprise in a node binary somebody forgot to rebuild; and
//  2. the semantics the type system cannot express, asserted in
//     contract_test.go — the wire spellings of the event names, the constant
//     values that are compared against strings read from a config file, and
//     the behaviours the mapper depends on.
//
// The pins are written as package-level `var _ = ...` initialisers rather
// than as declarations in a function so that no linter can decide they are
// dead code, and so the compiler type-checks every one of them on every
// build. Bodies of anonymous functions are type-checked but never run, which
// is what makes the call-shaped pins possible without knowing PiG's
// unexported and structural types: `run, err := ag.BeginSendMessages(...)`
// pins the method's existence and argument list without this file having to
// name the concrete type it returns.
//
// What this package is NOT:
//
//   - It is not a conformance suite for the parts of PiG OpsKeeper does not
//     use. Widening it to cover the whole upstream API would make it a second
//     copy of PiG's documentation, and a contract nobody can read is a
//     contract nobody updates.
//   - It does not pin behaviour that OpsKeeper is free to absorb in
//     core/pig's own tests. The division is: shapes here, semantics in
//     contract_test.go, adapter behaviour in pigmodel/pigagent/pigrpc's own
//     tests.
//
// When a pin here fails: the fix belongs in core/pig. That is the whole
// point of the module. Change the adapter to match upstream, or, if upstream
// removed something OpsKeeper needs, that is a conversation with PiG and not
// a repository-wide rebuild.
package pigcontract

import (
	"context"
	"encoding/json"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/piglet"
	"github.com/MichaelKinsy/PiG/coding/rpcclient"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

// ── ai: providers, models and the per-request options ──────────────────────
//
// Used by core/pig/pigmodel to turn a settings row into a PiG provider and a
// *ai.Model, and to carry the per-request API key in ai.StreamOptions rather
// than in the long-lived provider (decision 26).

var (
	_ func(ai.OpenAIConfig) ai.Provider     = ai.NewOpenAIProvider
	_ func(ai.AnthropicConfig) ai.Provider  = ai.NewAnthropicProvider
	_ func(ai.GoogleConfig) ai.Provider     = ai.NewGoogleProvider
	_ func(ai.Context) ai.TranscriptContext = ai.NormalizeContext
)

// The provider configs, with every field OpsKeeper sets.
//
// Compat is pinned through DetectCompat because it is the one field whose
// value OpsKeeper does not author: it is derived from the provider id and the
// base URL, and it is what makes a self-hosted OpenAI-compatible gateway
// work. A rename of DetectCompat or a change to OpenAIConfig.Compat's type
// fails here.
var _ = ai.OpenAIConfig{
	BaseURL:    "",
	Model:      "",
	ProviderID: "",
	Compat:     ai.DetectCompat("", ""),
	GetAPIKey:  func(context.Context) (string, error) { return "", nil },
}

var (
	_ = ai.AnthropicConfig{APIKey: "", Model: "", ProviderID: "", BaseURL: ""}
	_ = ai.GoogleConfig{APIKey: "", Model: "", ProviderID: "", BaseURL: ""}
)

// ai.Model is what the registry hands the agent loop. Every field listed is
// one OpsKeeper sets; the ones it leaves nil are listed as nil so that a
// field that becomes mandatory is a compile error rather than a zero value
// PiG interprets as "not configured".
var _ = ai.Model{
	ID:               "",
	DisplayName:      "",
	Provider:         nil,
	ProviderMeta:     ai.ProviderMetadata{ProviderID: "", BaseURL: ""},
	Capabilities:     ai.ModelCapabilities{SupportsToolUse: true, SupportsImages: false, MaxThinking: ai.ThinkingOff, ContextWindow: 0, MaxOutputTokens: 0},
	SamplingParams:   map[string]any{},
	PromptCache:      nil,
	InputLimits:      nil,
	ThinkingLevelMap: ai.ThinkingLevelMap{},
}

var _ = ai.StreamOptions{APIKey: "", SessionID: "", Thinking: ai.ThinkingOff}

// The thinking levels the registry maps a stored string onto. They are
// compared against `ai.ThinkingLevel(raw)` in parseThinkingLevel, so an
// upstream rename is a compile error and a value change is caught by
// contract_test.go.
var (
	_ ai.ThinkingLevel = ai.ThinkingOff
	_ ai.ThinkingLevel = ai.ThinkingNone
	_ ai.ThinkingLevel = ai.ThinkingMinimal
	_ ai.ThinkingLevel = ai.ThinkingLow
	_ ai.ThinkingLevel = ai.ThinkingMedium
	_ ai.ThinkingLevel = ai.ThinkingHigh
	_ ai.ThinkingLevel = ai.ThinkingXHigh
)

// The chat path: a tool schema as the model sees it, and the four message
// shapes the LLM port is translated into.
var (
	_ = []ai.ToolSchema{{Name: "", Description: "", Parameters: map[string]any{}}}

	_ ai.Message = ai.SystemMessage{Content: ai.SystemText("")}
	_ ai.Message = ai.UserMessage{Content: ai.UserText("")}
	_ ai.Message = ai.ToolResultMessage{
		ToolCallID: "",
		ToolName:   "",
		Content:    []ai.ToolResultMessageContent{ai.TextContent{Text: ""}},
	}
	_ = ai.AssistantMessage{Content: []ai.AssistantContentBlock{
		ai.TextContent{Text: ""},
		ai.ToolCall{ID: "", Name: "", Arguments: ai.JsonObject{}},
	}}
	_ = ai.ToolCall{ID: "", Name: "", Arguments: ai.JsonObject{}}
)

// The content unions are read with a type switch in pigagent/message.go,
// because the ContentText helper covers the concrete types and not the
// interface. Both arms of each switch are named here so that dropping one is
// a compile error rather than a silently empty transcript.
var (
	_ ai.UserContent           = ai.UserText("")
	_ ai.UserContent           = ai.UserContentBlocks(nil)
	_ ai.SystemContent         = ai.SystemText("")
	_ ai.SystemContent         = ai.SystemTextBlocks(nil)
	_ ai.AssistantContentBlock = ai.TextContent{Text: ""}
	_ ai.AssistantContentBlock = ai.ToolCall{ID: "", Name: "", Arguments: ai.JsonObject{}}
)

// ── agent: the loop, its options, and the hooks the gate hangs off ─────────

var _ func(agent.AgentOptions) *agent.Agent = agent.NewAgent

// AgentOptions carries the tool bag, the prompt, the stream function and the
// four host callbacks. MaxTurns and SessionID are set too: the first is the
// budget the host computes per turn, the second is what a steer targets.
var _ = agent.AgentOptions{
	Model:            nil,
	Tools:            []agent.AgentTool{},
	SystemPrompt:     "",
	MaxTurns:         0,
	SessionID:        "",
	DefaultStreamFn:  nil,
	OnEvent:          nil,
	OnMessagePersist: func(agent.AgentMessage) error { return nil },
	BeforeToolCall:   []agent.BeforeToolCallHook{},
	AfterToolCall:    []agent.AfterToolCallHook{},
	FinishTurn:       nil,
}

// The agent surface: how a turn is begun, how a steer is delivered, and how
// a run is cancelled. Written as a call-shaped pin because BeginSendMessages
// returns a type whose name is PiG's business, not this file's.
var _ = func(ctx context.Context, ag *agent.Agent, prompt []agent.AgentMessage) {
	run, err := ag.BeginSendMessages(ctx, prompt)
	_ = err
	if run == nil {
		return
	}
	messages, runErr := run.Run()
	_ = messages
	_ = runErr

	ag.Steer(agent.AgentMessage{User: &agent.UserMessage{
		Role:      "user",
		Content:   ai.UserText(""),
		Timestamp: 0,
	}})
	ag.Abort()
}

// The three hooks, at their exact signatures. A hook that gains a parameter
// stops the build here rather than silently not being called by PiG — which
// is the failure mode that matters, because a policy hook that is never
// invoked looks exactly like a policy hook that always allows.
var (
	_ agent.BeforeToolCallHook = func(ctx context.Context, toolCallID, toolName string, args json.RawMessage) agent.ToolCallHookResult {
		return agent.ToolCallHookResult{Block: true, Reason: ""}
	}
	_ agent.AfterToolCallHook = func(ctx context.Context, toolCallID, toolName string, args json.RawMessage, result agent.AgentToolResult) agent.AfterToolCallResult {
		return agent.AfterToolCallResult{}
	}
	_ agent.FinishTurn = func(ctx context.Context, turn agent.AgentTurnContext) (*agent.AgentTurnDecision, error) {
		return &agent.AgentTurnDecision{Action: agent.AgentTurnEnd}, nil
	}
)

// The stream function: the seam that keeps the provider chosen at resolution
// time instead of rebuilt from the model's metadata (decision 26).
var _ agent.StreamFn = func(ctx context.Context, model *ai.Model, transcript ai.TranscriptContext, callOpts ai.StreamOptions) (*ai.AssistantMessageEventStream, error) {
	if model == nil || model.Provider == nil {
		return nil, nil
	}
	merged := callOpts
	merged.APIKey = ""
	merged.MaxTokens = callOpts.MaxTokens
	merged.Temperature = callOpts.Temperature
	merged.ModelCost = callOpts.ModelCost
	merged.ToolChoice = callOpts.ToolChoice
	merged.Thinking = callOpts.Thinking
	merged.IsReasoning = callOpts.IsReasoning
	merged.SessionID = callOpts.SessionID
	return model.Provider.Stream(ctx, transcript, merged)
}

// The tool contract: the method set an OpsKeeper tool adapter must satisfy,
// and the parallelism mode every adapter currently reports.
var (
	_ agent.ToolExecutionMode = agent.ToolModeParallel
	_                         = agent.AgentToolResult{}
	_                         = agent.AgentTurnContext{}
)

// ── agent events: the seven the SSE mapper translates ──────────────────────
//
// These are the only upstream events with a console counterpart. The default
// arm of the mapper drops everything else, so a new event type is not a
// compile error — it is a silently missing frame, which is why the frames
// themselves are pinned by the golden test in pigagent.
var (
	_ agent.AgentEvent = agent.TurnStartEvent{}
	_ agent.AgentEvent = agent.MessageUpdateEvent{}
	_ agent.AgentEvent = agent.MessageEndEvent{}
	_ agent.AgentEvent = agent.ToolExecutionStartEvent{}
	_ agent.AgentEvent = agent.ToolExecutionUpdateEvent{}
	_ agent.AgentEvent = agent.ToolExecutionEndEvent{}
	_ agent.AgentEvent = agent.AgentEndEvent{}
)

// The fields the mapper reads off those events. Reading a field that moved is
// the quiet failure: the frame still renders, with an empty string in it.
var _ = func() {
	var update agent.MessageUpdateEvent
	_, _ = update.AssistantMessageEvent.(ai.TextDeltaEvent)

	var end agent.MessageEndEvent
	_ = end.Message

	var start agent.ToolExecutionStartEvent
	_, _, _ = start.ToolCallID, start.ToolName, start.Args

	// v0.4.0 replaced the streaming field: `Content []ai.ToolResultMessageContent`
	// became `PartialResult AgentToolResult`, which is a complete-so-far
	// snapshot rather than a delta, and it added Args and ParentToolCallID.
	// The mapper reads PartialResult, so the pin names it.
	var mid agent.ToolExecutionUpdateEvent
	_, _, _ = mid.ToolCallID, mid.ToolName, mid.ParentToolCallID
	_ = mid.Args
	var _ []ai.ToolResultMessageContent = mid.PartialResult.Content

	var done agent.ToolExecutionEndEvent
	_, _, _ = done.ToolCallID, done.ToolName, done.Duration
	_ = done.Result.IsError
	_ = done.Result.Content
	_ = done.Result.Text()
}

// ── rpcclient: the node's handle on a `pig --mode rpc` process ─────────────

var _ = rpcclient.RpcClientOptions{
	CliPath:  "",
	Cwd:      "",
	Env:      map[string]string{},
	Provider: "",
	Model:    "",
	Args:     []string{},
}

var _ = rpcclient.JsonAgentSessionEvent{Type: "", Raw: nil}

// The client surface the node supervisor drives. Stop, Wait and GetStderr are
// how a crashed process is observed rather than guessed at; the five RPC
// verbs are the console's whole vocabulary for a remote agent.
var _ = func(ctx context.Context, opts rpcclient.RpcClientOptions) {
	client := rpcclient.NewRpcClient(opts)
	unsubscribe := client.OnEvent(func(rpcclient.JsonAgentSessionEvent) {})
	defer unsubscribe()

	_ = client.Start()
	client.Wait()

	_, _ = client.Prompt("", nil, nil)
	_, _ = client.Steer("", nil)
	_ = rpcclient.PromptDispositionHandled
	_ = rpcclient.QueuedInputDispositionQueued
	_ = rpcclient.StreamingBehaviorSteer
	_ = client.Abort()
	_, _ = client.SetModel("", "")

	state, err := client.GetState()
	_ = err
	_ = state.SessionID
	_ = state.IsStreaming
	_ = state.PendingMessageCount
	if state.Model != nil {
		_, _ = state.Model.ID, state.Model.Provider
	}

	_ = client.GetStderr()
	client.Stop()
}

// ── coding: the release line the node reports ──────────────────────────────

var (
	// PigVersion is re-exported by pigrpc as the fallback for a node's
	// min_pig_version. A rename here would silently change what every node
	// reports about itself, so it is pinned rather than referenced.
	_ string = coding.PigVersion
	// UpstreamVersion is the Pi release PiG targets, reported alongside.
	_ string = coding.UpstreamVersion
)

// ── coding: the embedded SDK the control plane drives its turns on ─────────
//
// This is the largest single block of PiG surface OpsKeeper depends on, and
// it arrived with the SDK driver (decision 86): the control plane builds a
// coding.Session per turn and runs the whole agent loop inside it. Before
// that, the control plane used agent.Agent directly and this file pinned two
// version strings for "coding".
//
// Every field below is one pigcoding.Runtime sets or a SessionKernel sets.
// They are pinned by field, not by type, because the interesting drift is a
// field being renamed or its type widened — the shapes still compile under
// a type alias, and the failure would surface as a session that quietly
// keeps a file on disk instead of failing to start.

var (
	// The container. CWD and AgentDir are required by pigcoding and are set
	// to deployment-owned paths; ProjectTrusted is pinned because it is the
	// one field whose value is a security decision rather than a path.
	_ func(coding.ServicesOptions) (*coding.Services, error) = coding.NewServices
	_ func(coding.RuntimeOptions) (*coding.Runtime, error)   = coding.NewRuntime
	_                                                        = coding.ServicesOptions{
		CWD:             "",
		AgentDir:        "",
		SessionManager:  nil,
		SettingsManager: nil,
		ProjectTrusted:  nil,
	}

	_ = coding.RuntimeOptions{
		Services:      nil,
		AbortContext:  nil,
		NewExtensions: nil,
	}

	// Model resolution. BuildModel is the allowlist-gated entry point
	// pigcoding.Runtime wraps, and the spec string is the wire form an
	// operator reads in the settings table.
	_ func(string, *coding.Services) (*ai.Model, error) = coding.BuildModel
	_                                                   = coding.ScopedModel{}

	// In-memory settings and session logs. These are what make a server
	// stateless on disk, and they are the reason a deployment can run the
	// control plane and the node agent out of one state directory without
	// either writing into an operator's home.
	_ func(coding.Settings) *coding.SettingsManager = coding.NewInMemorySettingsManager
	_ func(string) (*coding.SessionManager, error)  = coding.NewInMemorySessionManager
)

// The session start panel, with every field SessionKernel sets.
//
// The two that are not obvious are pinned here for the same reason they are
// set: NoSession because OpsKeeper's own session table is the transcript of
// record, and SkipBuiltinTools because an operations agent's capabilities
// are its own catalogue — PiG's read/write/edit/bash tools are a
// general-purpose filesystem editor, and on a production node they are an
// attack surface nobody asked for.
var _ = coding.SessionStartOptions{
	Model:                nil,
	SystemPrompt:         "",
	SystemPromptSections: nil,
	BeforeToolCall:       nil,
	ExtraTools:           nil,
	AllowedTools:         nil,
	ExcludedTools:        nil,
	NoTools:              "",
	SkipBuiltinTools:     false,
	SkipExtensionTools:   false,
	NoSession:            false,
	SessionID:            "",
	ThinkingLevel:        ai.ThinkingOff,
	ScopedModels:         nil,
	SessionManager:       nil,
	SessionDir:           "",
	CWDOverride:          nil,
	ResumePath:           "",
}

// The methods a turn calls.
//
// These are call-shaped pins: the body type-checks and never runs, which is
// what lets the method's existence and argument list be pinned without this
// file naming PiG's unexported and structural return types. RunAgentPrompt
// is here rather than Session.Send because the host assembles the opening
// transcript itself — it applies the history window, drops superseded tool
// batches and redacts what a viewer may not read.
var _ = func(rt *coding.Runtime, s *coding.Session, ctx context.Context, ag *agent.Agent) {
	_, _ = rt.New(coding.SessionStartOptions{})
	_ = rt.Close()

	_, _ = s.RunAgentPrompt(ctx, func(context.Context) ([]agent.AgentMessage, error) { return nil, nil })
	_ = s.Events()
	_, _ = s.Send(ctx, "")
	_, _ = s.Steer(ctx, "", nil, nil)
	_ = s.Abort(ctx)
	_ = s.WaitForIdle(ctx)
	_ = s.Messages()
	_ = s.ID()
	_ = s.Close()
	_ = s.Agent()

	run, _ := ag.BeginSendMessages(ctx, nil)
	_, _ = run.Run()

	// The two hook families OpsKeeper installs. Both are APPEND. The
	// difference from SetFinishTurn is the reason the round cap could not
	// use it, and contract_test.go holds that behaviour rather than this
	// shape: see the finish-turn pin there.
	_ = ag.AddBeforeToolCallHook
	_ = ag.AddAfterToolCallHook

	// The barrier a Session's Events consumer must answer, or the run
	// waits at the turn boundary and the turn hangs.
	var _ func(agent.AgentEvent) bool = coding.AcknowledgeEvent
}

// The hook result the policy gate returns, field by field.
//
// Terminate is the one that is easy to lose and expensive to lose: it is
// what makes a spent round cap stop the loop rather than let the model try
// again forever.
var _ = agent.ToolCallHookResult{Block: false, Reason: "", Terminate: false, Args: nil}

// ── coding/piglet: the profile parser the node's security posture rests on ─
//
// The piglet file is what removes PiG's own bash/edit/write tools from a node
// agent, and it is validated with PiG's parser and PiG's ScopeTools rather
// than with a reading of the schema. That validation runs in pigprofile's
// tests; the shapes it uses are pinned here so a parser change lands in this
// module instead of in a red test nobody can attribute.
var (
	_ = func(path string) {
		p, err := piglet.Parse(path)
		_ = err
		if p == nil {
			return
		}
		_, _ = piglet.ResolveExtensions(p)
		_, _ = piglet.ResolveSkills(p)
		_ = piglet.ScopeTools(p, []piglet.ToolInfo{{Name: "", Source: ""}})
	}
	_ = func(body []byte) {
		_, _ = piglet.ParseBytes(body)
	}
)

// ── extensions/sdk: what a PiG extension is written against ────────────────
//
// The five OpsKeeper extension modules are separate Go modules, so each of
// them would fail on its own if this SDK moved. Pinning it here as well is
// deliberate: the SDK is part of the surface OpsKeeper depends on, and a
// contract that covers four of five upstream packages is a contract with a
// hole in the one that is hardest to change, because every extension would
// have to be rebuilt together.
var (
	_ func(string) *sdk.Extension = sdk.New

	_ = sdk.ToolDefinition{
		Name:        "",
		Label:       "",
		Description: "",
		Parameters:  sdk.Schema{},
		Execute:     func(ctx sdk.Context, params map[string]any) (any, error) { return nil, nil },
	}
)

// The two extension entry points: the tool_call event the gate courier
// intercepts, and the session id it forwards as a hint.
var _ = func(ext *sdk.Extension) {
	ext.OnEvent("tool_call", func(ctx sdk.Context, data map[string]any) (any, error) {
		_, _ = data["toolName"].(string)
		_, _ = data["input"].(map[string]any)
		return nil, nil
	})
	ext.RegisterTool(sdk.ToolDefinition{
		Name:       "",
		Parameters: sdk.Schema{},
		Execute: func(ctx sdk.Context, params map[string]any) (any, error) {
			id, err := ctx.GetSessionID()
			_ = id
			_ = err
			return nil, nil
		},
	})
}

// sdk.Schema is decoded from the raw JSON OpsKeeper ships in its extension
// sources, so it must be a JSON target. Pin that fact rather than its shape:
// the shape is PiG's business, the round trip is ours.
var _ = func() {
	var schema sdk.Schema
	_ = json.Unmarshal([]byte(`{"type":"object"}`), &schema)
}
