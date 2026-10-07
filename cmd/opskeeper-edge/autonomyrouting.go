package main

// Why this file exists, in one paragraph, because the change it carries
// looks like a special case and is the opposite of one.
//
// A node's autonomy is meant to apply while the control plane is away. The
// arbiter enforces that itself: it defers to the approval gate whenever the
// centre answers, and only adjudicates on its own once the link has been
// down long enough. That contract was sound, complete, and tested — the
// arbiter, the local audit spool, the replay pump, the tunnel method and the
// centre's audit chain all existed and all worked.
//
// And none of it could ever run, because the only way in required the very
// link the arbiter insists on being without. The model calls
// host_autonomy_run; the node's tool router sees a non-read skill and
// upcalls it to the control plane, because a mutating call's approval
// authority is deliberately not the node's; and the node's approval policy
// demands a receipt for it, because the skill's class is destructive. Both
// legs need the tunnel. So the request could not arrive during an outage,
// and during an outage is the only time the arbiter would have said yes.
//
// The fix is one rule in two places, and the rule is about *who holds the
// permission*, not about relaxing a check:
//
//   - While the control plane is reachable, nothing changes. The call is
//     still upcalled and still needs a receipt, so the approval channel is
//     exactly the channel it was.
//   - While it is not, the permission for this one tool is the signed
//     manifest — the autonomy block a package could not have installed
//     without review and a signature — and the arbiter is the authority
//     that decides. Every check it makes still runs: the action must be
//     declared, its trigger must actually hold against the node's own
//     reading, the argv must be the declared one byte for byte, the reach
//     must be inside the node's ceiling, the TTL must not have expired, and
//     the idempotency key must be unspent.
//
// The exemption is keyed on one tool name and on the link being down. A node
// with no autonomy installed cannot be made to run anything by this rule,
// because the arbiter does not exist there and the tool answers "this node
// has no autonomy declared" before it looks at the request.

// autonomyIsLocal reports whether the node must answer an autonomy request
// itself rather than handing it to the control plane.
//
// It is deliberately the *link* and not the arbiter's own verdict. The
// arbiter would also answer "run" for a request that arrives while the
// centre is briefly flapping, and routing on the link instead means the two
// answers cannot come from two different clocks: the node either asks the
// centre (and the centre's approval channel decides) or it does not.
func autonomyIsLocal(obs autonomyObservations) bool {
	if obs == nil {
		// No observations means no witness of the link, and a node that
		// cannot tell whether the centre is there must behave like a node
		// whose centre is there: ask.
		return false
	}
	online, _ := obs.LinkReach()
	return !online
}
