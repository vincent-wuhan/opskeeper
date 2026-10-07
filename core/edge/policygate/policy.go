package policygate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	floorSkill "github.com/vincent-wuhan/opskeeper/core/floor/skill"
)

// ToolBinding is one tool the host has decided is present, and what it is.
//
// The host builds this from the installed plugin manifests. It is the
// allow-list: a tool that is not bound here does not exist, whatever the
// agent asks for and whatever a plugin's own manifest claims.
type ToolBinding struct {
	Name string
	// Class is the class the manifest's safety level admits for this
	// plugin. It is a ceiling, not the tool's own claim.
	Class domain.ToolClass
	// FromPlugin is the manifest that introduced the tool, so an audit row
	// can name which package put it there.
	FromPlugin string
	// MaxBlastRadius caps what an approval may authorise for this tool.
	// A package cannot widen it by declaration.
	MaxBlastRadius domain.BlastRadius
	// Limits is what this tool declared it may consume. Carried here rather
	// than looked up from the manifest at call time because the registry is
	// the host's own record of what it admitted: a ceiling the broker reads
	// from the same place it reads the class is a ceiling that cannot be
	// satisfied by one object and ignored by the other.
	Limits domain.ToolLimits
}

// Budget returns the tool's output ceiling, with the host default applied.
//
// A zero in the declaration is not zero here. The default is applied at
// lookup rather than at the call site so that "the package declared nothing"
// and "the package declared nothing and nobody applied a default" cannot
// both be true.
func (b ToolBinding) Budget() int64 {
	if b.Limits.OutputBytes <= 0 {
		return floorSkill.DefaultMaxOutputBytes
	}
	return b.Limits.OutputBytes
}

// Timeout returns the tool's wall-clock ceiling, with the broker's global
// default left to the broker: only a declared value is an override, so the
// answer here is zero for "use the global one" and the broker reads that as
// an absence rather than as a zero-second budget.
func (b ToolBinding) Timeout() time.Duration {
	if b.Limits.TimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(b.Limits.TimeoutSeconds) * time.Second
}

// Registry is the host's tool allow-list.
//
// It is built once from what is installed and then read. Rebuilding it is
// an explicit act — a package install or a policy change — so a turn in
// flight is never re-adjudicated against a half-changed list.
type Registry struct {
	tools map[string]ToolBinding
}

// NewRegistry returns an empty registry, which permits nothing.
//
// Empty is the correct starting state rather than a permissive default. A
// node whose packages failed to load must produce an agent that cannot act,
// not one that falls back to allowing what it used to allow.
func NewRegistry() *Registry { return &Registry{tools: map[string]ToolBinding{}} }

// Bind records a tool the host permits.
//
// A tool already bound is not overwritten silently: two packages claiming
// the same name is a packaging mistake, and taking whichever loaded last
// would make the effective class depend on install order.
func (r *Registry) Bind(b ToolBinding) error {
	if b.Name == "" {
		return fmt.Errorf("policygate: a tool must have a name")
	}
	if prev, clash := r.tools[b.Name]; clash {
		return fmt.Errorf("policygate: tool %q is already bound by %q (class %s); %q also claims it (class %s)",
			b.Name, prev.FromPlugin, prev.Class, b.FromPlugin, b.Class)
	}
	if !b.Class.Valid() {
		return fmt.Errorf("policygate: tool %q has an unrecognised class %q", b.Name, b.Class)
	}
	if b.MaxBlastRadius == "" {
		// An unbounded approval is not a safe default for a mutating tool;
		// the host picks the narrowest radius that still covers the class.
		b.MaxBlastRadius = radiusCeiling(b.Class)
	}
	r.tools[b.Name] = b
	return nil
}

// BindAll records several tools, stopping at the first conflict.
func (r *Registry) BindAll(bindings ...ToolBinding) error {
	for _, b := range bindings {
		if err := r.Bind(b); err != nil {
			return err
		}
	}
	return nil
}

// Len reports how many tools are permitted.
func (r *Registry) Len() int { return len(r.tools) }

// Names returns the bound tool names in a stable order.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Lookup returns a tool's binding.
func (r *Registry) Lookup(name string) (ToolBinding, bool) {
	b, ok := r.tools[name]
	return b, ok
}

// Policy narrows a registry to one caller's role.
//
// The role is a ceiling on class, applied at the moment of the call rather
// than at install time, because the same plugin legitimately serves an
// operator and a viewer and the difference is who is asking, not what is
// deployed.
func (r *Registry) Policy(ceiling domain.ToolClass) Policy {
	// An unrecognised or absent ceiling is read-only. A role the host does
	// not understand must not be the most permissive thing in the system.
	if !ceiling.Valid() || ceiling == domain.ClassUnknown {
		ceiling = domain.ClassRead
	}
	return &rolePolicy{registry: r, ceiling: ceiling}
}

// rolePolicy is the registry narrowed to a class ceiling.
type rolePolicy struct {
	registry *Registry
	ceiling  domain.ToolClass
}

// Permitted reports whether the tool may run for this call at all.
//
// Three refusals, in the order an operator would want to hear them: the
// tool is not deployed, this role may not run it, or what the call site
// classified it as is worse than what its package declared.
func (p *rolePolicy) Permitted(c Call) (bool, string) {
	binding, ok := p.registry.Lookup(c.ToolName)
	if !ok {
		return false, fmt.Sprintf("%q is not in this node's tool set; the host permits %s",
			c.ToolName, orNone(p.registry.Names()))
	}
	if underdeclared(binding, c) {
		// The call site saw something worse than the package promised at
		// install time. That is not a judgement call to be adjusted
		// upwards and let through: it is evidence the manifest is wrong,
		// and a manifest that understates a tool cannot be trusted on any
		// other tool it introduced either. Refuse, and say why, so the
		// operator is looking at a discrepancy rather than a strange
		// approval prompt.
		return false, fmt.Sprintf(
			"%q was declared %s by %q but the call site classified it as %s; refusing rather than reclassifying it",
			c.ToolName, binding.Class, binding.FromPlugin, c.Class)
	}
	// The effective class is the worse of what the package declared and
	// what the call site independently assessed. A tool that reports
	// itself as read-only to its own manifest does not get to be read-only.
	effective := effectiveClass(binding.Class, c.Class)
	if !effective.AtMost(p.ceiling) {
		return false, fmt.Sprintf("%q is %s, which exceeds what this role may run (%s)",
			c.ToolName, effective, p.ceiling)
	}
	return true, ""
}

// EffectiveClass implements Policy.
func (p *rolePolicy) EffectiveClass(c Call) domain.ToolClass {
	binding, ok := p.registry.Lookup(c.ToolName)
	if !ok {
		// Unbound, and already refused. An unbound tool has no class to
		// report, and everything downstream reads the zero value as the
		// widest thing it could be.
		return domain.ClassUnknown
	}
	return effectiveClass(binding.Class, c.Class)
}

// MaxRadius implements Policy.
func (p *rolePolicy) MaxRadius(c Call) domain.BlastRadius {
	if binding, ok := p.registry.Lookup(c.ToolName); ok {
		return binding.MaxBlastRadius
	}
	// An unbound tool is refused before its radius is read. Cluster rather
	// than none, so a bug which let one through cannot have landed on the
	// narrow end of the clamp.
	return domain.RadiusCluster
}

// NeedsApproval reports whether a permitted call still requires a human.
//
// Read is observation and needs nobody's permission. Everything else
// changes something, and the change is what the operator is being asked
// about.
func (p *rolePolicy) NeedsApproval(c Call) bool {
	binding, ok := p.registry.Lookup(c.ToolName)
	if !ok {
		// Unreachable through Permitted, and treated as needing approval
		// rather than as free, in case a caller skips that step.
		return true
	}
	return effectiveClass(binding.Class, c.Class).AtLeast(domain.ClassWrite)
}

// Ceiling reports the role ceiling this policy was built with.
func (p *rolePolicy) Ceiling() domain.ToolClass { return p.ceiling }

// StaticPolicy is a policy decided entirely by the caller.
//
// It exists for the built-in tools of the agent itself — read, write, bash
// — which the host knows about without a plugin manifest behind them. It
// is a map rather than a closure so a policy decision is a table lookup
// somebody can read, which matters more here than flexibility: this is the
// thing standing between an agent and a shell.
type StaticPolicy struct {
	// Tools is the tool set and its classes.
	Tools map[string]domain.ToolClass
	// Ceiling is the role ceiling, as above.
	Ceiling domain.ToolClass
}

// NewStaticPolicy returns a static policy, copying the tool table so a
// later mutation of the caller's map cannot widen the gate underneath it.
func NewStaticPolicy(ceiling domain.ToolClass, tools map[string]domain.ToolClass) *StaticPolicy {
	copied := make(map[string]domain.ToolClass, len(tools))
	for name, class := range tools {
		copied[name] = class
	}
	if !ceiling.Valid() || ceiling == domain.ClassUnknown {
		ceiling = domain.ClassRead
	}
	return &StaticPolicy{Tools: copied, Ceiling: ceiling}
}

// Permitted implements Policy.
func (p *StaticPolicy) Permitted(c Call) (bool, string) {
	class, ok := p.Tools[c.ToolName]
	if !ok {
		return false, fmt.Sprintf("%q is not an approved tool on this node", c.ToolName)
	}
	effective := effectiveClass(class, c.Class)
	if !effective.AtMost(p.Ceiling) {
		return false, fmt.Sprintf("%q is %s, which exceeds what this role may run (%s)",
			c.ToolName, effective, p.Ceiling)
	}
	return true, ""
}

// NeedsApproval implements Policy.
// EffectiveClass implements Policy.
func (p *StaticPolicy) EffectiveClass(c Call) domain.ToolClass {
	class, ok := p.Tools[c.ToolName]
	if !ok {
		return domain.ClassUnknown
	}
	return effectiveClass(class, c.Class)
}

// MaxRadius implements Policy.
//
// The built-in tools carry the radius their class implies. There is no
// manifest to narrow it, so this is the whole of the ceiling for them.
func (p *StaticPolicy) MaxRadius(c Call) domain.BlastRadius {
	return radiusCeiling(p.EffectiveClass(c))
}

func (p *StaticPolicy) NeedsApproval(c Call) bool {
	class, ok := p.Tools[c.ToolName]
	if !ok {
		return true
	}
	return effectiveClass(class, c.Class).AtLeast(domain.ClassWrite)
}

// DenyAll is the policy for a node with no tools and no packages.
//
// It exists so "nothing is permitted" is a named thing rather than an
// absent one. A node whose packages failed to load reaches the gate through
// this, and every call it makes is refused with a reason an operator can
// act on.
type DenyAll struct{}

// Permitted implements Policy.
func (DenyAll) Permitted(c Call) (bool, string) {
	return false, fmt.Sprintf("%q cannot run: this node has no tool set configured", c.ToolName)
}

// NeedsApproval implements Policy.
func (DenyAll) NeedsApproval(Call) bool { return false }

// EffectiveClass implements Policy.
func (DenyAll) EffectiveClass(Call) domain.ToolClass { return domain.ClassUnknown }

// MaxRadius implements Policy.
func (DenyAll) MaxRadius(Call) domain.BlastRadius { return domain.RadiusNone }

// radiusCeiling is the widest blast radius a class may be approved for
// without an explicit per-tool setting.
//
// A read reaches one pod because it reads one thing. A write to
// OpsKeeper-managed state is scoped to a namespace because that is the
// unit an operator thinks in. A destructive call is cluster-wide unless
// the operator narrows it, because the alternative is a tool that can
// reach further than the host will admit on its own.
func radiusCeiling(class domain.ToolClass) domain.BlastRadius {
	switch class {
	case domain.ClassRead:
		return domain.RadiusPod
	case domain.ClassWrite:
		return domain.RadiusNamespace
	case domain.ClassDestructive:
		return domain.RadiusCluster
	default:
		return domain.RadiusCluster
	}
}

// orNone renders a list for an error message, or says there is nothing.
func orNone(names []string) string {
	if len(names) == 0 {
		return "no tools"
	}
	if len(names) > 8 {
		// A refusal message listing thirty tools is unreadable, and an
		// unreadable refusal gets skimmed past.
		return strings.Join(names[:8], ", ") + fmt.Sprintf(", and %d more", len(names)-8)
	}
	return strings.Join(names, ", ")
}

// effectiveClass combines a tool's declared class with the host's
// assessment of one call.
//
// The assessment may only raise the class. A zero Class means the host made
// no independent assessment - the ordinary case, where the manifest is the
// only evidence there is - and the declared class stands on its own. That
// is not the same as trusting the manifest: the manifest was checked at
// load against the package's safety level, the tool's class was checked
// against the package's capabilities, and a tool that is in neither is
// refused before this is reached.
func effectiveClass(declared, assessed domain.ToolClass) domain.ToolClass {
	if assessed == domain.ClassUnknown {
		return declared
	}
	return domain.Classify(declared, assessed)
}

// underdeclared reports whether the call site saw a more dangerous call
// than the package's manifest admitted to.
//
// Only the under-declaring direction counts. A package that declares a
// tool more dangerously than it turns out to be is merely conservative,
// and that costs the operator an approval — the cheap direction to be
// wrong in. The other direction is the one that has to stop.
func underdeclared(binding ToolBinding, c Call) bool {
	if !c.Class.Valid() || c.Class == domain.ClassUnknown {
		return false
	}
	return c.Class != binding.Class && c.Class.AtLeast(binding.Class)
}

// RegistryFromManifests builds the host allow-list from the governance
// manifests of the packages a node has installed.
//
// The node's whole security posture rests on this being the only way a
// tool becomes permitted: the agent discovers a package's tools at run time
// from the package itself, and a tool that never passed through here is not
// in the map, so a package that ships a tool it did not declare is refused
// at the gate rather than quietly running.
//
// An empty manifest set produces an empty registry, not a permissive one. A
// node whose packages failed to load then has an agent that can talk and
// cannot act, which is the correct failure and a legible one.
func RegistryFromManifests(manifests []domain.PluginManifest) (*Registry, error) {
	r := NewRegistry()
	for _, m := range manifests {
		name := m.Metadata.Name
		for _, t := range m.Spec.Tools {
			if err := r.Bind(ToolBinding{
				Name:           t.Name,
				Class:          t.Class,
				FromPlugin:     name,
				MaxBlastRadius: m.Spec.Approval.MaxBlastRadius,
				Limits:         t.Limits,
			}); err != nil {
				// Naming both packages matters here: a clash is a
				// packaging problem in one of two trees, and an operator
				// needs to know which.
				return nil, fmt.Errorf("policygate: plugin %q: %w", name, err)
			}
		}
	}
	return r, nil
}
