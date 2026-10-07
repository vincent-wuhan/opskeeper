package pluginmanifest

import (
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// A profile is a bundle of decisions a fleet's operator would have to make
// anyway. The properties worth locking are the ones that make it more than
// a comment: that the two profiles actually differ in the direction they
// claim, that a profile never turns on unsigned installs, and that the
// catalogue cannot grow a name that resolves to nothing.

func TestBothShippedProfilesResolve(t *testing.T) {
	for _, name := range []ProfileName{ProfileFinance, ProfileSaaS} {
		if _, err := ProfileForName(string(name)); err != nil {
			t.Errorf("the shipped profile %q does not resolve: %v", name, err)
		}
	}
}

func TestAnUnknownProfileIsRefusedRatherThanDefaulted(t *testing.T) {
	// A fallback would be worse than a refusal in both directions: the
	// narrow one silently strips capabilities from a fleet entitled to
	// them, and the wide one silently widens the fleet where it matters.
	// The error has to name the alternatives, because the operator reading
	// it does not know the catalogue from memory.
	_, err := ProfileForName("finace")
	if err == nil {
		t.Fatal("a misspelled profile resolved to something")
	}
	for _, want := range []string{"finace", string(ProfileFinance), string(ProfileSaaS)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q, so the operator cannot see the typo", err, want)
		}
	}
}

func TestTheProfilesDifferInTheDirectionTheyClaim(t *testing.T) {
	// This is what makes two profiles worth having. The finance profile
	// trades reach for containment; the SaaS profile trades containment
	// reach for containment *shape* — the same tool is allowed, but only
	// where one tenant's approval cannot reach another's data.
	fin, err := ProfileForName(string(ProfileFinance))
	if err != nil {
		t.Fatal(err)
	}
	saas, err := ProfileForName(string(ProfileSaaS))
	if err != nil {
		t.Fatal(err)
	}

	if fin.MaxRadius.Rank() >= saas.MaxRadius.Rank() {
		t.Errorf("finance radius %q is not narrower than SaaS %q; the profiles have collapsed into one",
			fin.MaxRadius, saas.MaxRadius)
	}
	if fin.Ceiling.Rank() >= saas.Ceiling.Rank() {
		t.Errorf("finance ceiling %s is not lower than SaaS %s", fin.Ceiling, saas.Ceiling)
	}
	// Satisfies reads as "the argument covers the receiver's scopes", so
	// the receiver is the required set and the argument is the grant.
	// Both directions are asserted because the reading inverts easily and
	// an inverted assertion here would pass for the wrong reason.
	if !fin.Granted.Satisfies(saas.Granted) {
		t.Error("SaaS does not grant everything finance does; the wider profile is supposed to be a superset")
	}
	if saas.Granted.Satisfies(fin.Granted) {
		t.Error("finance grants everything SaaS does; the withholding is the finance profile's whole safety story")
	}
	if fin.Strategy == saas.Strategy {
		t.Errorf("both profiles deploy %q; the deployment cadence is part of the profile", fin.Strategy)
	}
}

func TestNoProfileTurnsOnUnsignedInstalls(t *testing.T) {
	// The escape hatch is a development affordance that lives on a node.
	// A profile is the shape of a production fleet, and a profile that
	// could switch signing off would be a way to disable it for a whole
	// deployment from a file two levels away from the hosts it affects.
	for _, p := range All() {
		if p.Policy().AllowUnsigned {
			t.Errorf("profile %q would accept unsigned packages", p.Name)
		}
	}
}

func TestEveryProfileSaysWhatItWithholds(t *testing.T) {
	// An operator asking "why was this refused" is shown the profile. A
	// profile whose NotGranted is empty reads as "nothing is withheld",
	// which for the permission-restricted profile is the opposite of true.
	for _, p := range All() {
		if strings.TrimSpace(p.NotGranted) == "" {
			t.Errorf("profile %q does not say what it withholds; an empty field reads as %q", p.Name, "nothing")
		}
		if strings.TrimSpace(p.Intent) == "" {
			t.Errorf("profile %q has no stated intent, so the next editor cannot tell what they would break", p.Name)
		}
		if strings.TrimSpace(p.BlockedNote) == "" {
			t.Errorf("profile %q has no note on what it refuses", p.Name)
		}
	}
}

func TestTheFinanceProfileRefusesTheWritesItSaysItWithholds(t *testing.T) {
	// The claim is checked against the actual admission rule rather than
	// restated. A profile that said it withheld k8s.exec but granted it
	// would pass an assertion on the string and fail a real install.
	fin, err := ProfileForName(string(ProfileFinance))
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"k8s.exec", "db.write", "mq.write"} {
		if fin.Granted.Has(domain.Scope(scope)) {
			t.Errorf("the finance profile grants %s, which its own note says it withholds", scope)
		}
	}
	// And the one it says it grants really is there, so the profile is
	// not simply empty.
	if !fin.Granted.Has(domain.ScopeHostWrite) {
		t.Error("the finance profile withholds host.write; its documented intent is host-scoped repair")
	}
}

func TestAllIsSortedAndStable(t *testing.T) {
	// A listing an operator reads twice must come back in the same order.
	first := All()
	second := All()
	if len(first) != len(second) {
		t.Fatalf("two calls returned %d and %d profiles", len(first), len(second))
	}
	for i := range first {
		if first[i].Name != second[i].Name {
			t.Fatalf("call order changed between two calls: %v vs %v", first[i].Name, second[i].Name)
		}
		if i > 0 && first[i-1].Name >= first[i].Name {
			t.Errorf("profiles are not sorted: %q before %q", first[i-1].Name, first[i].Name)
		}
	}
}

func TestAProfilePolicyIsUsableAndFinite(t *testing.T) {
	// A profile whose ceiling or radius fails Valid() would produce a
	// Policy that refuses everything, and the symptom would be a fleet
	// refusing plugins rather than a config error.
	for _, p := range All() {
		if !p.Ceiling.Valid() {
			t.Errorf("profile %q has an invalid ceiling %q", p.Name, p.Ceiling)
		}
		if !p.MaxRadius.Valid() {
			t.Errorf("profile %q has an invalid radius %q", p.Name, p.MaxRadius)
		}
		if len(p.Granted) == 0 {
			t.Errorf("profile %q grants no scopes, so it could host nothing", p.Name)
		}
		if !(domain.InstallPolicy{Strategy: p.Strategy}).Valid() {
			t.Errorf("profile %q has an invalid install strategy %q", p.Name, p.Strategy)
		}
	}
}

func TestAProfileWithAnUnsetCeilingIsNotSilentlyPermissive(t *testing.T) {
	// A zero Profile is not a mistake this code can prevent, but the
	// Policy it produces must still be the fail-closed one: an empty
	// ceiling refuses at admission rather than admitting everything.
	var zero Profile
	pol := zero.Policy()
	if pol.MaxSafetyLevel != "" {
		t.Fatalf("a zero profile produced ceiling %q, want empty", pol.MaxSafetyLevel)
	}
	if err := admitUnder(pol, domain.SafetyL0); err == nil {
		t.Error("a zero-value profile admitted a package; the zero Policy must refuse")
	}
}

// admitUnder runs a real package through the real admission rule under the
// profile's policy, so the test asserts admission rather than a field. It
// calls sdk.Admit rather than restating the rule, which is the whole point:
// a profile that looked right and admitted wrongly would pass a field-level
// assertion.
func admitUnder(pol Policy, level domain.SafetyLevel) error {
	return sdk.Admit(domain.PluginManifest{
		Spec: domain.PluginSpec{
			Targets:     domain.Targets{domain.TargetEdge},
			SafetyLevel: level,
		},
	}, sdk.Admission{
		GrantedScopes:  pol.GrantedScopes,
		MaxSafetyLevel: pol.MaxSafetyLevel,
		MaxBlastRadius: pol.MaxBlastRadius,
	})
}
