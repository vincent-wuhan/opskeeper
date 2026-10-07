package pluginmanifest

import (
	"errors"
	"fmt"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// Comparing the version a package needs against the version a node runs.
//
// This is the PiG × edge compatibility matrix, reduced to the one edge of
// it a node can check for itself. A package declares min_edge_version; the
// node knows what it is; the comparison happens on the node rather than in
// the manager, for the same reason the review does — the manager may ask
// for a package, but only the node can say whether *it* can run it.
//
// The comparison itself lives in core/domain, because the SDK has to run
// the same check for a plugin author at build time and the SDK reaches core
// and nothing else internal. What stays here is the node's *policy*: which
// refusals are fatal, and what an operator is told. Keeping the arithmetic
// in one place is what stops the build-time answer and the install-time
// answer from disagreeing — a disagreement whose only symptom is a package
// that passes its author's check and is refused on a node.

// CompareVersions compares two dotted numeric versions.
//
// It returns -1 when a < b, 0 when they are equal, and 1 when a > b. The
// second return is false when either input is not a dotted numeric version
// — "dev", "", "0.8.0-rc1", "1.2.x". Callers must treat that as "cannot
// tell", which for an admission check means refuse: a node that guessed
// here would either run a package it cannot host or refuse one it can, and
// only one of those is recoverable.
func CompareVersions(a, b string) (int, bool) {
	av, err := domain.ParseVersion(a)
	if err != nil {
		return 0, false
	}
	bv, err := domain.ParseVersion(b)
	if err != nil {
		return 0, false
	}
	return domain.Compare(av, bv), true
}

// MeetsMinEdgeVersion reports whether a node running nodeVersion may host a
// package that requires minEdgeVersion.
func MeetsMinEdgeVersion(minEdgeVersion, nodeVersion string) (bool, string) {
	return meetsVersion(domain.AxisEdge, minEdgeVersion, nodeVersion,
		"this node reports its agent version as %q, which is not a version, so it cannot tell whether it is new enough for a package needing %s",
		"edge")
}

// MeetsMinPigVersion reports whether a node whose agent binary is pigVersion
// may host a package that requires minPigVersion.
//
// It is the second axis of the same matrix as MeetsMinEdgeVersion, and it
// is deliberately a separate function rather than a table-driven loop over
// two requirements: the two sides differ in what an unreadable value means.
// A node that cannot state its *edge* version has a provisioning problem;
// a node that cannot state its *agent* version has a different one, and the
// refusal has to name which, or an operator upgrades the wrong component.
//
// pigVersion is the version of the `pig` binary this node launches, not the
// version of the edge agent around it. They are reported by different
// things — the edge from its build metadata, the agent from whatever
// answers on its stdio — and a node that conflated them would enforce a
// requirement against a number that has nothing to do with it.
func MeetsMinPigVersion(minPigVersion, pigVersion string) (bool, string) {
	return meetsVersion(domain.AxisPig, minPigVersion, pigVersion,
		"this node reports its PiG agent version as %q, which is not a version, so it cannot tell whether it is new enough for a package needing %s",
		"PiG")
}

// CheckVersions runs both axes of the compatibility matrix, in the order
// Review runs them, and answers with the same step and the same sentence.
//
// It exists so the control plane can ask the question before it starts a
// release rather than finding out node by node. That is only safe if both
// sides get the same answer, and "the same answer" is a property that
// cannot be maintained by two implementations agreeing today — it has to
// be one implementation called twice. So Review calls this, and so does the
// manager's pre-flight, and a change to the wording or the ordering lands
// in both at once.
//
// The ordering is load-bearing and is the node's, not the manager's: the
// edge build is checked first because a node too old to run the package at
// all is the more actionable answer, and because a node that fails both is
// fixed by the same upgrade in most fleets, so naming the edge first sends
// the operator to the component that will actually clear the refusal.
func CheckVersions(minEdgeVersion, minPigVersion, nodeVersion, pigVersion string) (ok bool, step, reason string) {
	if ok, reason := MeetsMinEdgeVersion(minEdgeVersion, nodeVersion); !ok {
		return false, StepVersion, reason
	}
	if ok, reason := MeetsMinPigVersion(minPigVersion, pigVersion); !ok {
		return false, StepAgentVersion, reason
	}
	return true, "", ""
}

// meetsVersion runs one axis and converts the domain-layer refusal into the
// node's operator-facing wording.
//
// The host-unreadable case gets a component-specific sentence because that
// is the case where an operator has something to do; the requirement-unreadable
// case is the package author's mistake on either axis and reads the same.
func meetsVersion(axis, minimum, host, hostUnknownFormat, component string) (bool, string) {
	// An empty requirement means the package did not say, and the answer
	// is yes: refusing every package that omits an optional field would
	// make the field mandatory by accident.
	if strings.TrimSpace(minimum) == "" {
		return true, ""
	}
	req := domain.VersionRequirement{Axis: axis, Min: minimum, Host: host}
	ok, err := req.Satisfied()
	if ok {
		return true, ""
	}
	// errors.As rather than a type assertion: these errors are the
	// domain layer's, and a future revision that wrapped them (to add
	// context, say) would break a bare assertion silently — the branch
	// would fall through and the operator would get the generic message
	// instead of the one naming which component to upgrade.
	var unreadable *domain.UnreadableVersionError
	if errors.As(err, &unreadable) {
		if unreadable.Side == "host" {
			return false, fmt.Sprintf(hostUnknownFormat, host, minimum)
		}
		return false, fmt.Sprintf("the package asks for %s version %q, which is not a version this node can compare", component, minimum)
	}
	var tooOld *domain.VersionTooOldError
	if errors.As(err, &tooOld) {
		return false, fmt.Sprintf("this node runs %s %s, but the package needs at least %s", component, host, minimum)
	}
	return false, err.Error()
}
