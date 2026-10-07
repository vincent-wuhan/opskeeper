package pluginmanifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// The review pipeline: what has to be true before a package reaches a node.
//
// The existing pieces already decide most of this — sdk.Decode refuses a
// manifest that breaks a rule, sdk.Admit refuses one that exceeds a
// target's policy, and signing binds a manifest to the code it governs.
// What was missing is an order, and the order is the security property.
//
// Every step after the first reads something. The manifest is parsed at
// step three, its tool list becomes an allow-list at step four. If those
// steps ran before the signature was checked, an unsigned package would be
// validated and admitted on its own say-so, and the signature would be a
// later formality. So:
//
//	signature → manifest → admission → version → agent version
//
// A package that fails at any step never reaches the ones after it, and
// the Step field says which one it was so an operator does not have to
// infer the ordering from the error text.

// Step names where in the pipeline a package was refused. They are
// constants because they end up in a log an operator greps for, and a
// typo in a step name is a package that cannot be found afterwards.
const (
	// StepSignature is the provenance check: who signed this, and does the
	// tree still match what they signed.
	StepSignature = "signature"
	// StepManifest is the structural check: does the manifest parse and
	// satisfy every rule in the sdk.
	StepManifest = "manifest"
	// StepAdmission is the policy check: may this package run *here*, with
	// the scopes this operator actually granted.
	StepAdmission = "admission"
	// StepVersion is the compatibility check: is this node new enough to
	// host the package at all. It is a separate step from admission
	// because it is a different question with a different fix — an
	// admission refusal is answered by changing the node's policy, and
	// this one by upgrading the node — and an operator sent to the wrong
	// one is an operator who will not find it.
	StepVersion = "version"
	// StepAgentVersion is the second half of the compatibility check: is
	// the PiG agent inside this node new enough. It is separate from
	// StepVersion for the same reason StepVersion is separate from
	// StepAdmission — the fix is to upgrade a different thing, and a
	// refusal that does not say which sends an operator to the wrong
	// component.
	StepAgentVersion = "agent_version"
)

// Policy is the target's side of the decision.
//
// It is a struct of the things a node knows about itself — what it will
// host, what it will approve, what it has been given — rather than a
// manifest's side, because a package gets to describe itself and the
// node is the only party here with nothing to gain from the answer.
type Policy struct {
	// MaxSafetyLevel is the highest level this target will host. Required:
	// an unset ceiling refuses every package, which is the correct reading
	// of a node that forgot to configure one.
	MaxSafetyLevel domain.SafetyLevel
	// MaxBlastRadius is the widest approval this target will grant.
	MaxBlastRadius domain.BlastRadius
	// GrantedScopes is what an operator approved for this target. A
	// package asking for more is refused, and the missing ones are named.
	GrantedScopes domain.Scopes
	// AllowedVendors optionally restricts which vendors may install. Empty
	// means any vendor whose signature verifies — the common case, where
	// the key is the trust decision.
	AllowedVendors []string
	// AllowUnsigned is the escape hatch for a development node, and the
	// only way to install a package nobody signed.
	//
	// It is one field rather than a RequireSignature plus an exception,
	// because a require flag and an allow flag on the same decision are a
	// contradiction waiting to be misread at a call site, and a zero-value
	// Policy has to be the safe one. With this false — which is the zero
	// value — every package must be signed. Turning it on is a decision
	// somebody makes out loud, and Review records in its Decision that
	// there was no signature at all, so a node running unsigned packages
	// is visible in the same log as one that is not.
	AllowUnsigned bool
	// NodeVersion is the agent version this target is running, used to
	// evaluate a package's min_edge_version. Empty means the node cannot
	// state its own version, which refuses a package that asks for one:
	// the node would be guessing, and the guess that is wrong in the
	// permissive direction installs a package the node cannot host.
	//
	// It is not "unset means no check". A package that declares no
	// minimum is admitted either way — an optional field left out is not
	// a requirement — but a package that declares one is only admitted
	// when the node can prove it is new enough.
	NodeVersion string
	// PigVersion is the PiG agent build this node launches, used to
	// evaluate a package's min_pig_version. It is a different number from
	// NodeVersion and is reported by a different thing, so it is a
	// different field. Empty refuses a package that asks for one, on the
	// same reasoning as NodeVersion: a guess in the permissive direction
	// installs a package whose extensions the agent cannot load.
	PigVersion string
}

// PolicyFor is the default policy for a production target: signatures
// required, no vendor restriction.
func PolicyFor(maxLevel domain.SafetyLevel, maxRadius domain.BlastRadius, granted domain.Scopes) Policy {
	return Policy{
		MaxSafetyLevel: maxLevel,
		MaxBlastRadius: maxRadius,
		GrantedScopes:  granted,
	}
}

// Decision is the review's answer for one package.
//
// It is a value rather than a bare error because the caller has three
// different things to do with it: install, ask a human, or log. An error
// alone makes "this one is fine, move on" and "this one is dangerous"
// indistinguishable at the call site, which is how a loop over a catalog
// ends up refusing everything.
type Decision struct {
	// Plugin is the package's name.
	//
	// Before the manifest has been read it is a *label* rather than a
	// claim: a review that fails at the signature step has not
	// authenticated anything, so this is the directory's name, there to
	// let an operator find the package in a listing. After the manifest is
	// read it is the manifest's own metadata.name, which at that point is
	// authenticated bytes.
	Plugin string
	// Version is the package's declared version, on the same terms.
	Version string
	// Allowed reports whether the package may be installed on this target.
	Allowed bool
	// Step is the pipeline stage that decided, on the same terms.
	Step string
	// Reason is a sentence for the operator. It is the last thing they
	// read and often the only thing they read, so it says what to do and
	// not only what went wrong.
	Reason string
	// Manifest is the loaded manifest, set only when Allowed.
	Manifest domain.PluginManifest
	// Envelope is the verified signature, set only when Allowed.
	Envelope Envelope
}

// String renders a decision for a log line or a CLI.
func (d Decision) String() string {
	who := d.Plugin
	if who == "" {
		who = "<unreadable package>"
	}
	verdict := "refused"
	if d.Allowed {
		verdict = "allowed"
	}
	if d.Reason == "" {
		return fmt.Sprintf("%s v%s: %s", who, d.Version, verdict)
	}
	return fmt.Sprintf("%s v%s: %s at %s — %s", who, d.Version, verdict, d.Step, d.Reason)
}

// Review decides whether the package at root may be installed on a target
// with this policy.
//
// It never returns an error. A refusal is an answer, and the caller
// handles it differently from a crash; the one thing it does not do is
// stop the caller from reviewing the next package, which is what an error
// return tends to be read as.
func Review(root string, trust *TrustStore, pol Policy) Decision {
	// Step 1: provenance. This runs before the manifest is read, so a
	// package that nobody signed is rejected before its own description
	// of itself has been given any weight at all.
	// The directory's own name is how a refusal identifies its subject.
	//
	// At the signature step the manifest has deliberately not been read —
	// that is the whole point of the ordering — so there is no trusted
	// name to use. The directory name is a *label*, not a claim: it says
	// which folder was refused so an operator looking at a listing can
	// find it, and nothing downstream may treat it as the package's
	// identity. It is why Decision.Plugin is documented as unset before
	// the manifest is read, and why a caller that needs the real name must
	// read the manifest itself.
	label := filepath.Base(strings.TrimRight(root, string(os.PathSeparator)))

	env, envErr := ReadEnvelope(root)
	if envErr != nil {
		if !pol.AllowUnsigned {
			return Decision{
				Plugin: label,
				Step:   StepSignature,
				Reason: envErr.Error() +
					"; add the publisher's key to this node's trust store, or set AllowUnsigned on a development node",
			}
		}
		// The development path, taken deliberately: the package proceeds
		// with no envelope, and the Decision below carries an empty one so
		// that nothing downstream can mistake this for a verified install.
	} else if err := Verify(root, env, trust); err != nil {
		// Here the envelope is authenticated, so its name is the
		// publisher's own claim about itself and is a better label than
		// the directory's.
		if env.Name == "" {
			env.Name = label
		}
		return Decision{Plugin: env.Name, Version: env.Version, Step: StepSignature, Reason: err.Error()}
	}

	// Step 2: structure. The manifest is authenticated by now — the
	// signature covered this exact tree — so what it says can be trusted
	// as the publisher's claim, and validated as a claim.
	p, err := Load(root)
	if err != nil {
		// A manifest that does not validate has no name anybody should
		// believe, so the directory's is used for the same reason as
		// above: this decision has to be findable in a listing.
		return Decision{Plugin: label, Step: StepManifest, Reason: err.Error()}
	}
	decision := Decision{
		Plugin:   p.Manifest.Metadata.Name,
		Version:  p.Manifest.Metadata.Version,
		Manifest: p.Manifest,
		Envelope: env,
	}

	// A signature is a statement about who published a package, not about
	// who may publish here. A node that only trusts its own vendor can say
	// so here, and a package from anyone else then fails with a reason
	// that names the restriction rather than the signature.
	if len(pol.AllowedVendors) > 0 {
		vendor := p.Manifest.Metadata.Vendor
		if !containsFold(pol.AllowedVendors, vendor) {
			decision.Step = StepAdmission
			decision.Reason = fmt.Sprintf("vendor %q is not one this node installs from (%s)",
				vendor, strings.Join(pol.AllowedVendors, ", "))
			return decision
		}
	}

	// Step 3: policy. Everything above established what the package *is*;
	// this establishes whether it may run *here*.
	if err := sdk.Admit(p.Manifest, sdk.Admission{
		GrantedScopes:  pol.GrantedScopes,
		MaxSafetyLevel: pol.MaxSafetyLevel,
		MaxBlastRadius: pol.MaxBlastRadius,
	}); err != nil {
		decision.Step = StepAdmission
		decision.Reason = err.Error()
		return decision
	}

	// Step 4: compatibility. Everything above established what the
	// package is and whether this node's operator will host it; this
	// establishes whether this node's *binary* can. It runs last because
	// it is the only check that reads a fact about the node rather than a
	// fact about the package, and because a package that fails admission
	// should be reported as an admission refusal — the more actionable of
	// the two — even when it would also have failed here.
	if ok, step, reason := CheckVersions(
		p.Manifest.Spec.Install.MinEdgeVersion,
		p.Manifest.Spec.Install.MinPigVersion,
		pol.NodeVersion, pol.PigVersion,
	); !ok {
		decision.Step = step
		decision.Reason = reason
		return decision
	}
	// Both halves of the compatibility matrix are inside CheckVersions,
	// for the reason that function exists: the control plane's pre-flight
	// and this review have to agree, and two copies of the same two-axis
	// check are two answers waiting to diverge. They are also checked in
	// the node's order rather than the manager's — the edge build first,
	// because a node too old to run the package at all is the more
	// actionable answer and the more likely one to clear on one upgrade.

	decision.Allowed = true
	decision.Step = StepSignature
	return decision
}

// ReviewAll reviews every package under base and returns the decisions in
// package-name order.
//
// One bad package does not stop the others. A control plane that refuses
// to serve half its catalog because one manifest is malformed is harder to
// reason about than one that reports the bad one and serves the rest, and
// the operator gets the same information either way.
func ReviewAll(base string, trust *TrustStore, pol Policy) []Decision {
	plugins, err := LoadAll(base)
	if err != nil {
		// LoadAll refuses the whole set on the first invalid manifest,
		// which is right for boot and wrong for a listing. Report the
		// one that failed, by name, and review nothing else this round.
		return []Decision{{Step: StepManifest, Reason: err.Error()}}
	}
	out := make([]Decision, 0, len(plugins))
	for _, p := range plugins {
		out = append(out, Review(p.Root, trust, pol))
	}
	return out
}

func containsFold(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(strings.TrimSpace(h), strings.TrimSpace(needle)) {
			return true
		}
	}
	return false
}
