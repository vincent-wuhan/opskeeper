package mq

import (
	"strings"
	"testing"
	"time"
)

// The history is only worth reading if it is quiet when nothing happens.
// Every test here is about that: a sampler running every fifteen seconds on a
// healthy group must produce one event, not thousands.

func state(members ...MemberState) GroupState {
	return GroupState{Group: "orders", State: "Stable", Members: members}
}

func member(id string, topic string, parts ...int) MemberState {
	return MemberState{ID: id, ClientID: id + "-client", Host: "/10.0.0.1", Assignment: map[string][]int{topic: parts}}
}

// A fingerprint that depended on map iteration order would report a rebalance
// on every sample, so this pins the property the whole feature rests on.
func TestFingerprintIgnoresOrdering(t *testing.T) {
	// Partitions listed in a different order inside one member must not
	// count: the broker does not promise an order, so an ordering-sensitive
	// fingerprint would report a rebalance on every sample.
	shuffled := state(member("m1", "orders", 2, 0, 1), member("m2", "orders", 3))
	ordered := state(member("m1", "orders", 0, 1, 2), member("m2", "orders", 3))
	// Member order must not count either.
	reordered := state(member("m2", "orders", 3), member("m1", "orders", 1, 2, 0))
	if shuffled.Fingerprint() != ordered.Fingerprint() || ordered.Fingerprint() != reordered.Fingerprint() {
		t.Errorf("ordering changed the fingerprint: %q vs %q vs %q",
			shuffled.Fingerprint(), ordered.Fingerprint(), reordered.Fingerprint())
	}
}

func TestFingerprintNoticesARealChange(t *testing.T) {
	before := state(member("m1", "orders", 0, 1))
	after := state(member("m1", "orders", 0, 2))
	if before.Fingerprint() == after.Fingerprint() {
		t.Error("swapping a partition did not change the fingerprint; the feature would report no history at all")
	}
}

func TestDiffNamesJoinedLeftAndReassigned(t *testing.T) {
	prev := state(member("m1", "orders", 0, 1), member("m2", "orders", 2, 3))
	cur := state(member("m1", "orders", 0), member("m3", "orders", 1, 2, 3))

	event, changed := diffGroups(prev, cur)
	if !changed {
		t.Fatal("a member swap was reported as no change")
	}
	if strings.Join(event.Joined, ",") != "m3" {
		t.Errorf("joined = %v, want [m3]", event.Joined)
	}
	if strings.Join(event.Left, ",") != "m2" {
		t.Errorf("left = %v, want [m2]", event.Left)
	}
	// Partition 1 moved m2 -> m3, 2 moved m2 -> m3, 3 moved m2 -> m3.
	if event.Reassigned != 3 {
		t.Errorf("reassigned = %d, want 3 — one per partition that changed hands", event.Reassigned)
	}
}

func TestDiffIsQuietWhenNothingChanged(t *testing.T) {
	prev := state(member("m1", "orders", 0, 1))
	cur := state(member("m1", "orders", 1, 0))
	if _, changed := diffGroups(prev, cur); changed {
		t.Error("an identical assignment was reported as a rebalance")
	}
}

func TestAStateOnlyChangeIsAnEvent(t *testing.T) {
	// PreparingRebalance -> Stable is the transition an operator looks for,
	// and it changes no assignment at all.
	prev := GroupState{Group: "orders", State: "PreparingRebalance", Members: []MemberState{member("m1", "orders", 0)}}
	cur := GroupState{Group: "orders", State: "Stable", Members: []MemberState{member("m1", "orders", 0)}}
	event, changed := diffGroups(prev, cur)
	if !changed {
		t.Fatal("a state transition was reported as no change")
	}
	if event.Empty() {
		t.Error("the event reports nothing at all: no membership change, no reassignment, and the only real news was the state")
	}
	if event.From != "PreparingRebalance" || event.To != "Stable" {
		t.Errorf("transition = %s->%s", event.From, event.To)
	}
}

func TestRecordKeepsOneEventForAnUnchangedGroup(t *testing.T) {
	h := NewRebalanceHistory(64, time.Hour)
	now := time.Unix(1_700_000_000, 0)
	first := state(member("m1", "orders", 0, 1))
	if _, ok := h.Record(first, now); !ok {
		t.Fatal("the first sample produced no event; a history that started at the second sample would hide when watching began")
	}
	for i := 1; i < 10; i++ {
		if _, ok := h.Record(first, now.Add(time.Duration(i)*time.Minute)); ok {
			t.Fatalf("sample %d recorded an event for an unchanged group", i)
		}
	}
	events, _ := h.Query("orders", 0, 0)
	if len(events) != 1 {
		t.Errorf("%d events for an unchanged group, want 1", len(events))
	}
}

func TestRecordKeepsTheFirstEventUnboundedByReassignmentNoise(t *testing.T) {
	h := NewRebalanceHistory(2, time.Hour)
	now := time.Unix(1_700_000_000, 0)
	h.Record(state(member("m1", "orders", 0)), now)
	h.Record(state(member("m1", "orders", 0), member("m2", "orders", 1)), now.Add(time.Minute))
	h.Record(state(member("m1", "orders", 0), member("m2", "orders", 1), member("m3", "orders", 2)), now.Add(2*time.Minute))
	events, _ := h.Query("orders", 0, 0)
	if len(events) != 2 {
		t.Fatalf("%d events, want the store's own bound of 2", len(events))
	}
	// Newest first: the third sample must be at the head.
	if strings.Join(events[0].Joined, ",") != "m3" {
		t.Errorf("newest event joined %v, want [m3]", events[0].Joined)
	}
}

func TestRecordDropsEventsOlderThanTheRetention(t *testing.T) {
	h := NewRebalanceHistory(64, 10*time.Minute)
	now := time.Unix(1_700_000_000, 0)
	h.Record(state(member("m1", "orders", 0)), now)
	h.Record(state(member("m1", "orders", 0), member("m2", "orders", 1)), now.Add(time.Hour))
	events, _ := h.Query("orders", 0, 0)
	if len(events) != 1 {
		t.Errorf("%d events, want the stale one dropped", len(events))
	}
}

// A group nobody has sampled and a group nobody has seen move are different
// sentences, and the caller cannot tell them apart unless this says so.
func TestQueryDistinguishesNeverSampledFromNeverRebalanced(t *testing.T) {
	h := NewRebalanceHistory(64, time.Hour)
	if _, ok := h.Query("orders", 0, 0); ok {
		t.Error("an unsampled group reported as observed")
	}
	h.Record(state(member("m1", "orders", 0)), time.Unix(1_700_000_000, 0))
	if _, ok := h.Query("orders", 0, 0); !ok {
		t.Error("a sampled group reported as never observed")
	}
}

func TestRowsRefuseRatherThanInvent(t *testing.T) {
	h := NewRebalanceHistory(64, time.Hour)
	_, _, err := h.rebalanceRows("orders", 0, 50)
	if err == nil {
		t.Fatal("an unsampled group produced rows")
	}
	if !strings.Contains(err.Error(), "never been sampled") {
		t.Errorf("err = %v, want it to say the group was never sampled", err)
	}

	// A group's first sample IS an event — watching began, and these are the
	// members — so this answers rather than refusing.
	h.Record(state(member("m1", "orders", 0)), time.Unix(1_700_000_000, 0))
	rows, summary, err := h.rebalanceRows("orders", 0, 50)
	if err != nil {
		t.Fatalf("an observed group produced no rows: %v", err)
	}
	if len(rows) != 1 || !strings.Contains(summary, "rebalance event") {
		t.Errorf("rows = %v, summary = %q, want the initial membership reported as one event", rows, summary)
	}

	// Asking about a window the group did not move in is the third sentence:
	// we were watching, and nothing moved in the window you asked about.
	// The sample above is an hour old by the time this runs, so a two-minute
	// window excludes it — and this is the only path to that sentence, which
	// is why the window is measured back from now.
	_, _, err = h.rebalanceRows("orders", 2*time.Minute, 50)
	if err == nil || !strings.Contains(err.Error(), "did not rebalance") {
		t.Errorf("err = %v, want the third sentence for an observed group that did not move in the window", err)
	}
}

// The summary is where a window that started this morning is kept from
// reading like a window that covers last night's incident.
func TestSummaryStatesTheWindowItCovers(t *testing.T) {
	h := NewRebalanceHistory(64, time.Hour)
	first := time.Unix(1_700_000_000, 0).UTC()
	h.Record(state(member("m1", "orders", 0)), first)
	h.Record(state(member("m1", "orders", 0), member("m2", "orders", 1)), first.Add(5*time.Minute))
	rows, summary, err := h.rebalanceRows("orders", 0, 50)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	if !strings.Contains(summary, first.Format(time.RFC3339)) {
		t.Errorf("summary %q does not state the start of the window it covers", summary)
	}
}
