// Package correlate pulls every signal around one alert incident into a
// single bundle: the metric series that fired the rule, error logs from
// the same device in the same window, slow and erroring traces, and the
// edge's own recent state changes.
//
// It is the one tool in this tree that reaches four backends at once, and
// the reason it was the hardest of the extractions is worth writing down
// because the obvious next question is "why could this not just move like
// the other five".
//
// It could, but not before the duplication went. The fan-out existed
// twice: once as Registry methods for the node-side upcall, once as
// methods on CorrelateIncidentTool for the BaseTool bag. The second copy
// carried a comment saying it "mirrors Registry.queryLogPanel", and the
// copies had already drifted on the wire — the Registry one computed its
// JSON before nil-ing an empty Truncated map, so it emitted a
// "truncated":{} that the BaseTool one omitted. A model reading both
// shapes for one tool is how a downstream prompt quietly stops matching.
//
// Fanout is the single implementation, and both entry points build one.
// Collapsing the copies is what made the extraction possible, and it is
// why the remaining Registry method in the parent package is dispatch
// only: twenty lines that construct a Fanout and call BuildBundle.
package correlate
