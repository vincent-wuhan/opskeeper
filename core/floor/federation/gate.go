package federation

// The live policy as a gate. Until this file existed, a policy bundle
// travelled from a root to a child, was verified, and was atomically swapped
// into place — and then nothing read it. The channel was complete and the
// enforcement was not, which is the shape that looks finished from the root's
// side and does nothing on the node's.
//
// What "enforcing" means here is deliberately one question and not two, and
// getting that boundary wrong is the mistake worth writing down. The obvious
// implementation reuses pluginmanifest.Review on the live tree, because that
// function already answers "may this package be installed on this target"
// and reusing it looks like the way to avoid a second implementation.
//
// It is the wrong reuse, for a reason that only shows up on the version
// checks. Review evaluates min_edge_version and min_pig_version against the
// versions the *caller* states, and a child manager cannot state them: it
// does not run the agent, it does not launch the PiG build, and every node
// in the cluster may be on a different one. Answering that question here
// would mean a control plane guessing at a node's build and refusing
// packages on the guess — while the node, which knows, was never asked. The
// node's own Review is authoritative for admission and always runs; see
// cmd/opskeeper-edge/plugininstall.go. service/plugin/compatibility.go is
// the same problem solved the other way round: the control plane projects
// the version matrix as advice, and does not decide on it.
//
// So the gate answers membership only: is this package in the policy the
// root published and this cluster accepted? Everything else about the
// package — provenance, safety ceiling, scopes, version compatibility — was
// already decided once, by Receiver.Apply, against the same trust store and
// the same policy, at the moment the tree was accepted. A tree that reached
// the live link has been through that. A tree that failed it never got
// there, and re-deciding it here would only produce a second answer to a
// question that was already settled.
//
// The gate is fail-closed, and the reason it can afford to be is that it is
// only ever installed on a cluster that was configured as a child. A
// single-cluster deployment has no gate at all, which is the same shape the
// root side uses: federationDistributor returns nil when no artifact
// directory is set, and capability follows configuration rather than a
// default this package guesses at.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// The three refusals, kept apart because an operator acts on them
// differently. "No policy yet" is a root that has not pushed; "not in the
// policy" is a root that pushed something else; "the policy refuses it" is a
// root that pushed this and said no. Collapsing them produces a console that
// says "denied" for a cluster that is merely new.
var (
	// ErrNoLivePolicy means this cluster has been enrolled as a child and
	// has not been given a policy yet. It is the state a child is in for
	// the minutes between hello and the first push, and it is a state in
	// which the child can enforce nothing — which is why it is an error
	// rather than a permission.
	ErrNoLivePolicy = errors.New("federation: this cluster has not been given a policy yet")

	// ErrNotInPolicy means the live policy is present and does not carry
	// the package. This is the refusal that matters most: a package the
	// root never blessed must not reach a node of this cluster even if
	// every other check on it passes.
	ErrNotInPolicy = errors.New("federation: the live policy does not carry that package")

	// ErrPolicyRefused means the policy in force exists but cannot be
	// read — a half-written tree, a directory something else is writing,
	// a symlink pointing nowhere. It is kept apart from ErrNoLivePolicy
	// because the two are different incidents: one is a cluster that has
	// not been told anything, and the other is a cluster enforcing a
	// decision it cannot read.
	ErrPolicyRefused = errors.New("federation: the live policy cannot be read")
)

// LiveGate answers whether a package is in the policy this cluster enforces.
type LiveGate struct {
	// live is the path of the symlink the policy store keeps pointing at
	// the tree in force. It is read on every call rather than cached: a
	// cached answer would be wrong the instant a push swaps the link, and
	// the link is the state — that is the property Store is built around.
	live string
}

// NewLiveGate builds a gate over the live symlink at path.
//
// It takes no trust store and no policy, and the absence is the design: see
// the file header for why admission is not re-decided here. A constructor
// that wanted them would be asking this package to hold state that Receiver
// already holds, and the two would eventually disagree.
func NewLiveGate(path string) (*LiveGate, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("federation: a live gate needs the path of the live symlink")
	}
	return &LiveGate{live: path}, nil
}

// Live reports the tree in force, empty when the cluster has never been
// given one.
func (g *LiveGate) Live() string { return g.live }

// Check reports whether name at version may be installed on a node of this
// cluster.
//
// An empty version matches whatever version the live policy carries, and that
// asymmetry is deliberate: the two call sites have different information.
// FetchPackage is handed a version the operator picked and must be refused
// when the policy does not name it, while ApplyPackage is handed nothing —
// the tree is already on the node and this call is only the switch. Asking
// ApplyPackage to supply a version it does not have would mean either
// inventing one or weakening the check for both callers.
func (g *LiveGate) Check(name, version string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: a package was named by nothing at all", ErrNotInPolicy)
	}
	version = strings.TrimSpace(version)

	root, err := g.resolve()
	if err != nil {
		return err
	}

	packages, err := pluginmanifest.LoadAll(root)
	if err != nil {
		// A policy tree that cannot be read is not an absent policy. The
		// difference is worth a sentence: an absent policy means the root
		// has not spoken yet, a broken one means this cluster is enforcing
		// a decision it cannot read, and the second is an incident.
		return fmt.Errorf("%w: %v", ErrPolicyRefused, err)
	}

	for _, p := range packages {
		if p.Name() != name {
			continue
		}
		if version != "" && p.Manifest.Metadata.Version != version {
			continue
		}
		// Found. Everything else about this package was decided when the
		// tree was accepted; see the file header.
		return nil
	}

	if version != "" {
		return fmt.Errorf("%w: %s@%s is not in it, and it carries %s@%s",
			ErrNotInPolicy, name, version, name, g.onlyVersion(packages, name))
	}
	return fmt.Errorf("%w: %s is not in it", ErrNotInPolicy, name)
}

// Carried reports the packages the policy in force names, without deciding
// anything about them.
//
// It is what a console needs to answer "what is this cluster running", and it
// deliberately returns names rather than verdicts. The verdicts live with the
// node, which is where they are authoritative, and a console that displayed
// a verdict computed here would be showing an operator a decision this
// process is not entitled to make — see the file header.
func (g *LiveGate) Carried() ([]CarriedPackage, error) {
	root, err := g.resolve()
	if err != nil {
		return nil, err
	}
	packages, err := pluginmanifest.LoadAll(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPolicyRefused, err)
	}
	out := make([]CarriedPackage, 0, len(packages))
	for _, p := range packages {
		out = append(out, CarriedPackage{
			Name:    p.Name(),
			Version: p.Manifest.Metadata.Version,
			Vendor:  p.Manifest.Metadata.Vendor,
		})
	}
	return out, nil
}

// CarriedPackage is one package the policy in force names.
type CarriedPackage struct {
	Name    string
	Version string
	Vendor  string
}

// resolve returns the tree in force, or the error that says there is none.
func (g *LiveGate) resolve() (string, error) {
	if _, err := os.Lstat(g.live); err != nil {
		if os.IsNotExist(err) {
			return "", ErrNoLivePolicy
		}
		return "", fmt.Errorf("%w: the live link: %v", ErrPolicyRefused, err)
	}
	return g.live, nil
}

// onlyVersion names what the policy does carry, so a refusal tells an
// operator which version to ask the root for instead of only what was
// turned down. It answers "" when the policy carries no such package at all,
// which is the case where there is nothing useful to say and the caller
// renders a message with a hole in it rather than a false claim.
func (g *LiveGate) onlyVersion(packages []pluginmanifest.Plugin, name string) string {
	for _, p := range packages {
		if p.Name() == name {
			return p.Manifest.Metadata.Version
		}
	}
	return ""
}
