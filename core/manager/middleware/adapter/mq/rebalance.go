package mq

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Rebalance history is the one thing a Kafka client cannot be asked for.
//
// DescribeGroups answers a question about NOW: who is in the group right now,
// what state it is in, which member holds which partitions. There is no admin
// API that returns what the assignment used to be, so "what happened to this
// group last Tuesday" has no endpoint — it has to be built from successive
// answers, and a tool that printed today's assignment under the name
// "rebalance history" would be answering a different question than it was
// asked. That is why the name was left unimplemented for so long, and why
// closing it meant writing a collector rather than adding a tool.
//
// The file is split so the part worth trusting is pure:
//
//   - MemberState / GroupState and their Fingerprint: what "the group looks
//     like at an instant" means, canonically. Maps are sorted before hashing
//     because two identical assignments that happen to iterate differently
//     are the same assignment, and a history that reported a rebalance every
//     sample because of map ordering would be worse than no history.
//   - diffGroups: what changed between two instants. Joined, left, reassigned
//     and the state transition — the four things a person asking "did it
//     rebalance" is actually asking.
//   - RebalanceHistory: a bounded store. Bounded on purpose: an unbounded
//     history of a busy group is a disk incident, and a rebalance history
//     nobody prunes is a rebalance history that eventually takes the control
//     plane down.
//
// What fills the store, and when, is decided by the caller rather than here,
// because only the caller knows whether a read is worth a sample. The
// contract this package relies on is narrower and it is worth stating: an
// absent sample is never invented. A query over a window OpsKeeper was not
// watching returns the window it did watch, and says so.

// MemberState is one member of a consumer group as of one sample.
type MemberState struct {
	ID       string
	ClientID string
	Host     string
	// Assignment maps a topic to the partitions this member owns.
	Assignment map[string][]int
}

// GroupState is one consumer group as of one sample.
type GroupState struct {
	Group   string
	State   string
	Members []MemberState
}

// Fingerprint is the canonical identity of an assignment.
//
// Everything is sorted before it is joined, because a fingerprint that
// depended on Go's map iteration order would make every sample look like a
// change and turn the history into noise. It is a string rather than a hash
// so that a fingerprint can be printed in an event and compared by a human
// reading a log.
func (g GroupState) Fingerprint() string {
	var b strings.Builder
	b.WriteString(g.State)
	members := append([]MemberState(nil), g.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
	for _, m := range members {
		b.WriteString("|m=" + m.ID + "," + m.ClientID + "@" + m.Host)
		for _, topic := range sortedKeys(m.Assignment) {
			parts := append([]int(nil), m.Assignment[topic]...)
			sort.Ints(parts)
			strs := make([]string, 0, len(parts))
			for _, p := range parts {
				strs = append(strs, fmt.Sprintf("%d", p))
			}
			b.WriteString(",t=" + topic + ":" + strings.Join(strs, "+"))
		}
	}
	return b.String()
}

// RebalanceEvent is one observed change in a group's membership.
type RebalanceEvent struct {
	At time.Time
	// From and To are the group states, Empty meaning "no previous sample".
	From string
	To   string
	// Joined and Left name members by ID, sorted.
	Joined []string
	Left   []string
	// Reassigned counts partitions that moved to a different member. This is
	// the number that distinguishes a rebalance caused by a member joining
	// from the ordinary churn of one joining and one leaving.
	Reassigned int
	// Fingerprints of the two states, so an event can be checked later.
	FromFP string
	ToFP   string
}

// Empty reports whether the event says nothing happened.
func (e RebalanceEvent) Empty() bool {
	return len(e.Joined) == 0 && len(e.Left) == 0 && e.Reassigned == 0 && e.From == e.To
}

func diffGroups(prev, cur GroupState) (RebalanceEvent, bool) {
	if prev.Fingerprint() == cur.Fingerprint() {
		return RebalanceEvent{}, false
	}
	before := map[string]MemberState{}
	for _, m := range prev.Members {
		before[m.ID] = m
	}
	after := map[string]MemberState{}
	for _, m := range cur.Members {
		after[m.ID] = m
	}
	event := RebalanceEvent{From: prev.State, To: cur.State, FromFP: prev.Fingerprint(), ToFP: cur.Fingerprint()}
	for id := range after {
		if _, ok := before[id]; !ok {
			event.Joined = append(event.Joined, id)
		}
	}
	for id := range before {
		if _, ok := after[id]; !ok {
			event.Left = append(event.Left, id)
		}
	}
	sort.Strings(event.Joined)
	sort.Strings(event.Left)

	// A partition counts as reassigned when the set of members holding it
	// changes. Counting it once per (member, partition) pair would count a
	// partition moving from one member to another as two, and the number is
	// meant to be comparable across events.
	holders := map[string]map[string]bool{}
	for _, m := range prev.Members {
		for topic, parts := range m.Assignment {
			for _, p := range parts {
				key := fmt.Sprintf("%s/%d", topic, p)
				if holders[key] == nil {
					holders[key] = map[string]bool{}
				}
				holders[key][m.ID] = true
			}
		}
	}
	next := map[string]map[string]bool{}
	for _, m := range cur.Members {
		for topic, parts := range m.Assignment {
			for _, p := range parts {
				key := fmt.Sprintf("%s/%d", topic, p)
				if next[key] == nil {
					next[key] = map[string]bool{}
				}
				next[key][m.ID] = true
			}
		}
	}
	for key, prevHolders := range holders {
		if !sameSet(prevHolders, next[key]) {
			event.Reassigned++
		}
	}
	for key, nextHolders := range next {
		if _, existed := holders[key]; !existed && len(nextHolders) > 1 {
			// A partition that did not exist before and is now shared was
			// split by this rebalance rather than moved, and it is worth the
			// same number as a move: the cost was paid either way.
			event.Reassigned++
		}
	}
	return event, true
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// RebalanceHistory is a bounded record of observed changes.
//
// In memory by default; when a store is attached (NewRebalanceHistoryStore)
// every change is also written through, and a group nobody has seen since the
// last restart is loaded from disk the first time it is asked about. The
// trimming, diffing and querying are the same code either way — persistence
// is the only difference — so the two configurations cannot drift apart in
// what they report.
type RebalanceHistory struct {
	mu sync.Mutex
	// latest is the last state seen per group, so a sample that changed
	// nothing costs one comparison rather than an event row.
	latest map[string]GroupState
	events map[string][]RebalanceEvent
	// loadErr holds a per-group read failure from the store, so the refusal
	// a caller gets can name the cause instead of only the absence.
	loadErr map[string]error
	// maxEvents and maxAge bound each group independently. A group with a
	// rebalance every second and a group with one a week are both served by
	// the same limits without either crowding the other out.
	maxEvents int
	maxAge    time.Duration
	// store is nil for the in-memory history. Write failures do not fail a
	// Record: the sample is still true in memory, and a history that
	// refuses to observe because a disk is full would turn a storage problem
	// into a diagnostics outage. The failure is surfaced by the next load.
	store rebalanceStore
}

// NewRebalanceHistory returns a history keeping maxEvents per group and
// dropping events older than maxAge.
func NewRebalanceHistory(maxEvents int, maxAge time.Duration) *RebalanceHistory {
	if maxEvents <= 0 {
		maxEvents = 64
	}
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	return &RebalanceHistory{
		latest:    map[string]GroupState{},
		events:    map[string][]RebalanceEvent{},
		loadErr:   map[string]error{},
		maxEvents: maxEvents,
		maxAge:    maxAge,
	}
}

// Record takes one sample and returns the event it produced, if any.
//
// A first sample is recorded as an event with From empty: the group's initial
// membership is a change from "nothing was known" to "these are the members",
// and a history that started at the second sample would hide the fact that
// OpsKeeper only began watching when it did.
func (h *RebalanceHistory) Record(cur GroupState, at time.Time) (RebalanceEvent, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev, seen := h.latest[cur.Group]
	if !seen && h.store != nil {
		// Load before diffing, not only before answering. Without this a
		// restarted process diffs the first sample of its new life against
		// nothing and reports the group's entire membership as having
		// joined — a rebalance that never happened, dated to the restart.
		loaded, latest, err := h.store.load(cur.Group)
		if err != nil {
			h.loadErr[cur.Group] = err
		} else if len(loaded) > 0 {
			h.events[cur.Group] = loaded
			h.latest[cur.Group] = latest
			prev, seen = latest, true
		}
	}
	if seen {
		event, changed := diffGroups(prev, cur)
		if !changed {
			return RebalanceEvent{}, false
		}
		event.At = at
		h.events[cur.Group] = append(h.events[cur.Group], event)
		h.trimLocked(cur.Group, at)
		h.latest[cur.Group] = cur
		h.persistLocked(cur.Group)
		return event, true
	}
	event := RebalanceEvent{At: at, To: cur.State, Joined: memberIDs(cur), ToFP: cur.Fingerprint()}
	h.events[cur.Group] = append(h.events[cur.Group], event)
	h.trimLocked(cur.Group, at)
	h.latest[cur.Group] = cur
	h.persistLocked(cur.Group)
	return event, true
}

// persistLocked writes the group through to the store, if there is one.
func (h *RebalanceHistory) persistLocked(group string) {
	if h.store == nil {
		return
	}
	delete(h.loadErr, group)
	_ = h.store.save(group, h.events[group], h.latest[group])
}

// LoadError returns why a persisted window could not be read, if it could
// not. The query path refuses in that case; this is for the caller that wants
// the sentence rather than the refusal.
func (h *RebalanceHistory) LoadError(group string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.loadErr[group]
}

func (h *RebalanceHistory) trimLocked(group string, now time.Time) {
	rows := h.events[group]
	// Rows are oldest first, so the ones worth keeping are the tail. The
	// first version of this counted the fresh prefix and then sliced rows
	// PAST it, which threw away exactly the events that were in date and
	// left the store empty after one sample — a history that silently
	// forgets everything the moment it writes something.
	fresh := 0
	for fresh < len(rows) && now.Sub(rows[len(rows)-1-fresh].At) <= h.maxAge {
		fresh++
	}
	rows = rows[len(rows)-fresh:]
	if len(rows) > h.maxEvents {
		rows = rows[len(rows)-h.maxEvents:]
	}
	h.events[group] = rows
}

// Query returns the events for one group, newest first, within the window.
//
// limit <= 0 falls back to the store's own bound. A group that was never
// sampled returns nothing and ok=false: an empty answer and "we have been
// watching and nothing moved" are different sentences, and the caller has to
// be able to tell them apart.
func (h *RebalanceHistory) Query(group string, since time.Duration, limit int) (events []RebalanceEvent, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rows, seen := h.events[group]
	if !seen && h.store != nil {
		loaded, latest, err := h.store.load(group)
		if err != nil {
			// An unreadable window is reported as unreadable, which the
			// caller turns into a refusal. Reporting it as "no history"
			// would be the one answer that is never right.
			h.loadErr[group] = err
			return nil, false
		}
		if len(loaded) > 0 {
			h.events[group] = loaded
			h.latest[group] = latest
			rows, seen = loaded, true
		}
	}
	if !seen {
		return nil, false
	}
	if limit <= 0 || limit > len(rows) {
		limit = len(rows)
	}
	// The window is measured back from now rather than from the newest
	// event. Measuring from the newest event makes every non-zero window
	// include that event by construction, which leaves "we were watching
	// and nothing moved in the window you asked about" unreachable — and an
	// unreachable sentence is a sentence nobody reads.
	cutoff := time.Time{}
	if since > 0 {
		cutoff = time.Now().Add(-since)
	}
	out := make([]RebalanceEvent, 0, limit)
	for i := len(rows) - 1; i >= 0 && len(out) < limit; i-- {
		if cutoff.IsZero() || rows[i].At.After(cutoff) {
			out = append(out, rows[i])
		}
	}
	return out, true
}

// Groups lists the groups this history has observed.
func (h *RebalanceHistory) Groups() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return sortedKeys(h.latest)
}

func memberIDs(g GroupState) []string {
	ids := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids
}

// rebalanceRows renders events as tool rows.
//
// The rendering states the window it covers in the summary. A history that
// began this morning must not be readable as a history of the incident that
// started last night, and the summary line is where that is prevented.
func (h *RebalanceHistory) rebalanceRows(group string, since time.Duration, limit int) ([]map[string]any, string, error) {
	events, seen := h.Query(group, since, limit)
	if !seen {
		return nil, "", fmt.Errorf("mq: consumer group %s has never been sampled on this manager, so there is no rebalance history to report; "+
			"the history starts when OpsKeeper first observes the group and covers only that window", group)
	}
	if len(events) == 0 {
		return nil, "", fmt.Errorf("mq: consumer group %s was observed and did not rebalance within the requested window", group)
	}
	rows := make([]map[string]any, 0, len(events))
	for _, e := range events {
		rows = append(rows, map[string]any{
			"at":         e.At.UTC().Format(time.RFC3339),
			"state":      fmt.Sprintf("%s->%s", orNone(e.From), orNone(e.To)),
			"joined":     joinOrNone(e.Joined),
			"left":       joinOrNone(e.Left),
			"reassigned": e.Reassigned,
		})
	}
	first, last := events[len(events)-1].At, events[0].At
	summary := fmt.Sprintf("%d rebalance event(s) for group %s between %s and %s",
		len(events), group, first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339))
	return rows, summary, nil
}

func orNone(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

func joinOrNone(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return strings.Join(ids, ",")
}
