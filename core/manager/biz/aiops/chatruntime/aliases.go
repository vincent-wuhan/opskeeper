package chatruntime

import "github.com/vincent-wuhan/opskeeper/core/extension/biz/container"

// The plugin package shapes moved to core/extension/biz/container (decision
// 270) and the loader entry points with them. Everything below is a
// re-export, not a copy: a copy is a second thing that can disagree, and the
// one this file replaces was a second thing that did — biz/marketplace used
// to carry its own LoadWarning for exactly that reason, with a comment
// defending it that was not true.
//
// The aliases that are left are here for one reason only: this package and
// its tests still reference them by this package's name, and rewriting them
// would make the move indistinguishable from a rewrite. A caller outside
// this package should import the container package directly — marketplace and
// biz/pluginimport both do, and that is what removed their dependency on the
// chat runtime.
//
// Decision 344 deleted sixteen of the twenty-four. They were re-exports
// nothing referenced: the callers had all moved to `container.X` at the same
// time the rest of the tree did, and these names were left behind as the
// fossil of a move that had already happened. Thirteen of the sixteen were on
// the tree's own deadcode report; `Pack`, `Provenance` and `Requires` were
// not — which is the more interesting half, see §4.277.2.
//
// `TestNoAliasInThisFileIsUnreferenced` keeps the file from rotting back. A
// re-export with no caller is not a convenience: it is a second name for the
// same type, and the next reader has to grep for it before they learn it is
// not the real one.
type (
	// Activation controls when a skill is mounted into the toolBag.
	Activation = container.Activation
	// ToolDecl is a single tool declared by a skill.
	ToolDecl = container.ToolDecl
	// Skill is one SKILL.md as loaded from a package.
	Skill = container.Skill
	// Agent is one agent persona as loaded from a package.
	Agent = container.Agent
	// LoadWarning is a non-fatal load issue.
	LoadWarning = container.LoadWarning
	// ContainerLoader adapts the loader to domain.ContainerLoader.
	ContainerLoader = container.ContainerLoader
	// LoadAllConfig is the input to LoadAll.
	LoadAllConfig = container.LoadAllConfig
)

// The loader functions are re-exported as values rather than as wrappers.
// A wrapper would be a second implementation of the same call; a value is the
// same function, and the only thing given up is the ability to take its
// address in a method expression, which nothing in this repository does.
var (
	// LoadPluginContainer loads a container of a known kind. Its only caller
	// left is a test in biz/marketplace, so it is test-only rather than
	// dead — which is a different claim, and the ratchet keeps the two
	// classes apart for exactly that reason.
	LoadPluginContainer = container.LoadPluginContainer
	// LoadAll walks the skill / agent roots and returns everything found.
	LoadAll = container.LoadAll
)
