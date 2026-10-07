package edge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
)

// presenceRepo records the filter it was handed. The shared fakeRepo in this
// package ignores ListFilter.Limit, so a test written against it could not tell
// a port that forwards its argument from one that hard-codes a default — and
// "the limit silently stopped mattering" is exactly the kind of change that
// looks like a refactor.
type presenceRepo struct {
	*fakeRepo
	rows  []*model.Edge
	err   error
	got   ListFilter
	calls int
}

func (r *presenceRepo) List(_ context.Context, f ListFilter) ([]*model.Edge, error) {
	r.calls++
	r.got = f
	return r.rows, r.err
}

func TestListPresenceForwardsTheCallersLimitUnchanged(t *testing.T) {
	repo := &presenceRepo{fakeRepo: newFakeRepo()}
	uc := &Usecase{repo: repo}

	if _, err := uc.ListPresence(context.Background(), 1000); err != nil {
		t.Fatalf("ListPresence returned %v", err)
	}
	if repo.calls != 1 {
		t.Fatalf("the repo was queried %d times, want once", repo.calls)
	}
	if repo.got.Limit != 1000 {
		t.Errorf("the repo saw Limit=%d, want the 1000 the caller passed. Both callers pass a "+
			"literal, so a port that replaced it with a default would look identical in "+
			"production and would silently change how much of the fleet either one reads",
			repo.got.Limit)
	}
	// The port takes a limit and nothing else, so there is nothing else to
	// assert here on purpose: a filter that gained a Status or a Name field
	// would be a filter the two callers would have to be taught to fill in.
}

func TestListPresenceCarriesTheSixColumnsAndNothingElse(t *testing.T) {
	deleted := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	createdBy := uint64(99)
	accessKey := "AKIAEXAMPLE"
	secretHash := "argon2id$..."
	version := "0.7.43"
	pigVersion := "0.4.0"
	device := uint64(7)
	seen := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	repo := &presenceRepo{fakeRepo: newFakeRepo(), rows: []*model.Edge{{
		ID:            11,
		Name:          "web-01",
		Status:        domain.EdgeStatusOnline,
		DeviceID:      &device,
		LastSeenAt:    &seen,
		CreatedAt:     seen.Add(-time.Hour),
		AccessKeyID:   accessKey,
		SecretKeyHash: secretHash,
		AgentVersion:  version,
		PigVersion:    pigVersion,
		Description:   "should not travel",
		CreatedBy:     &createdBy,
		UpdatedAt:     seen,
		DeletedAt:     &deleted,
		DeleteMarker:  1,
	}}}
	uc := &Usecase{repo: repo}

	got, err := uc.ListPresence(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListPresence returned %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	row := got[0]
	if row.ID != 11 || row.Name != "web-01" || row.Status != domain.EdgeStatusOnline {
		t.Errorf("identity columns did not survive the projection: %+v", row)
	}
	if row.DeviceID == nil || *row.DeviceID != 7 {
		t.Errorf("DeviceID = %v, want the host device's 7; the staleness gauge keys its "+
			"metric by it and falls back to ID when it is nil", row.DeviceID)
	}
	if row.LastSeenAt == nil || !row.LastSeenAt.Equal(seen) {
		t.Errorf("LastSeenAt = %v, want %v", row.LastSeenAt, seen)
	}
	if !row.CreatedAt.Equal(seen.Add(-time.Hour)) {
		t.Errorf("CreatedAt = %v, want %v", row.CreatedAt, seen.Add(-time.Hour))
	}
	// The other nine columns have nowhere to go, which is the point. This
	// assertion is a comment with a compiler attached: EdgePresence has six
	// fields and the credential ones are not among them, pinned by
	// core/domain's own test. What is asserted here is only that the row
	// the caller receives is the projected one and not the entity.
	if row.Status == accessKey || row.Name == secretHash || row.Name == pigVersion {
		t.Error("a credential or version column leaked into the projection")
	}
}

func TestListPresenceLeavesAnUnseenNodeUnseen(t *testing.T) {
	// A node that has never been seen has no last_seen_at. The alert gauge
	// falls back to created_at, and that fallback is a fact about the data —
	// if the projection substituted a zero time or "now" instead of nil, the
	// gauge would report a brand-new node as having been seen at the epoch
	// and fire a staleness incident for every freshly registered host.
	repo := &presenceRepo{fakeRepo: newFakeRepo(), rows: []*model.Edge{{ID: 1, CreatedAt: time.Now().UTC()}}}
	uc := &Usecase{repo: repo}

	got, err := uc.ListPresence(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListPresence returned %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].LastSeenAt != nil {
		t.Errorf("LastSeenAt = %v, want nil: the column is nullable and a node that has never "+
			"been seen has no value to report", got[0].LastSeenAt)
	}
}

func TestListPresenceDropsNilRowsRatherThanHandingThemOut(t *testing.T) {
	// The consumer is a value type now, so it cannot nil-check its way out of
	// a nil in the slice. A soft-deleted scan can produce one, and the two
	// callers both range over the result. Dropping it here makes that
	// impossible instead of leaving it to every future caller.
	repo := &presenceRepo{fakeRepo: newFakeRepo(), rows: []*model.Edge{nil, {ID: 2}, nil}}
	uc := &Usecase{repo: repo}

	got, err := uc.ListPresence(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListPresence returned %v", err)
	}
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("got %+v, want exactly the one real row", got)
	}
}

func TestListPresencePassesTheReposErrorThrough(t *testing.T) {
	// Both callers treat an error as "skip this tick" (the gauge) or
	// "Failed" (the probe). Swallowing it would turn a database outage into a
	// fleet that looks perfectly healthy.
	sentinel := errors.New("connection refused")
	uc := &Usecase{repo: &presenceRepo{fakeRepo: newFakeRepo(), err: sentinel}}

	got, err := uc.ListPresence(context.Background(), 10)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the repo's own error; a swallowed one reports a healthy fleet "+
			"during an outage", err)
	}
	if got != nil {
		t.Errorf("rows = %+v, want none alongside an error", got)
	}
}

func TestListPresenceOnAnUnwiredUsecaseIsNotWiredYet(t *testing.T) {
	// The not-wired path has to stay typed, because the system-health probe
	// renders the error text into a status line and a plain "no such method"
	// would read as a broken build rather than an unwired service.
	uc := &Usecase{}
	if _, err := uc.ListPresence(context.Background(), 10); err == nil {
		t.Fatal("an unwired usecase returned no error; the probe would report the fleet as " +
			"healthy because the dependency was never built")
	}
}

// presenceStatusRepo answers a point read and records what it was asked for.
//
// It is a separate fake from presenceRepo rather than a field on it, because
// the two ports ask different questions and a fake that answered both would
// let a test pass against an implementation wired to the wrong one — which is
// the one mistake the two-interface split exists to make impossible.
type presenceStatusRepo struct {
	*fakeRepo
	edge *model.Edge
	err  error
	ids  []uint64
}

func (r *presenceStatusRepo) GetByID(_ context.Context, id uint64) (*model.Edge, error) {
	r.ids = append(r.ids, id)
	if r.err != nil {
		return nil, r.err
	}
	return r.edge, nil
}

// TestPresenceStatusAnswersWithTheColumnAndNothingElse is the projection
// test for the webshell's question.
//
// The consumer compares the answer against domain.EdgeStatusOnline and puts
// it in an error message when it does not match. That is all it does, so the
// test asserts the same thing and no more: a caller that wanted a name or a
// last-seen stamp would not compile against a string, which is the property
// being bought.
//
// The three rows are the point, and the third one is why. The first draft of
// this test held a single online row, and the mutation that replaced the
// projection with the constant `return domain.EdgeStatusOnline` PASSED it —
// because a test that only ever sees "online" cannot tell reading a column
// from returning a literal. That is the sixteenth measuring hole in this
// ledger, and it was found by running the mutation rather than by reading the
// test, which is the only reason it was found at all.
//
// The third value is not reachable through the edges table's CHECK constraint,
// and it is here anyway: a projection that maps values is a different
// function from one that forwards them, and a test that cannot tell them apart
// is a test that will not notice the day somebody adds a third state.
func TestPresenceStatusAnswersWithTheColumnAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
	}{
		{"the node is online", model.StatusOnline},
		{"the node is not answering", model.StatusOffline},
		{"a value no current row can hold", "draining"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &presenceStatusRepo{
				fakeRepo: newFakeRepo(),
				edge:     &model.Edge{ID: 4321, Status: tc.status},
			}
			uc := &Usecase{repo: repo}

			got, err := uc.PresenceStatus(context.Background(), 4321)
			if err != nil {
				t.Fatalf("PresenceStatus returned %v", err)
			}
			if got != tc.status {
				t.Errorf("PresenceStatus = %q, want %q; the port forwards the column, and a "+
					"projection that substituted a constant here would report a node that "+
					"cannot be reached as one that can", got, tc.status)
			}
			if len(repo.ids) != 1 || repo.ids[0] != 4321 {
				t.Errorf("the repo was asked for %v, want exactly one read of edge 4321", repo.ids)
			}
		})
	}
}

// TestPresenceStatusReportsAMissingNodeAsAnErrorNotAsAState is the branch
// this cut deleted from the consumer.
//
// The old port returned `*model.Edge`, so its holder carried
// `if edge == nil { ... }` — a state no repository here produces, since
// u.Get maps a missing row to errs.ErrNotFound. This pins the behaviour that
// makes that branch unnecessary: a node that is gone is an error, and never a
// presence state a caller could mistake for a reachable one.
func TestPresenceStatusReportsAMissingNodeAsAnErrorNotAsAState(t *testing.T) {
	repo := &presenceStatusRepo{fakeRepo: newFakeRepo(), err: errs.ErrNotFound}
	uc := &Usecase{repo: repo}

	got, err := uc.PresenceStatus(context.Background(), 7)
	if err == nil {
		t.Fatalf("PresenceStatus returned %q for a node that is gone; a caller that checks "+
			"the value first would open a shell to a node that does not exist", got)
	}
	if !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("PresenceStatus returned %v, want errs.ErrNotFound; the three reasons a shell "+
			"cannot open are logged separately, so the reason has to survive the projection", err)
	}
	if got != "" {
		t.Errorf("PresenceStatus returned %q alongside an error; the value must be empty so a "+
			"caller that ignores the error cannot read an empty status as a presence state", got)
	}
}

// TestPresenceStatusRefusesWhenTheTreeIsNotWired covers the boot mistake.
//
// Usecase.Get answers errs.ErrNotWiredYet when the repository was never
// handed in. That state is exactly the one the composition root can produce by
// forgetting an argument, and it has to arrive as an error rather than as a
// zero value that a caller would read as "not online, but present".
func TestPresenceStatusRefusesWhenTheTreeIsNotWired(t *testing.T) {
	uc := &Usecase{}
	got, err := uc.PresenceStatus(context.Background(), 1)
	if !errors.Is(err, errs.ErrNotWiredYet) {
		t.Fatalf("an unwired usecase returned (%q, %v), want errs.ErrNotWiredYet", got, err)
	}
}
