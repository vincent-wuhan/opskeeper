package pluginmanifest

import (
	"fmt"
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// Named deployment profiles: the two shapes this platform is actually sold
// into, written down as one place to look rather than as a scattering of
// environment variables on each host.
//
// A profile is not a new mechanism. It is a reviewable bundle of the
// decisions a node's operator has to make anyway — the safety ceiling, the
// approval radius, the scopes granted — assembled so that "the finance
// deployment" is a thing with a definition instead of a checklist somebody
// half-remembers. Everything here composes into a Policy the review already
// consumes; there is no second admission path.
//
// The reason to have exactly two rather than a knob per field is that the
// trade they encode is not gradient. A core trading system's failure mode is
// an unrecoverable side effect on the ledger; a multi-tenant SaaS failure
// mode is one tenant's mistake reaching another's data. Those want opposite
// defaults, and a deployment that picked per-field would end up with the
// literal worst of both — a wide ceiling and a wide grant — because each
// field looked reasonable on its own.

// ProfileName identifies a deployment profile.
type ProfileName string

const (
	// ProfileFinance is a core trading / strong-consistency deployment.
	ProfileFinance ProfileName = "finance-strong-consistency"
	// ProfileSaaS is a multi-tenant deployment rolled out per tenant.
	ProfileSaaS ProfileName = "saas-multitenant"
)

// Profile is one named deployment's policy, with the reasoning attached.
//
// The intent strings are part of the value rather than comments because
// they are what an operator is shown when they ask why a package was
// refused — and because a profile edited by somebody who does not know why
// a field is what it is should have to delete a sentence, not a comment.
type Profile struct {
	Name        ProfileName
	Summary     string
	Ceiling     domain.SafetyLevel
	MaxRadius   domain.BlastRadius
	Granted     domain.Scopes
	Strategy    string
	Intent      string
	NotGranted  string
	BlockedNote string
	// Composes is the package set this profile installs.
	//
	// Everything above this field is a *ceiling* — it says what the fleet
	// may host, and a ceiling alone leaves the operator with a policy and
	// no answer to the question they actually have, which is "so which
	// packages do I install". A profile that granted host.write for
	// host-scoped repair and then composed nothing that needs it would be
	// a correct policy and a useless template.
	//
	// So the composition is written down, sorted, and checked against the
	// shipped catalogue by the tests beside it: every name here must exist,
	// every one must be admitted by the policy above it, and every shipped
	// package must be composed by at least one profile — a package no
	// profile installs is a package nobody put on a node, and nothing else
	// in the system would say so.
	Composes []string
}

// ComposesPackage reports whether the profile installs p by name.
func (p Profile) ComposesPackage(name string) bool {
	for _, n := range p.Composes {
		if n == name {
			return true
		}
	}
	return false
}

// Policy renders the profile as the node review policy.
//
// AllowUnsigned is deliberately absent from Profile and forced false here.
// A profile is the shape of a production fleet; the unsigned escape hatch
// is a development affordance that lives on the node, and a profile that
// could switch it on would be a way to turn signing off for a whole
// deployment from a file two levels away from the host it affects.
func (p Profile) Policy() Policy {
	pol := PolicyFor(p.Ceiling, p.MaxRadius, p.Granted)
	pol.AllowUnsigned = false
	return pol
}

// deploymentProfiles is the catalogue.
//
// Sorted by name so a listing is stable; see All.
var deploymentProfiles = []Profile{
	{
		Name:    ProfileFinance,
		Summary: "Core trading and settlement: strong consistency, every mutation confined to one workload and reversible by restarting it.",
		// L2, not L3. The ceiling is a statement about what this fleet
		// will host, and an L3 package is by definition one that mutates
		// an external system in a way its own author cannot undo. A
		// trading core is the last place to find out what that means.
		Ceiling: domain.SafetyL2,
		// pod. Of the five radii, this is the widest that still describes
		// an action a single operator can reason about under pressure:
		// restart this, roll back this, reconfigure this. single-ns and
		// above are reachable only by an explicit operator decision, and
		// the profile exists so that decision is never made by default.
		MaxRadius: domain.RadiusPod,
		// Pin, not rolling. A trading host has no canary to spare: the
		// first node a rolling release touches is a live node. Pinning
		// makes each host an explicit, operator-scheduled decision.
		Strategy: domain.InstallPin,
		Granted: domain.Scopes{
			domain.ScopeHostRead, domain.ScopeHostWrite,
			domain.ScopeK8sRead, domain.ScopeDBRead, domain.ScopeMQRead,
			domain.ScopeMetricsRO, domain.ScopeTopologyRO,
			domain.ScopeAlertRO, domain.ScopeAlertWrite,
		},
		Intent: "Host-scoped repair (host.write) is granted because restarting one unit on one host is the operation this fleet most needs to automate, and alert.write is granted alongside it because the repair package that does the restarting also commits confirmed alert-rule drafts — a package that bundles the two, so granting one without the other leaves the flagship package uninstallable in the one deployment it was written for. Neither scope reaches trading state: one restarts a systemd unit, the other edits when a rule fires. Every scope that reaches past the host and past the alert system — k8s.exec, db.write, mq.write — is withheld, so a package needing one is refused at admission rather than at the moment it tries.",
		// Named so an operator asking "why was this refused" gets the
		// missing scope rather than a generic denial. This list was wrong
		// until the composition tests existed: it did not mention
		// alert.write, which the profile was withholding while its own
		// Intent said the repair package was the reason host.write was
		// granted. A profile whose stated reason and stated refusal
		// disagree is worse than one with no prose, because the prose is
		// what an operator trusts.
		NotGranted:  "k8s.exec, db.write, mq.write",
		BlockedNote: "an L3 package or one declaring a radius wider than pod is refused here even if its scopes were granted; widen MaxRadius in this profile rather than in a node's environment, so the change is reviewable. The L3 refusal is what keeps the unattended self-heal package (opskeeper-sre-autonomy) off this fleet, which is the intended answer rather than an oversight: strong consistency and reversibility by restarting the thing you restarted is the last place to install a capability whose premise is that nobody is watching.",
		Composes: []string{
			"opskeeper-sre-middleware",
			"opskeeper-sre-observability",
			"opskeeper-sre-readonly",
			"opskeeper-sre-repair",
		},
	},
	{
		Name:    ProfileSaaS,
		Summary: "Multi-tenant SaaS: a tenant is a namespace, and one tenant's mistake must not reach another's data.",
		// L3, because a SaaS platform's routine operations genuinely do
		// reach into external systems — migrating a tenant's database,
		// cordoning a namespace's nodes — and refusing all of it would
		// make the profile useless and get it widened by hand on the
		// first real incident.
		Ceiling: domain.SafetyL3,
		// namespace, capped there. The ceiling is the whole point of this
		// profile: tenant isolation is enforced by the *radius*, not by
		// the tool list. A cluster-scoped action approved for one
		// tenant's incident is an action that can reach every other
		// tenant, and no capability declaration can make that safe.
		MaxRadius: domain.RadiusNamespace,
		// Rolling, and this is the profile's reason for existing: a wave
		// is a set of tenants, so a bad version is measured in tenants
		// before it is measured in customers.
		Strategy: domain.InstallRolling,
		Granted: domain.Scopes{
			domain.ScopeHostRead, domain.ScopeHostWrite,
			domain.ScopeK8sRead, domain.ScopeK8sExec,
			domain.ScopeDBRead, domain.ScopeDBWrite,
			domain.ScopeMQRead, domain.ScopeMQWrite,
			domain.ScopeMetricsRO, domain.ScopeTopologyRO, domain.ScopeAlertRO,
			domain.ScopeAlertWrite,
		},
		Intent: "Every scope is granted because the operators here are the tenant's own SREs and need the whole toolkit. The containment is the radius: an approval is minted for one namespace, so a write that would cross a tenant boundary is refused by the policy engine before the approval is even requested.",
		// The scope list is unrestricted, so the note has to say what is
		// actually doing the containing, or a reviewer will read the
		// grant as the control.
		NotGranted:  "nothing — the namespace ceiling is what contains this profile, not the scope list",
		BlockedNote: "a cluster-radius action is refused here. That is the tenancy boundary; a fleet that needs one is a fleet considering a different profile, not a reason to edit this one.",
		// Autonomy is composed here and not in the financial profile, and
		// the ceiling decided it rather than a preference. The self-heal
		// package is L3 — it mutates a live host with nobody available to
		// approve — and the financial profile is capped at L2 with a note
		// saying so. A fleet whose rule is strong consistency and
		// reversibility by restarting the thing you restarted is the last
		// place to install a capability whose entire premise is that the
		// restart is unattended. The SaaS profile is the defensible home:
		// one pod's worth of reach, an explicit list of vectors a tenant's
		// own operator signed, and a fleet large enough that one node's
		// agent answering while the control plane is unreachable is a
		// thing somebody would actually want.
		Composes: []string{
			"opskeeper-sre-autonomy",
			"opskeeper-sre-middleware",
			"opskeeper-sre-observability",
			"opskeeper-sre-readonly",
			"opskeeper-sre-repair",
		},
	},
}

// ProfileForName returns the named profile.
//
// An unknown name is an error rather than a fallback. Falling back to the
// narrower profile would silently move a SaaS fleet under a pod ceiling —
// refusing work it is entitled to do, with an error message about a
// package — and falling back to the wider one would do the reverse on the
// fleet where it matters most.
func ProfileForName(name string) (Profile, error) {
	want := ProfileName(strings.TrimSpace(name))
	for _, p := range deploymentProfiles {
		if p.Name == want {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("no deployment profile named %q (have: %s)", name, profileNames())
}

// All returns every profile, sorted by name.
func All() []Profile {
	out := append([]Profile(nil), deploymentProfiles...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func profileNames() string {
	names := make([]string, 0, len(deploymentProfiles))
	for _, p := range deploymentProfiles {
		names = append(names, string(p.Name))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
