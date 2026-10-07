// Package federation holds the vocabulary a root control plane and a child
// cluster share, so that the root can push policy downward and the child can
// keep running when the root is gone.
//
// # What this is for
//
// The plan's line is: "多集群联邦：中心下发策略而非实时决策，子集群可独立运行。"
// Both halves of that sentence are load-bearing and they pull against each
// other, which is what makes this package exist.
//
// "下发策略而非实时决策" says the root's job ends at publishing a version. It
// is not on the path of an action: a child does not ask the root "may I restart
// this service" and wait. That is what makes the root's outage survivable, and
// it is also what makes the root a single point of failure for everything else
// about the cluster. The design answer is that the root publishes *policy*, and
// the child keeps *enforcing* it locally, exactly as a node already enforces
// what the control plane told it to enforce.
//
// "子集群可独立运行" is the harder half, because a child that has applied a
// policy and then loses the root still has to keep enforcing the same one. That
// is the whole reason Apply below is a state machine with a recorded outcome
// rather than a call that returns an error and forgets.
//
// # The rule that carries the security weight
//
// A bundle's version is the sequence number of the *decision*, not of the
// content. Version 7 is "the seventh policy decision this root made", whatever
// that decision was.
//
// That definition is what makes rollback safe. A rollback is not a bundle
// saying "go back to version 5" — it is a new bundle, version 8, carrying the
// content of version 5, because version 6 was wrong. There is no message in
// this package that can move a version backwards.
//
// Without that rule, every signed bundle ever published is a weapon: they
// never expire, so anyone who can capture one can replay an old, perfectly
// valid, perfectly signed policy onto a child and walk it back to a
// deliberately weakened configuration. The monotonic rule is the whole defence,
// and it is why Apply refuses a version it has already seen — including one it
// already *refused*, which is what makes a retry idempotent instead of a way to
// re-run a rejected policy through a different code path.
//
// # Why the signature is not re-invented here
//
// A policy bundle is a plugin tree plus a decision about it, so it travels the
// channel that already exists: pluginmanifest signs the tree, the TrustStore
// resolves the key, and VerifyDir / Review decide whether the bytes on this
// disk are the bytes the operator's release key vouched for. A second signing
// scheme for the same artifact would mean two keys to rotate, two revocation
// lists, and a window where the two disagree about what is installed.
package federation
