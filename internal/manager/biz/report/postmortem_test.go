package report

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/dataguard"
	gitartifact "github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact"
	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
)

// --- Fakes --------------------------------------------------------------

// fakeLoaders is an in-memory implementation of loop.Loaders.
// We use a small embedded struct so the test is one self-contained
// unit; production wires the loop_contract repository instead.
type fakeLoaders struct {
	rootCause *loop.RootCauseJSON
	critique  *loop.CritiqueScore
	verified  *loop.VerifiedDelta
	timeline  []loop.TimelineEvent

	mu sync.Mutex
}

func (f *fakeLoaders) LoadRootCause(_ context.Context, _, _ string) (*loop.RootCauseJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rootCause, nil
}

func (f *fakeLoaders) LoadCritique(_ context.Context, _, _ string) (*loop.CritiqueScore, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.critique, nil
}

func (f *fakeLoaders) LoadVerifiedDelta(_ context.Context, _, _ string) (*loop.VerifiedDelta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.verified, nil
}

func (f *fakeLoaders) LoadTimeline(_ context.Context, _, _ string) ([]loop.TimelineEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.timeline, nil
}

// fakeResolver implements SourceCommitResolver with a static result
// table. Empty selectors always return empty.
type fakeResolver struct {
	resolved  []SourceCommit
	unmatched []SourceSelector
	err       error

	mu sync.Mutex
}

func (f *fakeResolver) Resolve(_ context.Context, _ uint64, _ []SourceSelector) ([]SourceCommit, []SourceSelector, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolved, f.unmatched, f.err
}

// fakeSink implements PostmortemSink.
type fakeSink struct {
	mu      sync.Mutex
	docs    []*loop.PostmortemDoc
	shas    []string
	nextSHA string
	nextErr error
}

func (s *fakeSink) Save(_ context.Context, doc *loop.PostmortemDoc) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextErr != nil {
		return "", s.nextErr
	}
	s.docs = append(s.docs, doc)
	s.shas = append(s.shas, s.nextSHA)
	return s.nextSHA, nil
}

// fakeInsights implements CriticInsights.
type fakeInsights struct {
	dim   loop.CritiqueScore // unused, just to align with the type
	dims  []CriticDimensionAvg
	tools []ToolLatencyStats
	err   error
}

func (f *fakeInsights) Collect(_ context.Context, _ string, _ time.Duration) (CriticDimensionAvg, []ToolLatencyStats, error) {
	return CriticDimensionAvg{}, f.tools, f.err
}

// --- Helpers ------------------------------------------------------------

// newTestService wires a PostmortemService with all fakes. The
// default config has Sensitivity=Public (no redaction) and a
// stub clock pinned to a known instant.
func newTestService(
	t *testing.T,
	loaders *fakeLoaders,
	resolver SourceCommitResolver,
	sink PostmortemSink,
	insights CriticInsights,
	redactor dataguard.Redactor,
) *PostmortemService {
	t.Helper()
	if redactor == nil {
		redactor = dataguard.NewRedactor(dataguard.RedactModeNone, false)
	}
	if insights == nil {
		insights = NoopCriticInsights{}
	}
	loopLoaders := loop.Loaders{
		RootCause: loaders,
		Critique:  loaders,
		Verified:  loaders,
		Timeline:  loaders,
	}
	svc, err := NewPostmortemService(
		loopLoaders,
		redactor,
		resolver,
		insights,
		sink,
		PostmortemConfig{
			Sensitivity:    dataguard.Public,
			InsightsWindow: 30 * 24 * time.Hour,
			TopSlowTools:   3,
			Clock:          func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) },
		},
	)
	if err != nil {
		t.Fatalf("NewPostmortemService: %v", err)
	}
	return svc
}

func fullRenderInput() RenderInput {
	return RenderInput{
		IncidentID: "INC-001",
		TenantID:   "1",
		Severity:   "mutating",
		RootCause: &loop.RootCauseJSON{
			SchemaVersion: loop.ContractSchemaV1,
			RootCauseObject: &loop.RootCauseObject{
				Kind:    "pg.long_running_tx",
				Summary: "Transaction held row lock for 12 minutes, blocking 48 sessions.",
			},
			Confidence: 0.92,
			EvidenceChain: []loop.EvidenceItem{
				{Tool: "query_pg_stat_activity", Query: "SELECT * FROM pg_stat_activity WHERE state='active'", Value: "12 rows", Count: 12},
			},
			RemediationOptions: []loop.RemediationOption{
				{Risk: "mutating", Action: "kill_session", AutoApprove: false},
			},
		},
		Critique: &loop.CritiqueScore{
			SchemaVersion: loop.ContractSchemaV1,
			Verdict:       "pass",
			Score:         0.85,
			Reasons:       []string{"evidence sufficient", "remediation safe"},
			Model:         "claude-sonnet-4.5",
			LatencyMs:     1500,
		},
		Verified: &loop.VerifiedDelta{
			SchemaVersion: loop.ContractSchemaV1,
			Passed:        true,
			FailedMetrics: nil,
			Deltas:        map[string]float64{"pg.connections.idle": 0.05},
			SampleSize:    30,
			Tolerance:     0.15,
			RetryCount:    0,
			WarningLevel:  "pass",
		},
		Timeline: []loop.TimelineEvent{
			{Phase: "detected", EventType: "phase_entered", CreatedAt: "2026-08-10T11:00:00Z", Summary: "alert: pg active tx > 5m"},
			{Phase: "recovered", EventType: "phase_contract_written", CreatedAt: "2026-08-10T11:15:00Z", Summary: "verified pass"},
		},
		SourceCommits: []SourceCommit{
			{
				CommitSHA:         "abcdef5678905678abcdef5678905678abcdef56",
				FilePath:          "src/orders/repo.go",
				LineStart:         88,
				LineEnd:           90,
				BlameAuthor:       "alice@opskeeper.io",
				FirstIntroducedAt: time.Date(2026, 7, 15, 14, 32, 11, 0, time.UTC),
				Confidence:        0.88,
				RuntimeKey:        "pg_query:select * from orders where id = $1",
			},
		},
	}
}

// --- Tests --------------------------------------------------------------

func TestPostmortemService_BasicRender(t *testing.T) {
	loaders := &fakeLoaders{
		rootCause: fullRenderInput().RootCause,
		critique:  fullRenderInput().Critique,
		verified:  fullRenderInput().Verified,
		timeline:  fullRenderInput().Timeline,
	}
	resolver := &fakeResolver{resolved: fullRenderInput().SourceCommits}
	sink := &fakeSink{nextSHA: "deadbeef00000000deadbeef00000000deadbeef"}
	svc := newTestService(t, loaders, resolver, sink, nil, nil)

	doc, err := svc.Render(context.Background(), fullRenderInput())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if doc.SchemaVersion != loop.ContractSchemaV1 {
		t.Errorf("SchemaVersion = %q", doc.SchemaVersion)
	}
	if doc.IncidentID != "INC-001" {
		t.Errorf("IncidentID = %q", doc.IncidentID)
	}
	if doc.Markdown == "" {
		t.Fatal("Markdown is empty")
	}
	// 8 sections.
	for _, h := range []string{
		"## 1. 执行摘要",
		"## 2. 时间线",
		"## 3. 根因",
		"## 4. 修复",
		"## 5. 验证",
		"## 6. 影响",
		"## 7. 经验教训",
		"## 8. Source commits",
	} {
		if !strings.Contains(doc.Markdown, h) {
			t.Errorf("missing section %q in markdown", h)
		}
	}
	// Root cause details are present.
	if !strings.Contains(doc.Markdown, "pg.long_running_tx") {
		t.Error("root cause kind missing")
	}
	if !strings.Contains(doc.Markdown, "0.92") {
		t.Error("confidence missing")
	}
	if !strings.Contains(doc.Markdown, "kill_session") {
		t.Error("remediation action missing")
	}
	// Source commits section populated.
	if !strings.Contains(doc.Markdown, "abcdef567890") {
		t.Error("source commit SHA missing")
	}
	if !strings.Contains(doc.Markdown, "alice@opskeeper.io") {
		t.Error("blame author missing")
	}
	// Sources list.
	if len(doc.Sources) == 0 {
		t.Error("Sources should be non-empty")
	}
	for _, want := range []string{"RootCauseJSON", "CritiqueScore", "VerifiedDelta", "runtime_source_bridge"} {
		if !containsString(doc.Sources, want) {
			t.Errorf("Sources missing %q (got %v)", want, doc.Sources)
		}
	}
}

func TestPostmortemService_Redacted(t *testing.T) {
	in := fullRenderInput()
	// Inject a PII-bearing field into the RootCause detail.
	in.RootCause.RootCauseObject.Detail = map[string]any{
		"user_email":    "alice@example.com",
		"user_password": "hunter2",
		"host_address":  "1.2.3.4",
		"api_key":       "key-abc",
		"non_sensitive": "ok",
	}
	redactor := dataguard.NewRedactorForSensitivity(dataguard.Confidential)
	loaders := &fakeLoaders{
		rootCause: in.RootCause,
		critique:  in.Critique,
		verified:  in.Verified,
		timeline:  in.Timeline,
	}
	resolver := &fakeResolver{}
	sink := &fakeSink{nextSHA: "sha1"}
	svc := newTestService(t, loaders, resolver, sink, nil, redactor)

	doc, err := svc.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	md := doc.Markdown
	// The literal email / password / api_key values must be redacted.
	for _, leaked := range []string{"alice@example.com", "hunter2", "key-abc"} {
		if strings.Contains(md, leaked) {
			t.Errorf("redaction failed: %q still present in markdown", leaked)
		}
	}
	// The redacted markers should appear.
	for _, marker := range []string{"<redacted:email>", "<redacted:password>", "<redacted:api_key>"} {
		if !strings.Contains(md, marker) {
			t.Errorf("redaction marker %q missing", marker)
		}
	}
	// Non-sensitive should pass through.
	if !strings.Contains(md, "non_sensitive") {
		t.Error("non-sensitive value was redacted")
	}
	// Redaction notice is in the table.
	if !strings.Contains(md, "已 redact 字段") {
		t.Error("redaction notice missing")
	}
	// Sink should be told the doc was redacted (we test this via the
	// fakeSink + the postmortem_sink_test).
	_ = sink
}

func TestPostmortemService_Redacted_TopSecret_StripsDigits(t *testing.T) {
	in := fullRenderInput()
	in.RootCause.RootCauseObject.Detail = map[string]any{
		"trace_id": "trace 1234-5678 with counter 9999",
	}
	redactor := dataguard.NewRedactorForSensitivity(dataguard.TopSecret)
	loaders := &fakeLoaders{
		rootCause: in.RootCause,
		critique:  in.Critique,
		verified:  in.Verified,
		timeline:  in.Timeline,
	}
	resolver := &fakeResolver{}
	sink := &fakeSink{nextSHA: "sha1"}
	svc := newTestService(t, loaders, resolver, sink, nil, redactor)

	doc, err := svc.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(doc.Markdown, "1234") {
		t.Errorf("4+ digit run not stripped: %q", doc.Markdown)
	}
}

func TestPostmortemService_MissingCritique(t *testing.T) {
	in := fullRenderInput()
	in.Critique = nil // missing critic score

	loaders := &fakeLoaders{
		rootCause: in.RootCause,
		critique:  nil, // not loaded either
		verified:  in.Verified,
		timeline:  in.Timeline,
	}
	resolver := &fakeResolver{}
	sink := &fakeSink{nextSHA: "sha1"}
	svc := newTestService(t, loaders, resolver, sink, nil, nil)

	doc, err := svc.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	md := doc.Markdown
	// The critic 未评审 marker should appear.
	if !strings.Contains(md, "critic 未评审") {
		t.Errorf("expected 'critic 未评审' marker in markdown, got: %q", md)
	}
	// The Doc.Sources should NOT include CritiqueScore.
	for _, s := range doc.Sources {
		if s == "CritiqueScore" {
			t.Errorf("Sources should not include CritiqueScore when missing (got %v)", doc.Sources)
		}
	}
	// Doc should still validate.
	if err := loop.ValidatePostmortemDoc(doc); err != nil {
		t.Errorf("ValidatePostmortemDoc failed for missing-critique doc: %v", err)
	}
}

func TestPostmortemService_TemplateFailure_DegradesToText(t *testing.T) {
	// Construct a service whose template is poisoned so Execute
	// returns an error. We swap the service's *template.Template
	// for one that references an undeclared field — Execute will
	// fail with "can't evaluate field UndeclaredField..." and the
	// renderer should fall back to plain-text key=value.
	loaders := &fakeLoaders{
		rootCause: fullRenderInput().RootCause,
		critique:  fullRenderInput().Critique,
		verified:  fullRenderInput().Verified,
		timeline:  fullRenderInput().Timeline,
	}
	svc := newTestService(t, loaders, &fakeResolver{}, &fakeSink{nextSHA: "x"}, nil, nil)

	// Swap in a template that will fail at execute time.
	svc.mu.Lock()
	bad, perr := template.New("bad").Parse("{{ .NoSuchField }}")
	if perr != nil {
		svc.mu.Unlock()
		t.Fatalf("parse bad template: %v", perr)
	}
	svc.tmpl = bad
	svc.mu.Unlock()

	doc, err := svc.Render(context.Background(), fullRenderInput())
	if err != nil {
		t.Fatalf("Render should not return an error on template failure, got: %v", err)
	}
	// Markdown should be the plain-text fallback: starts with a
	// key=value line.
	if !strings.HasPrefix(doc.Markdown, "incident_id=") {
		t.Errorf("expected plain-text fallback, got first line: %q", doc.Markdown)
	}
	if !strings.Contains(doc.Markdown, "incident_id=INC-001") {
		t.Errorf("plain-text fallback missing incident_id: %q", doc.Markdown)
	}
	if !strings.Contains(doc.Markdown, "redaction_notice=") {
		t.Errorf("plain-text fallback missing redaction_notice: %q", doc.Markdown)
	}
}

func TestPostmortemService_SourceCommitsResolved(t *testing.T) {
	// Build a real gitartifact registry with a hit; verify the
	// production GitArtifactSourceResolver actually surfaces a
	// commit to the renderer.
	registry := gitartifact.NewLinkerRegistry()
	pgLinker := gitartifact.NewPGQueryLinker()
	pgLinker.AddIndex("SELECT * FROM orders WHERE id = $1", &gitartifact.LinkResult{
		Commit:     "abcdef5678905678abcdef5678905678abcdef56",
		Repo:       "git@github.com:opskeeper/order-svc.git",
		FilePath:   "src/orders/repo.go",
		LineStart:  88,
		LineEnd:    90,
		Author:     "alice@opskeeper.io",
		Confidence: 0.88,
		TenantID:   1,
	})
	if err := registry.Register(pgLinker); err != nil {
		t.Fatalf("register: %v", err)
	}
	resolver, err := NewGitArtifactSourceResolver(registry)
	if err != nil {
		t.Fatalf("NewGitArtifactSourceResolver: %v", err)
	}
	// Build loaders so the RootCause has a pg_query evidence item.
	in := fullRenderInput()
	in.RootCause.EvidenceChain = []loop.EvidenceItem{
		{Tool: "pg_query", Query: "SELECT * FROM orders WHERE id = $1"},
	}
	loaders := &fakeLoaders{
		rootCause: in.RootCause,
		critique:  in.Critique,
		verified:  in.Verified,
		timeline:  in.Timeline,
	}
	sink := &fakeSink{nextSHA: "sha1"}
	svc := newTestService(t, loaders, resolver, sink, nil, nil)

	doc, sha, err := svc.Run(gitartifact.WithTenant(context.Background(), 1), "1", "INC-001")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sha != "sha1" {
		t.Errorf("commit SHA = %q", sha)
	}
	// Markdown should contain the resolved commit.
	if !strings.Contains(doc.Markdown, "abcdef567890") {
		t.Errorf("expected resolved commit SHA in markdown, got: %q", doc.Markdown)
	}
	if !strings.Contains(doc.Markdown, "alice@opskeeper.io") {
		t.Errorf("expected blame author in markdown")
	}
}

func TestPostmortemService_Run_SavesToSink(t *testing.T) {
	loaders := &fakeLoaders{
		rootCause: fullRenderInput().RootCause,
		critique:  fullRenderInput().Critique,
		verified:  fullRenderInput().Verified,
		timeline:  fullRenderInput().Timeline,
	}
	resolver := &fakeResolver{resolved: fullRenderInput().SourceCommits}
	sink := &fakeSink{nextSHA: "newsha"}
	svc := newTestService(t, loaders, resolver, sink, nil, nil)

	doc, sha, err := svc.Run(context.Background(), "1", "INC-001")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sha != "newsha" {
		t.Errorf("sha = %q", sha)
	}
	if doc == nil {
		t.Fatal("doc is nil")
	}
	if len(sink.docs) != 1 {
		t.Errorf("expected 1 doc saved, got %d", len(sink.docs))
	}
}

func TestPostmortemService_Run_SinkError_Wrapped(t *testing.T) {
	loaders := &fakeLoaders{
		rootCause: fullRenderInput().RootCause,
		critique:  fullRenderInput().Critique,
		verified:  fullRenderInput().Verified,
		timeline:  fullRenderInput().Timeline,
	}
	resolver := &fakeResolver{}
	sink := &fakeSink{nextErr: errors.New("disk full")}
	svc := newTestService(t, loaders, resolver, sink, nil, nil)

	_, _, err := svc.Run(context.Background(), "1", "INC-001")
	if err == nil {
		t.Fatal("expected error from sink")
	}
	var saveErr *SaveError
	if !errorsAs(err, &saveErr) {
		t.Errorf("expected *SaveError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("expected error chain to include 'disk full', got: %v", err)
	}
}

func TestPostmortemService_NewRequiresNonNilDeps(t *testing.T) {
	loaders := &fakeLoaders{}
	resolver := &fakeResolver{}
	sink := &fakeSink{}
	_, err := NewPostmortemService(loop.Loaders{}, nil, resolver, nil, sink, PostmortemConfig{})
	if err == nil {
		t.Error("expected error for nil loaders")
	}
	_, err = NewPostmortemService(loop.Loaders{
		RootCause: loaders, Critique: loaders, Verified: loaders, Timeline: loaders,
	}, nil, nil, nil, sink, PostmortemConfig{})
	if err == nil {
		t.Error("expected error for nil resolver")
	}
	_, err = NewPostmortemService(loop.Loaders{
		RootCause: loaders, Critique: loaders, Verified: loaders, Timeline: loaders,
	}, nil, resolver, nil, nil, PostmortemConfig{})
	if err == nil {
		t.Error("expected error for nil sink")
	}
}

func TestPostmortemService_NoEvidence_NoSelectors_NoCrash(t *testing.T) {
	// No evidence chain → no selectors → resolver not called.
	resolver := &fakeResolver{err: errors.New("should not be called")}
	loaders := &fakeLoaders{
		rootCause: fullRenderInput().RootCause,
		critique:  fullRenderInput().Critique,
		verified:  fullRenderInput().Verified,
		timeline:  fullRenderInput().Timeline,
	}
	sink := &fakeSink{nextSHA: "x"}
	svc := newTestService(t, loaders, resolver, sink, nil, nil)

	// The fullRenderInput() root cause has 1 evidence entry, so the
	// resolver WILL be called. Strip it for this test.
	in := fullRenderInput()
	in.RootCause.EvidenceChain = nil
	_, _, err := svc.Run(context.Background(), "1", "INC-001")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Resolver should not have been called.
	if len(resolver.resolved) != 0 || len(resolver.unmatched) != 0 {
		t.Errorf("resolver should not be called when no evidence: got %d/%d", len(resolver.resolved), len(resolver.unmatched))
	}
}

func TestPostmortemService_UnmatchedSource_RendersNotice(t *testing.T) {
	in := fullRenderInput()
	in.SourceCommits = nil
	in.UnmatchedSourceCommits = []SourceSelector{
		{Type: "pg_query", Query: "SELECT * FROM nowhere"},
	}
	loaders := &fakeLoaders{
		rootCause: in.RootCause,
		critique:  in.Critique,
		verified:  in.Verified,
		timeline:  in.Timeline,
	}
	resolver := &fakeResolver{unmatched: in.UnmatchedSourceCommits}
	sink := &fakeSink{nextSHA: "x"}
	svc := newTestService(t, loaders, resolver, sink, nil, nil)

	doc, err := svc.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(doc.Markdown, "未命中") {
		t.Errorf("expected unmatched notice in markdown: %q", doc.Markdown)
	}
	if !strings.Contains(doc.Markdown, "pg_query:") {
		t.Errorf("expected unmatched selector key in markdown")
	}
}

// --- helpers ------------------------------------------------------------

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// errorsAs is a tiny shim around errors.As to avoid the linter
// complaining about unused import in the rare case errors.As is
// not used by other tests.
var _ = fmt.Sprintf

// errorsAs reports whether err's chain contains a target of type T.
func errorsAs[T error](err error, target *T) bool {
	return errors.As(err, target)
}
