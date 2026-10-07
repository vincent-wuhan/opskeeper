package edge

import (
	"context"
	"errors"
	"testing"
	"time"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
)

// The PiG version contract, as the control plane holds it.
//
// This is the write half of the compatibility matrix's second axis. The
// decision side — whether a node is new enough for a package — belongs to
// pluginmanifest.CheckVersions and runs on the node, and the manager's
// pre-flight is a projection of it rather than a second opinion. That only
// works if the two are looking at the same number, which puts the burden
// entirely on this side: store exactly what the node reported, and never
// substitute a value the node did not send.
//
// The three tests below are the three ways that can go wrong, and each one
// fails for a different reason, which is why they are three tests and not
// one with a table.

// A reported version is stored. The obvious case, kept because the
// non-obvious two below are only meaningful against a working baseline: if
// this one regressed, the other two would still pass by refusing to write
// anything at all.
func TestAHeartbeatRecordsThePigVersionTheNodeReports(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUsecase(repo, nil, nil, nil)
	ctx := context.Background()

	res, err := uc.Create(ctx, "edge-pig-report", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := uc.HandleHeartbeat(ctx, res.Edge.ID, time.Now().UTC(), "0.3.0"); err != nil {
		t.Fatalf("HandleHeartbeat: %v", err)
	}
	after, err := uc.Get(ctx, res.Edge.ID)
	if err != nil {
		t.Fatalf("Get after heartbeat: %v", err)
	}
	if after.PigVersion != "0.3.0" {
		t.Errorf("PigVersion = %q, want %q", after.PigVersion, "0.3.0")
	}
}

// An empty report must not blank a known version.
//
// This is the direction that matters. A node that stops reporting — an
// edge predating the field, a build that declines to, a truncated payload
// on one unlucky beat — is saying nothing, and the correct reading of
// nothing is "keep what you knew". The opposite, writing the empty string
// through, would silently retire the version of every node in the fleet at
// the same moment, and every package that declares min_pig_version would
// then be refused everywhere for a reason that looks like a fleet-wide
// incompatibility rather than the missing column it is.
func TestANodeThatStopsReportingDoesNotLoseTheVersionItLastReported(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUsecase(repo, nil, nil, nil)
	ctx := context.Background()

	res, err := uc.Create(ctx, "edge-pig-silence", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := uc.HandleHeartbeat(ctx, res.Edge.ID, time.Now().UTC(), "0.3.0"); err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}

	// Three shapes of "nothing", because a node can go quiet in more than
	// one way and only the empty-after-trim one is obviously handled.
	for _, reported := range []string{"", "   ", "\t\n"} {
		if err := uc.HandleHeartbeat(ctx, res.Edge.ID, time.Now().UTC(), reported); err != nil {
			t.Fatalf("heartbeat reporting %q: %v", reported, err)
		}
		after, err := uc.Get(ctx, res.Edge.ID)
		if err != nil {
			t.Fatalf("Get after %q: %v", reported, err)
		}
		if after.PigVersion != "0.3.0" {
			t.Errorf("after a heartbeat reporting %q, PigVersion = %q, want it left at %q",
				reported, after.PigVersion, "0.3.0")
		}
	}
}

// An unchanged version must cost no write.
//
// The heartbeat is periodic — every 30 seconds per node, fleet-wide, for as
// long as the fleet is up — and the version is a build constant that only
// moves on upgrade. Writing it unconditionally therefore means issuing a
// row update that sets a column to the value it already holds, tens of
// thousands of times a day across a deployment, against the table that also
// carries every liveness timestamp. The read-before-write this asserts is
// what makes the column passive, and the alternative — trusting the ORM or
// the storage layer to elide a no-op update — is not a thing either of them
// promises.
func TestAnUnchangedPigVersionDoesNotWriteOnEveryHeartbeat(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUsecase(repo, nil, nil, nil)
	ctx := context.Background()

	res, err := uc.Create(ctx, "edge-pig-idle", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := uc.HandleHeartbeat(ctx, res.Edge.ID, time.Now().UTC(), "0.3.0"); err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}
	writesAfterFirst := repo.pigVersionWrites
	if writesAfterFirst != 1 {
		t.Fatalf("first heartbeat wrote the version %d times, want exactly 1", writesAfterFirst)
	}

	// Far more beats than any real deployment would send in a test run,
	// because the property is "every one of them", and one would not
	// distinguish a guard from a coincidence.
	for i := 0; i < 50; i++ {
		if err := uc.HandleHeartbeat(ctx, res.Edge.ID, time.Now().UTC(), "0.3.0"); err != nil {
			t.Fatalf("steady-state heartbeat %d: %v", i, err)
		}
	}
	if got := repo.pigVersionWrites; got != writesAfterFirst {
		t.Errorf("50 heartbeats reporting an unchanged version produced %d writes, want 0",
			got-writesAfterFirst)
	}

	// The upgrade is the case the write exists for, and it must still land
	// after all those no-op beats — a guard that also swallowed real
	// changes would be worse than no guard.
	if err := uc.HandleHeartbeat(ctx, res.Edge.ID, time.Now().UTC(), "0.4.0"); err != nil {
		t.Fatalf("upgrade heartbeat: %v", err)
	}
	after, err := uc.Get(ctx, res.Edge.ID)
	if err != nil {
		t.Fatalf("Get after upgrade: %v", err)
	}
	if after.PigVersion != "0.4.0" {
		t.Errorf("PigVersion after an upgrade heartbeat = %q, want %q", after.PigVersion, "0.4.0")
	}
}

// A failed version write must not fail the heartbeat.
//
// The heartbeat's contract with the node is liveness, and the node's
// response to an error is to count a consecutive failure and eventually
// exit for a clean respawn. Trading a process restart for one version
// column is the wrong trade in a direction that is not obvious until you
// have watched a fleet do it: the liveness signal is load-bearing for every
// other feature on the node, and this one field is a convenience for the
// release page.
func TestAFailedPigVersionWriteDoesNotCostTheNodeItsHeartbeat(t *testing.T) {
	repo := newFakeRepo()
	uc := NewUsecase(repo, nil, nil, nil)
	ctx := context.Background()

	res, err := uc.Create(ctx, "edge-pig-write-fail", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The read is what fails here, which is the realistic case: the row
	// that UpdateStatus just touched is suddenly unreadable, or the store
	// is in a state where a read is refused. Either way the usecase has no
	// version to compare and must treat that as best-effort, not fatal.
	failing := &failingPigRepo{fakeRepo: repo}
	uc2 := NewUsecase(failing, nil, nil, nil)

	if err := uc2.HandleHeartbeat(ctx, res.Edge.ID, time.Now().UTC(), "0.3.0"); err != nil {
		t.Fatalf("HandleHeartbeat returned %v for a best-effort version write; "+
			"a node that cannot record a version must still be told it is alive", err)
	}
	// Liveness is the part that must have happened regardless.
	after, err := uc.Get(ctx, res.Edge.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Status != model.StatusOnline {
		t.Errorf("Status = %q, want online", after.Status)
	}
	if after.PigVersion != "" {
		t.Errorf("PigVersion = %q, want it left unset by a failed write", after.PigVersion)
	}
}

// failingPigRepo refuses the read the version guard depends on, and counts
// nothing else differently — so the heartbeat's real work still runs.
type failingPigRepo struct {
	*fakeRepo
}

func (r *failingPigRepo) GetByID(_ context.Context, id uint64) (*model.Edge, error) {
	if _, ok := r.byID[id]; !ok {
		return nil, errs.ErrNotFound
	}
	return nil, errors.New("store unavailable")
}
