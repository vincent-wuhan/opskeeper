// Package metriccatalog answers "which metrics do I actually have, and what
// do they look like" — the question a model has to get right before it can
// write a metric-based alert rule from natural language.
//
// It lists the metric names Prometheus is currently scraping, with series
// counts and a few representative label sets per name, filtered by prefix,
// name regex, selector or labels, and scored against the user's question so
// the most likely candidates come back first.
//
// Why this is its own package and not a file in the parent: it is the one
// tool whose whole job is to know what the other tools' metrics are called.
// Keeping it next to query_promql meant the catalog and the tool it exists
// to feed were neighbours by file order, with the metric name tables and
// the label-priority rules living among both.
//
// The PromQL plumbing it shares with the database analyzer already lives in
// toolcore, next to the querier it operates on, so nothing here reaches
// sideways for it.
//
// The one Registry method stayed in the parent package — a method is welded
// to its receiver, and the node-side pig agent reaches this tool through an
// upcall rather than through the BaseTool bag. It is ten lines that build a
// Runner and call Run.
package metriccatalog
