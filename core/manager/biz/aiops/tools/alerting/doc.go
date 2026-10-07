// Package alerting holds the four tools that read the alert domain:
// query_incidents, get_incident_detail, query_alert_rules and
// query_change_events.
//
// What makes this a cluster rather than a coincidence is that all four
// read through one seam, AlertUsecase (plus the two lister seams
// query_change_events adds), and that all four declare their wire
// identity the same way. identity.go now owns that wire identity, so
// the BaseTool form and the node-side Registry form of the same tool
// read one name, one description, one schema and one timeout. Before
// the split those lived next to the Registry method, which meant the
// two forms of a tool could drift and nothing would say so.
//
// correlate_incident is deliberately NOT here, and the reason is worth
// writing down because "why didn't you just move it" is the obvious
// next question. It needs thirteen symbols from the tools package, all
// of them the panel types and querier seams that implement its
// metric/log/trace/edge fan-out. Moving it would mean moving that
// whole mechanism too, which is a different and much larger cut. It
// stays, and it consumes alerting.AlertUsecase like the rest of the
// parent package does.
//
// The other asymmetry: the four Registry methods stayed in the parent
// package. A method cannot be lifted off its receiver, and these four
// are not vestigial — they are the entry point the node-side pig agent
// reaches through an upcall, not through the BaseTool bag. Both paths
// now share identity.go, so the split costs indirection rather than
// consistency.
package alerting
