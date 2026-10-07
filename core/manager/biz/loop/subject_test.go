package loop

import (
	"context"
	"strings"
	"testing"
)

// subjectCause wraps a recorded subject the way a real contract carries it.
func subjectCause(labels map[string]string) *RootCauseJSON {
	return causeWithEvidence(NewSubjectEvidenceItem("alert-17", "k8s:alert-17", labels))
}

func resolveSubject(t *testing.T, action string, required []string, labels map[string]string) (map[string]any, error) {
	t.Helper()
	r := EvidenceArgResolver{Causes: scriptedRootCauseLoader{rc: subjectCause(labels)}}
	return r.Resolve(context.Background(), RemediationRequest{
		IncidentID: "inc-1", TenantID: "t-1",
		Option: opt(action, "mutating", false),
	}, ToolSpec{Name: action, RequiredArgs: required})
}

func TestSubjectExtractors_ResolveEachRecordedEntity(t *testing.T) {
	cases := []struct {
		action   string
		required []string
		labels   map[string]string
		wantKey  string
		wantVal  any
	}{
		{"k8s.evict_pod", []string{"pod"}, map[string]string{"pod": "order-svc-7d9"}, "pod", "order-svc-7d9"},
		{"k8s.rolling_restart", []string{"deployment"}, map[string]string{"deployment": "order-svc"}, "deployment", "order-svc"},
		{"k8s.scale", []string{"deployment", "replicas"}, map[string]string{"deployment": "order-svc", "replicas": "3"}, "deployment", "order-svc"},
		{"mq.drain_queue", []string{"queue"}, map[string]string{"queue": "payments-in"}, "queue", "payments-in"},
		{"mq.replay_messages", []string{"queue"}, map[string]string{"queue": "payments-in"}, "queue", "payments-in"},
		{"host.restart_service", []string{"unit"}, map[string]string{"unit": "nginx.service"}, "unit", "nginx.service"},
		{"redis.client_kill", []string{"addr"}, map[string]string{"addr": "10.0.0.4:52310"}, "addr", "10.0.0.4:52310"},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			args, err := resolveSubject(t, tc.action, tc.required, tc.labels)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got := args[tc.wantKey]; got != tc.wantVal {
				t.Fatalf("%s = %#v, want %#v", tc.wantKey, got, tc.wantVal)
			}
		})
	}
}

func TestSubjectExtractors_ScaleResolvesTheReplicaCount(t *testing.T) {
	args, err := resolveSubject(t, "k8s.scale", []string{"deployment", "replicas"},
		map[string]string{"deployment": "order-svc", "replicas": "5"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, ok := args["replicas"].(int); !ok || got != 5 {
		t.Fatalf("replicas = %#v, want int 5", args["replicas"])
	}
}

// The rule that matters: an action whose subject was never recorded must
// refuse with a message that says what to record, not merely that something
// is missing.
func TestSubjectExtractors_RefuseWhenTheSubjectWasNeverRecorded(t *testing.T) {
	r := EvidenceArgResolver{Causes: scriptedRootCauseLoader{
		rc: causeWithEvidence(EvidenceItem{Tool: "query_promql", Value: `[{"value":"3"}]`}),
	}}
	_, err := r.Resolve(context.Background(), RemediationRequest{
		IncidentID: "inc-1", Option: opt("k8s.evict_pod", "mutating", false),
	}, ToolSpec{Name: "k8s.evict_pod", RequiredArgs: []string{"pod"}})
	if err == nil {
		t.Fatal("a metric-only evidence chain cannot name a pod")
	}
	for _, want := range []string{"pod", SubjectEvidenceTool} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must mention %q: %q", want, err.Error())
		}
	}
}

// Two labels naming two different pods is a contradiction. Acting on either
// is a guess, so it refuses and says which values conflicted.
func TestSubjectExtractors_RefuseWhenTwoLabelsNameDifferentObjects(t *testing.T) {
	_, err := resolveSubject(t, "k8s.evict_pod", []string{"pod"},
		map[string]string{"pod": "order-svc-a", "k8s_pod": "order-svc-b"})
	if err == nil {
		t.Fatal("two labels naming different pods must not resolve to one")
	}
	for _, want := range []string{"order-svc-a", "order-svc-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name both objects; %q does not mention %q", err.Error(), want)
		}
	}
}

// The same object under two spellings is normal, not a contradiction.
func TestSubjectExtractors_CollapseIdenticalValuesAcrossAliases(t *testing.T) {
	args, err := resolveSubject(t, "k8s.evict_pod", []string{"pod"},
		map[string]string{"pod": "order-svc-7d9", "k8s_pod": "order-svc-7d9"})
	if err != nil {
		t.Fatalf("two labels naming the same pod are not ambiguous: %v", err)
	}
	if got := args["pod"]; got != "order-svc-7d9" {
		t.Fatalf("pod = %#v", got)
	}
}

// A replica count is a decision the evidence either records or does not. A
// label that is not a number is not a replica count, and scaling to it would
// be inventing a target.
func TestSubjectExtractors_RefuseANonNumericReplicaCount(t *testing.T) {
	_, err := resolveSubject(t, "k8s.scale", []string{"deployment", "replicas"},
		map[string]string{"deployment": "order-svc", "replicas": "unknown"})
	if err == nil {
		t.Fatal("a non-numeric replica label must not be scaled to")
	}
	if !strings.Contains(err.Error(), "replica count") {
		t.Errorf("the refusal must say the value is not a replica count: %q", err.Error())
	}
}

// Scale needs both arguments; a subject that names the deployment but not the
// replica count is still incomplete.
func TestSubjectExtractors_RefuseScaleWhenOnlyTheDeploymentIsRecorded(t *testing.T) {
	_, err := resolveSubject(t, "k8s.scale", []string{"deployment", "replicas"},
		map[string]string{"deployment": "order-svc"})
	if err == nil {
		t.Fatal("a subject without a replica count cannot complete k8s.scale")
	}
	if !strings.Contains(err.Error(), "replicas") {
		t.Errorf("the refusal must name the missing argument: %q", err.Error())
	}
}

// An unrelated label must not become an argument. This is the property that
// keeps the alias lists from becoming an injection surface.
func TestSubjectExtractors_DoNotReadLabelsOutsideTheirOwnField(t *testing.T) {
	// redis.client_kill must not accept a queue label as an address.
	_, err := resolveSubject(t, "redis.client_kill", []string{"addr"},
		map[string]string{"queue": "payments-in"})
	if err == nil {
		t.Fatal("a queue label is not a redis client address")
	}
	if !strings.Contains(err.Error(), "addr") {
		t.Errorf("the refusal must name the argument it could not find: %q", err.Error())
	}
}

// The contract round-trips evidence through JSON, so the subject arrives as
// raw JSON in production. A resolver that only understood the typed map would
// fail on exactly that path.
func TestSubjectLabels_SurviveAJSONRoundTrip(t *testing.T) {
	r := EvidenceArgResolver{Causes: scriptedRootCauseLoader{rc: causeWithEvidence(EvidenceItem{
		Tool:  SubjectEvidenceTool,
		Value: `{"alert_id":"alert-17","labels":{"pod":"order-svc-7d9","deployment":"order-svc"}}`,
	})}}
	args, err := r.Resolve(context.Background(), RemediationRequest{
		Option: opt("k8s.evict_pod", "mutating", false),
	}, ToolSpec{Name: "k8s.evict_pod", RequiredArgs: []string{"pod"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := args["pod"]; got != "order-svc-7d9" {
		t.Fatalf("pod = %#v", got)
	}
}

func TestNewSubjectEvidenceItem_DropsBlankLabels(t *testing.T) {
	item := NewSubjectEvidenceItem("a", "k8s:a", map[string]string{
		"pod": "order-svc-7d9", "": "no-key", "blank": "   ", "keep": "yes",
	})
	labels, ok := decodeLabelMap(item.Value)
	if !ok {
		t.Fatal("a subject with real labels must decode")
	}
	if _, present := labels["blank"]; present {
		t.Error("a blank label value must not be recorded")
	}
	if len(labels) != 2 {
		t.Fatalf("labels = %#v, want exactly the two non-blank keyed labels", labels)
	}
}

// The declared vocabulary is a claim the evaluation gate reads. All nine
// actions that a RemediationOption cannot carry arguments for must now have
// somewhere to get them.
func TestActionsResolvableFromEvidence_CoversEverySubjectBackedAction(t *testing.T) {
	declared := map[string]struct{}{}
	for _, action := range ActionsResolvableFromEvidence() {
		declared[action] = struct{}{}
	}
	for _, want := range []string{
		"pg.kill_session", "pg.connection_pause",
		"k8s.evict_pod", "k8s.rolling_restart", "k8s.scale",
		"mq.drain_queue", "mq.replay_messages",
		"host.restart_service", "redis.client_kill",
	} {
		if _, ok := declared[want]; !ok {
			t.Errorf("%s has no evidence-backed extractor, so its dispatch can never complete", want)
		}
	}
}
