// Package domain holds the vocabulary every OpsKeeper module speaks: the
// safety classification attached to a tool, a skill, or a whole plugin.
//
// These values are part of the plugin ABI. Changing a constant name or a
// level's meaning is a breaking change for every deployed plugin and must
// therefore be gated the same way a wire field is.
package domain

// ToolClass categorises a capability's blast radius. The runtime filters a
// tool bag by class before a turn starts, so a viewer role can never reach a
// mutating tool regardless of what an agent profile or a plugin asks for.
//
// The zero value is intentionally ClassUnknown rather than ClassRead: an
// unclassified capability must fail closed, never open.
type ToolClass string

const (
	// ClassUnknown is the zero value and is treated as destructive by
	// Classify. A capability that does not declare its class is not trusted
	// to be read-only.
	ClassUnknown ToolClass = ""
	// ClassRead is a pure observation. Touches no external system.
	ClassRead ToolClass = "read"
	// ClassWrite mutates OpsKeeper-managed state (silencing an alert,
	// editing a config draft).
	ClassWrite ToolClass = "write"
	// ClassDestructive mutates state outside OpsKeeper's control: a
	// service restart, a kubectl apply, a SQL statement.
	ClassDestructive ToolClass = "destructive"
)

// Rank orders classes from least to most dangerous. Higher rank wins every
// comparison so a caller can take the max across a set of capabilities
// without a switch.
func (c ToolClass) Rank() int {
	switch c {
	case ClassRead:
		return 1
	case ClassWrite:
		return 2
	case ClassDestructive:
		return 3
	default:
		// ClassUnknown and any unrecognised value rank with
		// destructive. Fail closed.
		return 3
	}
}

// Valid reports whether c is one of the four declared classes.
func (c ToolClass) Valid() bool {
	switch c {
	case ClassRead, ClassWrite, ClassDestructive, ClassUnknown:
		return true
	default:
		return false
	}
}

// String implements fmt.Stringer, rendering the zero value as "unknown"
// rather than the empty string so logs and audit rows stay readable.
func (c ToolClass) String() string {
	if c == ClassUnknown {
		return "unknown"
	}
	return string(c)
}

// Classify returns the most dangerous class across values. It is the
// aggregation rule used when a plugin declares several capabilities or an
// agent profile unions a persona's tools with a policy's allow-list.
func Classify(values ...ToolClass) ToolClass {
	worst := ClassRead
	seen := false
	for _, v := range values {
		seen = true
		if v.Rank() > worst.Rank() {
			worst = v
		}
	}
	if !seen {
		return ClassUnknown
	}
	return worst
}

// AtLeast reports whether c is at least as dangerous as floor.
func (c ToolClass) AtLeast(floor ToolClass) bool { return c.Rank() >= floor.Rank() }

// AtMost reports whether c is no more dangerous than ceiling. It is the
// admission test, and it is stated here rather than at each call site
// because inverting an AtLeast is exactly the place a policy engine
// accidentally opens instead of closes.
func (c ToolClass) AtMost(ceiling ToolClass) bool { return c.Rank() <= ceiling.Rank() }

// SafetyLevel is the coarse, reviewable risk tier an operator assigns to a
// whole plugin or skill package. It is deliberately coarser than ToolClass:
// a human signs off on a SafetyLevel, and the policy engine derives
// ToolClass admission from it.
//
//	L0 — read-only observation, no approval.
//	L1 — read-only plus local evidence collection, no approval.
//	L2 — mutates OpsKeeper state, single approval.
//	L3 — mutates external systems, scoped approval with a blast radius.
type SafetyLevel string

const (
	SafetyL0 SafetyLevel = "L0"
	SafetyL1 SafetyLevel = "L1"
	SafetyL2 SafetyLevel = "L2"
	SafetyL3 SafetyLevel = "L3"
)

// Valid reports whether l is a declared level.
func (l SafetyLevel) Valid() bool {
	switch l {
	case SafetyL0, SafetyL1, SafetyL2, SafetyL3:
		return true
	default:
		return false
	}
}

// Rank orders levels 0..3. An unrecognised level ranks as L3 so a typo in a
// manifest cannot lower a plugin's privileges.
func (l SafetyLevel) Rank() int {
	switch l {
	case SafetyL0:
		return 0
	case SafetyL1:
		return 1
	case SafetyL2:
		return 2
	case SafetyL3:
		return 3
	default:
		return 3
	}
}

// MinimumClass is the most dangerous tool class a plugin at level l may
// expose without an explicit per-tool override. A level is a ceiling on
// capability, so a plugin cannot smuggle a destructive tool past it by
// declaring L0.
func (l SafetyLevel) MinimumClass() ToolClass {
	switch l {
	case SafetyL0, SafetyL1:
		return ClassRead
	case SafetyL2:
		return ClassWrite
	case SafetyL3:
		return ClassDestructive
	default:
		return ClassDestructive
	}
}

// RequiresApproval reports whether a plugin at level l must route any
// mutating call through the host approval gate. L0 and L1 are read-only by
// construction, so they never need an approval round trip.
func (l SafetyLevel) RequiresApproval() bool { return l.Rank() >= SafetyL2.Rank() }

// BlastRadius bounds a destructive capability's reach. It is set by the host
// policy engine from the target the agent resolved, never by the plugin.
type BlastRadius string

const (
	RadiusNone      BlastRadius = ""
	RadiusPod       BlastRadius = "pod"
	RadiusSingleNS  BlastRadius = "single-ns"
	RadiusNamespace BlastRadius = "namespace"
	RadiusCluster   BlastRadius = "cluster"
)

// Valid reports whether r is a declared radius. RadiusNone is valid and
// means the action has no external reach.
func (r BlastRadius) Valid() bool {
	switch r {
	case RadiusNone, RadiusPod, RadiusSingleNS, RadiusNamespace, RadiusCluster:
		return true
	default:
		return false
	}
}

// Rank orders radii from narrowest to widest.
func (r BlastRadius) Rank() int {
	switch r {
	case RadiusNone:
		return 0
	case RadiusPod:
		return 1
	case RadiusSingleNS:
		return 2
	case RadiusNamespace:
		return 3
	case RadiusCluster:
		return 4
	default:
		// An unrecognised radius ranks strictly wider than cluster, so it
		// is refused even under a cluster ceiling. Returning 4 here would
		// let a typo like "cluser" pass as cluster.
		return 5
	}
}

// AtMost reports whether r is within the ceiling. A plugin asking for
// cluster radius is refused unless the host ceiling is also cluster.
func (r BlastRadius) AtMost(ceiling BlastRadius) bool { return r.Rank() <= ceiling.Rank() }
