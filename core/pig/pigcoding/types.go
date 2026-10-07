package pigcoding

import (
	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

// Type re-exports.
//
// OpsKeeper's rule for core/pig is that it is the one module allowed to
// name PiG. These aliases exist so the *rest* of the repository names PiG
// through core/pig rather than importing github.com/MichaelKinsy/PiG from
// twenty directories. They are aliases, not definitions: a field of type
// AgentEvent here IS agent.AgentEvent, so a value crosses this package
// without a conversion and there is no second shape to keep in sync.
//
// What this deliberately does not re-export is any OpsKeeper-shaped wrapper
// around them. Every type below is something a caller could have imported
// directly; the only reason it comes from here is the module boundary.

// AIModel is ai.Model — a resolved provider and model.
type AIModel = ai.Model

// AIStreamOptions is ai.StreamOptions — the per-request knobs a session or
// a direct completion accepts.
type AIStreamOptions = ai.StreamOptions

// AgentEvent is agent.AgentEvent, one frame of the agent loop's stream.
type AgentEvent = agent.AgentEvent

// AgentMessage is agent.AgentMessage, one entry of the loop's transcript.
type AgentMessage = agent.AgentMessage

// AgentTool is agent.AgentTool, the contract OpsKeeper's tool catalogue is
// adapted onto.
type AgentTool = agent.AgentTool

// AgentToolResult is agent.AgentToolResult.
type AgentToolResult = agent.AgentToolResult

// BeforeToolCallHook is agent.BeforeToolCallHook, the seam OpsKeeper's
// approval and policy gates are installed on.
type BeforeToolCallHook = agent.BeforeToolCallHook

// AfterToolCallHook is agent.AfterToolCallHook.
type AfterToolCallHook = agent.AfterToolCallHook

// ThinkingLevel is ai.ThinkingLevel.
type ThinkingLevel = ai.ThinkingLevel

// ToolSchema is ai.ToolSchema, a tool's model-facing declaration.
type ToolSchema = ai.ToolSchema

// Extension is coding/extension.Extension, a PiG extension definition.
type Extension = extension.Extension

// ScopedModel is coding.ScopedModel, an entry in a session's model cycle.
type ScopedModel = coding.ScopedModel

// SettingsManager is coding.SettingsManager, the live settings reader.
type SettingsManager = coding.SettingsManager

// Settings is coding.Settings, the merged settings view PiG resolves
// against. OpsKeeper builds one from its own settings table.
type Settings = coding.Settings

// SessionManager is coding.SessionManager, a session's append-only log.
type SessionManager = coding.SessionManager

// InMemorySettings builds a settings manager that never touches the
// filesystem. OpsKeeper always uses this: model configuration is a row in
// its settings table, and a second copy in a JSON file is a second thing to
// keep in sync and to secure.
func InMemorySettings(initial Settings) *SettingsManager {
	return coding.NewInMemorySettingsManager(initial)
}

// InMemorySessionLog builds an append-only log that never touches the
// filesystem.
func InMemorySessionLog(cwd string) (*SessionManager, error) {
	return coding.NewInMemorySessionManager(cwd)
}
