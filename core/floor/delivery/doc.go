// Package delivery holds the assertions about how a release reaches a node.
//
// It contains no runtime code, and that is the point. Everything it checks
// is a fact about files outside the Go build — a Makefile target, a shell
// bundle script, a Dockerfile, an install script — and every one of those is
// a place where the node's AI agent can be dropped without a single compiler
// or test failing.
//
// The agent is `pig`, the edge spawns it as a child process, and a node
// without it is not visibly broken: the service starts, the tunnel
// authenticates, metrics flow, and the console shows a healthy edge. What is
// missing is the ability to answer anything, and nothing in the product
// surface says so. So the links between "the source says the node has an
// agent" and "a tarball on a host contains one" are asserted here, by name,
// because the failure they guard is otherwise found by a customer.
package delivery
