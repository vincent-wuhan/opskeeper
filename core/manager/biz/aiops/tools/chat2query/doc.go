// Package chat2query turns a natural-language question into a PromQL,
// LogQL or TraceQL expression, and runs it.
//
// It was the cleanest of the clusters to lift out: after the three
// queriers moved to toolcore, it had no dependency outside itself and no
// method on Registry. That is what a finished extraction looks like, and
// it is worth naming the condition rather than the file count — the other
// clusters still owe a registry method they cannot move.
//
// The shape is three stages, and the seams between them are interfaces
// declared here rather than in the wiring: a Translator that asks a model
// for an expression, a Validator that refuses anything the cluster will
// not run, and an Executor that actually asks the backends. A test can
// replace any one of them, and the production wiring in
// registry_basetool does exactly that through NewPromCatalogFetcher.
package chat2query
