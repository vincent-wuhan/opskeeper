package pluginmanifest

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// A deployment profile is a ceiling. A ceiling says what a fleet may host
// and nothing about what it hosts, which leaves the operator holding a
// policy and still needing to answer the question they came with.
//
// The composition is that answer: the package set a profile installs. It
// was missing, and the tests below are what found out that it mattered.
// Without them the finance profile granted host.write for the express
// purpose of enabling host-scoped repair, withheld alert.write, and
// therefore could not install the only package in the catalogue that uses
// host.write — in the one deployment whose stated purpose is that repair.
// The profile was internally consistent in every field and answered none of
// its own sentences, because no test ever put a profile and the shipped
// catalogue in the same assertion.
//
// Everything here therefore joins the two. A ceiling is tested on its own
// in profiles_test.go; these tests are about the fleet.

// shippedCatalogue is the set of packages under plugins/pig-ops, keyed by
// the name a profile would name them by.
func shippedCatalogue(t *testing.T) map[string]Plugin {
	t.Helper()
	byName := map[string]Plugin{}
	for _, p := range shippedPlugins(t) {
		byName[p.Name()] = p
	}
	if len(byName) == 0 {
		t.Fatalf("no packages under %s", filepath.Join(repoRoot(t), "plugins", "pig-ops"))
	}
	return byName
}

func TestEveryProfileNamesOnlyPackagesThatExist(t *testing.T) {
	// A composition naming a package nobody ships is a template that
	// fails on first use, at an install, on a node, during an incident.
	// It is the cheapest thing in this file to check and the most
	// expensive to discover late.
	catalogue := shippedCatalogue(t)
	for _, profile := range All() {
		for _, name := range profile.Composes {
			if _, ok := catalogue[name]; !ok {
				t.Errorf("profile %s composes %q, which no package in plugins/pig-ops declares",
					profile.Name, name)
			}
		}
	}
}

func TestEveryComposedPackageIsActuallyAdmittedByItsProfile(t *testing.T) {
	// The assertion that was missing, and the one that would have caught
	// the finance/repair inconsistency on the day it was written.
	//
	// A profile lists what it installs. sdk.Admit is what the host
	// actually runs, over the real manifest, against the real scopes. If a
	// composed package is refused, the profile is a document describing a
	// deployment that cannot be built — and the refusal surfaces at
	// install time on a node, as an error naming a scope, to whoever is
	// least able to act on it.
	catalogue := shippedCatalogue(t)
	for _, profile := range All() {
		for _, name := range profile.Composes {
			p, ok := catalogue[name]
			if !ok {
				continue // already reported by the test above
			}
			if err := sdk.Admit(p.Manifest, sdk.Admission{
				GrantedScopes:  profile.Granted,
				MaxSafetyLevel: profile.Ceiling,
				MaxBlastRadius: profile.MaxRadius,
			}); err != nil {
				t.Errorf("profile %s composes %s but admits it with nothing:\n  %v\n"+
					"either the package does not belong in this profile or the profile "+
					"does not grant what the package needs", profile.Name, name, err)
			}
		}
	}
}

// TestEveryProfileNamesEveryScopeItRefusesToGrant is the test that found
// the bug, generalised so it finds the next one.
//
// NotGranted is the sentence an operator reads when a package is refused,
// and it is the only place the profile says what it is withholding. It
// used to read "k8s.exec, db.write, mq.write" while the profile was also
// withholding alert.write — the scope the repair package needs and the
// scope its own Intent said it was granting host.write for. An operator
// refused on alert.write would have been told it was refused on
// k8s.exec.
//
// The invariant is deliberately one-directional. A profile may withhold
// scopes nothing in the catalogue asks for — that is what a ceiling is for
// — but every scope a *shipped* package needs and the profile does not
// grant has to be named. Silence about a live refusal is the bug; silence
// about a hypothetical one is the design.
func TestEveryProfileNamesEveryScopeItRefusesToGrant(t *testing.T) {
	catalogue := shippedCatalogue(t)
	for _, profile := range All() {
		granted := map[string]bool{}
		for _, s := range profile.Granted {
			granted[string(s)] = true
		}
		// Every scope any shipped package declares.
		needed := map[string]bool{}
		for _, p := range catalogue {
			for _, s := range p.Manifest.Spec.RequiredScopes {
				needed[string(s)] = true
			}
		}
		var unnamed []string
		for scope := range needed {
			if granted[scope] {
				continue
			}
			if !strings.Contains(profile.NotGranted, scope) {
				unnamed = append(unnamed, scope)
			}
		}
		sort.Strings(unnamed)
		for _, scope := range unnamed {
			t.Errorf("profile %s refuses %q to a shipped package but its NotGranted (%q) "+
				"does not name it; an operator refused on this scope is told about a different one",
				profile.Name, scope, profile.NotGranted)
		}
	}
}

func TestEveryShippedPackageIsComposedByAtLeastOneProfile(t *testing.T) {
	// The other direction, and the one that catches a new package.
	//
	// A package that ships, validates, signs and rolls out correctly and
	// that no profile installs is a package nobody has put on a node. It
	// has no error: it loads, it passes admission, it appears in the
	// marketplace, and it is never executed. The only signal is a
	// composition list that did not grow, and the only place to notice is
	// a test that reads the catalogue rather than the profiles.
	catalogue := shippedCatalogue(t)
	composed := map[string]bool{}
	for _, profile := range All() {
		for _, name := range profile.Composes {
			composed[name] = true
		}
	}
	for name := range catalogue {
		if !composed[name] {
			t.Errorf("package %s ships but no profile composes it, so no deployment "+
				"installs it and nothing else in the system reports that", name)
		}
	}
}

func TestComposesIsSortedAndUnique(t *testing.T) {
	// A composition is the thing an operator diffs when they ask "what
	// changed about this deployment", so it is held to the same rule as
	// every other review surface in the repository.
	for _, profile := range All() {
		if !sort.StringsAreSorted(profile.Composes) {
			t.Errorf("profile %s has an unsorted composition: %v", profile.Name, profile.Composes)
		}
		seen := map[string]bool{}
		for _, n := range profile.Composes {
			if seen[n] {
				t.Errorf("profile %s composes %q twice", profile.Name, n)
			}
			seen[n] = true
		}
		if len(profile.Composes) == 0 {
			t.Errorf("profile %s composes nothing; a profile with no package set is a policy, not a template", profile.Name)
		}
	}
}

// TestTheFinanceProfileIsNotMerelyAStricterSaaS pins the direction the two
// profiles claim, at the level an operator experiences it.
//
// profiles_test.go already asserts the ceilings differ. This asserts the
// consequence: a fleet on the finance profile must never end up able to do
// something the SaaS profile forbids, because "finance is stricter" is only
// true if every package it can host, it can host no more widely.
func TestTheFinanceProfileIsNotMerelyAStricterSaaS(t *testing.T) {
	finance, err := ProfileForName(string(ProfileFinance))
	if err != nil {
		t.Fatalf("finance: %v", err)
	}
	saas, err := ProfileForName(string(ProfileSaaS))
	if err != nil {
		t.Fatalf("saas: %v", err)
	}
	catalogue := shippedCatalogue(t)

	for _, name := range finance.Composes {
		p, ok := catalogue[name]
		if !ok {
			continue
		}
		// The same package, under the same profile ceiling, must be
		// refused by the wider profile if the narrower one is stricter.
		// If a future edit makes finance grant something SaaS withholds,
		// this fails — and the two profiles have swapped, which is not a
		// difference of degree.
		underSaaS := sdk.Admit(p.Manifest, sdk.Admission{
			GrantedScopes:  saas.Granted,
			MaxSafetyLevel: saas.Ceiling,
			MaxBlastRadius: saas.MaxRadius,
		})
		if underSaaS != nil {
			t.Errorf("finance composes %s but SaaS — the wider profile — refuses it: %v", name, underSaaS)
		}
	}
	// And the scope containment the two profiles claim, stated directly.
	saasHas := map[string]bool{}
	for _, s := range saas.Granted {
		saasHas[string(s)] = true
	}
	for _, s := range finance.Granted {
		if !saasHas[string(s)] {
			t.Errorf("finance grants %q, which SaaS does not; the profiles are not nested", s)
		}
	}
	if finance.Ceiling.Rank() > saas.Ceiling.Rank() {
		t.Errorf("finance ceiling %s is above the SaaS ceiling %s", finance.Ceiling, saas.Ceiling)
	}
}

// TestTheFinanceProfileCanActuallyRunTheRepairItExistsFor is the worked
// example, kept separate because it is the one that regressed.
//
// Finance grants host.write for host-scoped repair. The repair package is
// the only thing in the catalogue that uses host.write. If finance cannot
// admit it, the profile's Intent is a sentence about a deployment that
// cannot be assembled, and the scope is granted for nothing.
func TestTheFinanceProfileCanActuallyRunTheRepairItExistsFor(t *testing.T) {
	finance, err := ProfileForName(string(ProfileFinance))
	if err != nil {
		t.Fatalf("finance: %v", err)
	}
	catalogue := shippedCatalogue(t)
	repair, ok := catalogue["opskeeper-sre-repair"]
	if !ok {
		t.Fatal("the repair package is not among the shipped packages")
	}
	if !finance.ComposesPackage("opskeeper-sre-repair") {
		t.Fatal("finance composes no repair package, yet it grants host.write specifically to enable one")
	}
	if err := sdk.Admit(repair.Manifest, sdk.Admission{
		GrantedScopes:  finance.Granted,
		MaxSafetyLevel: finance.Ceiling,
		MaxBlastRadius: finance.MaxRadius,
	}); err != nil {
		t.Fatalf("the finance profile cannot install the repair package: %v", err)
	}
	// The repair package declares host.write, so the grant has to be real
	// rather than incidental.
	var usesHostWrite bool
	for _, s := range repair.Manifest.Spec.RequiredScopes {
		if s == domain.ScopeHostWrite {
			usesHostWrite = true
		}
	}
	if !usesHostWrite {
		t.Fatal("the repair package no longer asks for host.write, so this test is asserting " +
			"something about the profile that no longer has a reason to exist")
	}
}
