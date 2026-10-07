// Package chatruntime is the in-process orchestration layer for the opskeeper
// AIOps agent. It owns the per-request capability gate and the runtime that
// feeds a turn; the data shape of a "skill bundle" and an "agent persona" used
// to live here too and now lives in core/extension/biz/container, with aliases
// below so every call site in this package and its tests still spells the name
// it always spelled.
//
// scaffolding only — no kernel, no LLM
// wiring, no tool execution. Old core/floor/skill stays running for compat.
package chatruntime

import "github.com/vincent-wuhan/opskeeper/core/domain"

// ToolClass categorizes a tool's blast radius. Policies filter tools by
// class so the LLM never sees tools that aren't allowed by the channel /
// agent profile. Mirrors sealsuite-agent/internal/chatruntime.ToolClass.
//
// It is an alias, and it is an alias to core/domain rather than to the
// container package, because that enum was declared twice: once here for the
// per-request gate and once in core/domain for the capability the control
// plane grades a declaration on. The two carried the same three spellings and
// no fourth, so a skill's declared class had to be translated between them by
// hand somewhere. There is now one.
type ToolClass = domain.ToolClass

const (
	// ClassRead is read-only — no side effects on the system or device.
	ClassRead = domain.ClassRead
	// ClassWrite mutates state (creates / updates resources) — requires a
	// permissionMode >= mutating-with-confirm.
	ClassWrite = domain.ClassWrite
	// ClassDestructive is irreversible (deletes, restarts, exec-arbitrary)
	// — requires dual-sign-required (SOP)
	ClassDestructive = domain.ClassDestructive
)

// Policy is the per-request capability gate. The HTTP layer / agent
// persona constructs a Policy; ChatRuntime feeds it into the skill and
// tool registries — SkillRegistry.Resolve for the skill side, and the
// catalogue in core/manager/biz/aiops/toolregistry for tool retrieval.
// This file only defines the data shape; the filtering predicates live in
// skill_registry.go and in toolregistry's Catalogue.Filter.
type Policy struct {
	// AllowedClasses lists which ToolClass values are permitted.
	// ["*"] means unrestricted. Empty defaults to read-only (matches
	// sealsuite-agent semantics — see Allows()).
	AllowedClasses []string

	// ConfirmationRequired lists classes that need a two-phase commit
	// before execution. PR-2 defines the field but does not implement
	// the gate (reviewer flow lands in a later PR).
	ConfirmationRequired []string

	// MaxOutputChars caps the final reply length. 0 = no cap.
	MaxOutputChars int

	// MaxIterations bounds the ReAct loop. 0 = use agent default.
	MaxIterations int
}

// Allows reports whether class is permitted under this policy.
// Empty AllowedClasses defaults to read-only (safe default).
func (p Policy) Allows(class ToolClass) bool {
	if len(p.AllowedClasses) == 0 {
		return class == ClassRead
	}
	for _, c := range p.AllowedClasses {
		if c == "*" || c == string(class) {
			return true
		}
	}
	return false
}

// RequiresConfirmation reports whether class must pass a confirmation
// gate (permissionMode = mutating-with-confirm).
func (p Policy) RequiresConfirmation(class ToolClass) bool {
	for _, c := range p.ConfirmationRequired {
		if c == "*" || c == string(class) {
			return true
		}
	}
	return false
}
