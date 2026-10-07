package tenantctx

import "testing"

// TestRoleValuesAreWireFormat is the reason this package holds the
// vocabulary at all.
//
// The three strings are not internal identifiers. They are written into
// the `users.role` column by the iam BC, minted into JWT claims by the
// auth middleware, and compared by every handler that gates a mutation.
// A deployment that upgrades the binary keeps its rows and its unexpired
// tokens, so a value that changes here does not fail a test on a fresh
// database — it silently reclassifies every existing admin as a viewer,
// and the first symptom is a 403 in production on a route that worked
// yesterday.
//
// Pinning the literals is therefore the honest reading of "these are
// constants": it is the one property here that must never move, and the
// cheapest place to say so is the package that now owns them.
func TestRoleValuesAreWireFormat(t *testing.T) {
	for _, c := range []struct{ name, got, want string }{
		{"RoleAdmin", RoleAdmin, "admin"},
		{"RoleUser", RoleUser, "user"},
		{"RoleViewer", RoleViewer, "viewer"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestRolesAreDistinct catches the copy that would be invisible at the
// call sites: a handler comparing `t.Role != RoleAdmin` still compiles
// and still passes its own test if RoleAdmin were accidentally given
// RoleViewer's value, and the effect would be a handler that admits
// viewers and refuses admins.
func TestRolesAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for name, v := range map[string]string{
		"RoleAdmin":  RoleAdmin,
		"RoleUser":   RoleUser,
		"RoleViewer": RoleViewer,
	} {
		if prev, dup := seen[v]; dup {
			t.Errorf("%s and %s are both %q", prev, name, v)
			continue
		}
		seen[v] = name
	}
}
