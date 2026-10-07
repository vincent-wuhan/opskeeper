// Package querybackend holds the three "the model already wrote the
// expression, now run it" tools: query_promql, query_logql and
// query_traceql.
//
// They are not merged with chat2query and are not a general observability
// layer. chat2query takes a question in English and *writes* the expression;
// these three take an expression the caller already has and only *run* it. The
// split matters because the two halves have opposite trust shapes: a written
// PromQL is bounded by toolcore.MaxQueryPromQLLookbackSeconds and
// toolcore.StepFor, while a model that may invent an expression needs the
// seven-day cap and the coarsening step precisely *because* it does not know
// what it is asking for. Merging them would make it impossible to hold either
// line.
//
// The reason this is one package rather than three is that the three tools are
// the same shape three times: a wire name, a description, a schema, a typed
// args struct, a parse-a-time-window step, a per-dispatch timeout, one backend
// call, and a JSON marshal. They differ only in the backend and in which
// filters they insist on. Anything the third one needed and the first two did
// not was the reason each was its own file; what they all needed is why they
// are one package.
//
// It also fixes a duplication that had been carried for a long time. Each tool
// had two implementations — a Registry method (the node-side upcall path) and a
// BaseTool (the bag the model calls directly) — and each pair was nearly
// line-for-line identical, held in step by a comment that said "mirrors". That
// is the same failure the correlate cluster had: two copies of one mechanism do
// not stay in step, and when they drift the model sees two different JSON
// shapes for one tool name. Here the shared work is RunQueryPromQL /
// RunQueryLogQL / RunQueryTraceQL, and both entry points are three lines
// around it, so there is nothing left to mirror.
package querybackend
