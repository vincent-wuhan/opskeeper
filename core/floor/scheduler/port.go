// Package scheduler holds the missed-run detection contract at the seam
// between the scheduler that *asks* whether a schedule was missed and the
// domain that *owns the schedule table* and can therefore answer.
//
// It used to live in core/domains/biz/scheduler as `Repo` + `MissedRunInfo`
// (decision 230). That put the port in the same package as its consumer, so
// the only party able to implement it — core/domains/data/flow/store, which
// reads the `flow_schedule_next_fire` table — had to import the consumer to
// name the interface. The result was a declared cross-domain edge
// `flow -> scheduler` whose entire content was one interface satisfied by one
// method pair: a use of a data layer reaching up into a biz package to say
// "I implement your port", which is the inversion the port pattern exists to
// prevent.
//
// The interface and the DTO it traffics are moved here, next to nothing in
// particular, because the honest description of this pair is: a detector that
// needs to be told what was missed, and a table that knows. Neither owns the
// other. `core/floor` is the one tree both the detector (core/domains/biz/
// scheduler) and the store (core/domains/data/flow/store) are already allowed
// to import, so with the port here neither has to import the other and the
// edge disappears rather than moves.
//
// This is the same move as decision 227's `HostMetricIngest` (port next to
// `HostMetricPoint` in core/floor/tunnel): the finding there was that a port
// named in one domain but implemented in another is a naming bug, and the
// cure is to name it where both halves can see it.
package scheduler

import (
	"context"
	"time"
)

// MissedRunInfo is one schedule that should have fired and did not. It is
// deliberately a flat DTO rather than a GORM entity: the detector reads it,
// the alert sink re-emits it, and neither should be able to reach through it
// into the store's tables. The store is the only party that knows about rows.
type MissedRunInfo struct {
	FlowID            string
	NodeID            string
	CronSpec          string
	ExpectedFireAt    time.Time
	MissedDurationSec int64
}

// Repo is the question "what was missed, and record that we noticed" asked
// against whoever owns the schedule table.
//
// The two methods are the whole contract, and the pairing is deliberate:
// ListMissed is the read that drives detection, RecordMissedAudit is the
// write that makes the detection idempotent across restarts. A store that
// implements one without the other cannot dedupe, and a detector that only
// reads cannot dedupe at all — which is why this is one interface and not
// two.
type Repo interface {
	ListMissed(ctx context.Context, before time.Time) ([]MissedRunInfo, error)
	RecordMissedAudit(ctx context.Context, missed MissedRunInfo) error
}
