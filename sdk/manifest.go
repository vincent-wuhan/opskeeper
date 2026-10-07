// Package sdk is the stable surface third-party OpsKeeper plugins compile
// against. It carries the plugin manifest schema, its loader, and the
// admission rules a manifest must satisfy.
//
// A plugin ships two files side by side. The Pi package manifest stays
// Pi-compatible and is what the agent runtime actually mounts; pig-ops.yaml
// adds only the governance fields Pi has no vocabulary for — where the
// plugin may run, how dangerous it is, and what it needs. This package owns
// the second file and nothing else.
//
// Loading is strict. A manifest that half-parses is worse than one that is
// refused, because the plugin would install and then fail at the moment an
// operator is relying on it.
package sdk

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// ManifestFile is the governance manifest's filename inside a plugin root.
const ManifestFile = "pig-ops.yaml"

// LoadError describes why a manifest was refused. It is deliberately
// structured: a plugin author needs to know which field was wrong, and an
// operator reviewing a submission needs the reason to be quotable.
type LoadError struct {
	// Field is the manifest path of the offending value, e.g.
	// "spec.safety_level". Empty for a whole-document problem.
	Field string
	// Reason is a human-readable explanation.
	Reason string
	Err    error
}

func (e *LoadError) Error() string {
	if e.Field == "" {
		return "pig-ops.yaml: " + e.Reason
	}
	return fmt.Sprintf("pig-ops.yaml: %s: %s", e.Field, e.Reason)
}

func (e *LoadError) Unwrap() error { return e.Err }

func fieldErr(field, reason string) *LoadError {
	return &LoadError{Field: field, Reason: reason}
}

// Decode parses a manifest and runs every admission rule. It is the single
// entry point: callers never parse a manifest without validating it, so a
// rule added here applies everywhere at once.
func Decode(data []byte) (domain.PluginManifest, error) {
	var m domain.PluginManifest

	dec := yaml.NewDecoder(bytes.NewReader(data))
	// KnownFields makes a typo'd key an error rather than a silently
	// ignored setting. A plugin author who misspells `required_scopes`
	// would otherwise ship a plugin the host reads as declaring no scopes
	// at all — which the host treats as "needs no grant".
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			return m, fieldErr("", "file is empty")
		}
		return m, fieldErr("", "not valid: "+err.Error())
	}

	if err := Validate(m); err != nil {
		return m, err
	}
	return m, nil
}

// Validate applies every admission rule to an already-parsed manifest.
//
// The rules fall into three groups:
//
//   - identity: the manifest must say which schema, which kind, and which
//     name and version it is.
//   - safety: the declared level must be a real level, and the declared
//     capabilities must not exceed what that level allows.
//   - governance: the host must not be asked to trust something it cannot
//     constrain. Audit mutation and an over-wide blast radius are refused.
func Validate(m domain.PluginManifest) error {
	if m.APIVersion != domain.PluginAPIVersion {
		return fieldErr("apiVersion", fmt.Sprintf("want %q, got %q", domain.PluginAPIVersion, m.APIVersion))
	}
	if m.Kind != domain.PluginKind {
		return fieldErr("kind", fmt.Sprintf("want %q, got %q", domain.PluginKind, m.Kind))
	}
	if err := validateMeta(m.Metadata); err != nil {
		return err
	}
	if err := validateSpec(m); err != nil {
		return err
	}
	return nil
}

func validateMeta(meta domain.PluginMeta) error {
	if strings.TrimSpace(meta.Name) == "" {
		return fieldErr("metadata.name", "required")
	}
	if strings.TrimSpace(meta.Version) == "" {
		return fieldErr("metadata.version", "required")
	}
	return nil
}

func validateSpec(m domain.PluginManifest) error {
	spec := m.Spec

	if !spec.Targets.Valid() {
		if len(spec.Targets) == 0 {
			return fieldErr("spec.targets", "at least one of edge, manager is required")
		}
		return fieldErr("spec.targets", "must contain only edge or manager")
	}
	if !spec.SafetyLevel.Valid() {
		return fieldErr("spec.safety_level", fmt.Sprintf("want one of L0, L1, L2, L3; got %q", spec.SafetyLevel))
	}
	if !spec.Install.Valid() {
		return fieldErr("spec.install.strategy", fmt.Sprintf("want %q or %q", domain.InstallRolling, domain.InstallPin))
	}
	for i, c := range spec.Capabilities {
		if !c.Valid() {
			return fieldErr(fmt.Sprintf("spec.capabilities[%d]", i), fmt.Sprintf("unknown class %q", c))
		}
	}
	if err := validateTools(m); err != nil {
		return err
	}
	if err := validateAutonomy(m); err != nil {
		return err
	}
	return validateGovernance(m)
}

// validateAutonomy checks the package's request to act without a human.
//
// Every check here is a load error rather than a runtime refusal. The
// asymmetry is the point: a manifest that fails admission is found by the
// person who submitted it, before it reaches a node, while the same
// manifest installed anyway would surface as a self-heal that never fires
// at 03:00 — which is discovered by the incident it was meant to prevent.
//
// The rules are the ones the plan states, and each has a failure it closes:
//
//   - argv is a non-empty bounded vector with nothing in it that only
//     means something to a shell. A string here would be a program.
//   - the reach is pod or narrower. A namespace is an outage with steps.
//   - the TTL is positive and inside the host's cap.
//   - the idempotency key is a template the node can derive, so the key a
//     claim is checked against is one the host produced.
//   - the action names a tool the package actually declares. An autonomy
//     action aimed at an undeclared tool is a request for a capability the
//     package's inventory says it does not have.
func validateAutonomy(m domain.PluginManifest) error {
	policy := m.Spec.Autonomy
	if err := validateAutonomyPolicy(policy); err != nil {
		return err
	}
	if len(policy.Actions) == 0 {
		return nil
	}

	// The action's tool has to be in the inventory, and it has to be a tool
	// this package may run at all. Autonomy is a different *timing* for a
	// capability, never a different capability.
	declared := make(map[string]domain.ToolDecl, len(m.Spec.Tools))
	for _, t := range m.Spec.Tools {
		declared[t.Name] = t
	}
	for i, a := range policy.Actions {
		field := fmt.Sprintf("spec.autonomy.actions[%d]", i)
		tool, ok := declared[a.Tool]
		if !ok {
			return fieldErr(field+".tool",
				fmt.Sprintf("%q is not in this package's tool inventory, so this action may not name it", a.Tool))
		}
		if tool.Class == domain.ClassRead {
			return fieldErr(field+".tool",
				fmt.Sprintf("%q is a read, which needs no autonomy: reads run without a human either way", a.Tool))
		}
	}
	return nil
}

// validateAutonomyPolicy checks the block's own shape.
func validateAutonomyPolicy(policy domain.AutonomyPolicy) error {
	if time.Duration(policy.OfflineAfter) != 0 {
		if !policy.OfflineAfter.Valid() {
			return fieldErr("spec.autonomy.offline_after",
				`must be a positive duration such as "2m"`)
		}
		if time.Duration(policy.OfflineAfter) < domain.MinAutonomyOfflineAfter {
			return fieldErr("spec.autonomy.offline_after",
				fmt.Sprintf("must be at least %s: a threshold at or below the tunnel's reconnect cycle fires the whole list on every blip",
					domain.MinAutonomyOfflineAfter))
		}
	}
	if len(policy.Actions) == 0 {
		if time.Duration(policy.OfflineAfter) != 0 {
			return fieldErr("spec.autonomy.offline_after",
				"declares a threshold with no actions to run under it")
		}
		return nil
	}

	seenName := make(map[string]struct{}, len(policy.Actions))
	seenTool := make(map[string]struct{}, len(policy.Actions))
	for i, a := range policy.Actions {
		field := fmt.Sprintf("spec.autonomy.actions[%d]", i)
		if a.Name == "" {
			return fieldErr(field+".name", "is required")
		}
		if _, dup := seenName[a.Name]; dup {
			return fieldErr(field+".name", fmt.Sprintf("duplicates an earlier action named %q", a.Name))
		}
		seenName[a.Name] = struct{}{}
		if a.Tool == "" {
			return fieldErr(field+".tool", "is required: an action with no tool is a shell command")
		}
		if _, dup := seenTool[a.Tool]; dup {
			return fieldErr(field+".tool",
				fmt.Sprintf("drives %q, which an earlier action already drives; two actions on one tool are one grant wearing two names", a.Tool))
		}
		seenTool[a.Tool] = struct{}{}
		if !a.Trigger.Valid() {
			return fieldErr(field+".trigger",
				fmt.Sprintf("needs a host-evaluated kind (only %q exists) and, for it, a metric name", domain.TriggerMetricAbove))
		}
		if !a.ArgvValid() {
			return fieldErr(field+".argv",
				"must be a non-empty list of literal arguments; a shell string, an empty element or a shell metacharacter is refused")
		}
		if !a.BlastRadius.Valid() {
			return fieldErr(field+".blast_radius", fmt.Sprintf("unknown radius %q", a.BlastRadius))
		}
		if a.BlastRadius.Rank() > domain.RadiusSingleNS.Rank() {
			return fieldErr(field+".blast_radius",
				fmt.Sprintf("%q is wider than the %s an edge self-heal may reach; autonomy that can take a namespace down is an outage, not a heal",
					a.BlastRadius, domain.RadiusSingleNS))
		}
		if !a.TTL.Valid() {
			return fieldErr(field+".ttl", `must be a positive duration such as "30m"`)
		}
		if time.Duration(a.TTL) > domain.MaxAutonomyTTL {
			return fieldErr(field+".ttl",
				fmt.Sprintf("must be at most %s", domain.MaxAutonomyTTL))
		}
		if !domain.KeyTemplateValid(a.IdempotencyKey) {
			return fieldErr(field+".idempotency_key",
				`must be a template with {{target}} and/or {{window}}, e.g. "restart:{{target}}:{{window}}"; a constant key is spent by the first run`)
		}
	}
	return nil
}

// validateTools checks the declared tool inventory.
//
// The inventory is what the host's allow-list is built from, so an entry
// that is malformed or inconsistent with the package's own capability
// declaration is refused here rather than at some later point where the
// only symptom would be a tool refusing to run.
func validateTools(m domain.PluginManifest) error {
	spec := m.Spec
	seen := make(map[string]int, len(spec.Tools))
	for i, t := range spec.Tools {
		field := fmt.Sprintf("spec.tools[%d]", i)
		if strings.TrimSpace(t.Name) == "" {
			return fieldErr(field+".name", "required")
		}
		if !t.Class.Valid() {
			return fieldErr(field+".class", fmt.Sprintf("unknown class %q", t.Class))
		}
		// A negative ceiling is a package that misunderstood the field,
		// and a host that read it as "no ceiling" would enforce nothing
		// while the manifest reads as if it had. Refusing it at load is
		// the only place this can be caught before the node is running.
		if !t.Limits.Valid() {
			return fieldErr(field+".limits",
				fmt.Sprintf("output_bytes=%d timeout_seconds=%d; a limit is never negative",
					t.Limits.OutputBytes, t.Limits.TimeoutSeconds))
		}
		if prev, dup := seen[t.Name]; dup {
			return fieldErr(field+".name",
				fmt.Sprintf("%q is already declared at spec.tools[%d]", t.Name, prev))
		}
		seen[t.Name] = i
		// A tool cannot be more dangerous than the package that ships it.
		// The capability list is the ceiling the package was reviewed
		// against, so a tool above it means the review did not cover it.
		highest := m.HighestCapability()
		if highest != domain.ClassUnknown && t.Class != highest && t.Class.AtLeast(highest) {
			return fieldErr(field+".class",
				fmt.Sprintf("tool is %q but spec.capabilities tops out at %q", t.Class, highest))
		}
	}
	return nil
}

// validateGovernance refuses the two things a plugin must never be able to
// ask the host for. These are not style checks: each one closes a path by
// which a plugin could widen its own authority.
func validateGovernance(m domain.PluginManifest) error {
	spec := m.Spec

	// Audit-ledger writes are not delegable. A manifest requesting them is
	// refused outright rather than downgraded, because a plugin that asked
	// for this does not understand the boundary.
	if spec.Audit.Mutates {
		return fieldErr("spec.audit.mutates",
			"audit-ledger writes are host-only and cannot be delegated to a plugin")
	}

	// A capability beyond the declared level is refused, not clamped. The
	// two failures mean different things to an operator reviewing a
	// submission: one is a manifest that lies about itself, the other is a
	// manifest that is merely ambitious.
	ceiling := spec.SafetyLevel.MinimumClass()
	if worst := m.HighestCapability(); worst.Rank() > ceiling.Rank() {
		return fieldErr("spec.capabilities",
			fmt.Sprintf("declares %q but safety_level %s permits at most %q",
				worst, spec.SafetyLevel, ceiling))
	}

	// A read-only plugin that requests a blast radius has misread what the
	// field is for; accepting it would put a mutation radius in the record
	// for a package that cannot mutate.
	if !spec.SafetyLevel.RequiresApproval() && spec.Approval.MaxBlastRadius != domain.RadiusNone {
		return fieldErr("spec.approval.max_blast_radius",
			fmt.Sprintf("safety_level %s is read-only and cannot carry a blast radius", spec.SafetyLevel))
	}
	if spec.Approval.MaxBlastRadius != domain.RadiusNone && !spec.Approval.MaxBlastRadius.Valid() {
		return fieldErr("spec.approval.max_blast_radius",
			fmt.Sprintf("unknown radius %q", spec.Approval.MaxBlastRadius))
	}
	return nil
}

// Load reads and validates a manifest from a plugin root directory.
func Load(root string) (domain.PluginManifest, error) {
	path := filepath.Join(root, ManifestFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.PluginManifest{}, fmt.Errorf("read %s: %w", path, err)
	}
	m, err := Decode(data)
	if err != nil {
		return m, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// HasGovernanceManifest reports whether a directory carries a manifest. It
// lets a caller distinguish a plugin that forgot the file from a directory
// that was never a plugin.
func HasGovernanceManifest(root string) bool {
	st, err := os.Stat(filepath.Join(root, ManifestFile))
	return err == nil && !st.IsDir()
}

// Admission is the host's decision about one plugin, made against the
// scopes an operator actually granted.
type Admission struct {
	// GrantedScopes is what the operator approved for this plugin on this
	// target. A plugin missing one of its declared scopes is refused.
	GrantedScopes domain.Scopes
	// MaxSafetyLevel is the node's policy ceiling. A plugin above it is
	// refused: a node that cannot host a cluster-scoped plugin must not be
	// handed one.
	MaxSafetyLevel domain.SafetyLevel
	// MaxBlastRadius is the node's policy ceiling for approvals.
	MaxBlastRadius domain.BlastRadius
}

// Admit decides whether a plugin may be installed on a target.
//
// Every clause fails closed. A zero-value Admission refuses everything,
// which is what a host should get if it forgets to populate it.
func Admit(m domain.PluginManifest, a Admission) error {
	// A zero-value Admission must refuse, not admit. SafetyLevel.Rank
	// treats an unrecognised level as L3 so that a manifest typo cannot
	// lower a plugin's privileges; applied to a host-side ceiling the same
	// default would make an unpopulated Admission the most permissive
	// policy in the system. Refuse the unset case explicitly.
	if a.MaxSafetyLevel == "" {
		return fieldErr("admission.max_safety_level",
			"target policy ceiling is unset; refusing every plugin")
	}
	// Satisfies asks "does the argument cover every scope the receiver
	// declares", so the required set must be the receiver. Passing them the
	// other way round inverts the check into "is every granted scope also
	// required", which admits a plugin whose scopes were never granted.
	if !m.Spec.RequiredScopes.Satisfies(a.GrantedScopes) {
		var missing []string
		for _, s := range m.Spec.RequiredScopes {
			if !a.GrantedScopes.Has(s) {
				missing = append(missing, string(s))
			}
		}
		return fieldErr("spec.required_scopes",
			"not granted: "+strings.Join(missing, ", "))
	}
	if m.Spec.SafetyLevel.Rank() > a.MaxSafetyLevel.Rank() {
		return fieldErr("spec.safety_level",
			fmt.Sprintf("plugin is %s, target policy allows at most %s",
				m.Spec.SafetyLevel, a.MaxSafetyLevel))
	}
	if r := m.Spec.Approval.MaxBlastRadius; !r.AtMost(a.MaxBlastRadius) {
		return fieldErr("spec.approval.max_blast_radius",
			fmt.Sprintf("plugin requests %s, target policy allows at most %s",
				r, a.MaxBlastRadius))
	}
	return nil
}
