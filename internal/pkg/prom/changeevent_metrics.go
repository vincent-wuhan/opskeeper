package prom

import "github.com/prometheus/client_golang/prometheus"

// Change event observability (A.3 follow-up). Edge pushes batches of
// changewatcher events over the tunnel; manager persists into
// edge_change_events. These metrics cover both halves.
//
// Cardinality: result is a closed set (ok / drop / reject / fail);
// kind is bounded by the changewatcher's known kinds (ssh_login,
// sudo_use, container_start, package_install, ...). No high-cardinality
// labels (no edge_id / subject / user).
var (
	// ChangeEventsPushedTotal counts events the edge side attempted
	// to push over the tunnel. result=ok on accepted, result=drop
	// on buffer overflow, result=reject on server-side reject.
	//
	// Labels:
	//   result = ok | drop | reject
	ChangeEventsPushedTotal *prometheus.CounterVec

	// ChangeEventsInsertedTotal counts events the manager successfully
	// persisted. kind is the changewatcher kind (ssh_login, ...).
	//
	// Labels:
	//   kind = ssh_login | sudo_use | service_restart |
	//          container_start | container_stop | container_die |
	//          package_install | package_upgrade | package_remove |
	//          unknown
	ChangeEventsInsertedTotal *prometheus.CounterVec

	// ChangeEventsQueryDuration observes query_change_events wall-clock
	// latency when the edge source is involved. Edge-only because
	// audit-only is already covered by the audit module's metrics.
	//
	// Labels:
	//   source = audit | edge | merged
	ChangeEventsQueryDuration *prometheus.HistogramVec
)

// registerChangeEventMetrics creates the metric vectors. Called from
// RegisterManagerMetrics.
func registerChangeEventMetrics() {
	ChangeEventsPushedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "opskeeper_change_events_pushed_total",
			Help: "Edge-side change events pushed to manager (result=ok | drop | reject).",
		},
		[]string{"result"},
	)
	ChangeEventsInsertedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "opskeeper_change_events_inserted_total",
			Help: "Manager-side change events persisted into edge_change_events.",
		},
		[]string{"kind"},
	)
	ChangeEventsQueryDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "opskeeper_change_events_query_duration_seconds",
			Help:    "Wall-clock latency of query_change_events for the edge source path.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"source"},
	)
}
