// Package tenantctx stores the per-request caller identity on
// context.Context.
//
// The auth middleware (core/base/pkg/auth) decodes JWT claims and calls With;
// downstream service/biz/data layers read the Tenant via From to check role /
// ownership. The name "tenant" is vestigial — in the single-tenant private MVP
// there is just one user namespace, so Tenant is really just the
// authenticated caller.
package tenantctx

import "context"

// ----- system role vocabulary -----
//
// The role strings that ride on Tenant.Role are spoken by every bounded
// context: the iam BC persists them in `users.role`, the auth middleware
// mints them into the JWT, and then a dozen HTTP handlers compare
// `t.Role` against one of them to decide who may mutate. That makes them
// the most-repeated string in the control plane.
//
// Until decision 229 they had no single owner. iam/model held the
// canonical three, and everyone else either re-declared the literal
// locally (six copies of `roleAdmin = "admin"` across domains and
// manager, each carrying a comment that it must be kept in sync "by
// convention") or compared against the bare string. A convention is not
// a mechanism: it survives only as long as every copy is remembered, and
// the copies are in packages that cannot see the declaration they are
// mirroring, so nothing would have told us if one drifted.
//
// The vocabulary lives here rather than in iam/model because of who reads
// it. Tenant.Role is declared in this file, so the legal values of that
// field are this package's business, and `core/base/pkg` is the one tree
// every bounded context is already allowed to import — iam included. A
// handler in domains and the iam persistence model can therefore name
// the same constant without either of them importing the other, which is
// what lets the imbridge -> iam edge disappear rather than move.
//
// Adding a role is still iam's decision: it owns the users table and
// RoleCanMutate. This package only holds the strings, so that the
// decision has exactly one definition every reader can reach.
const (
	// RoleAdmin is the SRE on call: full platform authority including
	// user and org management. In iam/model it is also the fallback the
	// auth middleware reads to decide IsSuperuser for tokens minted
	// before that claim shipped.
	RoleAdmin = "admin"
	// RoleUser can use every product feature, including the full chat
	// tool set, but cannot change platform configuration or manage users.
	RoleUser = "user"
	// RoleViewer is read-only. Its chat toolbag is filtered down to the
	// safe tool classes, and RoleCanMutate is false for it alone.
	//
	// Distinct from MembershipRoleViewer, which is the org-scoped
	// "viewer" in `org_memberships.role` — a different column with a
	// different subject.
	RoleViewer = "viewer"
)

// Tenant is the caller's user id + email + role + superuser flag,
// populated from the JWT claims by the auth middleware. Role holds one
// of the Role* constants below. IsSuperuser is the authoritative system-admin
// flag — the auth middleware sets it from the IsSuperuser JWT claim,
// falling back to Role=="admin" for tokens issued before the claim
// shipped. Email is included so the audit middleware can label rows
// without an extra DB lookup (added 2026-05-21 after the audit_view
// rows showed up with empty user fields).
type Tenant struct {
	UserID      uint64
	Email       string
	Role        string
	IsSuperuser bool
	AgentTeams  *AgentTeamsIdentity
}

// AgentTeamsIdentity 是已验证 Worker token 内嵌的服务身份。MCP 授权边界会在
// 工具分发前校验 tenant、service、worker、role 与工具白名单。
type AgentTeamsIdentity struct {
	TenantID     string
	Service      string
	Worker       string
	Role         string
	AllowedTools []string
}

type ctxKey struct{}

// With attaches t to ctx for downstream handlers (service / biz / data
// layers) — they read it via From. Set on the request context AFTER
// the auth middleware verifies the JWT.
func With(ctx context.Context, t Tenant) context.Context {
	return context.WithValue(ctx, ctxKey{}, t)
}

// From extracts the Tenant from ctx. The bool is false when no tenant
// has been attached (e.g. public endpoint or missing middleware). When
// a mutable slot is installed and populated, From prefers the slot —
// this lets outer middlewares (audit) see the tenant value that an
// inner middleware (auth) wrote even though the outer middleware's
// request reference doesn't carry the inner WithContext.
func From(ctx context.Context) (Tenant, bool) {
	if s, ok := ctx.Value(slotKey{}).(*slot); ok && s != nil && s.set {
		return s.t, true
	}
	t, ok := ctx.Value(ctxKey{}).(Tenant)
	return t, ok
}

// ----- mutable slot -----
//
// Mirrors the auditSlot pattern in the audit middleware: a *slot
// pointer in the OUTER request context lets outer code see what an
// inner middleware wrote, even though the inner's r.WithContext()
// produced a new ctx that the outer didn't capture.
//
// Wired by:
//   - AuditMiddleware: WithSlot(ctx) BEFORE next.ServeHTTP
//   - auth.Middleware: SetOnSlot(ctx, t) AFTER verifying the JWT
//   - enrichFromRequest (audit middleware post-handler): reads via From

type slotKey struct{}

type slot struct {
	t   Tenant
	set bool
}

// WithSlot installs an empty mutable Tenant slot in ctx. Subsequent
// SetOnSlot calls (typically from the auth middleware) populate it;
// From reads from the slot first so outer middlewares pick up the
// inner-set value.
func WithSlot(ctx context.Context) context.Context {
	return context.WithValue(ctx, slotKey{}, &slot{})
}

// SetOnSlot writes t into the slot stored in ctx, if one is installed.
// No-op when no slot was installed (defensive — public endpoints don't
// install one and shouldn't crash auth middleware).
func SetOnSlot(ctx context.Context, t Tenant) {
	if s, ok := ctx.Value(slotKey{}).(*slot); ok && s != nil {
		s.t = t
		s.set = true
	}
}
