package main

import (
	"fmt"
	"log/slog"
	"os"

	"gorm.io/gorm"

	managerbizaudit "github.com/vincent-wuhan/opskeeper/core/domains/biz/audit"
	audittstore "github.com/vincent-wuhan/opskeeper/core/domains/data/audit/store"
)

// The gateway's audit trail, and why it is a chain of its own (决策 324).
//
// Until this file existed, cmd/higress-console served every request from
// srv.Routes() with no middleware at all. That is invisible from the
// handlers: SetAuditEvent is documented to no-op without a slot, so a
// handler could be perfectly audited and write nothing at all — which is
// exactly what routeaudit would have reported as done. Decision 321 found
// the gap by asking a different question (does the *process* install the
// slot?) rather than the one it had always asked (does the *handler* call
// SetAuditEvent?).
//
// So this wires the same usecase, the same chain stamper and the same
// chain store that the control plane uses, over this process's own SQLite
// file. Reuse, not a second implementation: the canonicalisation, the
// length-prefixed digest and the compare-and-swap on the chain head are
// the parts that are easy to get subtly wrong, and a second copy of them
// is a second chance to get them subtly wrong.
//
// **Separate chain, separate key — and that is the decision, not an
// accident.** The chain store advances its head with a compare-and-swap
// and the caller retries on conflict, so two processes appending to one
// database would in fact be safe. The reason not to is asymmetric trust:
// this process holds OPSKEEPER_JWT_SECRET, the secret every Higress
// consumer's apikey is signed with. A gateway that could write into the
// control plane's chain could forge control-plane audit rows, so a
// compromise here would silently buy an attacker the ability to rewrite
// what the control plane says happened to it. Two chains, two keys, two
// databases: the gateway's rows answer "what did this gateway do", the
// control plane's answer "what did my control plane do", and neither can
// speak for the other.
//
// With no key configured the rows are still written, unchained — an empty
// audit trail is a worse failure than an untamper-evident one — and the
// startup warning says so out loud instead of handing the operator a
// clean bill of health from an absence.

// buildAuditSink wires the gateway's own audit trail over db and returns the
// sink the audit middleware emits into.
func buildAuditSink(db *gorm.DB, log *slog.Logger) (*managerbizaudit.Usecase, error) {
	if err := audittstore.Migrate(db); err != nil {
		return nil, fmt.Errorf("migrate higress audit tables: %w", err)
	}
	key := os.Getenv("OPSKEEPER_HIGRESS_AUDIT_HMAC_KEY")
	if key == "" {
		log.Warn("audit: OPSKEEPER_HIGRESS_AUDIT_HMAC_KEY is not set; the gateway trail will be recorded but not tamper-evident",
			slog.String("hint", "generate with: openssl rand -hex 32"),
			slog.String("scope", "higress console"))
	}
	return managerbizaudit.New(
		audittstore.New(db),
		log.With(slog.String("comp", "audit"), slog.String("scope", "higress")),
		managerbizaudit.WithChain(key, audittstore.NewChainStore(db)),
	), nil
}
