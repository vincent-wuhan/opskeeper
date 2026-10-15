package skill

import (
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	skillcore "github.com/vincent-wuhan/opskeeper/core/floor/skill"
)

// authorize is a security decision with no test on it.
//
// Three classes, three policies, and the third one — deny everyone — was the
// only one nobody had written down. That is how a *deliberate* fail-closed
// gate becomes indistinguishable from a feature that was never built: the
// code refused either way, and only the comment said which.

// The policy itself, all three classes and the roles each one admits.
func TestAuthorizeIsAThreeClassPolicy(t *testing.T) {
	cases := []struct {
		class   skillcore.Class
		role    string
		allowed bool
	}{
		{skillcore.ClassSafe, "admin", true},
		{skillcore.ClassSafe, "user", true},
		{skillcore.ClassSafe, "viewer", true},
		{skillcore.ClassSafe, "", true},

		{skillcore.ClassMutating, "admin", true},
		{skillcore.ClassMutating, "user", true},
		{skillcore.ClassMutating, "viewer", false},
		{skillcore.ClassMutating, "", false},
		{skillcore.ClassMutating, "operator", false},

		// The one that matters: no role, including admin, reaches a
		// dangerous skill until PR-G4 lands.
		{skillcore.ClassDangerous, "admin", false},
		{skillcore.ClassDangerous, "user", false},
		{skillcore.ClassDangerous, "viewer", false},
		{skillcore.ClassDangerous, "", false},
		{skillcore.ClassDangerous, "root", false},
	}

	for _, tc := range cases {
		err := authorize(tc.class, tc.role)
		if tc.allowed && err != nil {
			t.Errorf("authorize(%s, %q) = %v, want allowed", tc.class, tc.role, err)
		}
		if !tc.allowed {
			if err == nil {
				t.Errorf("authorize(%s, %q) allowed, want refused", tc.class, tc.role)
				continue
			}
			// Every refusal has to be a refusal for a reason this package
			// owns, so a caller can distinguish "policy said no" from
			// "something upstream broke".
			if !errors.Is(err, errs.ErrForbidden) {
				t.Errorf("authorize(%s, %q) = %v, which is not ErrForbidden", tc.class, tc.role, err)
			}
		}
	}
}

// The reason a dangerous skill cannot run is a policy with a named condition,
// and the message is the only place an operator ever learns that.
//
// "not implemented" is the specific wrong answer: it says the capability is
// missing, which sends the reader to file a bug against a decision nobody is
// going to revisit, and away from the one question they actually have — under
// what condition does this become runnable.
func TestTheDangerousRefusalNamesThePolicyNotAMissingFeature(t *testing.T) {
	err := authorize(skillcore.ClassDangerous, "admin")
	if err == nil {
		t.Fatal("a dangerous skill was allowed")
	}
	msg := err.Error()

	for _, wrong := range []string{"not implemented", "unimplemented", "todo", "not yet"} {
		if strings.Contains(strings.ToLower(msg), wrong) {
			t.Errorf("the refusal says %q: %s\n"+
				"This is a deliberate fail-closed gate, not a missing feature. A reader who is told "+
				"it is unimplemented files a bug instead of asking what would change the answer.",
				wrong, msg)
		}
	}
	// And it has to name the condition, or the corrected message is just a
	// refusal with better manners.
	for _, want := range []string{"policy", "PR-G4"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q, so it still does not say what would change "+
				"the answer: %s", want, msg)
		}
	}
}

// An unrecognised class is not a safe default. Treating it as anything but an
// error would make adding a fourth class a silent permission change.
func TestAnUnknownClassIsRefusedRatherThanTreatedAsSafe(t *testing.T) {
	if err := authorize(skillcore.Class("catastrophic"), "admin"); err == nil {
		t.Fatal("a class this build has never heard of was allowed")
	}
}
