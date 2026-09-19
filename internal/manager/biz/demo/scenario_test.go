package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	incidentcontrol "github.com/vincent-wuhan/opskeeper/internal/control/incident"
	repairpreview "github.com/vincent-wuhan/opskeeper/internal/control/repairpreview"
	alertmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/alert"
	demomodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/demo"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

type fakeIncidents struct {
	nextID uint64
	rows   map[string]*alertmodel.Incident
	byID   map[uint64]*alertmodel.Incident
	order  *[]string
	events []*alertmodel.Event
}

func newFakeIncidents() *fakeIncidents {
	var order []string
	return &fakeIncidents{
		nextID: 100, rows: map[string]*alertmodel.Incident{}, byID: map[uint64]*alertmodel.Incident{}, order: &order,
	}
}

func (f *fakeIncidents) GetIncidentByDedupeKey(_ context.Context, key string) (*alertmodel.Incident, error) {
	if incident, ok := f.rows[key]; ok {
		return incident, nil
	}
	return nil, errs.ErrNotFound
}

func (f *fakeIncidents) GetIncidentByID(_ context.Context, id uint64) (*alertmodel.Incident, error) {
	if incident, ok := f.byID[id]; ok {
		return incident, nil
	}
	return nil, errs.ErrNotFound
}

func (f *fakeIncidents) CreateIncident(_ context.Context, incident *alertmodel.Incident) error {
	*f.order = append(*f.order, "incident")
	incident.ID = f.nextID
	f.nextID++
	f.rows[incident.DedupeKey] = incident
	f.byID[incident.ID] = incident
	return nil
}

func (f *fakeIncidents) CreateEvent(_ context.Context, event *alertmodel.Event) error {
	*f.order = append(*f.order, "event")
	f.events = append(f.events, event)
	return nil
}

func (f *fakeIncidents) ListEventsByIncident(_ context.Context, incidentID uint64, limit int) ([]*alertmodel.Event, error) {
	if limit <= 0 || incidentID == 0 {
		return nil, nil
	}
	events := make([]*alertmodel.Event, 0, len(f.events))
	for _, event := range f.events {
		if event.IncidentID == incidentID {
			events = append(events, event)
		}
	}
	return events, nil
}

type fakeScenarios struct {
	nextID         uint64
	rows           map[string]*demomodel.ScenarioRun
	events         []*alertmodel.Event
	failEventWrite bool
	expiredRows    []demomodel.ScenarioRun
}

func newFakeScenarios() *fakeScenarios {
	return &fakeScenarios{nextID: 10, rows: map[string]*demomodel.ScenarioRun{}}
}

func (f *fakeScenarios) CreateOrUpdate(_ context.Context, run *demomodel.ScenarioRun) error {
	key := fmt.Sprintf("%d/%s/%s", run.TenantID, run.ScenarioID, run.IdempotencyKey)
	if existing, ok := f.rows[key]; ok {
		if existing.TargetFingerprint != run.TargetFingerprint {
			return errs.ErrConflict
		}
		run.ID = existing.ID
		run.UpdatedAt = time.Now().UTC()
	} else {
		run.ID = f.nextID
		f.nextID++
		run.UpdatedAt = time.Now().UTC()
	}
	copied := *run
	f.rows[key] = &copied
	return nil
}

func (f *fakeScenarios) GetByIdempotencyKey(_ context.Context, tenantID uint64, scenarioID, key string) (*demomodel.ScenarioRun, error) {
	row, ok := f.rows[fmt.Sprintf("%d/%s/%s", tenantID, scenarioID, key)]
	if !ok {
		return nil, errs.ErrNotFound
	}
	return row, nil
}

func (f *fakeScenarios) GetByIncident(_ context.Context, tenantID uint64, scenarioID string, incidentID uint64) (*demomodel.ScenarioRun, error) {
	for _, row := range f.rows {
		if row.TenantID == tenantID && row.ScenarioID == scenarioID && row.IncidentID == incidentID {
			return row, nil
		}
	}
	return nil, errs.ErrNotFound
}

func (f *fakeScenarios) ListExpiredAwaitingApproval(_ context.Context, now time.Time, limit int) ([]demomodel.ScenarioRun, error) {
	rows := make([]demomodel.ScenarioRun, 0, len(f.expiredRows))
	for _, row := range f.expiredRows {
		if row.Status == demomodel.ScenarioStatusAwaitingApproval && !row.ExpiresAt.After(now) {
			rows = append(rows, row)
		}
		if limit > 0 && len(rows) >= limit {
			break
		}
	}
	return rows, nil
}

func (f *fakeFixtures) Recover(_ context.Context, manifestID, reason string) error {
	f.recoverCalls++
	f.lastManifest = manifestID
	f.lastReason = reason
	if manifestID == "" || reason == "" {
		return errs.ErrInvalid
	}
	if f.recoverErr != nil {
		return f.recoverErr
	}
	return nil
}

func (f *fakeScenarios) UpdateStatus(_ context.Context, id uint64, status string, mutation func(*demomodel.ScenarioRun) error) error {
	for _, row := range f.rows {
		if row.ID != id {
			continue
		}
		row.Status = status
		if mutation != nil {
			if err := mutation(row); err != nil {
				return err
			}
		}
		row.UpdatedAt = time.Now().UTC()
		return nil
	}
	return errs.ErrNotFound
}

func (f *fakeScenarios) UpdateStatusWithEvent(
	_ context.Context, id uint64, status string, event *alertmodel.Event, allowedCurrent ...string,
) error {
	for _, row := range f.rows {
		if row.ID != id {
			continue
		}
		allowed := len(allowedCurrent) == 0
		for _, current := range allowedCurrent {
			if current == row.Status {
				allowed = true
			}
		}
		if !allowed {
			return errs.ErrConflict
		}
		if f.failEventWrite {
			return errors.New("event write failed")
		}
		previous := row.Status
		row.Status = status
		if event != nil {
			event.StatusAfter = alertmodel.IncidentStatusOpen
			f.events = append(f.events, event)
		}
		_ = previous
		row.UpdatedAt = time.Now().UTC()
		return nil
	}
	return errs.ErrNotFound
}

type fakeFixtures struct {
	starts       int
	business     int
	fails        bool
	recoverErr   error
	state        string
	order        *[]string
	lastStart    FixtureStartInput
	recoverCalls int
	lastManifest string
	lastReason   string
	statusCalls  int
}

type fakePreviewRepository struct {
	runs          []repairpreview.Run
	eligibleCalls int
}

type fakePreviewExecutor struct {
	calls     int
	previews  *fakePreviewRepository
	lastInput PreviewExecutionInput
}

type fakeWorkflowPublisher struct {
	stages []string
	fails  bool
}

type fakeArchiveWriter struct {
	events []incidentcontrol.Event
}

func (writer *fakeArchiveWriter) Append(_ context.Context, event incidentcontrol.Event) error {
	writer.events = append(writer.events, event)
	return nil
}

func (f *fakeWorkflowPublisher) PublishWorkflow(_ context.Context, _ *demomodel.ScenarioRun, stage string, _ *PreviewDecisionSummary) error {
	f.stages = append(f.stages, stage)
	if f.fails {
		return errors.New("matrix unavailable")
	}
	return nil
}

func (f *fakePreviewRepository) ListByIncident(
	_ context.Context, _, _ string, _ int,
) ([]repairpreview.Run, error) {
	return f.runs, nil
}

func (f *fakePreviewRepository) FindEligible(
	_ context.Context, _, _, runID, candidateID, action string,
) (repairpreview.Candidate, error) {
	f.eligibleCalls++
	for _, run := range f.runs {
		for _, candidate := range run.Candidates {
			if candidate.RunID == runID && candidate.CandidateID == candidateID &&
				candidate.Action == action && candidate.Decision == repairpreview.DecisionPass {
				return candidate, nil
			}
		}
	}
	return repairpreview.Candidate{}, repairpreview.ErrCandidateNotFound
}

func (f *fakePreviewRepository) Save(_ context.Context, run repairpreview.Run) error {
	f.runs = append(f.runs, run)
	return nil
}

func (f *fakePreviewExecutor) Execute(_ context.Context, input PreviewExecutionInput) error {
	f.calls++
	f.lastInput = input
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	passing.RunID = input.RunID
	rejected := previewCandidate("candidate-b", "reset_pool", repairpreview.DecisionReject)
	rejected.RunID = input.RunID
	run := previewRun(baseline, passing, rejected)
	run.ID = input.RunID
	run.TenantID = input.TenantID
	run.IncidentID = input.IncidentID
	run.ScenarioID = input.ScenarioID
	run.IdempotencyKey = input.IdempotencyKey
	run.TargetFingerprint = input.TargetFingerprint
	run.BindingFingerprint = repairpreview.WorkloadBinding{
		RunID: input.RunID, TenantID: input.TenantID, IncidentID: input.IncidentID,
		ScenarioID: input.ScenarioID, IdempotencyKey: input.IdempotencyKey,
		TargetFingerprint: input.TargetFingerprint,
	}.Fingerprint()
	f.previews.runs = append(f.previews.runs, run)
	return nil
}

func TestPreviewExecutionUsesArchiveTenant(t *testing.T) {
	previews := &fakePreviewRepository{}
	usecase, input, incidents, scenarios := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")
	run, err := scenarios.GetByIdempotencyKey(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = demomodel.ScenarioStatusAlertCorrelated
	incidents.events = append(incidents.events, &alertmodel.Event{
		IncidentID: run.IncidentID, EventType: alertmodel.EventTypeAIInitialDiagnosis,
	})
	executor := &fakePreviewExecutor{previews: previews}
	usecase.SetPreviewExecutor(executor)
	usecase.SetArchiveWriter(&fakeArchiveWriter{}, "goai-demo")

	status, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusAwaitingApproval ||
		executor.lastInput.TenantID != "goai-demo" ||
		previews.runs[len(previews.runs)-1].TenantID != "goai-demo" {
		t.Fatalf("status = %+v input = %+v", status, executor.lastInput)
	}
}

func (f *fakeFixtures) Start(_ context.Context, input FixtureStartInput) (FixtureStartResult, error) {
	f.starts++
	f.lastStart = input
	if f.order != nil {
		*f.order = append(*f.order, "fixture")
	}
	if f.fails {
		return FixtureStartResult{}, &FixtureError{HTTPStatus: http.StatusServiceUnavailable}
	}
	return FixtureStartResult{ManifestID: fmt.Sprintf("manifest-%d", f.starts)}, nil
}

func (f *fakeFixtures) Status(_ context.Context, _ string) (FixtureStatus, error) {
	f.statusCalls++
	state := f.state
	if state == "" {
		state = "running"
	}
	return FixtureStatus{State: state}, nil
}

func (f *fakeFixtures) BusinessSnapshot(_ context.Context, _ string) (json.RawMessage, error) {
	f.business++
	if f.fails {
		return nil, &FixtureError{HTTPStatus: http.StatusServiceUnavailable, Code: "pool_exhausted"}
	}
	return json.RawMessage(`{"section":"orders"}`), nil
}

func TestBaselineBusinessSnapshotDoesNotRequireScenario(t *testing.T) {
	fixtures := &fakeFixtures{}
	usecase := NewUsecase(newFakeScenarios(), newFakeIncidents(), fixtures)
	data, err := usecase.BusinessSnapshotBaseline(context.Background(), "orders")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"section":"orders"}` || fixtures.business != 1 || fixtures.starts != 0 {
		t.Fatalf("data = %s business = %d starts = %d", data, fixtures.business, fixtures.starts)
	}
	if _, err := usecase.BusinessSnapshotBaseline(context.Background(), "unknown"); err == nil {
		t.Fatal("expected invalid section")
	}
}

func validInput() StartScenarioInput {
	return StartScenarioInput{
		IdempotencyKey: "final-demo-key", ScenarioID: ScenarioID, Target: ScenarioTarget,
		TargetFingerprint: "0123456789abcdef", AlertFingerprint: "fedcba9876543210", DurationSeconds: 90,
	}
}

func TestStartCreatesIncidentBeforeFixture(t *testing.T) {
	incidents := newFakeIncidents()
	fixtures := &fakeFixtures{order: incidents.order}
	usecase := NewUsecase(newFakeScenarios(), incidents, fixtures)
	status, err := usecase.Start(context.Background(), 1, validInput())
	if err != nil {
		t.Fatal(err)
	}
	if order := *incidents.order; len(order) != 3 || order[0] != "incident" || order[1] != "fixture" || order[2] != "event" {
		t.Fatalf("order = %v", order)
	}
	if fixtures.starts != 1 || status.PoolManifestID != "manifest-1" || status.Status != demomodel.ScenarioStatusAwaitingAlert {
		t.Fatalf("status = %+v fixture starts = %d", status, fixtures.starts)
	}
	if len(incidents.events) != 1 || strings.Contains(incidents.events[0].SnapshotJSON, "token") {
		t.Fatalf("event = %+v", incidents.events[0])
	}
}

func TestStartUsesFinalDemoPoolCapacity(t *testing.T) {
	fixtures := &fakeFixtures{}
	usecase := NewUsecase(newFakeScenarios(), newFakeIncidents(), fixtures)
	if _, err := usecase.Start(context.Background(), 1, validInput()); err != nil {
		t.Fatal(err)
	}
	if fixtures.lastStart.InitialCapacity != 4 || fixtures.lastStart.TargetCapacity != 8 {
		t.Fatalf(
			"capacity = %d/%d, want initial 4 and recovered 8",
			fixtures.lastStart.InitialCapacity,
			fixtures.lastStart.TargetCapacity,
		)
	}
}

func TestRepeatedStartReturnsSameIncident(t *testing.T) {
	fixtures := &fakeFixtures{}
	usecase := NewUsecase(newFakeScenarios(), newFakeIncidents(), fixtures)
	first, err := usecase.Start(context.Background(), 1, validInput())
	if err != nil {
		t.Fatal(err)
	}
	second, err := usecase.Start(context.Background(), 1, validInput())
	if err != nil {
		t.Fatal(err)
	}
	if first.IncidentID != second.IncidentID || fixtures.starts != 1 {
		t.Fatalf("first = %+v second = %+v starts = %d", first, second, fixtures.starts)
	}
}

func TestStartValidatesAllowlistedTargetAndDuration(t *testing.T) {
	usecase := NewUsecase(newFakeScenarios(), newFakeIncidents(), &fakeFixtures{})
	for name, mutate := range map[string]func(*StartScenarioInput){
		"scenario":  func(in *StartScenarioInput) { in.ScenarioID = "cpu" },
		"target":    func(in *StartScenarioInput) { in.Target = "redis" },
		"duration":  func(in *StartScenarioInput) { in.DurationSeconds = 59 },
		"idempower": func(in *StartScenarioInput) { in.IdempotencyKey = "short" },
		"fingerpr":  func(in *StartScenarioInput) { in.TargetFingerprint = "not-hex" },
	} {
		input := validInput()
		mutate(&input)
		if _, err := usecase.Start(context.Background(), 1, input); !errors.Is(err, errs.ErrInvalid) {
			t.Fatalf("%s err = %v", name, err)
		}
	}
}

func TestFixtureStartFailureLeavesRestartableScenario(t *testing.T) {
	incidents := newFakeIncidents()
	fixtures := &fakeFixtures{fails: true}
	scenarios := newFakeScenarios()
	usecase := NewUsecase(scenarios, incidents, fixtures)
	input := validInput()
	if _, err := usecase.Start(context.Background(), 1, input); err == nil {
		t.Fatal("first start should fail")
	}
	run, err := scenarios.GetByIdempotencyKey(context.Background(), 1, input.ScenarioID, input.IdempotencyKey)
	if err != nil || run.Status != demomodel.ScenarioStatusStartFailed {
		t.Fatalf("failed run = %+v err = %v", run, err)
	}
	fixtures.fails = false
	status, err := usecase.Start(context.Background(), 1, input)
	if err != nil {
		t.Fatal(err)
	}
	if status.IncidentID != 100 || fixtures.starts != 2 || len(incidents.rows) != 1 {
		t.Fatalf("restart status = %+v starts = %d incidents = %d", status, fixtures.starts, len(incidents.rows))
	}
}

func TestBusinessSnapshotReturnsPoolExhausted(t *testing.T) {
	scenarios := newFakeScenarios()
	input := validInput()
	run := &demomodel.ScenarioRun{
		TenantID: 1, ScenarioID: ScenarioID, IdempotencyKey: input.IdempotencyKey, IncidentID: 100,
		PoolManifestID: "manifest", TargetFingerprint: input.TargetFingerprint, Status: demomodel.ScenarioStatusAwaitingAlert,
		ExpiresAt: time.Now().Add(time.Minute), UpdatedAt: time.Now().UTC(),
	}
	if err := scenarios.CreateOrUpdate(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	_, err := NewUsecase(scenarios, newFakeIncidents(), &fakeFixtures{fails: true}).BusinessSnapshot(
		context.Background(), 1, ScenarioID, input.IdempotencyKey, "orders",
	)
	var fixtureErr *FixtureError
	if !errors.As(err, &fixtureErr) || fixtureErr.Code != "pool_exhausted" {
		t.Fatalf("err = %v", err)
	}
}

func TestPoolFixtureClientContinuesAfterExpectedPoolExhaustionProbe(t *testing.T) {
	var recoverCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/pool-fixtures/manifest/probe":
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte(`{"code":503,"message":"pool probe failed","data":{"status":"failed","error_code":"pool_exhausted"}}`))
		case "/v1/pool-fixtures/manifest/recover":
			recoverCalled = true
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(`{"code":200,"message":"success","data":{"status":"recovered"}}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewPoolFixtureClient(server.URL, "runtime-token")
	if err := client.Recover(context.Background(), "manifest", "approved repair"); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !recoverCalled {
		t.Fatal("expected recover request after expected pool-exhaustion probe")
	}
}

func previewCandidate(id, action string, decision repairpreview.Decision) repairpreview.Candidate {
	return repairpreview.Candidate{
		ID: "row-" + id, RunID: "preview-run", TenantID: "1", IncidentID: "100",
		CandidateID: id, Name: id, Kind: "candidate", Action: action,
		ChangeSummary: action, Branch: "preview/" + id, ResultChecksum: "sha256:" + id,
		Consistent: decision == repairpreview.DecisionPass, AverageLatencyMS: 18,
		MedianLatencyMS: 17, P95LatencyMS: 29, SampleCount: 20, TPS: 120,
		WriteImpact: "bounded", BusinessProbePass: decision == repairpreview.DecisionPass,
		Decision: decision,
	}
}

func previewRun(candidates ...repairpreview.Candidate) repairpreview.Run {
	return repairpreview.Run{
		ID: "preview-run", TenantID: "1", IncidentID: "100", BranchPrefix: "preview",
		SeedFingerprint: "sha256:seed-v1", WorkloadFingerprint: "sha256:workload-v1",
		WorkloadRevision: "workload-v1", ControlledLoad: true,
		IsolationBoundary: "preview-pg", Status: "finished", Candidates: candidates,
	}
}

func scenarioPartsWithPreview(
	t *testing.T, previews *fakePreviewRepository, expectedProfile string,
) (*Usecase, StartScenarioInput, *fakeIncidents, *fakeScenarios) {
	t.Helper()
	input := validInput()
	scenarios := newFakeScenarios()
	incidents := newFakeIncidents()
	incident := &alertmodel.Incident{
		ID: 100, Title: "PostgreSQL connection pool exhaustion rehearsal",
		DedupeKey: "demo-scenario:" + input.IdempotencyKey, Status: alertmodel.IncidentStatusOpen,
	}
	incidents.rows[incident.DedupeKey] = incident
	incidents.byID[incident.ID] = incident
	run := &demomodel.ScenarioRun{
		TenantID: 1, ScenarioID: ScenarioID, IdempotencyKey: input.IdempotencyKey,
		IncidentID: 100, PoolManifestID: "manifest", TargetFingerprint: input.TargetFingerprint,
		Status: demomodel.ScenarioStatusDiagnosisSent, ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := scenarios.CreateOrUpdate(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	for index := range previews.runs {
		previews.runs[index].ScenarioID = run.ScenarioID
		previews.runs[index].IdempotencyKey = run.IdempotencyKey
		previews.runs[index].TargetFingerprint = run.TargetFingerprint
		previews.runs[index].BindingFingerprint = repairpreview.WorkloadBinding{
			RunID: previews.runs[index].ID, TenantID: previews.runs[index].TenantID,
			IncidentID: previews.runs[index].IncidentID, ScenarioID: run.ScenarioID,
			IdempotencyKey: run.IdempotencyKey, TargetFingerprint: run.TargetFingerprint,
		}.Fingerprint()
	}
	return NewUsecaseWithPreviews(
		scenarios, incidents, &fakeFixtures{}, previews, expectedProfile,
	), input, incidents, scenarios
}

func scenarioWithPreview(
	t *testing.T, previews *fakePreviewRepository, expectedProfile string,
) (*Usecase, StartScenarioInput, *fakeIncidents) {
	usecase, input, incidents, _ := scenarioPartsWithPreview(t, previews, expectedProfile)
	return usecase, input, incidents
}

func TestPreviewPASSCreatesOnlyHITLEligibility(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	rejected := previewCandidate("candidate-b", "reset_pool", repairpreview.DecisionReject)
	rejected.RejectionReason = "business probe failed"
	previews := &fakePreviewRepository{runs: []repairpreview.Run{
		previewRun(baseline, passing, rejected),
	}}
	usecase, input, _, scenarios := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")

	status, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusAwaitingApproval {
		t.Fatalf("status = %s, want awaiting_approval", status.Status)
	}
	decision := status.PreviewDecision
	if decision == nil || !decision.EligibleForHITL ||
		decision.ReplayProfileID != "sha256:workload-v1" ||
		decision.CandidateA != "candidate-a" || decision.CandidateB != "candidate-b" {
		t.Fatalf("decision = %+v", decision)
	}
	if decision.RootCause == "" || decision.ImpactScope == "" ||
		decision.CandidateADetails == nil || decision.CandidateBDetails == nil ||
		decision.CandidateADetails.Action != "resize_pool" ||
		decision.CandidateADetails.Decision != string(repairpreview.DecisionPass) ||
		decision.CandidateBDetails.RejectionReason != "business probe failed" {
		t.Fatalf("decision must carry HITL context: %+v", decision)
	}
	if previews.eligibleCalls != 1 {
		t.Fatalf("eligible calls = %d", previews.eligibleCalls)
	}
	if len(scenarios.events) != 2 ||
		scenarios.events[0].EventType != demomodel.ScenarioStatusPreviewReady ||
		scenarios.events[1].EventType != demomodel.ScenarioStatusAwaitingApproval {
		t.Fatalf("preview events = %+v", scenarios.events)
	}
	if _, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if previews.eligibleCalls != 1 {
		t.Fatalf("repeat calls = %d", previews.eligibleCalls)
	}
}

func TestApproveExecutesVerifiedRecoveryAndWritesArchive(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	previews := &fakePreviewRepository{runs: []repairpreview.Run{
		previewRun(baseline, passing),
	}}
	usecase, input, _, scenarios := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")
	fixtures := &fakeFixtures{}
	usecase.fixtures = fixtures
	publisher := &fakeWorkflowPublisher{}
	usecase.workflowPublisher = publisher
	archive := &fakeArchiveWriter{}
	usecase.SetArchiveWriter(archive, "goai-demo")

	if _, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	publisher.stages = nil
	status, err := usecase.Approve(context.Background(), 1, 100, "@admin:matrix-local.agentteams.io:18080")
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusRecovered {
		t.Fatalf("status = %s, want recovered", status.Status)
	}
	if fixtures.recoverCalls != 1 || fixtures.lastManifest != "manifest" {
		t.Fatalf("recovery calls = %d manifest = %q", fixtures.recoverCalls, fixtures.lastManifest)
	}
	if fixtures.business != len([]string{"orders", "inventory", "audit"}) {
		t.Fatalf("business verification calls = %d", fixtures.business)
	}
	if strings.Join(publisher.stages, ",") != "repair_dispatched,verifying,recovered" {
		t.Fatalf("published stages = %v", publisher.stages)
	}
	got := make([]string, 0, len(archive.events))
	for _, event := range archive.events {
		if event.TenantID != "goai-demo" || event.IncidentID != "100" {
			t.Fatalf("unexpected archive event = %+v", event)
		}
		got = append(got, event.EventType)
	}
	want := []string{
		incidentcontrol.EventRootCause,
		incidentcontrol.EventApproved,
		incidentcontrol.EventAction,
		incidentcontrol.EventRecovery,
		incidentcontrol.EventClosed,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("archive events = %v, want %v", got, want)
	}
	_ = scenarios
}

func TestApproveResumesAfterPartialRecoveryFailure(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	previews := &fakePreviewRepository{runs: []repairpreview.Run{
		previewRun(baseline, passing),
	}}
	usecase, input, _, scenarios := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")
	fixtures := &fakeFixtures{}
	usecase.fixtures = fixtures
	archive := &fakeArchiveWriter{}
	usecase.SetArchiveWriter(archive, "goai-demo")

	if _, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	fixtures.recoverErr = errors.New("pool fixture unavailable")
	if _, err := usecase.Approve(context.Background(), 1, 100, "@admin:matrix-local.agentteams.io:18080"); err == nil {
		t.Fatal("first approval unexpectedly succeeded")
	}
	run, err := scenarios.GetByIncident(context.Background(), 1, ScenarioID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != demomodel.ScenarioStatusRepairDispatched {
		t.Fatalf("partial-failure status = %s, want repair_dispatched", run.Status)
	}

	fixtures.recoverErr = nil
	status, err := usecase.Approve(context.Background(), 1, 100, "@admin:matrix-local.agentteams.io:18080")
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusRecovered {
		t.Fatalf("retry status = %s, want recovered", status.Status)
	}
	if fixtures.recoverCalls != 2 {
		t.Fatalf("recovery calls = %d, want failed attempt plus retry", fixtures.recoverCalls)
	}
	approved := 0
	for _, event := range archive.events {
		if event.EventType == incidentcontrol.EventApproved {
			approved++
		}
	}
	if approved != 1 {
		t.Fatalf("approved archive events = %d, want 1", approved)
	}
}

func TestApproveDoesNotRepeatRecoveredFixtureOperation(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	previews := &fakePreviewRepository{runs: []repairpreview.Run{
		previewRun(baseline, passing),
	}}
	usecase, _, _, scenarios := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")
	fixtures := &fakeFixtures{state: "recovered", recoverErr: errors.New("fixture already recovered")}
	usecase.fixtures = fixtures
	archive := &fakeArchiveWriter{}
	usecase.SetArchiveWriter(archive, "goai-demo")

	run, err := scenarios.GetByIncident(context.Background(), 1, ScenarioID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := scenarios.UpdateStatus(
		context.Background(), run.ID, demomodel.ScenarioStatusRepairDispatched,
		func(*demomodel.ScenarioRun) error { return nil },
	); err != nil {
		t.Fatal(err)
	}

	status, err := usecase.Approve(context.Background(), 1, 100, "@admin:matrix-local.agentteams.io:18080")
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusRecovered {
		t.Fatalf("status = %s, want recovered", status.Status)
	}
	if fixtures.recoverCalls != 0 {
		t.Fatalf("recovery calls = %d, want 0 when fixture is already recovered", fixtures.recoverCalls)
	}
	eventTypes := make([]string, 0, len(archive.events))
	for _, event := range archive.events {
		eventTypes = append(eventTypes, event.EventType)
	}
	for _, required := range []string{incidentcontrol.EventAction, incidentcontrol.EventRecovery, incidentcontrol.EventClosed} {
		found := false
		for _, eventType := range eventTypes {
			if eventType == required {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("archive events %v lack %s", eventTypes, required)
		}
	}
}

func TestExpiredApprovalClosesScenarioReleasesFixtureAndSkipsRepair(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	previews := &fakePreviewRepository{runs: []repairpreview.Run{previewRun(baseline, passing)}}
	usecase, _, _, scenarios := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")
	fixtures := &fakeFixtures{state: "expired", recoverErr: errors.New("expired fixture must not be recovered")}
	usecase.fixtures = fixtures
	publisher := &fakeWorkflowPublisher{}
	usecase.workflowPublisher = publisher
	archive := &fakeArchiveWriter{}
	usecase.SetArchiveWriter(archive, "goai-demo")

	run, err := scenarios.GetByIncident(context.Background(), 1, ScenarioID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := scenarios.UpdateStatus(
		context.Background(), run.ID, demomodel.ScenarioStatusAwaitingApproval,
		func(*demomodel.ScenarioRun) error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	run.ExpiresAt = time.Now().Add(-time.Second)

	status, err := usecase.Approve(context.Background(), 1, 100, "@admin:matrix-local.agentteams.io:18080")
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusClosed || status.PreviewDecision == nil ||
		status.PreviewDecision.EligibleForHITL {
		t.Fatalf("status = %+v decision = %+v", status, status.PreviewDecision)
	}
	if fixtures.statusCalls != 1 || fixtures.recoverCalls != 0 {
		t.Fatalf("status calls = %d recover calls = %d", fixtures.statusCalls, fixtures.recoverCalls)
	}
	if len(publisher.stages) != 1 || publisher.stages[0] != demomodel.ScenarioStatusClosed {
		t.Fatalf("published stages = %v", publisher.stages)
	}
	if len(scenarios.events) != 1 || scenarios.events[0].EventType != demomodel.ScenarioStatusClosed {
		t.Fatalf("events = %+v", scenarios.events)
	}
	foundExpiredArchive := false
	for _, event := range archive.events {
		if event.EventType == incidentcontrol.EventClosed && event.Status == "expired" {
			foundExpiredArchive = true
		}
	}
	if !foundExpiredArchive {
		t.Fatalf("archive events = %+v lack expired closure", archive.events)
	}
}

func TestAppendArchiveEventUsesStableUUID(t *testing.T) {
	usecase := &Usecase{clock: realClock{}}
	archive := &fakeArchiveWriter{}
	usecase.SetArchiveWriter(archive, "goai-demo")
	run := &demomodel.ScenarioRun{TenantID: 1, IncidentID: 83, IdempotencyKey: "final-demo-e82d97d1-20260919T014144Z-3886582"}

	if err := usecase.appendArchiveEvent(
		context.Background(), run, incidentcontrol.EventAlertReceived,
		"detection", "system", "opskeeper-demo", "firing", "opskeeper://incidents/83/alert", "", false,
	); err != nil {
		t.Fatal(err)
	}
	if err := usecase.appendArchiveEvent(
		context.Background(), run, incidentcontrol.EventAlertReceived,
		"detection", "system", "opskeeper-demo", "firing", "opskeeper://incidents/83/alert", "", false,
	); err != nil {
		t.Fatal(err)
	}

	if len(archive.events) != 2 {
		t.Fatalf("archive event count = %d, want 2", len(archive.events))
	}
	_, err := uuid.Parse(archive.events[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if archive.events[0].ID != archive.events[1].ID {
		t.Fatalf("archive event IDs differ: %q != %q", archive.events[0].ID, archive.events[1].ID)
	}
}

func TestPreviewFAILCannotReachApproval(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	rejected := previewCandidate("candidate-b", "reset_pool", repairpreview.DecisionReject)
	usecase, input, _ := scenarioWithPreview(
		t, &fakePreviewRepository{runs: []repairpreview.Run{previewRun(baseline, rejected)}}, "",
	)

	status, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusPreviewReady {
		t.Fatalf("status = %s, want preview_ready", status.Status)
	}
	if decision := status.PreviewDecision; decision == nil || decision.EligibleForHITL ||
		decision.CandidateA != "" || decision.CandidateB != "candidate-b" {
		t.Fatalf("decision = %+v", status.PreviewDecision)
	}
}

func TestMismatchedReplayProfileIsNotComparable(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	run := previewRun(baseline, passing)
	run.WorkloadFingerprint = "sha256:workload-v2"
	previews := &fakePreviewRepository{runs: []repairpreview.Run{run}}
	usecase, input, _ := scenarioWithPreview(t, previews, "sha256:workload-v1")

	status, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusPreviewReady {
		t.Fatalf("status = %s, want preview_ready", status.Status)
	}
	decision := status.PreviewDecision
	if decision == nil || decision.EligibleForHITL ||
		decision.ReplayProfileID != "sha256:workload-v2" ||
		!strings.Contains(decision.BoundaryText, "NOT COMPARABLE") {
		t.Fatalf("decision = %+v", decision)
	}
	if previews.eligibleCalls != 0 {
		t.Fatalf("mismatched profile queried eligibility calls = %d", previews.eligibleCalls)
	}
}

func TestMismatchedPreviewRunBindingCannotReachApproval(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	previews := &fakePreviewRepository{runs: []repairpreview.Run{previewRun(baseline, passing)}}
	usecase, input, _, _ := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")
	previews.runs[0].IdempotencyKey = "other-demo-key"
	previews.runs[0].BindingFingerprint = repairpreview.WorkloadBinding{
		RunID: previews.runs[0].ID, TenantID: previews.runs[0].TenantID,
		IncidentID: previews.runs[0].IncidentID, ScenarioID: previews.runs[0].ScenarioID,
		IdempotencyKey:    previews.runs[0].IdempotencyKey,
		TargetFingerprint: previews.runs[0].TargetFingerprint,
	}.Fingerprint()

	status, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusPreviewReady ||
		status.PreviewDecision == nil || status.PreviewDecision.EligibleForHITL ||
		!strings.Contains(status.PreviewDecision.BoundaryText, "RESULT BINDING MISMATCH") {
		t.Fatalf("status = %+v decision = %+v", status, status.PreviewDecision)
	}
	if previews.eligibleCalls != 0 {
		t.Fatalf("mismatched binding queried eligibility calls = %d", previews.eligibleCalls)
	}
}

func TestMissingPreviewRunBindingCannotReachApproval(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	previews := &fakePreviewRepository{runs: []repairpreview.Run{previewRun(baseline, passing)}}
	usecase, input, _, _ := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")
	previews.runs[0].ScenarioID = ""
	previews.runs[0].IdempotencyKey = ""
	previews.runs[0].TargetFingerprint = ""
	previews.runs[0].BindingFingerprint = ""

	status, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusPreviewReady ||
		status.PreviewDecision == nil || status.PreviewDecision.EligibleForHITL ||
		!strings.Contains(status.PreviewDecision.BoundaryText, "RESULT BINDING MISMATCH") {
		t.Fatalf("status = %+v decision = %+v", status, status.PreviewDecision)
	}
	if previews.eligibleCalls != 0 {
		t.Fatalf("missing binding queried eligibility calls = %d", previews.eligibleCalls)
	}
}

func TestIncompleteBaselineMetricsCannotReachApproval(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	baseline.SampleCount = 0
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	previews := &fakePreviewRepository{runs: []repairpreview.Run{
		previewRun(baseline, passing),
	}}
	usecase, input, _ := scenarioWithPreview(t, previews, "sha256:workload-v1")

	status, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusPreviewReady ||
		status.PreviewDecision == nil || status.PreviewDecision.EligibleForHITL {
		t.Fatalf("status = %+v decision = %+v", status, status.PreviewDecision)
	}
	if previews.eligibleCalls != 0 {
		t.Fatalf("incomplete baseline queried eligibility calls = %d", previews.eligibleCalls)
	}
}

func TestEmptyExpectedReplayProfileIsNotComparable(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	previews := &fakePreviewRepository{runs: []repairpreview.Run{previewRun(baseline, passing)}}
	usecase, input, _ := scenarioWithPreview(t, previews, "")

	status, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusPreviewReady ||
		status.PreviewDecision == nil || status.PreviewDecision.EligibleForHITL ||
		!strings.Contains(status.PreviewDecision.BoundaryText, "NOT COMPARABLE") {
		t.Fatalf("status = %+v decision = %+v", status, status.PreviewDecision)
	}
	if previews.eligibleCalls != 0 {
		t.Fatalf("empty profile queried eligibility calls = %d", previews.eligibleCalls)
	}
}

func TestAlertCorrelatedCannotSkipDiagnosis(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	previews := &fakePreviewRepository{runs: []repairpreview.Run{previewRun(baseline, passing)}}
	usecase, input, incidents := scenarioWithPreview(t, previews, "sha256:workload-v1")
	run, err := usecase.scenarios.GetByIdempotencyKey(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = demomodel.ScenarioStatusAlertCorrelated

	status, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusAlertCorrelated || previews.eligibleCalls != 0 {
		t.Fatalf("status = %+v calls = %d", status, previews.eligibleCalls)
	}
	if len(incidents.events) != 0 {
		t.Fatalf("unexpected events = %+v", incidents.events)
	}
}

func TestPreviewEventFailureRollsBackStatusAfterIdempotentPublish(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	previews := &fakePreviewRepository{runs: []repairpreview.Run{previewRun(baseline, passing)}}
	usecase, input, incidents, scenarios := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")
	scenarios.failEventWrite = true
	publisher := &fakeWorkflowPublisher{}
	usecase.workflowPublisher = publisher

	status, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err == nil {
		t.Fatalf("expected atomic transition failure, status = %+v", status)
	}
	run, getErr := scenarios.GetByIdempotencyKey(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if getErr != nil || run.Status != demomodel.ScenarioStatusDiagnosisSent {
		t.Fatalf("run = %+v err = %v", run, getErr)
	}
	if len(scenarios.events) != 0 || len(incidents.events) != 0 ||
		strings.Join(publisher.stages, ",") != demomodel.ScenarioStatusPreviewReady {
		t.Fatalf("events = %+v incident events = %+v stages = %v", scenarios.events, incidents.events, publisher.stages)
	}
}

func TestPreviewPublishFailureDoesNotAdvanceStatus(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	previews := &fakePreviewRepository{runs: []repairpreview.Run{previewRun(baseline, passing)}}
	usecase, input, _, scenarios := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")
	run, err := scenarios.GetByIdempotencyKey(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = demomodel.ScenarioStatusDiagnosisSent
	publisher := &fakeWorkflowPublisher{fails: true}
	usecase.workflowPublisher = publisher

	if _, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey); err == nil {
		t.Fatal("expected workflow publisher failure")
	}
	run, err = scenarios.GetByIdempotencyKey(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil || run.Status != demomodel.ScenarioStatusDiagnosisSent {
		t.Fatalf("run = %+v err = %v", run, err)
	}
	if strings.Join(publisher.stages, ",") != demomodel.ScenarioStatusPreviewReady {
		t.Fatalf("publisher stages = %v", publisher.stages)
	}
}

func TestWorkflowAdvancePublishesManagerAuthorityStages(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	rejected := previewCandidate("candidate-b", "reset_pool", repairpreview.DecisionReject)
	previews := &fakePreviewRepository{runs: []repairpreview.Run{
		previewRun(baseline, passing, rejected),
	}}
	usecase, input, _, scenarios := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")
	publisher := &fakeWorkflowPublisher{}
	usecase.workflowPublisher = publisher

	if _, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{
		demomodel.ScenarioStatusRepairDispatched,
		demomodel.ScenarioStatusVerifying,
		demomodel.ScenarioStatusRecovered,
	} {
		if _, err := usecase.AdvanceWorkflow(context.Background(), 1, ScenarioID, input.IdempotencyKey, stage); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		demomodel.ScenarioStatusPreviewReady,
		demomodel.ScenarioStatusAwaitingApproval,
		demomodel.ScenarioStatusRepairDispatched,
		demomodel.ScenarioStatusVerifying,
		demomodel.ScenarioStatusRecovered,
	}
	if strings.Join(publisher.stages, ",") != strings.Join(want, ",") {
		t.Fatalf("stages = %v want = %v", publisher.stages, want)
	}
	if len(scenarios.events) != len(want) {
		t.Fatalf("event count = %d want = %d", len(scenarios.events), len(want))
	}
}

func TestInitialDiagnosisAdvancesAndTriggersPreviewOnce(t *testing.T) {
	previews := &fakePreviewRepository{}
	usecase, input, incidents, scenarios := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")
	run, err := scenarios.GetByIdempotencyKey(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = demomodel.ScenarioStatusAlertCorrelated
	incidents.events = append(incidents.events, &alertmodel.Event{
		IncidentID: run.IncidentID, EventType: alertmodel.EventTypeAIInitialDiagnosis,
	})
	executor := &fakePreviewExecutor{previews: previews}
	publisher := &fakeWorkflowPublisher{}
	usecase.SetPreviewExecutor(executor)
	usecase.workflowPublisher = publisher

	status, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	second, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusAwaitingApproval ||
		second.Status != demomodel.ScenarioStatusAwaitingApproval || executor.calls != 1 {
		t.Fatalf("status = %+v/%+v executor calls = %d", status, second, executor.calls)
	}
	if executor.lastInput.RunID != DeterministicPreviewRunID(
		1, ScenarioID, input.IdempotencyKey, run.TargetFingerprint, run.IncidentID,
	) {
		t.Fatalf("preview binding = %+v", executor.lastInput)
	}
	if len(scenarios.events) != 3 {
		t.Fatalf("scenario events = %+v", scenarios.events)
	}
	if scenarios.events[0].EventType != demomodel.ScenarioStatusDiagnosisSent ||
		scenarios.events[1].EventType != demomodel.ScenarioStatusPreviewReady ||
		scenarios.events[2].EventType != demomodel.ScenarioStatusAwaitingApproval {
		t.Fatalf("unexpected event order = %+v", scenarios.events)
	}
	if strings.Join(publisher.stages, ",") != strings.Join([]string{
		demomodel.ScenarioStatusPreviewReady, demomodel.ScenarioStatusAwaitingApproval,
	}, ",") {
		t.Fatalf("publisher stages = %v", publisher.stages)
	}
}

func TestDiagnosisOrchestrationSurvivesUsecaseRestart(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	rejected := previewCandidate("candidate-b", "reset_pool", repairpreview.DecisionReject)
	previews := &fakePreviewRepository{runs: []repairpreview.Run{
		previewRun(baseline, passing, rejected),
	}}
	usecase, input, _, scenarios := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")
	executor := &fakePreviewExecutor{previews: previews}
	usecase.SetPreviewExecutor(executor)

	status, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusAwaitingApproval || executor.calls != 0 ||
		len(scenarios.events) != 2 {
		t.Fatalf("status = %+v calls = %d events = %+v", status, executor.calls, scenarios.events)
	}
}

func TestDiagnosisTransitionRollsBackWhenEvidenceWriteFails(t *testing.T) {
	previews := &fakePreviewRepository{}
	usecase, input, incidents, scenarios := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")
	run, err := scenarios.GetByIdempotencyKey(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = demomodel.ScenarioStatusAlertCorrelated
	incidents.events = append(incidents.events, &alertmodel.Event{
		IncidentID: run.IncidentID, EventType: alertmodel.EventTypeAIInitialDiagnosis,
	})
	scenarios.failEventWrite = true
	usecase.SetPreviewExecutor(&fakePreviewExecutor{previews: previews})

	if _, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey); err == nil {
		t.Fatal("expected diagnosis transition failure")
	}
	if run.Status != demomodel.ScenarioStatusAlertCorrelated || len(scenarios.events) != 0 {
		t.Fatalf("run = %+v events = %+v", run, scenarios.events)
	}
}

func TestExpiredScenarioClosesBeforeApprovalWithoutFabricatingHITL(t *testing.T) {
	baseline := previewCandidate("baseline", "baseline", repairpreview.DecisionPass)
	baseline.Kind = "baseline"
	passing := previewCandidate("candidate-a", "resize_pool", repairpreview.DecisionPass)
	previews := &fakePreviewRepository{runs: []repairpreview.Run{previewRun(baseline, passing)}}
	usecase, input, _, scenarios := scenarioPartsWithPreview(t, previews, "sha256:workload-v1")
	run, err := scenarios.GetByIdempotencyKey(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	run.ExpiresAt = time.Now().Add(-time.Second)
	publisher := &fakeWorkflowPublisher{}
	usecase.workflowPublisher = publisher

	status, err := usecase.Get(context.Background(), 1, ScenarioID, input.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != demomodel.ScenarioStatusClosed || status.PreviewDecision == nil ||
		status.PreviewDecision.EligibleForHITL || len(publisher.stages) != 0 {
		t.Fatalf("status = %+v decision = %+v stages = %v", status, status.PreviewDecision, publisher.stages)
	}
	if len(scenarios.events) != 1 || scenarios.events[0].EventType != demomodel.ScenarioStatusClosed {
		t.Fatalf("events = %+v", scenarios.events)
	}
}
