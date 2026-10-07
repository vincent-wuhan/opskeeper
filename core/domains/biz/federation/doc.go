// Package federation is the root control plane's side of cluster federation:
// which child clusters exist, who is allowed to speak for each of them, and
// what policy version each has been told to enforce.
//
// # What the root does and does not do here
//
// The root publishes. It does not approve, does not arbitrate, and is not on
// the path of an action inside a child cluster. That is the whole reason a
// child survives the root being down, and it is why everything in this package
// is about *delivering a decision* rather than about making one.
//
// The consequence worth stating plainly: the root's view of a child is a
// claim, and it is the weaker of the two. The child is the thing enforcing
// policy; the root is the thing that published it. When they disagree about
// which version is live, the child is right — and this package is written to
// reconcile towards the child rather than to overwrite it.
//
// The rules about what a version *means* — monotonic, no rollback message,
// refusals replayed as refusals — live in core/floor/federation, because a
// child enforces them whether it heard the decision over a tunnel or read it
// off a disk someone carried in by hand. See that package for why that
// separation is the security boundary rather than a convenience.
package federation
