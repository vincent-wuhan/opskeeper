// Package database answers questions about the databases OpsKeeper
// collects metrics for: MySQL, PostgreSQL, Redis and MongoDB.
//
// It ships two tools. analyze_database_status reads the exporter metrics
// for one or more sources and returns a per-source verdict — health,
// connection pressure, slow queries, memory, replication, plus a
// capability matrix saying which of those the current exporter
// configuration can actually answer. list_database_sources answers the
// smaller question of what is configured, without querying Prometheus at
// all, so the model can route rather than guess.
//
// This was the cleanest large extraction in the package, and the reason
// is worth recording: the implementation is reachable without a single
// Registry method on it. Both entry points are plain structs with
// exported fields, so the two Registry methods that stayed behind in the
// parent package are ten lines each — construct a runner, call Run. There
// is no panel mechanism welded to Registry here, which is exactly what
// makes correlate_incident a different and much larger job.
//
// Two things that look like database concerns and are not, and so live
// elsewhere: the PromQL value plumbing went to toolcore beside the
// querier it operates on, because the metric catalog needs the same
// three helpers; the PromQL call timeout went with it, for the same
// reason. Neither belongs to a database, and both used to be reachable
// only by reaching into this file.
package database
