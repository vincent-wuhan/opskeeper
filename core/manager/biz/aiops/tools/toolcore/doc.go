// Package toolcore is the vocabulary every tool cluster shares.
//
// The tools package grew to a hundred and twenty-nine files, and its size
// was the wrong number to look at. What actually made it hard to change
// was that the shared words — the seam to the edge, the shape of a result,
// the batch fan-out, the redirect stub — lived in the same package as every
// tool, so no tool could be lifted out without dragging the god Registry
// and all one hundred and twenty-nine files with it.
//
// This package is those words and nothing else. A cluster that depends on
// toolcore and on two or three narrow interfaces can be moved to its own
// package and evolve on its own; a cluster that needs the Registry is
// telling you it is not finished being separated yet.
//
// It depends on basetool and the standard library. It must not come to
// depend on the Registry, on any biz package, or on any cluster — the day
// it does, the words stop being shared and become another thing everything
// imports.
package toolcore
