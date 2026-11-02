package notify

import (
	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/datasource"
)

// Policy is the admission ceiling for a control-plane host that hosts
// notification channel plugins.
//
// It is derived from the datasource policy rather than written from scratch,
// so the two extension points cannot drift apart by accident: a change to the
// control-plane ceiling is a change to one place, and the notifier inherits
// it. The derivation has exactly two deltas, and the count is load-bearing
// rather than incidental.
//
// The first delta is the KindNotifier ceiling. A notification channel writes
// into somebody else's system — a Slack workspace, a Feishu group, a
// customer's Webhook — so L3 is the honest level for it, while the global
// ceiling stays L2 for everything else on the host. The widening goes
// through WithWiderKindCeiling because the notifier ceiling is wider than
// the global one; WithKindCeiling is the narrowing entry point and panics
// when asked to widen.
//
// The second delta is the alert.write scope. A channel package has to be
// able to write an alert out to the outside world, and a package that asks
// for a scope the host has not granted is refused at Admit with
// "not granted: alert.write". Without this grant all six L3 manifests die
// before they reach the registry.
//
// The consequence is stated plainly because it outlives this slice:
// alert.write is now granted at host level, which means any package on this
// host that declares it is granted it, and the package-level ceiling for
// this host is now L3 while the per-kind refusal still stands — an L3
// package registering into KindTool is still refused by the per-kind gate.
func Policy() kernel.Policy {
	p := datasource.ManagerPolicy()

	scopes := make(domain.Scopes, 0, len(p.GrantedScopes)+1)
	scopes = append(scopes, p.GrantedScopes...)
	scopes = append(scopes, domain.ScopeAlertWrite)
	p.GrantedScopes = scopes

	return p.WithWiderKindCeiling(kernel.KindNotifier, domain.SafetyL3)
}
