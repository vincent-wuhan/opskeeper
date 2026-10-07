// Version vocabulary for the plugin ABI.
//
// It lives in domain rather than beside the manifest loader because two
// modules need it and only one of them can reach the other: the node
// enforces a package's requirements during admission, and a plugin author
// checks the same requirements at build time through the SDK. The SDK
// depends on core and nothing else internal, so a comparison implemented
// in the loader would either be duplicated into the SDK or left out of it —
// and a plugin author who cannot run the check ships a package whose first
// failure is on somebody's node.
//
// This file is therefore the single comparison in the repository. See
// ParseVersion for why it is not semver.
package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a dotted numeric version as this fleet writes them.
//
// It is a type rather than a bare string so that "is this a version" is
// answered once, at the boundary, instead of at every comparison. A
// Version that exists is a Version that parsed.
type Version struct {
	// Raw is the string as written, kept for messages: an operator
	// comparing a refusal against a build log wants to see the same
	// characters they typed, not a normalised form.
	Raw   string
	parts []int
}

// ParseVersion parses a dotted numeric version.
//
// Deliberately not semver. The versions on this fleet come from build
// metadata: "0.8.0", "0.7.43", and on a developer machine "dev". A semver
// library would reject the last one, and the honest answer for it is
// "unknown, so refuse" — not "assume it is old". Pre-release tags would add
// a rule no version in this repository uses, and a rule nobody exercises is
// a rule nobody can trust.
//
// Missing components count as zero, so "0.8" and "0.8.0" are equal: that is
// what a person writing "0.8" means, and the alternative — treating a short
// version as unparseable — would refuse a package over a trailing zero.
//
// A leading "v" is accepted on the first component only, because that is
// how a git tag is spelled; "1.v2.3" is not a version.
func ParseVersion(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Version{}, fmt.Errorf("version is empty")
	}
	parts := strings.Split(raw, ".")
	out := make([]int, 0, len(parts))
	for i, p := range parts {
		if i == 0 {
			p = strings.TrimPrefix(p, "v")
		}
		if p == "" {
			return Version{}, fmt.Errorf("%q is not a dotted numeric version", raw)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("%q is not a dotted numeric version", raw)
		}
		out = append(out, n)
	}
	return Version{Raw: raw, parts: out}, nil
}

// MustParseVersion is ParseVersion for constants and tests.
func MustParseVersion(s string) Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return v
}

// IsZero reports whether v is the unparsed zero value.
func (v Version) IsZero() bool { return v.Raw == "" && len(v.parts) == 0 }

// String renders the version as written.
func (v Version) String() string { return v.Raw }

// Compare orders two versions numerically.
//
// Numeric, not lexical: a string comparison puts "0.9.0" after "0.10.0",
// which would let a node two minor versions behind install a package that
// needs the newer one — a bug that only appears at the tenth release.
func Compare(a, b Version) int {
	n := len(a.parts)
	if len(b.parts) > n {
		n = len(b.parts)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(a.parts) {
			av = a.parts[i]
		}
		if i < len(b.parts) {
			bv = b.parts[i]
		}
		switch {
		case av < bv:
			return -1
		case av > bv:
			return 1
		}
	}
	return 0
}

// AtLeast reports whether v is at least floor.
func (v Version) AtLeast(floor Version) bool { return Compare(v, floor) >= 0 }

// HostComponents is what a host can state about itself.
//
// The two version axes are separate fields rather than one because they
// move independently: a node fleet is upgraded on one cadence and the agent
// binary inside it on another. A refusal that named the wrong axis would
// send an operator to upgrade the component that was already new enough.
type HostComponents struct {
	// Edge is the node agent's own version. Empty means the node cannot
	// state it, which is a provisioning problem.
	Edge string
	// Pig is the version of the agent binary the node launches. Empty
	// means nobody asked it, or it did not answer.
	Pig string
}

// VersionRequirement is one axis of a package's compatibility declaration.
type VersionRequirement struct {
	// Axis names which host component the requirement is against, so a
	// refusal can say which binary to upgrade.
	Axis string
	// Min is the lowest version that can host the package. Empty means
	// the package did not say, and an optional field left out is not a
	// requirement — every package written before it existed must keep
	// installing.
	Min string
	// Host is the version the host runs, which the requirement is
	// checked against.
	Host string
}

// Version axes.
const (
	// AxisEdge is the node agent around the Pig process.
	AxisEdge = "edge"
	// AxisPig is the PiG agent binary itself.
	AxisPig = "pig"
)

// UnreadableVersionError reports a value that could not be compared.
//
// It is a distinct type because "the requirement is wrong" and "the host is
// unknown" have different fixes, and a caller that flattened them into one
// string would make an operator guess which side to look at.
type UnreadableVersionError struct {
	// Axis is which component's requirement this is.
	Axis string
	// Side is "requirement" when the package's declared minimum is
	// unparseable, and "host" when the running component cannot state
	// itself.
	Side string
	// Value is the offending string.
	Value string
}

func (e *UnreadableVersionError) Error() string {
	if e.Side == "host" {
		return fmt.Sprintf("this %s reports its version as %q, which is not a version, so it cannot tell whether it is new enough", e.Axis, e.Value)
	}
	return fmt.Sprintf("the package asks for %s version %q, which is not a version that can be compared", e.Axis, e.Value)
}

// Satisfied decides one axis.
//
// An empty Min is satisfied. An unparseable value on either side is a
// refusal: a host that guessed would either run a package it cannot host or
// refuse one it can, and only one of those is recoverable.
func (r VersionRequirement) Satisfied() (bool, error) {
	min := strings.TrimSpace(r.Min)
	if min == "" {
		return true, nil
	}
	floor, err := ParseVersion(min)
	if err != nil {
		return false, &UnreadableVersionError{Axis: r.Axis, Side: "requirement", Value: min}
	}
	host, err := ParseVersion(r.Host)
	if err != nil {
		return false, &UnreadableVersionError{Axis: r.Axis, Side: "host", Value: r.Host}
	}
	if Compare(host, floor) < 0 {
		return false, &VersionTooOldError{Axis: r.Axis, Host: host.Raw, Min: floor.Raw}
	}
	return true, nil
}

// VersionTooOldError reports a host below a requirement.
type VersionTooOldError struct {
	Axis string
	Host string
	Min  string
}

func (e *VersionTooOldError) Error() string {
	// Which component to upgrade is the whole content of this message, so
	// it names the axis twice rather than saying "the host".
	return fmt.Sprintf("this node runs %s %s, but the package needs at least %s", e.Axis, e.Host, e.Min)
}

// Requirement is a package's whole compatibility declaration.
type Requirement struct {
	Edge VersionRequirement
	Pig  VersionRequirement
}

// Negotiate checks both axes and returns the first failure.
//
// Two separate checks rather than a loop over a slice of requirements: the
// axes fail for different reasons and the messages differ, and a
// table-driven version would produce one message for both — which is how an
// operator ends up upgrading the edge because a package wanted a newer
// agent.
func Negotiate(req Requirement, host HostComponents) error {
	edge := req.Edge
	edge.Axis = AxisEdge
	edge.Host = host.Edge
	if ok, err := edge.Satisfied(); !ok {
		return err
	}
	pig := req.Pig
	pig.Axis = AxisPig
	pig.Host = host.Pig
	if ok, err := pig.Satisfied(); !ok {
		return err
	}
	return nil
}
