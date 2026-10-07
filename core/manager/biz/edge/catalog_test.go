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

// catalogRepo answers the three reads the catalog port makes and records the
// ListFilter it was handed.
//
// It is a third fake in this package rather than a second field on
// presenceRepo because the two ports ask different questions of the store, and
// a fake that answered both would let a test pass against an implementation
// wired to the wrong read — which is the one mistake the three-port split is
// meant to make impossible.
type catalogRepo struct {
	*fakeRepo
	rows     []*model.Edge
	listErr  error
	byID     *model.Edge
	byIDErr  error
	byName   *model.Edge
	nameErr  error
	got      ListFilter
	askedIDs []uint64
	asked    []string
}

func (r *catalogRepo) List(_ context.Context, f ListFilter) ([]*model.Edge, error) {
	r.got = f
	return r.rows, r.listErr
}

func (r *catalogRepo) GetByID(_ context.Context, id uint64) (*model.Edge, error) {
	r.askedIDs = append(r.askedIDs, id)
	return r.byID, r.byIDErr
}

func (r *catalogRepo) GetByName(_ context.Context, name string) (*model.Edge, error) {
	r.asked = append(r.asked, name)
	return r.byName, r.nameErr
}

// TestListCatalogPushesOnlyWhatTheStoreCanAnswer pins the split between the
// three filter fields and the two the store cannot do.
//
// Status and Limit are the edge domain's own ListFilter columns, so they go
// down to SQL. NameContains and SeenAfter are not: ListFilter.Name is an exact
// match, and a window is not a column. Handing a substring to an exact-match
// column would answer "the node called exactly web" for a tool that asked for
// "something like web" — a change invisible in any single call and wrong in
// every one of them. The two post-filters stay in ListCatalog, and this test
// fails the day someone "simplifies" them into the pushed-down pair.
func TestListCatalogPushesOnlyWhatTheStoreCanAnswer(t *testing.T) {
	repo := &catalogRepo{fakeRepo: newFakeRepo(), rows: []*model.Edge{{ID: 1, Name: "web-01"}}}
	uc := &Usecase{repo: repo}

	cutoff := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if _, err := uc.ListCatalog(context.Background(), domain.EdgeFilter{
		Status:       domain.EdgeStatusOnline,
		NameContains: "web",
		SeenAfter:    &cutoff,
		Limit:        25,
	}); err != nil {
		t.Fatalf("ListCatalog returned %v", err)
	}
	if repo.got.Status != domain.EdgeStatusOnline {
		t.Errorf("the store saw Status=%q, want %q", repo.got.Status, domain.EdgeStatusOnline)
	}
	if repo.got.Limit != 25 {
		t.Errorf("the store saw Limit=%d, want the 25 the caller passed", repo.got.Limit)
	}
	if repo.got.Name != "" {
		t.Errorf("the store saw Name=%q; ListFilter.Name is an exact match and this port was "+
			"asked for a substring, so pushing it down would silently answer a different question",
			repo.got.Name)
	}
}

// TestListCatalogMatchesNamesAsSubstrings is the other half of the same rule,
// and the mutation that proves it: rewriting the match as `e.Name == f.NameContains`
// passes every other test in this file, because every other fixture contains
// exactly one row whose name is either a superset or unrelated. The tool asks
// for "db" and gets back two nodes instead of one, and an investigator
// comparing two node summaries starts comparing the wrong two.
func TestListCatalogMatchesNamesAsSubstrings(t *testing.T) {
	repo := &catalogRepo{fakeRepo: newFakeRepo(), rows: []*model.Edge{
		{ID: 1, Name: "db-primary"},
		{ID: 2, Name: "web-primary"},
		{ID: 3, Name: "cache-01"},
	}}
	uc := &Usecase{repo: repo}

	got, err := uc.ListCatalog(context.Background(), domain.EdgeFilter{NameContains: "-primary"})
	if err != nil {
		t.Fatalf("ListCatalog returned %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows (%v), want the 2 whose names contain %q", len(got), got, "-primary")
	}
	if got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("got IDs %d,%d; the store's order must survive the filter", got[0].ID, got[1].ID)
	}

	empty, err := uc.ListCatalog(context.Background(), domain.EdgeFilter{})
	if err != nil {
		t.Fatalf("ListCatalog returned %v", err)
	}
	if len(empty) != 3 {
		t.Errorf("an empty NameContains filtered %d rows out of 3; a match rule that fires on "+
			"the empty string would make an unfiltered listing return nothing at all", len(empty))
	}
}

// TestListCatalogDropsNodesThatHaveNeverReported pins the nil half of the
// window filter, and it is the assertion this decision nearly got wrong.
//
// The rule the tools had before the filter moved was
// `LastSeenAt == nil || LastSeenAt.Before(cutoff)` — a node that has never
// phoned home counts as not-seen-recently. The tempting rewrite is to treat
// the missing stamp as unknown and keep the row, which reads better and means
// `last_seen_within_minutes: 30` also returns every node that has never run an
// agent: an RCA would conclude that a node registered that morning was active
// during last night's incident. The tool asks about the last thirty minutes;
// a node with no thirty minutes is not an answer to that.
func TestListCatalogDropsNodesThatHaveNeverReported(t *testing.T) {
	seen := time.Date(2026, 5, 1, 12, 30, 0, 0, time.UTC)
	repo := &catalogRepo{fakeRepo: newFakeRepo(), rows: []*model.Edge{
		{ID: 1, Name: "recent", LastSeenAt: &seen},
		{ID: 2, Name: "stale", LastSeenAt: func() *time.Time { t := seen.Add(-2 * time.Hour); return &t }()},
		{ID: 3, Name: "never", LastSeenAt: nil},
	}}
	uc := &Usecase{repo: repo}

	cutoff := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	got, err := uc.ListCatalog(context.Background(), domain.EdgeFilter{SeenAfter: &cutoff})
	if err != nil {
		t.Fatalf("ListCatalog returned %v", err)
	}
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("got %d rows (%v), want only node 1: node 2's stamp is before the cutoff and "+
			"node 3 has none at all", len(got), got)
	}

	all, err := uc.ListCatalog(context.Background(), domain.EdgeFilter{})
	if err != nil {
		t.Fatalf("ListCatalog returned %v", err)
	}
	if len(all) != 3 {
		t.Errorf("with no window the filter kept %d of 3 rows; a nil SeenAfter means \"no "+
			"window\", which is a different request from \"the window that includes "+
			"everything\" — that is why the field is a pointer", len(all))
	}
}

// TestListCatalogKeepsTheNodeExactlyOnTheBoundary pins the inclusive edge of
// the comparison, which is the one case where "older than" and "not at or
// after" would read differently in the two places the rule is written down.
func TestListCatalogKeepsTheNodeExactlyOnTheBoundary(t *testing.T) {
	cutoff := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	atCutoff := cutoff
	repo := &catalogRepo{fakeRepo: newFakeRepo(), rows: []*model.Edge{
		{ID: 1, Name: "exactly", LastSeenAt: &atCutoff},
	}}
	uc := &Usecase{repo: repo}

	got, err := uc.ListCatalog(context.Background(), domain.EdgeFilter{SeenAfter: &cutoff})
	if err != nil {
		t.Fatalf("ListCatalog returned %v", err)
	}
	if len(got) != 1 {
		t.Errorf("a node last seen exactly at the cutoff was dropped; the filter says "+
			"\"at or after\", and an operator asking for the last hour means the hour they named")
	}
}

// TestPresenceReportsAMissingNodeAsAnAnswer pins the ErrNotFound translation.
//
// Before this port the tools each spelled `err == nil && edge != nil` at every
// call site, six times, and one of them got it wrong on the not-found path and
// reported an empty summary as if the node existed. The port removes the state
// the check was guarding, so the translation has to be pinned here rather than
// left to the reader of an if-statement.
func TestPresenceReportsAMissingNodeAsAnAnswer(t *testing.T) {
	repo := &catalogRepo{fakeRepo: newFakeRepo(), byIDErr: errs.ErrNotFound}
	uc := &Usecase{repo: repo}

	got, found, err := uc.Presence(context.Background(), 7)
	if err != nil {
		t.Fatalf("Presence returned %v for a node that is gone; a missing node is an answer, "+
			"and a caller that treats it as a failure reports a lookup error instead of "+
			"\"that node is not registered\"", err)
	}
	if found {
		t.Errorf("Presence reported found=true for a node that is gone: %+v", got)
	}
	if len(repo.askedIDs) != 1 || repo.askedIDs[0] != 7 {
		t.Errorf("the store was asked for %v, want exactly one read of node 7", repo.askedIDs)
	}
}

// TestPresenceByNameIsAPointReadNotAListing is the reason the port has four
// methods instead of one ListCatalog with a name in the filter: an operator
// hands a tool a node's name and nothing else. The implementation that folds
// it in still answers correctly, so only a count of store reads can tell the
// two apart — and on a fleet of five thousand nodes the difference is the
// difference between one row and the whole estate on a path the RCA loop walks
// for every suspect it forms.
func TestPresenceByNameIsAPointReadNotAListing(t *testing.T) {
	repo := &catalogRepo{fakeRepo: newFakeRepo(), byName: &model.Edge{ID: 9, Name: "db-primary"}}
	uc := &Usecase{repo: repo}

	got, found, err := uc.PresenceByName(context.Background(), "db-primary")
	if err != nil {
		t.Fatalf("PresenceByName returned %v", err)
	}
	if !found || got.ID != 9 || got.Name != "db-primary" {
		t.Fatalf("PresenceByName = (%+v, %t), want node 9 found", got, found)
	}
	if len(repo.asked) != 1 || repo.asked[0] != "db-primary" {
		t.Errorf("the store was asked for names %v, want exactly one lookup of db-primary", repo.asked)
	}
	if repo.got.Limit != 0 {
		t.Errorf("a name lookup went through the listing path with Limit=%d; a method that "+
			"loads the estate to find one row is the reason this method exists", repo.got.Limit)
	}
}

// TestPresenceByNameReportsAMissingNodeTheSameWayPresenceDoes keeps the two
// keys of the same question from drifting apart, which is what happens when
// two methods each carry their own copy of a translation.
func TestPresenceByNameReportsAMissingNodeTheSameWayPresenceDoes(t *testing.T) {
	repo := &catalogRepo{fakeRepo: newFakeRepo(), nameErr: errs.ErrNotFound}
	uc := &Usecase{repo: repo}

	got, found, err := uc.PresenceByName(context.Background(), "gone")
	if err != nil {
		t.Fatalf("PresenceByName returned %v, want the same nil error Presence gives", err)
	}
	if found || got.ID != 0 {
		t.Errorf("PresenceByName = (%+v, %t), want the zero row and found=false", got, found)
	}
}

// TestCatalogPropagatesAStoreFailureUntranslated keeps the ErrNotFound
// translation from swallowing real errors: a port that turned every error into
// found=false would report a database outage as "this node is not registered",
// and an investigator would spend its turns concluding the fleet is healthy.
func TestCatalogPropagatesAStoreFailureUntranslated(t *testing.T) {
	boom := errors.New("connection refused")
	repo := &catalogRepo{fakeRepo: newFakeRepo(), byIDErr: boom, nameErr: boom, listErr: boom}
	uc := &Usecase{repo: repo}

	if _, found, err := uc.Presence(context.Background(), 1); !errors.Is(err, boom) || found {
		t.Errorf("Presence = (found=%t, %v), want (false, the store's error)", found, err)
	}
	if _, found, err := uc.PresenceByName(context.Background(), "x"); !errors.Is(err, boom) || found {
		t.Errorf("PresenceByName = (found=%t, %v), want (false, the store's error)", found, err)
	}
	if _, err := uc.ListCatalog(context.Background(), domain.EdgeFilter{}); !errors.Is(err, boom) {
		t.Errorf("ListCatalog returned %v, want the store's error", err)
	}
}

// TestCatalogRefusesWhenTheTreeIsNotWired covers the boot mistake, for all
// three reads. ErrNotWiredYet is not ErrNotFound, so the translation above
// does not swallow it: an unwired composition root produces a visible
// failure instead of a fleet that looks empty.
func TestCatalogRefusesWhenTheTreeIsNotWired(t *testing.T) {
	uc := &Usecase{}
	if _, _, err := uc.Presence(context.Background(), 1); !errors.Is(err, errs.ErrNotWiredYet) {
		t.Errorf("Presence on an unwired usecase returned %v, want errs.ErrNotWiredYet", err)
	}
	if _, _, err := uc.PresenceByName(context.Background(), "x"); !errors.Is(err, errs.ErrNotWiredYet) {
		t.Errorf("PresenceByName on an unwired usecase returned %v, want errs.ErrNotWiredYet", err)
	}
	if _, err := uc.ListCatalog(context.Background(), domain.EdgeFilter{}); !errors.Is(err, errs.ErrNotWiredYet) {
		t.Errorf("ListCatalog on an unwired usecase returned %v, want errs.ErrNotWiredYet", err)
	}
}

// TestCatalogCarriesTheSameSixColumnsAsTheOtherTwoPorts keeps the three ports
// from projecting the row three different ways. The edge domain row has fifteen
// columns including two credential ones; a fourth method with its own copy of
// the field list is where one of them would start leaking.
func TestCatalogCarriesTheSameSixColumnsAsTheOtherTwoPorts(t *testing.T) {
	device := uint64(3)
	seen := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	row := &model.Edge{
		ID: 5, Name: "web-01", Status: domain.EdgeStatusOnline, DeviceID: &device,
		LastSeenAt: &seen, CreatedAt: seen.Add(-time.Hour),
		AccessKeyID: "AKIAEXAMPLE", SecretKeyHash: "argon2id$...",
	}
	repo := &catalogRepo{
		fakeRepo: newFakeRepo(),
		rows:     []*model.Edge{row},
		byID:     row,
		byName:   row,
	}
	uc := &Usecase{repo: repo}

	fromList, err := uc.ListCatalog(context.Background(), domain.EdgeFilter{})
	if err != nil {
		t.Fatalf("ListCatalog returned %v", err)
	}
	byID, _, err := uc.Presence(context.Background(), 5)
	if err != nil {
		t.Fatalf("Presence returned %v", err)
	}
	byName, _, err := uc.PresenceByName(context.Background(), "web-01")
	if err != nil {
		t.Fatalf("PresenceByName returned %v", err)
	}
	if len(fromList) != 1 {
		t.Fatalf("ListCatalog returned %d rows, want 1", len(fromList))
	}
	a, b, c := fromList[0], byID, byName
	if a != b || b != c {
		t.Errorf("the three reads disagree about the same row:\n list: %+v\n by id: %+v\n by name: %+v",
			a, b, c)
	}
	// The credential assertion is a comment with a compiler attached:
	// domain.EdgePresence has six fields and AccessKeyID / SecretKeyHash
	// are not among them, so a projection that started copying them would
	// not build rather than fail here. That is the property worth having,
	// and it is why the check above is a comparison and not a field probe.
	_ = a
}
