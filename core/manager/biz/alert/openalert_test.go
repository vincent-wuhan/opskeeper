package alert

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
)

// openAlertRepo records the filter it was asked for and returns whatever it
// was told to. It embeds Repo so it does not have to implement two hundred
// methods to answer one question.
type openAlertRepo struct {
	Repo
	seen     IncidentFilter
	rows     []*model.Incident
	listedOK bool
}

func (r *openAlertRepo) ListIncidents(_ context.Context, f IncidentFilter) ([]*model.Incident, error) {
	r.seen = f
	r.listedOK = true
	return r.rows, nil
}

func openAlertUsecase(repo Repo) *Usecase { return NewUsecase(repo, nil) }

// TestTheOpenAlertAdapterAsksForOpenIncidents is the one assertion this
// adapter cannot lose.
//
// The method is named ListOpenAlerts, and its consumer has no status filter
// to set — "open" stopped being a parameter when the edge was cut and
// became a precondition written in the name. That makes the name the only
// place the precondition lives, so the thing worth testing is that the
// implementation still honours it: if the status filter is dropped, the
// adapter returns incidents that were already closed, and a recovery
// closure will "close" one a second time and write a resolution reason onto
// a row an operator has already signed off on.
func TestTheOpenAlertAdapterAsksForOpenIncidents(t *testing.T) {
	repo := &openAlertRepo{rows: []*model.Incident{{
		DedupeKey:  "pg-pool-exhaustion",
		Status:     model.IncidentStatusOpen,
		LabelsJSON: `{"incident_id":"incident-1"}`,
	}}}
	resolver := OpenAlertResolver{UC: openAlertUsecase(repo)}

	got, err := resolver.ListOpenAlerts(context.Background(), 500)
	if err != nil {
		t.Fatalf("ListOpenAlerts: %v", err)
	}
	if !repo.listedOK {
		t.Fatal("the repository was never asked, so the filter below proves nothing")
	}
	if repo.seen.Status != model.IncidentStatusOpen {
		t.Errorf("the adapter asked for status %q, not %q. A method called ListOpenAlerts "+
			"that does not filter to open is a method whose name is the only thing keeping "+
			"a closed incident out of a recovery closure",
			repo.seen.Status, model.IncidentStatusOpen)
	}
	if repo.seen.Limit != 500 {
		t.Errorf("the caller's limit was %d, want 500", repo.seen.Limit)
	}
	if len(got) != 1 || got[0].DedupeKey != "pg-pool-exhaustion" {
		t.Fatalf("got %+v, want the one incident projected", got)
	}
}

// TestTheOpenAlertAdapterProjectsTwoColumns pins the projection's width,
// because a projection that grows is a boundary that widens without a
// decision. The entity has twenty-five; this asserts what crosses.
func TestTheOpenAlertAdapterProjectsTwoColumns(t *testing.T) {
	repo := &openAlertRepo{rows: []*model.Incident{{
		ID:              7,
		DedupeKey:       "k",
		LabelsJSON:      "{}",
		Rule:            "r",
		Severity:        "critical",
		Scope:           "host",
		ScopeType:       "host",
		Summary:         "s",
		EventCount:      12,
		FirstFiredAt:    time.Now(),
		LastFiredAt:     time.Now(),
		ResolvedAt:      nil,
		Description:     "d",
		AnnotationsJSON: "{}",
		RunbookURL:      "u",
	}}}
	got, err := OpenAlertResolver{UC: openAlertUsecase(repo)}.ListOpenAlerts(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListOpenAlerts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0] != (domain.OpenAlert{DedupeKey: "k", LabelsJSON: "{}"}) {
		t.Errorf("projection carries %+v; DedupeKey and LabelsJSON are all this "+
			"consumer reads out of a twenty-five-column entity", got[0])
	}

	// The value comparison above cannot see a third column, and a previous
	// version of this file claimed it could. It cannot: adding a field to the
	// struct leaves both sides of that comparison zero-valued at it, so the
	// assertion still passes and the boundary quietly goes back to
	// twenty-six columns. Width is a property of the type, so it is measured
	// on the type.
	want := []string{"DedupeKey", "LabelsJSON"}
	typ := reflect.TypeOf(domain.OpenAlert{})
	got_ := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got_ = append(got_, typ.Field(i).Name)
	}
	if !reflect.DeepEqual(got_, want) {
		t.Errorf("domain.OpenAlert has fields %v, want exactly %v. A projection "+
			"that grows is a boundary that widens without a decision: the next "+
			"person to add a column to the entity adds it here too, and the "+
			"twenty-five columns this cut removed come back one at a time",
			got_, want)
	}
}

// TestANilRowBecomesNoRow: the store returns []*Incident, so a nil element is
// representable, and projecting it would produce an OpenAlert with an empty
// dedupe key — which reads as a real incident with no labels, and would be
// resolved against a key nobody chose.
func TestANilRowBecomesNoRow(t *testing.T) {
	repo := &openAlertRepo{rows: []*model.Incident{nil, {DedupeKey: "k"}}}
	got, err := OpenAlertResolver{UC: openAlertUsecase(repo)}.ListOpenAlerts(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListOpenAlerts: %v", err)
	}
	if len(got) != 1 || got[0].DedupeKey != "k" {
		t.Errorf("got %+v, want the nil dropped and the one row kept", got)
	}
}

// TestTheResolverWithNoUsecaseSaysSo: the zero value is a valid thing to
// hold, so the failure has to arrive at the call and be recognisable, not
// panic on a nil pointer.
func TestTheResolverWithNoUsecaseSaysSo(t *testing.T) {
	var resolver OpenAlertResolver
	if _, err := resolver.ListOpenAlerts(context.Background(), 1); !errors.Is(err, ErrUsecaseNotWired) {
		t.Errorf("ListOpenAlerts on the zero resolver: %v, want ErrUsecaseNotWired", err)
	}
	if _, err := resolver.SystemResolveIncident(context.Background(), "k", "r", time.Now()); !errors.Is(err, ErrUsecaseNotWired) {
		t.Errorf("SystemResolveIncident on the zero resolver: %v, want ErrUsecaseNotWired", err)
	}
}
