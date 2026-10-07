package model

import (
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
)

// TestRolesAreAliasesNotCopies is the assertion behind decision 229.
//
// Before it, iam/model declared `RoleAdmin = "admin"` and five other
// packages declared their own `roleAdmin = "admin"`, each with a comment
// telling the reader to keep them in sync. Nothing could: the copies
// lived in packages that arch-lint keeps away from iam, so a divergence
// would have shown up as a handler that quietly stops admitting admins,
// and only in production.
//
// The three names here now alias tenantctx, which is the declaration
// site of the Tenant.Role field they travel on. This test is what keeps
// them aliases. Re-declaring one as a literal still compiles — the
// constants are untyped strings and nothing at the call sites can tell
// the difference — so the failure has to be a test, and it has to
// compare values rather than lengths.
func TestRolesAreAliasesNotCopies(t *testing.T) {
	for _, c := range []struct {
		name      string
		iam, base string
	}{
		{"RoleAdmin", RoleAdmin, tenantctx.RoleAdmin},
		{"RoleUser", RoleUser, tenantctx.RoleUser},
		{"RoleViewer", RoleViewer, tenantctx.RoleViewer},
	} {
		if c.iam != c.base {
			t.Errorf("%s = %q but tenantctx.%s = %q; the alias was replaced with a copy",
				c.name, c.iam, c.name, c.base)
		}
	}
}

// TestIsValidRoleCoversTheVocabulary is the other half: the validator and
// the vocabulary must not drift apart either. A role added to tenantctx
// but not to IsValidRole would be persisted and then refused by every
// handler that asks, and the first report would be a confusing 400 on a
// role the operator can see in the table.
func TestIsValidRoleCoversTheVocabulary(t *testing.T) {
	for _, r := range []string{tenantctx.RoleAdmin, tenantctx.RoleUser, tenantctx.RoleViewer} {
		if !IsValidRole(r) {
			t.Errorf("IsValidRole(%q) = false, but it is one of the three system roles", r)
		}
	}
	if IsValidRole("") || IsValidRole("root") || IsValidRole("Admin") {
		t.Error("IsValidRole accepted a value outside the vocabulary; it is case-sensitive on purpose")
	}
}

// TestRoleCanMutateOnlyExcludesViewer pins the one behavioural rule that
// lives on these constants. It is here because the rule is expressed in
// terms of them, and a reader who wants to know what a role may do
// should not have to open the auth middleware to find out.
func TestRoleCanMutateOnlyExcludesViewer(t *testing.T) {
	if RoleCanMutate(tenantctx.RoleViewer) {
		t.Error("viewer must not be able to mutate")
	}
	for _, r := range []string{tenantctx.RoleAdmin, tenantctx.RoleUser} {
		if !RoleCanMutate(r) {
			t.Errorf("%s must be able to mutate", r)
		}
	}
}
