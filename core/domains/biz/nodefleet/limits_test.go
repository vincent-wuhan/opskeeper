package nodefleet

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Conversation limits are the difference between a fleet that is large and a
// manager that has a bad afternoon. Everything here is about the two ways a
// cap can be written so that it does not actually cap anything.

func openOn(t *testing.T, f *Fleet, edge uint64, session string) error {
	t.Helper()
	_, err := f.Open(PromptRequest{EdgeID: edge, SessionID: session}, &recordingSink{})
	return err
}

func TestThePerNodeCapRefusesTheOneAfterIt(t *testing.T) {
	f, err := New(Options{Dial: &fakeDial{reply: acceptAll()}, MaxSessionsPerEdge: 3, MaxSessions: 100})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(f.CloseAll)

	for i := 0; i < 3; i++ {
		if err := openOn(t, f, 7, fmt.Sprintf("s-%d", i)); err != nil {
			t.Fatalf("conversation %d within the cap: %v", i, err)
		}
	}
	err = openOn(t, f, 7, "s-over")
	if err == nil {
		t.Fatal("the fourth conversation on one node was accepted")
	}
	if !errors.Is(err, ErrFleetFull) {
		t.Errorf("err = %v, want it to unwrap to ErrFleetFull so a caller can branch on it", err)
	}
	var limit *LimitError
	if !errors.As(err, &limit) {
		t.Fatalf("err = %v, want a *LimitError carrying the numbers the operator needs", err)
	}
	if limit.Scope != "edge" || limit.EdgeID != 7 || limit.Open != 3 || limit.Limit != 3 {
		t.Errorf("LimitError = %+v, want edge 7 at 3 of 3", limit)
	}
	if !strings.Contains(limit.Error(), "edge 7") {
		t.Errorf("message %q does not name the node, so the operator cannot tell which one is full", limit.Error())
	}
	if got := f.SessionCount(); got != 3 {
		t.Errorf("SessionCount = %d, want 3: a refused conversation must leave nothing behind", got)
	}
}

func TestThePerNodeCapIsPerNode(t *testing.T) {
	// A cap that leaked across nodes would make one busy node's operators
	// unable to start work on a quiet one, which is the opposite of what a
	// per-node cap is for.
	f, err := New(Options{Dial: &fakeDial{reply: acceptAll()}, MaxSessionsPerEdge: 2, MaxSessions: 100})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(f.CloseAll)

	for edge := uint64(1); edge <= 3; edge++ {
		for i := 0; i < 2; i++ {
			if err := openOn(t, f, edge, fmt.Sprintf("s-%d", i)); err != nil {
				t.Fatalf("edge %d conversation %d: %v", edge, i, err)
			}
		}
	}
	if err := openOn(t, f, 3, "s-over"); err == nil {
		t.Error("the cap did not apply on the third node")
	}
	if err := openOn(t, f, 4, "s-0"); err != nil {
		t.Errorf("a fourth node was refused by another node's cap: %v", err)
	}
}

func TestTheFleetCapAppliesAcrossNodes(t *testing.T) {
	f, err := New(Options{Dial: &fakeDial{reply: acceptAll()}, MaxSessionsPerEdge: 10, MaxSessions: 4})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(f.CloseAll)

	for i := 0; i < 4; i++ {
		if err := openOn(t, f, uint64(i+1), "s-0"); err != nil {
			t.Fatalf("conversation %d within the fleet cap: %v", i, err)
		}
	}
	// One per node, so the per-edge cap of 10 is nowhere near reached: only
	// the fleet cap can be what refuses this.
	err = openOn(t, f, 5, "s-0")
	if err == nil {
		t.Fatal("the fleet accepted a fifth conversation under a cap of 4")
	}
	var limit *LimitError
	if !errors.As(err, &limit) || limit.Scope != "fleet" {
		t.Fatalf("err = %v, want a fleet-scoped LimitError; a per-edge cap of 10 was not reached", err)
	}
	if limit.Open != 4 || limit.Limit != 4 {
		t.Errorf("LimitError = %+v, want 4 of 4", limit)
	}
}

// What this test does and does not prove, because the difference is the
// whole point of writing it down.
//
// It proves the cap holds while eight consoles open conversations at once.
// It does NOT prove that the check and the insert share a critical section,
// and no amount of repetition here would. Moving the check out of the lock —
// reading the count under RLock, releasing, then inserting — was tried as a
// mutation and the test stayed green through five runs at 8 goroutines and
// five more at 64 x 100 attempts. -race does not catch it either, and cannot:
// the mutated read is properly locked, so this is a check-then-act race
// rather than a data race, and the race detector is not in the business of
// reasoning about two separately-correct critical sections.
//
// So the atomicity is a property of where the code sits, and it is reviewed
// the way the rest of the locking discipline is. The comment at the check in
// Open says so. What this test buys is the weaker but still useful claim that
// a burst of opens does not blow past the cap in practice, and — because each
// goroutine keeps opening until it is refused — that the cap actually stops
// them rather than merely being slow to notice.
func TestTheCapHoldsWhenManyConsolesOpenAtOnce(t *testing.T) {
	const cap = 8
	const goroutines = 8
	const maxEach = 200
	f, err := New(Options{Dial: &fakeDial{reply: acceptAll()}, MaxSessionsPerEdge: cap, MaxSessions: cap})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(f.CloseAll)

	var opened atomic.Int64
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for g := 0; g < goroutines; g++ {
		done.Add(1)
		go func(g int) {
			defer done.Done()
			start.Wait()
			for i := 0; i < maxEach; i++ {
				// A distinct id per attempt: the clash check would otherwise
				// refuse the second try and the goroutine would exit having
				// proved nothing.
				if err := openOn(t, f, 7, fmt.Sprintf("g%d-s%d", g, i)); err != nil {
					return
				}
				opened.Add(1)
			}
		}(g)
	}
	start.Done()
	done.Wait()

	if got := opened.Load(); got != cap {
		t.Errorf("%d conversations were opened under a cap of %d (%d goroutines x up to %d attempts)",
			got, cap, goroutines, maxEach)
	}
	if got := f.SessionCount(); got != cap {
		t.Errorf("SessionCount = %d, want %d", got, cap)
	}
}

// A refused Open has to leave nothing behind — no entry, no frame relay
// wired to a handle nobody will ever drive. The count is the part that is
// easy to assert; the leak is the part that would not show up in SessionCount
// at all if the relay were attached before the refusal.
func TestARefusedOpenLeavesNoSessionBehind(t *testing.T) {
	f, err := New(Options{Dial: &fakeDial{reply: acceptAll()}, MaxSessionsPerEdge: 1, MaxSessions: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(f.CloseAll)

	if err := openOn(t, f, 7, "s-1"); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	for i := 0; i < 5; i++ {
		_ = openOn(t, f, 7, fmt.Sprintf("s-refused-%d", i))
	}
	if got := f.SessionCount(); got != 1 {
		t.Errorf("SessionCount = %d after five refusals, want 1", got)
	}
	// The survivor must still work: a refusal that corrupted the map would
	// leave the count right and the session unusable.
	if _, ok := f.Stats(7, "s-1"); !ok {
		t.Error("the conversation that was already open is no longer reachable after five refusals")
	}
}

// A cap that never reopens is not a cap, it is a shutdown. Closing a
// conversation has to return its slot, or an operator who works through a
// shift eventually cannot start anything at all.
func TestClosingAConversationMakesRoomAgain(t *testing.T) {
	f, err := New(Options{Dial: &fakeDial{reply: acceptAll()}, MaxSessionsPerEdge: 1, MaxSessions: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(f.CloseAll)

	if err := openOn(t, f, 7, "s-1"); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := openOn(t, f, 7, "s-2"); err == nil {
		t.Fatal("the second conversation was accepted under a cap of 1")
	}
	f.Close(7, "s-1")
	if err := openOn(t, f, 7, "s-2"); err != nil {
		t.Errorf("a closed conversation did not release its slot: %v", err)
	}
}

func TestZeroCapsSelectTheDefaults(t *testing.T) {
	f, err := New(Options{Dial: &fakeDial{reply: acceptAll()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(f.CloseAll)

	perEdge, total := f.Limits()
	if perEdge != DefaultMaxSessionsPerEdge || total != DefaultMaxSessionsTotal {
		t.Errorf("Limits = (%d, %d), want the defaults (%d, %d)",
			perEdge, total, DefaultMaxSessionsPerEdge, DefaultMaxSessionsTotal)
	}
}

func TestANegativeCapIsRefused(t *testing.T) {
	// -1 is how "unlimited" gets typed into a config file by someone who
	// assumed it was supported. Failing here says so once, at startup,
	// instead of at the first conversation on the busiest node.
	if _, err := New(Options{Dial: &fakeDial{reply: acceptAll()}, MaxSessionsPerEdge: -1}); err == nil {
		t.Error("a negative per-node cap was accepted")
	}
	if _, err := New(Options{Dial: &fakeDial{reply: acceptAll()}, MaxSessions: -1}); err == nil {
		t.Error("a negative fleet cap was accepted")
	}
}
