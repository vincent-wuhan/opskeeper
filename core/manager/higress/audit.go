package higress

import (
	"net/http"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
)

// auditOK / auditFail mirror the pair in core/manager/server/loop and
// core/domains/server/nodeagent.
//
// What is different here, and what every reader of this file needs to know:
// these rows only exist because cmd/higress-console mounts
// middleware.AuditMiddleware around srv.Routes(). SetAuditEvent is a
// documented no-op without that slot, so a handler in this package that
// calls it writes nothing at all in a process that forgot the middleware —
// and nothing in this repository's tests can tell the difference, because
// a no-op and a write look identical from inside the handler. The entry-point
// table in scripts/routeaudit is what holds that fact (decision 321); this
// comment is the other end of it (decision 324).
func auditOK(r *http.Request, ev auditport.Event) {
	ev.Status = auditport.StatusSuccess
	auditport.SetAuditEvent(r, ev)
}

func auditFail(r *http.Request, ev auditport.Event, cause error) {
	ev.Status = auditport.StatusFailure
	if cause != nil {
		ev.ErrorMessage = cause.Error()
	}
	auditport.SetAuditEvent(r, ev)
}

// consumerPayload describes a consumer without its credential.
//
// The apikey is the credential this console exists to hold, so it never
// reaches the chain — not even as a digest. A digest of a 32-byte random key
// buys nothing (there is nothing to compare it against but another row
// holding the same key, and the store already keeps a fingerprint for that
// job) while a row that says "this key, in this order, hashed" is one more
// place the key has been written down. The store's own ApikeyHash is not
// repeated either: it is the value a consumer is looked up by, and putting
// it in a log turns a lookup key into a bearer credential.
func consumerPayload(c Consumer) map[string]any {
	p := map[string]any{
		"name":         c.Name,
		"jwt_required": c.JWTRequired,
		"worker_claim": c.WorkerClaim,
		"role_claim":   c.RoleClaim,
		"tenant_claim": c.TenantClaim,
	}
	if c.MetadataJSON != "" {
		p["metadata_len"] = len(c.MetadataJSON)
	}
	return p
}
