package spool

import "time"

// Class is a policy bucket. It is a string rather than an enum because the
// three users of this package live in three bounded contexts and none of
// them should import the others' vocabulary to agree on a drop order.
type Class string

// Drop priorities. The numbers only mean "larger goes first"; the gaps are
// deliberate so a fourth class can be slotted between two of them without
// renumbering, and renumbering would change which data a deployed node
// throws away.
const (
	// PriorityCritical is for rows whose loss is a correctness problem:
	// audit records and the like. It is also the default for a class
	// nobody declared, because an unclassified row dropping by default is
	// a row nobody chose to make disposable.
	PriorityCritical = 0
	// PriorityValuable is for the data a human is most likely to ask for
	// after an incident: change events, host metric points.
	PriorityValuable = 10
	// PriorityBulk is for high-volume, lower-density data — traces above
	// all. It goes first under pressure, which is the plan's
	// "traces 优先于 metrics 丢弃" stated as a number.
	PriorityBulk = 20
	// PriorityDisposable is for data that is genuinely cheaper to
	// re-derive than to keep.
	PriorityDisposable = 30
)

// Policy is what a class is worth and how long it stays true.
//
// The three fields answer three different questions, and conflating them is
// the mistake this type exists to prevent: DropPriority is about *volume
// pressure*, MaxAge is about *staleness*, and MaxBytes is about one class
// not being allowed to starve the others even when the file as a whole is
// nowhere near its cap.
type Policy struct {
	// DropPriority is how disposable a row is. Larger goes first.
	DropPriority int
	// MaxAge is how long after writing a row is still worth sending. Zero
	// means never ages out.
	//
	// A horizon is the honest answer to "how late is too late to deliver
	// this", and the answer is not the same for every class. A metric
	// pushed an hour late is not a delayed metric, it is a wrong one: the
	// center's store will either reject it as out of order or accept it
	// and produce a graph with a spike in the wrong place. An audit row
	// has no horizon at all, because the question "is it too late to
	// record that the node did this" has no useful answer.
	MaxAge time.Duration
	// MaxBytes is a per-class ceiling inside the global one. Zero means
	// the class may use the whole file.
	//
	// Without it, a node whose tracer is misconfigured can fill a spool
	// that metrics also live in, and the metrics go not because the disk
	// is full but because something else in the disk is.
	MaxBytes int64
}

// DefaultPolicy is what a class nobody declared gets.
func DefaultPolicy() Policy { return Policy{DropPriority: PriorityCritical} }

func (s *Spool) policy(c Class) Policy {
	if s.classes == nil {
		return DefaultPolicy()
	}
	if p, ok := s.classes[c]; ok {
		return p
	}
	return DefaultPolicy()
}

// Stats are a spool's counters.
//
// Dropped and DroppedAged exist because a node that is quietly throwing
// away telemetry looks exactly like a node with no telemetry, and the
// difference is the whole reason a health page needs to show them.
type Stats struct {
	// Written is every row accepted, including rows later dropped.
	Written uint64
	// Acked is every row a reader confirmed it had.
	Acked uint64
	// Dropped is every row the policy removed.
	Dropped uint64
	// DroppedAged is the subset removed for being stale rather than for
	// volume.
	DroppedAged uint64
	// DroppedCapacity is the subset removed under the byte cap.
	DroppedCapacity uint64
	// WrittenByClass and DroppedByClass are per-class breakdowns.
	WrittenByClass map[Class]uint64
	DroppedByClass map[Class]uint64
}

func (st *Stats) drop(row Row, aged bool) {
	st.Dropped++
	if aged {
		st.DroppedAged++
	} else {
		st.DroppedCapacity++
	}
	if st.DroppedByClass == nil {
		st.DroppedByClass = map[Class]uint64{}
	}
	st.DroppedByClass[row.Class]++
}

func (st *Stats) any() bool { return st.Dropped > 0 }

func (st *Stats) merge(other Stats) {
	st.Dropped += other.Dropped
	st.DroppedAged += other.DroppedAged
	st.DroppedCapacity += other.DroppedCapacity
	for k, v := range other.DroppedByClass {
		if st.DroppedByClass == nil {
			st.DroppedByClass = map[Class]uint64{}
		}
		st.DroppedByClass[k] += v
	}
}
