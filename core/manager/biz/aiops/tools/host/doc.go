// Package host is the host-plane tool cluster: the tools that ask a node
// what state it is in.
//
// It is the first of the clusters lifted out of the tools package, and it
// is the shape the rest should follow. What it depends on is deliberately
// short — the shared vocabulary in toolcore, the device and edge usecases,
// the tunnel types, and the decorators that wrap a tool for audit and
// rate limiting. What it does not depend on is the Registry, the god
// object the remaining tools still hang off.
//
// Two execution surfaces live here and they are not the same thing. The
// BaseTools are what the in-process agent loop presents to the model: they
// are batch-first, taking a device_ids array. The Registry methods that
// stayed in the parent package are the upcall half, the older single-device
// shape the node's own agent reaches. Both answer to the same wire names,
// and identity.go is what keeps the two from drifting apart.
//
// A tool that wants a resolver it did not build calls
// NewGetHostLoadToolWithResolver rather than reaching for one. Reaching
// into a struct field of another package is the coupling this package
// exists to end, and a constructor is the only way not to add it back.
package host
