package domain

// PluginAPIVersion is the schema version of pig-ops.yaml. The loader rejects
// a manifest whose apiVersion it does not recognise, so a manifest written
// for a future host never half-loads against an older host.
const PluginAPIVersion = "opskeeper.io/v1"

// PluginKind is the only kind this schema defines.
const PluginKind = "Plugin"

// DeploymentTarget names where a plugin is allowed to run. A plugin listing
// both targets ships its agent-side package to the node fleet and its
// control-plane half to the manager.
type DeploymentTarget string

const (
	// TargetEdge runs inside the per-node pig process, as PiG extensions,
	// skills, and MCP servers.
	TargetEdge DeploymentTarget = "edge"
	// TargetManager runs in the control plane.
	TargetManager DeploymentTarget = "manager"
)

// Targets is the declared set of deployment locations.
type Targets []DeploymentTarget

// Has reports whether t is in the set.
func (ts Targets) Has(t DeploymentTarget) bool {
	for _, v := range ts {
		if v == t {
			return true
		}
	}
	return false
}

// Valid reports whether every entry is a declared target and the set is
// non-empty. A manifest with no targets is refused: it would otherwise
// install successfully and run nowhere.
func (ts Targets) Valid() bool {
	if len(ts) == 0 {
		return false
	}
	for _, v := range ts {
		if v != TargetEdge && v != TargetManager {
			return false
		}
	}
	return true
}

// Scope is a permission a plugin declares it needs. The host injects
// credentials for exactly these scopes and nothing else, so a plugin cannot
// reach a credential it did not declare.
type Scope string

// Scopes used by the first-party plugins. Third parties may declare their
// own namespaced scopes; the host treats an unknown scope as unsatisfied
// rather than permissive.
const (
	ScopeHostRead   Scope = "host.read"
	ScopeHostWrite  Scope = "host.write"
	ScopeK8sRead    Scope = "k8s.read"
	ScopeK8sExec    Scope = "k8s.exec"
	ScopeDBRead     Scope = "db.read"
	ScopeDBWrite    Scope = "db.write"
	ScopeMQRead     Scope = "mq.read"
	ScopeMQWrite    Scope = "mq.write"
	ScopeMetricsRO  Scope = "observability.read"
	ScopeTopologyRO Scope = "topology.read"
	ScopeAlertRO    Scope = "alert.read"
	ScopeAlertWrite Scope = "alert.write"
)

// Scopes is a declared permission set.
type Scopes []Scope

// Has reports whether s is in the set.
func (ss Scopes) Has(s Scope) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// Satisfies reports whether granted covers every scope in ss. This is the
// host's admission check: it is the only place a plugin's declared needs are
// compared against what the operator actually granted.
func (ss Scopes) Satisfies(granted Scopes) bool {
	for _, s := range ss {
		if !granted.Has(s) {
			return false
		}
	}
	return true
}

// AuditPolicy declares a plugin's relationship to the audit ledger. It is a
// declaration, not a grant: only the host writes the ledger, and no manifest
// field can widen that.
type AuditPolicy struct {
	// Emits is true when the plugin reports tool activity the host should
	// record. The host derives the ledger entry from its own gate events, so
	// a plugin cannot forge one.
	Emits bool `json:"emits" yaml:"emits"`
	// Mutates is a declaration of intent. It MUST be false; a manifest that
	// sets it true is rejected at load, because audit-ledger writes are not
	// delegable to a plugin.
	Mutates bool `json:"mutates" yaml:"mutates"`
}

// ApprovalPolicy declares how a plugin's mutating calls reach a human.
type ApprovalPolicy struct {
	// Required forces every mutating call through the host approval gate.
	Required bool `json:"required" yaml:"required"`
	// MaxBlastRadius is the widest reach the host may approve. The host
	// clamps to the narrower of this and the node's policy ceiling; a
	// plugin cannot widen it by declaration.
	MaxBlastRadius BlastRadius `json:"max_blast_radius,omitempty" yaml:"max_blast_radius,omitempty"`
}

// InstallPolicy declares how the host rolls the plugin out.
type InstallPolicy struct {
	// Strategy is "rolling" (drain and replace node by node) or "pin"
	// (install once, never auto-upgrade).
	Strategy string `json:"strategy,omitempty" yaml:"strategy,omitempty"`
	// MinEdgeVersion is the lowest node-agent version that can host this
	// plugin. A node below it is skipped during a rolling install rather
	// than being handed a package it cannot run.
	MinEdgeVersion string `json:"min_edge_version,omitempty" yaml:"min_edge_version,omitempty"`
	// MinPigVersion is the lowest PiG agent build that can host this
	// plugin. It is the other half of the compatibility matrix: a package
	// that uses a tool-registration API, a hook name or an event field
	// that its PiG version does not yet have installs cleanly and fails
	// the first time a turn needs it.
	//
	// It is a separate field from MinEdgeVersion because the two move
	// independently. A node fleet is upgraded on one cadence and the
	// agent binary inside it on another, so "the edge is new enough" and
	// "the agent is new enough" are different questions with different
	// fixes.
	MinPigVersion string `json:"min_pig_version,omitempty" yaml:"min_pig_version,omitempty"`
}

// Install strategy values.
const (
	InstallRolling = "rolling"
	InstallPin     = "pin"
)

// Valid reports whether the strategy is empty or a declared value.
func (i InstallPolicy) Valid() bool {
	return i.Strategy == "" || i.Strategy == InstallRolling || i.Strategy == InstallPin
}

// PluginMeta is the manifest identity block.
type PluginMeta struct {
	Name     string `json:"name" yaml:"name"`
	Version  string `json:"version" yaml:"version"`
	Vendor   string `json:"vendor,omitempty" yaml:"vendor,omitempty"`
	Homepage string `json:"homepage,omitempty" yaml:"homepage,omitempty"`
	// Signature is the host's admission record: the digest of the package
	// contents the manager approved. It is written by the control plane,
	// not by the plugin author.
	Signature string `json:"signature,omitempty" yaml:"signature,omitempty"`
}

// PluginSpec is the manifest's behaviour block.
type PluginSpec struct {
	Targets        Targets        `json:"targets" yaml:"targets"`
	SafetyLevel    SafetyLevel    `json:"safety_level" yaml:"safety_level"`
	Capabilities   []ToolClass    `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Tools          Tools          `json:"tools,omitempty" yaml:"tools,omitempty"`
	RequiredScopes Scopes         `json:"required_scopes,omitempty" yaml:"required_scopes,omitempty"`
	Audit          AuditPolicy    `json:"audit" yaml:"audit"`
	Approval       ApprovalPolicy `json:"approval,omitempty" yaml:"approval,omitempty"`
	Install        InstallPolicy  `json:"install,omitempty" yaml:"install,omitempty"`
	// Autonomy is what this package may do with no human present, while
	// the control plane is unreachable. It is the only field in the
	// manifest that can produce a mutating call nobody approved, and it is
	// therefore the one that says the most about a package: a package with
	// an empty block here is a package that behaves identically whether or
	// not the network is up.
	//
	// It is a *block*, not a flag. The actions are enumerated, so review is
	// a reading of concrete argv rather than an assessment of an intent,
	// and a package cannot acquire autonomy by being trusted.
	Autonomy AutonomyPolicy `json:"autonomy,omitempty" yaml:"autonomy,omitempty"`
}

// ToolDecl is one tool a plugin declares it will register with the agent.
//
// This is the field that makes the host's allow-list possible. The agent
// discovers a package's tools at run time, but the host has to know which
// tools exist *before* the agent starts in order to decide whether any of
// them may run — so the inventory is declared, and a tool the agent
// produces that is not on this list is refused rather than judged on the
// spot.
type ToolDecl struct {
	// Name is the tool identifier the agent will present. It must match
	// the name the extension registers exactly; the host compares names,
	// not prefixes, so a plugin cannot widen a bound tool's reach by
	// registering a near neighbour.
	Name string `json:"name" yaml:"name"`
	// Class is what this tool is allowed to do. The host treats it as a
	// ceiling for the calls the tool makes, never as a claim about them:
	// the call site is free to classify an individual call worse, and a
	// tool whose observed behaviour is worse than its declaration is
	// refused at the gate.
	Class ToolClass `json:"class" yaml:"class"`
	// Limits is what this tool may consume. Optional, and the host applies
	// its own default to a tool that declares nothing — so omitting it is
	// never "unlimited", only "whatever the host's default is".
	Limits ToolLimits `json:"limits,omitempty" yaml:"limits,omitempty"`
}

// ToolLimits is the per-tool resource ceiling the host enforces on the node.
//
// Both fields are enforced by host code, on the node, at the tool broker —
// the one place every tool call passes through regardless of which package
// implements it. A limit declared and not enforced would be worse than no
// limit at all, because a review would read it as a guarantee.
//
// # Why output bytes and not memory
//
// The plan this schema came from asked for `limits.memory` and
// `limits.output_bytes`. Output bytes is here; memory is not, and the reason
// is that a memory ceiling on a skill that runs **in the edge process** is not
// enforceable from here: the allocation has already happened by the time this
// declaration is read. Bounding it needs the skill to run somewhere it can be
// killed from outside — a subprocess with an rlimit — which is the sandbox
// work in stage 1, not a manifest field. Declaring `memory` today would be a
// field that reads as a guarantee and enforces nothing, and this repository
// has spent several decisions deleting exactly that shape.
type ToolLimits struct {
	// OutputBytes is the largest tool reply the host will hand back. Beyond
	// it the reply is replaced by a truncation notice carrying the full
	// size, the limit, and — when the host could spill it — a path the
	// model can ask another tool to read. 0 means "the host's default",
	// which exists for the same reason this field does.
	OutputBytes int64 `json:"output_bytes,omitempty" yaml:"output_bytes,omitempty"`
	// TimeoutSeconds is the wall-clock ceiling for one call of this tool.
	// 0 means the broker's global ceiling.
	//
	// It is per tool because the tools that need minutes — host_sosreport,
	// host_strace on a busy process — and the tools that need seconds are
	// the same kind of tool, and one global number has to be the larger of
	// them.
	TimeoutSeconds int `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
}

// 这个结构**没有** memory 字段，而且缺席是判定的结果，不是遗漏。
//
// 计划里写的是「per-tool 内存/输出上限，否则 PB 级数据会爆 context」——
// 两个字段，一个后果。已落地的 output_bytes 解决的就是这句话写下的那个后果：
// 爆的是模型的 context window，而那是回复体积的问题，broker 在 tool socket
// 的宿主侧按包截断并落盘（toolbroker.replyFor / skill.Spill）。
//
// 内存是另一回事，而它**在当前的隔离粒度下无法按 tool 执行**：
// `core/edge/plugins.SubprocessPlugin.runOnce` 是每个**插件**起一个受监管的
// 进程（崩溃后退避重启），不是一个 tool 一个进程。同一个扩展里的八个
// host_* 工具共享同一个地址空间，所以一个 "host_strace 最多 256 MiB" 的字段
// 没有可以施加它的对象——真要施加，得到的是八个工具共用的一个上限，
// 而 manifest 上写着的是 per tool。**一个兑现不了的 per-tool 承诺，
// 比没有这个字段更糟**：包作者会照着它调大工具的查询范围，而宿主并不执行。
//
// 真的要有内存上限，条件是隔离粒度先变成**每次调用一个进程**，
// 或者宿主改用 cgroup / systemd-run 给整个扩展设上限——后者是 per-extension，
// 不是 per-tool，字段名得跟着改。这两件都不在本仓当前形态里，
// 所以字段不加，理由留在这里：**加它的人应该先读这段，而不是先看计划。**
//
// `pluginmanifest/limits_test.go` 里的 TestTheToolLimitVocabularyIsClosed
// 会在有人加字段时立刻失败，并在失败信息里把这三件事一起说出来。

// Valid reports whether the limits are expressible.
//
// Negative is invalid rather than "unlimited": a manifest that says
// output_bytes: -1 is a package that misunderstood the field, and reading it
// as a licence to return anything is how a ceiling stops being one.
func (l ToolLimits) Valid() bool {
	return l.OutputBytes >= 0 && l.TimeoutSeconds >= 0
}

// Tools is a declared tool inventory.
type Tools []ToolDecl

// Names returns the declared tool names in declaration order.
func (ts Tools) Names() []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

// Has reports whether the inventory declares the named tool.
func (ts Tools) Has(name string) bool {
	for _, t := range ts {
		if t.Name == name {
			return true
		}
	}
	return false
}

// HighestClass returns the most dangerous class the inventory declares.
func (ts Tools) HighestClass() ToolClass {
	if len(ts) == 0 {
		return ClassUnknown
	}
	classes := make([]ToolClass, 0, len(ts))
	for _, t := range ts {
		classes = append(classes, t.Class)
	}
	return Classify(classes...)
}

// PluginManifest is the parsed pig-ops.yaml.
//
// The Pi package manifest alongside it stays Pi-compatible: this file adds
// only the governance fields Pi has no vocabulary for. Loading a plugin
// means reading both, and the package manifest is what the agent runtime
// actually mounts.
type PluginManifest struct {
	APIVersion string     `json:"apiVersion" yaml:"apiVersion"`
	Kind       string     `json:"kind" yaml:"kind"`
	Metadata   PluginMeta `json:"metadata" yaml:"metadata"`
	Spec       PluginSpec `json:"spec" yaml:"spec"`
}

// HighestCapability returns the most dangerous class the plugin declares.
// An empty declaration yields ClassUnknown, which the admission check
// treats as destructive.
func (m PluginManifest) HighestCapability() ToolClass {
	if len(m.Spec.Capabilities) == 0 {
		return ClassUnknown
	}
	return Classify(m.Spec.Capabilities...)
}
