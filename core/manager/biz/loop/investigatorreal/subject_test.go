package investigatorreal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	loop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

type stubLabels struct {
	labels map[string]string
	err    error
	calls  int
}

func (s *stubLabels) AlertLabels(_ context.Context, _ string) (map[string]string, error) {
	s.calls++
	return s.labels, s.err
}

func findSubject(t *testing.T, evidence []loop.EvidenceItem) *loop.EvidenceItem {
	t.Helper()
	for i := range evidence {
		if evidence[i].Tool == loop.SubjectEvidenceTool {
			return &evidence[i]
		}
	}
	return nil
}

func TestInvestigate_RecordsTheAlertSubjectWhenALabelsSourceIsWired(t *testing.T) {
	labels := &stubLabels{labels: map[string]string{"pod": "order-svc-7d9", "namespace": "prod"}}
	toolset := NewWithLabels(&fakeMetricQuerier{}, nil, labels, nil)

	evidence, err := toolset.Investigate(context.Background(), "k8s", "alert-17", loop.TimeWindow{})
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	subject := findSubject(t, evidence)
	if subject == nil {
		t.Fatalf("no %s item in the chain: %#v", loop.SubjectEvidenceTool, evidence)
	}
	if labels.calls != 1 {
		t.Errorf("the labels source was called %d times, want once", labels.calls)
	}
	// The recorded subject must be the one the resolvers can act on.
	decoded, ok := decodeForTest(subject.Value)
	if !ok || decoded["pod"] != "order-svc-7d9" {
		t.Fatalf("subject labels = %#v", subject.Value)
	}
}

func TestInvestigate_OmitsTheSubjectWhenNoLabelsSourceIsWired(t *testing.T) {
	toolset := New(&fakeMetricQuerier{}, nil, nil)
	evidence, err := toolset.Investigate(context.Background(), "k8s", "alert-17", loop.TimeWindow{})
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if subject := findSubject(t, evidence); subject != nil {
		t.Fatal("a toolset with no labels source must not invent a subject")
	}
}

// A lookup failure must not block the investigation. The incident is still
// investigable on the resource_alert item alone, and the resolvers will
// refuse the write actions with a message naming the missing subject.
func TestInvestigate_SurvivesALabelsLookupFailure(t *testing.T) {
	toolset := NewWithLabels(&fakeMetricQuerier{}, nil, &stubLabels{err: errors.New("alert store down")}, nil)
	evidence, err := toolset.Investigate(context.Background(), "k8s", "alert-17", loop.TimeWindow{})
	if err != nil {
		t.Fatalf("a labels failure must not fail the investigation: %v", err)
	}
	if len(evidence) == 0 {
		t.Fatal("the resource_alert item must still be recorded")
	}
	if subject := findSubject(t, evidence); subject != nil {
		t.Fatal("a failed lookup must not produce a subject")
	}
}

func TestInvestigate_OmitsTheSubjectWhenTheAlertCarriesNoLabels(t *testing.T) {
	toolset := NewWithLabels(&fakeMetricQuerier{}, nil, &stubLabels{labels: map[string]string{}}, nil)
	evidence, err := toolset.Investigate(context.Background(), "k8s", "alert-17", loop.TimeWindow{})
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if subject := findSubject(t, evidence); subject != nil {
		t.Fatal("an alert with no labels has no subject to record")
	}
}

// decodeForTest reads the labels back out of the recorded item, through the
// same JSON round trip a persisted contract takes, so the test asserts on the
// stored shape rather than on the constructor's arguments.
func decodeForTest(value any) (map[string]string, bool) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var probe struct {
		Labels map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, false
	}
	return probe.Labels, len(probe.Labels) > 0
}
