package systemhealth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

type fakeDB struct{ err error }

func (f fakeDB) PingContext(context.Context) error { return f.err }

type fakeProm struct{ err error }

func (f fakeProm) Query(context.Context, string, time.Time) (any, error) { return nil, f.err }

type fakeProbe struct{ err error }

func (f fakeProbe) Probe(context.Context) error { return f.err }

type fakeGrafana struct{ err error }

func (f fakeGrafana) Test(context.Context) error { return f.err }

// fakeRules answers in counts, the way the port asks. It used to hand back
// alert's own Rule rows, which meant this file imported the alert domain to
// describe a fixture; the projection is what the probe actually consumes, so
// the fixture describes that instead.
type fakeRules struct {
	total   int
	enabled int
	err     error
}

func (f fakeRules) CountRules(context.Context) (int, int, error) {
	return f.total, f.enabled, f.err
}

type fakeIncidents struct {
	count int64
	err   error
}

func (f fakeIncidents) CountOpenIncidents(context.Context) (int64, error) {
	return f.count, f.err
}

type fakeEdges struct {
	edges []domain.EdgePresence
	err   error
}

func (f fakeEdges) ListPresence(context.Context, int) ([]domain.EdgePresence, error) {
	return f.edges, f.err
}

func TestCheckAggregatesFailedDependency(t *testing.T) {
	t.Parallel()
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/collections/opskeeper_knowledge" {
			t.Fatalf("qdrant path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(qdrant.Close)

	svc := New(Config{
		Version:             "v-test",
		ProbeTimeout:        time.Second,
		PromEnabled:         true,
		LogsEnabled:         true,
		TracesEnabled:       true,
		AlertEnabled:        true,
		EvaluatorInterval:   5 * time.Minute,
		NotifyCooldown:      10 * time.Minute,
		FrontierAddr:        "frontier:40011",
		LLMConfigured:       true,
		EmbeddingConfigured: true,
		QdrantURL:           qdrant.URL,
		QdrantCollection:    "opskeeper_knowledge",
	}, Dependencies{
		DB:        fakeDB{},
		Prom:      fakeProm{err: errors.New("prom down")},
		Grafana:   fakeGrafana{},
		Loki:      fakeProbe{},
		Tempo:     fakeProbe{},
		Rules:     fakeRules{total: 1, enabled: 1},
		Incidents: fakeIncidents{},
		Edges: fakeEdges{edges: []domain.EdgePresence{
			{ID: 1, Status: domain.EdgeStatusOnline},
		}},
	})

	report, err := svc.Check(context.Background())
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if report.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", report.Status, StatusFailed)
	}
	if report.Summary.Failed != 1 {
		t.Fatalf("failed count = %d, want 1", report.Summary.Failed)
	}
	prom := findCheck(report, "prometheus")
	if prom == nil || prom.Status != StatusFailed {
		t.Fatalf("prometheus check = %+v, want failed", prom)
	}
	qdrantCheck := findCheck(report, "qdrant")
	if qdrantCheck == nil || qdrantCheck.Status != StatusOK {
		t.Fatalf("qdrant check = %+v, want ok", qdrantCheck)
	}
}

func TestCheckReportsDegradedWhenOptionalCapabilitiesMissing(t *testing.T) {
	t.Parallel()
	svc := New(Config{
		AlertEnabled:        true,
		FrontierDisabled:    true,
		LLMConfigured:       false,
		EmbeddingConfigured: false,
	}, Dependencies{
		DB:        fakeDB{},
		Rules:     fakeRules{total: 1, enabled: 1},
		Incidents: fakeIncidents{},
		Edges:     fakeEdges{},
	})

	report, err := svc.Check(context.Background())
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if report.Status != StatusDegraded {
		t.Fatalf("status = %q, want %q", report.Status, StatusDegraded)
	}
	if report.Summary.Degraded == 0 {
		t.Fatalf("degraded count = 0, want > 0")
	}
}

func TestCheckReportsGrafanaMissingCredentialAsDegraded(t *testing.T) {
	t.Parallel()
	qdrant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/collections/opskeeper_knowledge" {
			t.Fatalf("qdrant path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(qdrant.Close)

	svc := New(Config{
		Version:             "v-test",
		ProbeTimeout:        time.Second,
		PromEnabled:         true,
		LogsEnabled:         true,
		TracesEnabled:       true,
		AlertEnabled:        true,
		EvaluatorInterval:   5 * time.Minute,
		NotifyCooldown:      10 * time.Minute,
		FrontierAddr:        "frontier:40011",
		LLMConfigured:       true,
		EmbeddingConfigured: true,
		QdrantURL:           qdrant.URL,
		QdrantCollection:    "opskeeper_knowledge",
	}, Dependencies{
		DB:        fakeDB{},
		Prom:      fakeProm{},
		Grafana:   fakeGrafana{err: errors.New("grafana: sa_token / api_key empty (create a Grafana service account and paste its token, or paste an api_key for external Grafana)")},
		Loki:      fakeProbe{},
		Tempo:     fakeProbe{},
		Rules:     fakeRules{total: 1, enabled: 1},
		Incidents: fakeIncidents{},
		Edges: fakeEdges{edges: []domain.EdgePresence{
			{ID: 1, Status: domain.EdgeStatusOnline},
		}},
	})

	report, err := svc.Check(context.Background())
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if report.Status != StatusDegraded {
		t.Fatalf("status = %q, want %q", report.Status, StatusDegraded)
	}
	if report.Summary.Failed != 0 {
		t.Fatalf("failed count = %d, want 0", report.Summary.Failed)
	}
	grafana := findCheck(report, "grafana")
	if grafana == nil || grafana.Status != StatusDegraded {
		t.Fatalf("grafana check = %+v, want degraded", grafana)
	}
}

func TestCheckEdgesReportsAccessStateSeparatelyFromPlatformHealth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		edges   []domain.EdgePresence
		want    Status
		message string
	}{
		{
			name:    "no registered edge is ok",
			edges:   nil,
			want:    StatusOK,
			message: "no edge agent is registered; edge access is ready",
		},
		{
			name: "all sampled edges offline is failed",
			edges: []domain.EdgePresence{
				{ID: 1, Status: domain.EdgeStatusOffline},
				{ID: 2, Status: domain.EdgeStatusOffline},
			},
			want:    StatusFailed,
			message: "all sampled edge agents are offline",
		},
		{
			name: "partial offline is degraded",
			edges: []domain.EdgePresence{
				{ID: 1, Status: domain.EdgeStatusOnline},
				{ID: 2, Status: domain.EdgeStatusOffline},
			},
			want:    StatusDegraded,
			message: "1 sampled edge agent(s) are offline",
		},
		{
			name: "all online is ok",
			edges: []domain.EdgePresence{
				{ID: 1, Status: domain.EdgeStatusOnline},
				{ID: 2, Status: domain.EdgeStatusOnline},
			},
			want:    StatusOK,
			message: "sampled edge agents are online",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(Config{}, Dependencies{Edges: fakeEdges{edges: tc.edges}})
			check := svc.checkEdges(context.Background())
			if check.Status != tc.want {
				t.Fatalf("status = %q, want %q", check.Status, tc.want)
			}
			if check.Message != tc.message {
				t.Fatalf("message = %q, want %q", check.Message, tc.message)
			}
		})
	}
}

func findCheck(report *Report, id string) *Check {
	for i := range report.Checks {
		if report.Checks[i].ID == id {
			return &report.Checks[i]
		}
	}
	return nil
}
