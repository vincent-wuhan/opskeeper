package knowledge

import (
	"context"
	"errors"
	"testing"

	incidentcontrol "github.com/vincent-wuhan/opskeeper/internal/control/incident"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/qdrantx"
)

func TestSearchPersistsHybridRRFRecall(t *testing.T) {
	vec := &rrfVec{
		vectorHits: []qdrantx.SearchHit{
			{ID: 20, Score: 0.99, Payload: rrfPayload(20, "Generic database note", "restart database")},
			{ID: 10, Score: 0.80, Payload: rrfPayload(10, "Pool exhaustion runbook", "pool waiters exhausted connections")},
		},
		scrollPoints: []qdrantx.SearchHit{
			{ID: 20, Payload: rrfPayload(20, "Generic database note", "restart database")},
			{ID: 10, Payload: rrfPayload(10, "Pool exhaustion runbook", "pool waiters exhausted connections")},
		},
	}
	recorder := &rrfRecorder{}
	u := &Usecase{vec: vec, embed: fakeEmbed{}, recallRepo: recorder}

	hits, err := u.Search(context.Background(), "pool exhaustion runbook", SearchOptions{
		TenantID: "opskeeper-demo", IncidentID: "RECALL-PG-POOL", Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if hits[0].Doc.ID != 10 {
		t.Fatalf("top doc = %d, want keyword/vector consensus doc 10", hits[0].Doc.ID)
	}
	if len(recorder.logs) != 2 {
		t.Fatalf("recall logs = %d, want 2", len(recorder.logs))
	}
	var selected *incidentcontrol.RecallLog
	for index := range recorder.logs {
		if recorder.logs[index].Selected {
			selected = &recorder.logs[index]
		}
	}
	if selected == nil {
		t.Fatal("no selected recall candidate")
	}
	if selected.CandidateRef != "knowledge:10" || selected.VectorRank != 2 || selected.KeywordRank != 1 {
		t.Fatalf("selected recall = %+v, want knowledge:10 vector=2 keyword=1", *selected)
	}
}

func TestSearchRequiresRecallRepositoryForIncident(t *testing.T) {
	u := &Usecase{vec: &rrfVec{vectorHits: []qdrantx.SearchHit{{ID: 10, Payload: rrfPayload(10, "DNS", "dig")}}}, embed: fakeEmbed{}}
	_, err := u.Search(context.Background(), "dns", SearchOptions{TenantID: "opskeeper-demo", IncidentID: "INC-1"})
	if err == nil || !errors.Is(err, err) || err.Error() != "knowledge: incident recall repository not wired" {
		t.Fatalf("error = %v, want recall repository not wired", err)
	}
}

func TestSearchIncludesStructuredRunbookMemory(t *testing.T) {
	vec := &rrfVec{
		vectorHits: []qdrantx.SearchHit{
			{ID: 30, Score: 0.99, Payload: rrfPayload(30, "Unrelated document", "network dns")},
			{ID: 20, Score: 0.90, Payload: rrfPayload(20, "Generic database note", "restart database")},
		},
	}
	recorder := &rrfRecorder{runbooks: []incidentcontrol.Postmortem{{
		TenantID: "opskeeper-demo", IncidentID: "INC-PG-POOL-001",
		DatabaseType: "PostgreSQL", FaultFingerprint: "pool_capacity_exhausted",
		Symptom:   incidentcontrol.PostmortemSymptom{Summary: "pool exhaustion runbook"},
		Diagnosis: incidentcontrol.PostmortemDiagnosis{RootCause: "pool exhaustion"},
	}}}
	u := &Usecase{vec: vec, embed: fakeEmbed{}, recallRepo: recorder}

	hits, err := u.Search(context.Background(), "pool exhaustion runbook", SearchOptions{
		TenantID: "opskeeper-demo", IncidentID: "RECALL-PG-POOL", DatabaseType: "PostgreSQL",
		FaultFingerprint: "pool_capacity_exhausted",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Doc.SourceType != "runbook_memory" {
		t.Fatalf("hits = %+v, want structured runbook first", hits)
	}
	selected := false
	for _, log := range recorder.logs {
		selected = selected || (log.Selected && log.CandidateRef == "runbook:INC-PG-POOL-001")
	}
	if !selected {
		t.Fatalf("recall logs = %+v, want runbook:INC-PG-POOL-001 selected", recorder.logs)
	}
}

type rrfVec struct {
	fakeVec
	vectorHits   []qdrantx.SearchHit
	scrollPoints []qdrantx.SearchHit
}

func (v *rrfVec) Search(_ context.Context, _ string, _ []float32, opts qdrantx.SearchOpts) ([]qdrantx.SearchHit, error) {
	v.searchOpts = append(v.searchOpts, opts)
	return v.vectorHits, nil
}

func (v *rrfVec) Scroll(context.Context, string, qdrantx.ScrollOpts) (*qdrantx.ScrollResult, error) {
	return &qdrantx.ScrollResult{Points: v.scrollPoints}, nil
}

type rrfRecorder struct {
	logs     []incidentcontrol.RecallLog
	runbooks []incidentcontrol.Postmortem
	err      error
}

func (r *rrfRecorder) AppendRecallLogs(_ context.Context, logs []incidentcontrol.RecallLog) error {
	if r.err != nil {
		return r.err
	}
	r.logs = append(r.logs, logs...)
	return nil
}

func (r *rrfRecorder) ListRunbooks(context.Context, string, string, string) ([]incidentcontrol.Postmortem, error) {
	return r.runbooks, nil
}

func rrfPayload(id uint64, title, content string) map[string]any {
	return map[string]any{
		"id_alias":      float64(id),
		"source_type":   "manual",
		"title":         title,
		"content":       content,
		"tenant_scopes": []any{"tenant:opskeeper-demo"},
	}
}
