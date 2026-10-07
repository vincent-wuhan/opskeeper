package domain

import "time"

// ChangeEvent is the part of an edge-observed change that a consumer outside
// the edge domain is allowed to see.
//
// It is seven fields out of the stored row's eleven, and the four it leaves
// behind are the ones that only the write path has a use for: the row's own
// primary key, the write-ahead log sequence number, the source column (which
// is always "edge" for this projection because the source that produced a row
// of this shape is decided by who answers the question, not by the row), and
// the creation stamp.
//
// The alternative was handing the RCA tool the GORM entity, which is what
// decision 283 removed: query_change_events had been the last place in the
// aiops domain that named a table row type from another domain's model
// package, and it did so through a port declaration that already looked like
// a boundary but was not one — an interface whose return type reached past
// the port into the store's entity is a port with the door open.
//
// Every field here is read by the tool and written by the edge domain's
// query, so none of them is speculative. Labels stays the raw JSON string the
// edge sent rather than a decoded map, because the tool passes it straight
// through to the model and decoding it here would be a decision about a
// consumer that belongs to the consumer.
type ChangeEvent struct {
	// EdgeID is the node the change was observed on.
	EdgeID uint64
	// Kind is the change category, e.g. package_install or service_restart.
	Kind string
	// Subject is what the change was about, e.g. a unit name.
	Subject string
	// Action is what happened to it, e.g. started or modified.
	Action string
	// Timestamp is when the edge observed it, in the edge's own clock. It is
	// not corrected for skew: the window filter is applied by the same clock
	// that the edge stamps, and a node with a skewed clock is a node whose
	// changes land outside the window the investigator asked about, which is
	// a fact worth surfacing rather than silently repairing.
	Timestamp time.Time
	// Severity is the edge's own grading of the change.
	Severity string
	// Labels is the JSON-encoded map the edge attached, carried verbatim.
	Labels string
}
