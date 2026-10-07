// Package topology is the cluster-graph tool cluster: the tools that
// answer questions about the shape of the fleet rather than about one
// host.
//
// It follows the shape host established. What a tool needs comes from
// toolcore, from the edge and device usecases, and from the tunnel
// types. It does not reach into the Registry, and the two upcall
// executors that stayed in the parent package name their wire identity
// here rather than keeping a second copy of it.
//
// The split is not even, and the reason is written down rather than left
// for the next reader to infer. Four BaseTools moved because they could:
// they depend on shared vocabulary and on each other, and on nothing
// else. The upcall executors stayed because they are methods on Registry,
// and Registry is the dispatch surface a node's agent reaches through.
// The seam is a method, and a method cannot be moved without moving the
// thing it hangs off.
//
// That leaves a deliberate asymmetry — the metric vocabulary, the
// argument types and the Prometheus decoding are declared here and used
// from two packages — which is exactly the coupling the next extraction
// is meant to remove. It is recorded in identity.go and promql.go rather
// than hidden, because an asymmetry nobody wrote down is one the next
// person reads as an accident.
package topology
