package sdk

import (
	"fmt"
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// Tool registration: the author's half of the host's allow-list.
//
// A package's manifest declares an inventory of tools, and the node builds
// its allow-list from that list: a tool the agent produces that is not
// named there is refused on every turn. The declaration is load-bearing,
// and it is a hand-written YAML list — which means the two ways it can be
// wrong are both silent.
//
//   - A tool the extension registers but the manifest omits: the model is
//     offered a tool that the gate refuses every time it is called. It
//     looks like a broken tool, not like a missing line in a YAML file.
//   - A tool the manifest declares but the extension never registers: the
//     node reserves the name, publishes it in the review surface, and no
//     call can ever succeed. A reviewer approved a capability that does
//     not exist.
//
// Both are order-one mistakes and both are invisible at run time, so the
// check has to happen before the package ships. This file is that check,
// written once in the SDK so every plugin author can run it without
// depending on OpsKeeper's internals — which is the whole reason the SDK
// exists.
//
// It is not a replacement for the node's own check. The node cannot trust
// a package to have run its build-time guard, and a package that skips it
// is indistinguishable from one that passed; the host-side enforcement is
// in core/edge/policygate against the manifest. This is the same invariant
// checked earlier, where the fix is cheap.

// Registry is a plugin's declared tool inventory, as the plugin itself
// sees it.
//
// A plugin builds one at start-up from the same table it registers tools
// from, then asks it to check the manifest. Building it from the
// registration table rather than from the YAML is the point: the YAML is
// what gets reviewed, and the table is what runs, and only a check that
// reads both can notice they differ.
type Registry struct {
	// name is the plugin name, used in messages.
	name string
	// byName is the declared set.
	byName map[string]domain.ToolClass
	// limits is what each tool says it needs. Kept beside byName rather
	// than inside it because the two are compared and reported separately:
	// a class mismatch is a question about authority, a limit mismatch is a
	// question about capacity, and an author fixing one does not know they
	// broke the other.
	limits map[string]domain.ToolLimits
	// order preserves registration order so a message can name the first
	// offending tool rather than an arbitrary one.
	order []string
}

// NewRegistry starts an inventory for a named plugin.
func NewRegistry(name string) *Registry {
	return &Registry{
		name:   name,
		byName: map[string]domain.ToolClass{},
		limits: map[string]domain.ToolLimits{},
	}
}

// Register records one tool.
//
// A class of ClassUnknown is refused rather than defaulted to read: the
// zero value's whole purpose is to fail closed, and a registry that
// silently promoted it would make every unclassified tool look safe in the
// author's own check while the host's Classify treated it as destructive.
// The two would then disagree about the same tool, which is the exact
// failure this file exists to prevent.
func (r *Registry) Register(name string, class domain.ToolClass) error {
	return r.RegisterWithLimits(name, class, domain.ToolLimits{})
}

// RegisterWithLimits is Register plus the resource ceiling the tool needs.
//
// The limits are what the *code* expects; the manifest is what the host
// enforces. Registering a ceiling and declaring none is allowed — the host's
// default applies — but Check reports the difference, because a tool written
// against a 64 MiB reply and served a 1 MiB one fails in production with a
// truncation notice the author never saw.
func (r *Registry) RegisterWithLimits(name string, class domain.ToolClass, limits domain.ToolLimits) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%s: a tool needs a name", r.pluginName())
	}
	if !class.Valid() || class == domain.ClassUnknown {
		return fmt.Errorf("%s: tool %q declares class %q; declare read, write or destructive",
			r.pluginName(), name, class)
	}
	if !limits.Valid() {
		return fmt.Errorf("%s: tool %q declares output_bytes=%d timeout_seconds=%d; a limit is never negative",
			r.pluginName(), name, limits.OutputBytes, limits.TimeoutSeconds)
	}
	if prev, dup := r.byName[name]; dup {
		return fmt.Errorf("%s: tool %q is registered twice (first as %q, now as %q)",
			r.pluginName(), name, prev, class)
	}
	r.byName[name] = class
	r.limits[name] = limits
	r.order = append(r.order, name)
	return nil
}

// Limits returns a registered tool's declared ceilings.
func (r *Registry) Limits(name string) (domain.ToolLimits, bool) {
	l, ok := r.limits[name]
	return l, ok
}

// MustRegister is Register for table literals in a package's own source.
//
// It panics, and that is correct for the same reason the PiG extension's
// schema decoder panics: the arguments are literals in the file being
// compiled, so a duplicate or an unclassified tool is an author-time
// mistake. A package that panicked at start-up would be caught by its own
// tests and by the node's first load; a package that silently dropped the
// tool would ship a capability the manifest claims and the code does not
// have.
func (r *Registry) MustRegister(name string, class domain.ToolClass) {
	if err := r.Register(name, class); err != nil {
		panic(err)
	}
}

// Names returns the registered names, sorted.
//
// Sorted because this is compared against a manifest whose list is sorted
// for review; an order-sensitive comparison would report a diff where
// there is none.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for name := range r.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Class returns a registered tool's class.
func (r *Registry) Class(name string) (domain.ToolClass, bool) {
	c, ok := r.byName[name]
	return c, ok
}

// Len reports how many tools are registered.
func (r *Registry) Len() int { return len(r.byName) }

// HighestClass returns the most dangerous class in the inventory.
func (r *Registry) HighestClass() domain.ToolClass {
	classes := make([]domain.ToolClass, 0, len(r.byName))
	for _, c := range r.byName {
		classes = append(classes, c)
	}
	return domain.Classify(classes...)
}

// ToolDecls renders the inventory as manifest declarations, sorted by name.
//
// This is what makes the declared list generated rather than transcribed.
// An author can write the YAML once, by running a generator that calls
// this, and then the only way the two drift is if the manifest is edited by
// hand afterwards — which Check reports.
func (r *Registry) ToolDecls() domain.Tools {
	names := r.Names()
	out := make(domain.Tools, 0, len(names))
	for _, name := range names {
		out = append(out, domain.ToolDecl{
			Name:   name,
			Class:  r.byName[name],
			Limits: r.limits[name],
		})
	}
	return out
}

// Check verifies the registry against a manifest.
//
// Every failure is reported, not just the first: an author fixing a
// nine-tool package wants the list, not nine runs.
//
// The three checks are the three ways the two can disagree, and they fail
// for different reasons, so they are reported differently:
//
//   - a tool registered but not declared is a call the node will refuse;
//   - a tool declared but not registered is a capability that does not
//     exist;
//   - a tool whose class differs is the dangerous one, because the class
//     decides whether a human is asked before a live system changes, and
//     the manifest's value is the one the gate uses.
func (r *Registry) Check(m domain.PluginManifest) error {
	var failures []string

	declared := make(map[string]domain.ToolClass, len(m.Spec.Tools))
	for _, t := range m.Spec.Tools {
		declared[t.Name] = t.Class
	}

	var undeclared []string
	for _, name := range r.Names() {
		if _, ok := declared[name]; !ok {
			undeclared = append(undeclared, name)
		}
	}
	if len(undeclared) > 0 {
		failures = append(failures, fmt.Sprintf(
			"these tools are registered but not declared in spec.tools, so the node will refuse every call to them: %s",
			strings.Join(undeclared, ", ")))
	}

	var unregistered []string
	for _, t := range m.Spec.Tools {
		if _, ok := r.byName[t.Name]; !ok {
			unregistered = append(unregistered, t.Name)
		}
	}
	if len(unregistered) > 0 {
		sort.Strings(unregistered)
		failures = append(failures, fmt.Sprintf(
			"these tools are declared in spec.tools but this package never registers them, so the review approved a capability that cannot be called: %s",
			strings.Join(unregistered, ", ")))
	}

	for _, t := range m.Spec.Tools {
		actual, ok := r.byName[t.Name]
		if !ok || actual == t.Class {
			continue
		}
		failures = append(failures, fmt.Sprintf(
			"tool %q is declared %q in spec.tools and registered as %q; the manifest is what the gate enforces, so the declaration decides whether an operator is asked before this runs",
			t.Name, t.Class, actual))
	}

	for _, t := range m.Spec.Tools {
		want, ok := r.limits[t.Name]
		if !ok || want == t.Limits {
			continue
		}
		failures = append(failures, fmt.Sprintf(
			"tool %q is registered with output_bytes=%d timeout_seconds=%d and declared as output_bytes=%d timeout_seconds=%d; the manifest is what the host enforces, so the declared numbers are the ones in force",
			t.Name, want.OutputBytes, want.TimeoutSeconds, t.Limits.OutputBytes, t.Limits.TimeoutSeconds))
	}

	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %s", r.pluginName(), strings.Join(failures, "\n  - "))
}

// DeclaredManifest returns m with spec.tools replaced by this registry's
// inventory.
//
// It is the generator half: an author runs it once, writes the result into
// pig-ops.yaml, and thereafter Check keeps the pair honest. It deliberately
// does not touch capabilities or safety_level — those are the operator's
// judgement about the package, not a restatement of what the code
// registers, and generating them would turn a review decision into a
// derivation.
func (r *Registry) DeclaredManifest(m domain.PluginManifest) domain.PluginManifest {
	m.Spec.Tools = r.ToolDecls()
	return m
}

func (r *Registry) pluginName() string {
	if strings.TrimSpace(r.name) == "" {
		return "plugin"
	}
	return r.name
}
