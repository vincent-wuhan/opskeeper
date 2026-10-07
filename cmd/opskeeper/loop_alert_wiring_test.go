package main

import (
	"context"
	"errors"
	"testing"

	"time"

	managerbizloop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"

	alertbiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/alert"
	alertmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
)

type stubLoopAlertRepo struct {
	alertbiz.Repo
	gotIncidentID uint64
	gotRuleID     uint64
	gotFilter     alertbiz.IncidentFilter
	incident      *alertmodel.Incident
	incidents     []*alertmodel.Incident
	rule          *alertmodel.Rule
	err           error
}

func (s *stubLoopAlertRepo) GetIncidentByID(_ context.Context, id uint64) (*alertmodel.Incident, error) {
	s.gotIncidentID = id
	return s.incident, s.err
}

func (s *stubLoopAlertRepo) ListIncidents(_ context.Context, f alertbiz.IncidentFilter) ([]*alertmodel.Incident, error) {
	s.gotFilter = f
	return s.incidents, s.err
}

func (s *stubLoopAlertRepo) GetRuleByID(_ context.Context, id uint64) (*alertmodel.Rule, error) {
	s.gotRuleID = id
	return s.rule, s.err
}

// Every projected field gets a value that is not the zero value of its
// type, and the id gets a value no other id in the test has. A conversion
// that dropped a field would otherwise produce a row that looks fine.
func TestTheLoopIncidentProjectionCarriesEveryField(t *testing.T) {
	ruleID := uint64(77)
	first := timeAt(3, 4)
	updated := timeAt(3, 5)
	repo := &stubLoopAlertRepo{incident: &alertmodel.Incident{
		ID:           11,
		Severity:     "critical",
		Scope:        "pg",
		Rule:         "pg.lock_waits",
		RuleID:       &ruleID,
		FirstFiredAt: first,
		UpdatedAt:    updated,
		LabelsJSON:   `{"host":"host-9"}`,
	}}
	got, err := loopAlertReader{repo: repo}.GetIncidentByID(context.Background(), 11)
	if err != nil {
		t.Fatalf("GetIncidentByID: %v", err)
	}
	if repo.gotIncidentID != 11 {
		t.Errorf("the id did not reach the repository: got %d", repo.gotIncidentID)
	}
	if got == nil {
		t.Fatal("got nil incident")
	}
	if got.ID != 11 {
		t.Errorf("ID: got %d, want 11", got.ID)
	}
	if got.Severity != "critical" {
		t.Errorf("Severity: got %q, want %q", got.Severity, "critical")
	}
	if got.Scope != "pg" {
		t.Errorf("Scope: got %q, want %q", got.Scope, "pg")
	}
	if got.Rule != "pg.lock_waits" {
		t.Errorf("Rule: got %q, want %q", got.Rule, "pg.lock_waits")
	}
	if got.RuleID == nil || *got.RuleID != 77 {
		t.Errorf("RuleID: got %v, want 77", got.RuleID)
	}
	if !got.FirstFiredAt.Equal(first) {
		t.Errorf("FirstFiredAt: got %v, want %v", got.FirstFiredAt, first)
	}
	if !got.UpdatedAt.Equal(updated) {
		t.Errorf("UpdatedAt: got %v, want %v", got.UpdatedAt, updated)
	}
	if got.LabelsJSON != `{"host":"host-9"}` {
		t.Errorf("LabelsJSON: got %q", got.LabelsJSON)
	}
}

// RuleID is a pointer on both sides and the two cases are different
// answers, not the same answer twice: nil means the incident fired from a
// static rule, and a non-nil pointer to zero does not. An adapter that
// dereferenced and copied the value would turn the second into the first,
// and the trigger adapter treats them differently.
func TestTheLoopIncidentProjectionKeepsAnAbsentRuleAbsent(t *testing.T) {
	repo := &stubLoopAlertRepo{incident: &alertmodel.Incident{ID: 5, RuleID: nil}}
	got, err := loopAlertReader{repo: repo}.GetIncidentByID(context.Background(), 5)
	if err != nil {
		t.Fatalf("GetIncidentByID: %v", err)
	}
	if got.RuleID != nil {
		t.Errorf("RuleID: got %v, want nil", *got.RuleID)
	}
}

// A row that does not exist is a real answer here, and the loop's adapters
// ask the question. Projecting it into a zero-valued struct would report an
// incident that exists with no rule, no labels and no severity — and the
// labels adapter would go on to parse an empty labels column and report
// that this alert "names no object" instead of that it is gone.
func TestAMissingIncidentStaysMissing(t *testing.T) {
	got, err := loopAlertReader{repo: &stubLoopAlertRepo{incident: nil}}.GetIncidentByID(context.Background(), 404)
	if err != nil {
		t.Fatalf("GetIncidentByID: %v", err)
	}
	if got != nil {
		t.Errorf("a missing incident was projected as %+v", got)
	}
	if incidentFrom(nil) != nil {
		t.Error("incidentFrom(nil) should be nil")
	}
}

func TestAMissingRuleStaysMissing(t *testing.T) {
	got, err := loopAlertReader{repo: &stubLoopAlertRepo{rule: nil}}.GetRuleByID(context.Background(), 9)
	if err != nil {
		t.Fatalf("GetRuleByID: %v", err)
	}
	if got != nil {
		t.Errorf("a missing rule was projected as %+v", got)
	}
}

func TestARepositoryErrorIsNotSwallowed(t *testing.T) {
	boom := errors.New("db down")
	if _, err := (loopAlertReader{repo: &stubLoopAlertRepo{err: boom}}).GetIncidentByID(context.Background(), 1); !errors.Is(err, boom) {
		t.Errorf("GetIncidentByID: got %v, want the repository's error", err)
	}
	if _, err := (loopAlertReader{repo: &stubLoopAlertRepo{err: boom}}).ListIncidents(context.Background(), alertIncidentFilterForTest()); !errors.Is(err, boom) {
		t.Errorf("ListIncidents: got %v, want the repository's error", err)
	}
	if _, err := (loopAlertReader{repo: &stubLoopAlertRepo{err: boom}}).GetRuleByID(context.Background(), 1); !errors.Is(err, boom) {
		t.Errorf("GetRuleByID: got %v, want the repository's error", err)
	}
}

// nils in the listing stay in the listing. The loop's own adapter has
// always skipped them, and moving that decision into this file would put
// it in a file that never mentioned it.
func TestTheListingPassesNilsThroughRatherThanCompactingThem(t *testing.T) {
	repo := &stubLoopAlertRepo{incidents: []*alertmodel.Incident{
		{ID: 1}, nil, {ID: 3},
	}}
	got, err := loopAlertReader{repo: repo}.ListIncidents(context.Background(), alertIncidentFilterForTest())
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3 — a nil was compacted away", len(got))
	}
	if got[1] != nil {
		t.Errorf("entry 1: got %+v, want nil", got[1])
	}
	if got[0].ID != 1 || got[2].ID != 3 {
		t.Errorf("the non-nil entries moved: %+v", got)
	}
}

func TestTheListingCarriesTheFilterAndProjectsEachRow(t *testing.T) {
	repo := &stubLoopAlertRepo{incidents: []*alertmodel.Incident{{ID: 1, Severity: "warning", Scope: "redis"}}}
	_, err := loopAlertReader{repo: repo}.ListIncidents(context.Background(), managerLoopFilter("pg.lock_waits", 100))
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if repo.gotFilter.RuleKey != "pg.lock_waits" {
		t.Errorf("RuleKey: got %q, want %q", repo.gotFilter.RuleKey, "pg.lock_waits")
	}
	if repo.gotFilter.Limit != 100 {
		t.Errorf("Limit: got %d, want 100", repo.gotFilter.Limit)
	}
	// The filter the loop declares has two fields and the one alert
	// declares has more; the ones the loop does not declare must arrive
	// zero rather than inheriting a previous call's value.
	if repo.gotFilter.Status != "" || repo.gotFilter.DeviceID != nil || repo.gotFilter.Offset != 0 {
		t.Errorf("undeclared filter fields were set: %+v", repo.gotFilter)
	}
}

func TestTheRuleProjectionCarriesItsConditions(t *testing.T) {
	repo := &stubLoopAlertRepo{rule: &alertmodel.Rule{ID: 3, ConditionsJSON: `[{"metric":"m","operator":">","threshold":1}]`}}
	got, err := loopAlertReader{repo: repo}.GetRuleByID(context.Background(), 3)
	if err != nil {
		t.Fatalf("GetRuleByID: %v", err)
	}
	if repo.gotRuleID != 3 {
		t.Errorf("the id did not reach the repository: got %d", repo.gotRuleID)
	}
	if got.ConditionsJSON != `[{"metric":"m","operator":">","threshold":1}]` {
		t.Errorf("ConditionsJSON: got %q", got.ConditionsJSON)
	}
}

func timeAt(month, day int) time.Time {
	return time.Date(2026, time.Month(month), day, 12, 0, 0, 0, time.UTC)
}

func alertIncidentFilterForTest() managerbizloop.AlertIncidentFilter {
	return managerbizloop.AlertIncidentFilter{}
}

func managerLoopFilter(key string, limit int) managerbizloop.AlertIncidentFilter {
	return managerbizloop.AlertIncidentFilter{RuleKey: key, Limit: limit}
}
