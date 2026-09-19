package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	incidentcontrol "github.com/vincent-wuhan/opskeeper/internal/control/incident"
	repairpreview "github.com/vincent-wuhan/opskeeper/internal/control/repairpreview"
	alertmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/alert"
	demomodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/demo"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

const (
	ScenarioID                     = "pg-pool-exhaustion"
	ScenarioTarget                 = "pg:pool-fixture"
	maxFixtureBytes                = 1 << 20
	requestTimeout                 = 5 * time.Second
	recoveryRequestTimeout         = 20 * time.Second
	businessTimeout                = 3 * time.Second
	finalDemoInitialPoolCapacity   = 4
	finalDemoRecoveredPoolCapacity = 8
)

var (
	idempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{12,128}$`)
	hexPattern         = regexp.MustCompile(`^[0-9a-fA-F]{16,128}$`)
	sha256Pattern      = regexp.MustCompile(`^sha256:[0-9a-fA-F-]{9,121}$`)
)

type StartScenarioInput struct {
	IdempotencyKey    string `json:"idempotency_key"`
	ScenarioID        string `json:"scenario_id"`
	Target            string `json:"target"`
	TargetFingerprint string `json:"target_fingerprint"`
	AlertFingerprint  string `json:"alert_fingerprint"`
	DurationSeconds   int    `json:"duration_seconds"`
}

type ScenarioStatus struct {
	IncidentID        uint64                  `json:"incident_id"`
	ScenarioID        string                  `json:"scenario_id"`
	Status            string                  `json:"status"`
	PoolManifestID    string                  `json:"pool_manifest_id"`
	TargetFingerprint string                  `json:"target_fingerprint"`
	AlertFingerprint  string                  `json:"alert_fingerprint"`
	UpdatedAt         string                  `json:"updated_at"`
	PreviewDecision   *PreviewDecisionSummary `json:"preview_decision,omitempty"`
}

type PreviewDecisionSummary struct {
	ReplayProfileID   string                   `json:"replay_profile_id"`
	BoundaryText      string                   `json:"boundary_text"`
	RootCause         string                   `json:"root_cause"`
	ImpactScope       string                   `json:"impact_scope"`
	CandidateA        string                   `json:"candidate_a"`
	CandidateB        string                   `json:"candidate_b"`
	CandidateADetails *PreviewCandidateSummary `json:"candidate_a_details,omitempty"`
	CandidateBDetails *PreviewCandidateSummary `json:"candidate_b_details,omitempty"`
	EligibleForHITL   bool                     `json:"eligible_for_hitl"`
}

type PreviewCandidateSummary struct {
	CandidateID       string  `json:"candidate_id"`
	Name              string  `json:"name"`
	Action            string  `json:"action"`
	ChangeSummary     string  `json:"change_summary"`
	Decision          string  `json:"decision"`
	RejectionReason   string  `json:"rejection_reason,omitempty"`
	Consistent        bool    `json:"consistent"`
	BusinessProbePass bool    `json:"business_probe_pass"`
	AverageLatencyMS  float64 `json:"average_latency_ms"`
	P95LatencyMS      float64 `json:"p95_latency_ms"`
	TPS               float64 `json:"tps"`
	ErrorCount        int     `json:"error_count"`
	WriteImpact       string  `json:"write_impact"`
}

type ApproveScenarioInput struct {
	ApproverID string `json:"approver_id"`
}

type IncidentRepository interface {
	GetIncidentByDedupeKey(ctx context.Context, dedupeKey string) (*alertmodel.Incident, error)
	GetIncidentByID(ctx context.Context, id uint64) (*alertmodel.Incident, error)
	CreateIncident(ctx context.Context, incident *alertmodel.Incident) error
	CreateEvent(ctx context.Context, event *alertmodel.Event) error
	ListEventsByIncident(ctx context.Context, incidentID uint64, limit int) ([]*alertmodel.Event, error)
}

type ScenarioRepository interface {
	CreateOrUpdate(ctx context.Context, run *demomodel.ScenarioRun) error
	GetByIdempotencyKey(ctx context.Context, tenantID uint64, scenarioID, key string) (*demomodel.ScenarioRun, error)
	GetByIncident(ctx context.Context, tenantID uint64, scenarioID string, incidentID uint64) (*demomodel.ScenarioRun, error)
	UpdateStatus(ctx context.Context, id uint64, status string, mutation func(*demomodel.ScenarioRun) error) error
	UpdateStatusWithEvent(ctx context.Context, id uint64, status string, event *alertmodel.Event, allowedCurrent ...string) error
}

type PoolFixtureRepository interface {
	Start(ctx context.Context, input FixtureStartInput) (FixtureStartResult, error)
	Status(ctx context.Context, manifestID string) (FixtureStatus, error)
	Recover(ctx context.Context, manifestID, reason string) error
	BusinessSnapshot(ctx context.Context, section string) (json.RawMessage, error)
}

type PreviewRepository interface {
	ListByIncident(ctx context.Context, tenantID, incidentID string, limit int) ([]repairpreview.Run, error)
	FindEligible(ctx context.Context, tenantID, incidentID, runID, candidateID, action string) (repairpreview.Candidate, error)
}

type WorkflowPublisher interface {
	PublishWorkflow(ctx context.Context, run *demomodel.ScenarioRun, stage string, decision *PreviewDecisionSummary) error
}

type PreviewExecutor interface {
	Execute(ctx context.Context, input PreviewExecutionInput) error
}

type ArchiveEventWriter interface {
	Append(ctx context.Context, event incidentcontrol.Event) error
}

type PreviewExecutionInput struct {
	RunID             string
	TenantID          string
	IncidentID        string
	ScenarioID        string
	IdempotencyKey    string
	TargetFingerprint string
}

type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

type Usecase struct {
	scenarios             ScenarioRepository
	incidents             IncidentRepository
	fixtures              PoolFixtureRepository
	previews              PreviewRepository
	previewExecutor       PreviewExecutor
	expectedReplayProfile string
	workflowPublisher     WorkflowPublisher
	archiveWriter         ArchiveEventWriter
	archiveTenantID       string
	clock                 Clock
	executionLocks        map[string]*sync.Mutex
	executionLocksGuard   sync.Mutex
}

func NewUsecase(scenarios ScenarioRepository, incidents IncidentRepository, fixtures PoolFixtureRepository) *Usecase {
	return &Usecase{
		scenarios: scenarios, incidents: incidents, fixtures: fixtures,
		clock: realClock{}, executionLocks: map[string]*sync.Mutex{},
	}
}

func NewUsecaseWithPreviews(
	scenarios ScenarioRepository,
	incidents IncidentRepository,
	fixtures PoolFixtureRepository,
	previews PreviewRepository,
	expectedReplayProfile string,
) *Usecase {
	return &Usecase{
		scenarios: scenarios, incidents: incidents, fixtures: fixtures, previews: previews,
		expectedReplayProfile: expectedReplayProfile, clock: realClock{},
		executionLocks: map[string]*sync.Mutex{},
	}
}

func NewUsecaseWithPreviewWorkflow(
	scenarios ScenarioRepository,
	incidents IncidentRepository,
	fixtures PoolFixtureRepository,
	previews PreviewRepository,
	expectedReplayProfile string,
	workflowPublisher WorkflowPublisher,
) *Usecase {
	usecase := NewUsecaseWithPreviews(scenarios, incidents, fixtures, previews, expectedReplayProfile)
	usecase.workflowPublisher = workflowPublisher
	return usecase
}

func (u *Usecase) SetPreviewExecutor(executor PreviewExecutor) {
	u.previewExecutor = executor
}

func (u *Usecase) SetArchiveWriter(writer ArchiveEventWriter, tenantID string) {
	u.archiveWriter = writer
	u.archiveTenantID = strings.TrimSpace(tenantID)
}

func (u *Usecase) Start(ctx context.Context, tenantID uint64, input StartScenarioInput) (*ScenarioStatus, error) {
	if err := ValidateStart(input); err != nil {
		return nil, err
	}
	existing, err := u.scenarios.GetByIdempotencyKey(ctx, tenantID, input.ScenarioID, input.IdempotencyKey)
	if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return nil, err
	}
	if existing != nil {
		if existing.TargetFingerprint != input.TargetFingerprint {
			return nil, errs.ErrConflict
		}
		if existing.Status != demomodel.ScenarioStatusStartFailed {
			return statusFromRun(existing), nil
		}
	}

	dedupeKey := "demo-scenario:" + input.IdempotencyKey
	incident, err := u.incidents.GetIncidentByDedupeKey(ctx, dedupeKey)
	if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return nil, err
	}
	now := u.clock.Now()
	if incident == nil {
		incident = &alertmodel.Incident{
			Title: "PostgreSQL connection pool exhaustion rehearsal", Rule: "pg_pool_exhaustion",
			RuleName: "PostgreSQL pool exhaustion", Severity: "critical", Status: alertmodel.IncidentStatusOpen,
			Summary:     "Controlled PostgreSQL connection-pool exhaustion for the final demo.",
			Description: "Manager-initiated controlled fault with bounded duration and blast radius.",
			DedupeKey:   dedupeKey, LabelsJSON: `{"scenario":"pg-pool-exhaustion","target":"pg:pool-fixture"}`,
			AnnotationsJSON: "{}", EventCount: 1, FirstFiredAt: now, LastFiredAt: now,
			SourceType: "demo",
		}
		if err := u.incidents.CreateIncident(ctx, incident); err != nil {
			return nil, fmt.Errorf("create incident: %w", err)
		}
	}

	expiresAt := now.Add(time.Duration(input.DurationSeconds) * time.Second)
	run := &demomodel.ScenarioRun{
		TenantID: tenantID, ScenarioID: input.ScenarioID, IdempotencyKey: input.IdempotencyKey,
		IncidentID: incident.ID, Target: input.Target, TargetFingerprint: input.TargetFingerprint,
		AlertFingerprint: input.AlertFingerprint, Status: demomodel.ScenarioStatusStarting, ExpiresAt: expiresAt,
	}
	if existing != nil {
		run.ID = existing.ID
	}
	if err := u.scenarios.CreateOrUpdate(ctx, run); err != nil {
		return nil, err
	}

	result, err := u.fixtures.Start(ctx, FixtureStartInput{
		CaseID: input.ScenarioID, IncidentID: strconv.FormatUint(incident.ID, 10),
		InitialCapacity: finalDemoInitialPoolCapacity,
		TargetCapacity:  finalDemoRecoveredPoolCapacity,
		TTLSeconds:      input.DurationSeconds,
	})
	if err != nil {
		_ = u.scenarios.UpdateStatus(ctx, run.ID, demomodel.ScenarioStatusStartFailed, nil)
		return nil, err
	}
	run.PoolManifestID = result.ManifestID
	run.Status = demomodel.ScenarioStatusAwaitingAlert
	if err := u.scenarios.CreateOrUpdate(ctx, run); err != nil {
		return nil, err
	}
	if err := u.appendStartEvent(ctx, incident.ID, input, result.ManifestID, now); err != nil {
		return nil, err
	}
	if err := u.appendArchiveEvent(
		ctx, run, incidentcontrol.EventAlertReceived, "detection", "system", "opskeeper-demo",
		"firing", "opskeeper://incidents/"+strconv.FormatUint(incident.ID, 10)+"/alert", "", false,
	); err != nil {
		return nil, err
	}
	if err := u.appendArchiveEvent(
		ctx, run, incidentcontrol.EventEvidenceRefreshed, "diagnosis", "system", "opskeeper-manager",
		"success", "opskeeper://incidents/"+strconv.FormatUint(incident.ID, 10)+"/timeline", "", false,
	); err != nil {
		return nil, err
	}
	return statusFromRun(run), nil
}

func (u *Usecase) Get(ctx context.Context, tenantID uint64, scenarioID, key string) (*ScenarioStatus, error) {
	if !idempotencyPattern.MatchString(key) {
		return nil, errs.ErrInvalid
	}
	run, err := u.scenarios.GetByIdempotencyKey(ctx, tenantID, scenarioID, key)
	if err != nil {
		return nil, err
	}
	if _, err := u.incidents.GetIncidentByID(ctx, run.IncidentID); err != nil {
		return nil, err
	}
	orchestratedDecision, err := u.orchestrateDiagnosisAndPreview(ctx, tenantID, scenarioID, key)
	if err != nil {
		return nil, err
	}
	run, err = u.scenarios.GetByIdempotencyKey(ctx, tenantID, scenarioID, key)
	if err != nil {
		return nil, err
	}
	if run.PoolManifestID != "" {
		fixture, fixtureErr := u.fixtures.Status(ctx, run.PoolManifestID)
		if fixtureErr == nil {
			aggregateStatus := run.Status
			switch fixture.State {
			case "recovered":
				aggregateStatus = demomodel.ScenarioStatusRecovered
			case "expired":
				aggregateStatus = demomodel.ScenarioStatusClosed
			}
			if fixture.State == "expired" && run.Status == demomodel.ScenarioStatusDiagnosisSent {
				aggregateStatus = run.Status
			}
			if aggregateStatus != run.Status {
				if aggregateStatus == demomodel.ScenarioStatusRecovered {
					decision := u.previewDecision(ctx, tenantID, run)
					if err := u.advanceLoadedWorkflow(ctx, run, aggregateStatus, decision); err != nil {
						return nil, err
					}
				} else if err := u.scenarios.UpdateStatus(ctx, run.ID, aggregateStatus, nil); err != nil {
					return nil, err
				}
			}
		}
	}
	status := statusFromRun(run)
	if orchestratedDecision == nil {
		status.PreviewDecision = u.previewDecision(ctx, tenantID, run)
		if err := u.applyPreviewTransition(ctx, run, status.PreviewDecision); err != nil {
			return nil, err
		}
	} else {
		status.PreviewDecision = orchestratedDecision
	}
	if run.Status != status.Status {
		status.Status = run.Status
	}
	return status, nil
}

func (u *Usecase) orchestrateDiagnosisAndPreview(
	ctx context.Context, tenantID uint64, scenarioID, key string,
) (*PreviewDecisionSummary, error) {
	initial, err := u.scenarios.GetByIdempotencyKey(ctx, tenantID, scenarioID, key)
	if err != nil {
		return nil, err
	}
	if initial.Status != demomodel.ScenarioStatusAlertCorrelated &&
		initial.Status != demomodel.ScenarioStatusDiagnosisSent &&
		initial.Status != demomodel.ScenarioStatusPreviewReady {
		return nil, nil
	}

	lock := u.executionLock(initial.ID)
	lock.Lock()
	defer lock.Unlock()

	run, err := u.scenarios.GetByIdempotencyKey(ctx, tenantID, scenarioID, key)
	if err != nil {
		return nil, err
	}
	if run.Status == demomodel.ScenarioStatusAlertCorrelated {
		found, err := u.hasInitialDiagnosis(ctx, run.IncidentID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, nil
		}
		if err := u.scenarios.UpdateStatusWithEvent(
			ctx, run.ID, demomodel.ScenarioStatusDiagnosisSent, u.diagnosisEvent(run),
			demomodel.ScenarioStatusAlertCorrelated,
		); err != nil {
			return nil, err
		}
		run.Status = demomodel.ScenarioStatusDiagnosisSent
	}
	if run.Status != demomodel.ScenarioStatusDiagnosisSent &&
		run.Status != demomodel.ScenarioStatusPreviewReady {
		return nil, nil
	}

	decision := u.previewDecision(ctx, tenantID, run)
	if decision == nil && run.Status == demomodel.ScenarioStatusDiagnosisSent && u.previewExecutor != nil {
		previewRunID := DeterministicPreviewRunID(
			tenantID, scenarioID, key, run.TargetFingerprint, run.IncidentID,
		)
		previewStartedAt := u.clock.Now()
		slog.Info(
			"demo repair preview execution started",
			slog.String("run_id", previewRunID),
			slog.Uint64("incident_id", run.IncidentID),
			slog.String("scenario_id", scenarioID),
			slog.String("idempotency_key", key),
		)
		executionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		err := u.previewExecutor.Execute(executionCtx, PreviewExecutionInput{
			RunID:             previewRunID,
			TenantID:          u.primaryPreviewTenantID(tenantID),
			IncidentID:        strconv.FormatUint(run.IncidentID, 10),
			ScenarioID:        run.ScenarioID,
			IdempotencyKey:    run.IdempotencyKey,
			TargetFingerprint: run.TargetFingerprint,
		})
		cancel()
		if err != nil {
			slog.Error(
				"demo repair preview execution failed",
				slog.String("run_id", previewRunID),
				slog.Uint64("incident_id", run.IncidentID),
				slog.String("scenario_id", scenarioID),
				slog.String("idempotency_key", key),
				slog.Duration("elapsed", u.clock.Now().Sub(previewStartedAt)),
				slog.Any("error", err),
			)
			if u.clock.Now().After(run.ExpiresAt) {
				return nil, u.scenarios.UpdateStatusWithEvent(
					ctx, run.ID, demomodel.ScenarioStatusClosed, u.expiryEvent(run),
					demomodel.ScenarioStatusDiagnosisSent,
				)
			}
			return nil, err
		}
		slog.Info(
			"demo repair preview execution finished",
			slog.String("run_id", previewRunID),
			slog.Uint64("incident_id", run.IncidentID),
			slog.String("scenario_id", scenarioID),
			slog.String("idempotency_key", key),
			slog.Duration("elapsed", u.clock.Now().Sub(previewStartedAt)),
		)
		decision = u.previewDecision(ctx, tenantID, run)
	}
	if u.clock.Now().After(run.ExpiresAt) && decision == nil {
		return nil, u.scenarios.UpdateStatusWithEvent(
			ctx, run.ID, demomodel.ScenarioStatusClosed, u.expiryEvent(run),
			demomodel.ScenarioStatusDiagnosisSent,
		)
	}
	if decision == nil {
		return nil, nil
	}
	if u.clock.Now().After(run.ExpiresAt) {
		closedDecision := *decision
		closedDecision.EligibleForHITL = false
		closedDecision.BoundaryText = "Scenario expired before approval. " + closedDecision.BoundaryText
		return &closedDecision, u.scenarios.UpdateStatusWithEvent(
			ctx, run.ID, demomodel.ScenarioStatusClosed, u.expiryEvent(run),
			demomodel.ScenarioStatusDiagnosisSent, demomodel.ScenarioStatusPreviewReady,
		)
	}
	return decision, u.applyPreviewTransition(ctx, run, decision)
}

func (u *Usecase) hasInitialDiagnosis(ctx context.Context, incidentID uint64) (bool, error) {
	events, err := u.incidents.ListEventsByIncident(ctx, incidentID, 100)
	if err != nil {
		return false, err
	}
	for _, event := range events {
		if event != nil && event.EventType == alertmodel.EventTypeAIInitialDiagnosis {
			return true, nil
		}
	}
	return false, nil
}

func (u *Usecase) executionLock(id uint64) *sync.Mutex {
	u.executionLocksGuard.Lock()
	defer u.executionLocksGuard.Unlock()
	key := strconv.FormatUint(id, 10)
	lock, ok := u.executionLocks[key]
	if !ok {
		lock = &sync.Mutex{}
		u.executionLocks[key] = lock
	}
	return lock
}

func (u *Usecase) BusinessSnapshot(ctx context.Context, tenantID uint64, scenarioID, key, section string) (json.RawMessage, error) {
	if !validBusinessSection(section) {
		return nil, errs.ErrInvalid
	}
	run, err := u.scenarios.GetByIdempotencyKey(ctx, tenantID, scenarioID, key)
	if err != nil {
		return nil, err
	}
	if run.PoolManifestID == "" {
		return nil, errs.ErrNotWiredYet
	}
	return u.fixtures.BusinessSnapshot(ctx, section)
}

func (u *Usecase) BusinessSnapshotBaseline(ctx context.Context, section string) (json.RawMessage, error) {
	if !validBusinessSection(section) {
		return nil, errs.ErrInvalid
	}
	return u.fixtures.BusinessSnapshot(ctx, section)
}

func (u *Usecase) Approve(
	ctx context.Context, tenantID uint64, incidentID uint64, approverID string,
) (*ScenarioStatus, error) {
	approverID = strings.TrimSpace(approverID)
	if incidentID == 0 || approverID == "" || len(approverID) > 256 {
		return nil, errs.ErrInvalid
	}

	lock := u.executionLock(incidentID)
	lock.Lock()
	defer lock.Unlock()

	run, err := u.scenarios.GetByIncident(ctx, tenantID, ScenarioID, incidentID)
	if err != nil {
		return nil, err
	}
	if run.TenantID != tenantID || run.ScenarioID != ScenarioID || run.IncidentID != incidentID {
		return nil, errs.ErrNotFound
	}
	if run.Status == demomodel.ScenarioStatusRecovered {
		status := statusFromRun(run)
		status.PreviewDecision = u.previewDecision(ctx, tenantID, run)
		return status, nil
	}
	if run.Status != demomodel.ScenarioStatusAwaitingApproval &&
		run.Status != demomodel.ScenarioStatusRepairDispatched &&
		run.Status != demomodel.ScenarioStatusVerifying {
		return nil, errs.ErrConflict
	}
	if !u.clock.Now().Before(run.ExpiresAt) {
		return nil, errs.ErrConflict
	}

	decision := u.previewDecision(ctx, tenantID, run)
	if decision == nil || !decision.EligibleForHITL || decision.CandidateA == "" {
		return nil, errs.ErrConflict
	}
	if run.Status == demomodel.ScenarioStatusAwaitingApproval {
		if err := u.appendArchiveEvent(
			ctx, run, incidentcontrol.EventApproved, "approval", "human", approverID,
			"approved", "opskeeper://repair-preview/"+run.IdempotencyKey+"/"+decision.CandidateA,
			run.TargetFingerprint+":"+decision.CandidateA, false,
		); err != nil {
			return nil, err
		}
		if err := u.advanceLoadedWorkflow(ctx, run, demomodel.ScenarioStatusRepairDispatched, decision); err != nil {
			return nil, err
		}
	}
	if run.Status == demomodel.ScenarioStatusRepairDispatched {
		fixtureStatus, err := u.fixtures.Status(ctx, run.PoolManifestID)
		if err != nil {
			return nil, err
		}
		if fixtureStatus.State != "recovered" {
			if err := u.fixtures.Recover(ctx, run.PoolManifestID, "Human approved "+decision.CandidateA); err != nil {
				return nil, err
			}
		}
		if err := u.appendArchiveEvent(
			ctx, run, incidentcontrol.EventAction, "recovery", "agent", "opskeeper-repairer",
			"executed", "opskeeper://pool-fixtures/"+run.PoolManifestID+"/recovery",
			run.TargetFingerprint+":"+decision.CandidateA, false,
		); err != nil {
			return nil, err
		}
		if err := u.appendArchiveEvent(
			ctx, run, incidentcontrol.EventRecovery, "recovery", "agent", "opskeeper-verifier",
			"success", "opskeeper://pool-fixtures/"+run.PoolManifestID+"/metrics",
			run.TargetFingerprint+":"+decision.CandidateA, true,
		); err != nil {
			return nil, err
		}
		if err := u.advanceLoadedWorkflow(ctx, run, demomodel.ScenarioStatusVerifying, decision); err != nil {
			return nil, err
		}
	}
	for _, section := range []string{"orders", "inventory", "audit"} {
		snapshot, snapshotErr := u.fixtures.BusinessSnapshot(ctx, section)
		if snapshotErr != nil || len(snapshot) == 0 {
			return nil, errs.ErrConflict
		}
	}
	if err := u.advanceLoadedWorkflow(ctx, run, demomodel.ScenarioStatusRecovered, decision); err != nil {
		return nil, err
	}
	if err := u.appendArchiveEvent(
		ctx, run, incidentcontrol.EventClosed, "closure", "agent", "opskeeper-manager",
		"closed", "opskeeper://incidents/"+strconv.FormatUint(run.IncidentID, 10)+"/archive",
		run.TargetFingerprint+":"+decision.CandidateA, false,
	); err != nil {
		return nil, err
	}

	status := statusFromRun(run)
	status.PreviewDecision = decision
	return status, nil
}

func (u *Usecase) appendStartEvent(ctx context.Context, incidentID uint64, input StartScenarioInput, manifestID string, now time.Time) error {
	snapshot, err := json.Marshal(map[string]any{
		"scenario_id": input.ScenarioID, "idempotency_key": input.IdempotencyKey, "target": input.Target,
		"target_fingerprint": input.TargetFingerprint, "alert_fingerprint": input.AlertFingerprint,
		"duration_seconds": input.DurationSeconds, "pool_manifest_id": manifestID,
		"initial_capacity": finalDemoInitialPoolCapacity,
		"target_capacity":  finalDemoRecoveredPoolCapacity,
	})
	if err != nil {
		return err
	}
	message := "Controlled pool-exhaustion rehearsal started with a bounded blast radius."
	return u.incidents.CreateEvent(ctx, &alertmodel.Event{
		IncidentID: incidentID, EventType: "scenario_start", StatusAfter: alertmodel.IncidentStatusOpen,
		Severity: "critical", Title: "Scenario started", Message: &message, ActorType: alertmodel.ActorTypeSystem,
		SnapshotJSON: string(snapshot), Reason: "Manager-controlled final demo injection", OccurredAt: now,
	})
}

func (u *Usecase) previewDecision(ctx context.Context, tenantID uint64, scenario *demomodel.ScenarioRun) *PreviewDecisionSummary {
	if u.previews == nil || scenario.IncidentID == 0 {
		return nil
	}
	var runs []repairpreview.Run
	queriedTenantID := ""
	for _, candidateTenantID := range u.previewTenantIDs(tenantID) {
		queriedRuns, err := u.previews.ListByIncident(
			ctx, candidateTenantID, strconv.FormatUint(scenario.IncidentID, 10), 20,
		)
		if err != nil || len(queriedRuns) == 0 {
			continue
		}
		boundRunExists := false
		for _, queriedRun := range queriedRuns {
			if queriedRun.TenantID == candidateTenantID &&
				queriedRun.IncidentID == strconv.FormatUint(scenario.IncidentID, 10) {
				boundRunExists = true
				break
			}
		}
		if !boundRunExists {
			continue
		}
		runs = queriedRuns
		queriedTenantID = candidateTenantID
		break
	}
	if queriedTenantID == "" {
		return nil
	}
	sort.SliceStable(runs, func(left, right int) bool {
		return runs[left].UpdatedAt.After(runs[right].UpdatedAt)
	})
	selected := runs[0]
	if selected.TenantID != queriedTenantID ||
		selected.IncidentID != strconv.FormatUint(scenario.IncidentID, 10) {
		return nil
	}

	var baseline, passing, rejected repairpreview.Candidate
	for _, candidate := range selected.Candidates {
		if candidate.IsBaseline() && baseline.CandidateID == "" {
			baseline = candidate
			continue
		}
		if candidate.Decision == repairpreview.DecisionPass && passing.CandidateID == "" {
			passing = candidate
			continue
		}
		if candidate.Decision != repairpreview.DecisionPass && rejected.CandidateID == "" {
			rejected = candidate
		}
	}
	if baseline.CandidateID == "" {
		return nil
	}

	resultBindingMatches := repairpreview.RunBindingMatches(
		selected, scenario.ScenarioID, scenario.IdempotencyKey, scenario.TargetFingerprint,
	)
	profileMatches := u.expectedReplayProfile != "" &&
		selected.WorkloadFingerprint == u.expectedReplayProfile
	gateStageReady := scenario.Status == demomodel.ScenarioStatusDiagnosisSent ||
		scenario.Status == demomodel.ScenarioStatusPreviewReady
	eligibilityAlreadyGranted := scenario.Status == demomodel.ScenarioStatusAwaitingApproval ||
		scenario.Status == demomodel.ScenarioStatusRepairDispatched ||
		scenario.Status == demomodel.ScenarioStatusVerifying
	summary := &PreviewDecisionSummary{
		ReplayProfileID: selected.WorkloadFingerprint,
		BoundaryText:    selected.IsolationBoundary,
		RootCause:       "症状：orders/inventory/audit 查询返回 503 或明显变慢；确认根因：PostgreSQL connection pool exhausted（pg_pool_exhaustion）。",
		ImpactScope:     "影响 orders、inventory、audit 演示业务查询；修复范围限定在 target pool fixture，不直接变更共享 PostgreSQL。",
		CandidateB:      rejected.CandidateID,
	}
	if passing.CandidateID != "" {
		details := newPreviewCandidateSummary(passing)
		summary.CandidateADetails = &details
	}
	if rejected.CandidateID != "" {
		details := newPreviewCandidateSummary(rejected)
		summary.CandidateBDetails = &details
	}
	if !profileMatches {
		summary.BoundaryText = fmt.Sprintf(
			"NOT COMPARABLE: replay profile %s does not match expected %s. %s",
			selected.WorkloadFingerprint, u.expectedReplayProfile, selected.IsolationBoundary,
		)
	}
	if !resultBindingMatches {
		summary.BoundaryText = "RESULT BINDING MISMATCH: preview run is not bound to this scenario, idempotency key, and target."
	}
	if (gateStageReady || eligibilityAlreadyGranted) && profileMatches && resultBindingMatches && selected.ControlledLoad &&
		completePreviewMetrics(baseline) &&
		passing.CandidateID != "" &&
		passing.Decision == repairpreview.DecisionPass && passing.Validate() == nil {
		eligible := passing
		if gateStageReady {
			queried, err := u.previews.FindEligible(
				ctx, selected.TenantID, strconv.FormatUint(scenario.IncidentID, 10),
				selected.ID, passing.CandidateID, passing.Action,
			)
			eligible = queried
			if err != nil {
				eligible = repairpreview.Candidate{}
			}
		}
		if eligible.ID == passing.ID && eligible.Decision == repairpreview.DecisionPass {
			summary.CandidateA = passing.CandidateID
			summary.EligibleForHITL = true
		}
	}
	return summary
}

func newPreviewCandidateSummary(candidate repairpreview.Candidate) PreviewCandidateSummary {
	return PreviewCandidateSummary{
		CandidateID: candidate.CandidateID, Name: candidate.Name, Action: candidate.Action,
		ChangeSummary: candidate.ChangeSummary, Decision: string(candidate.Decision),
		RejectionReason: candidate.RejectionReason, Consistent: candidate.Consistent,
		BusinessProbePass: candidate.BusinessProbePass, AverageLatencyMS: candidate.AverageLatencyMS,
		P95LatencyMS: candidate.P95LatencyMS, TPS: candidate.TPS, ErrorCount: candidate.ErrorCount,
		WriteImpact: candidate.WriteImpact,
	}
}

func (u *Usecase) appendArchiveEvent(
	ctx context.Context,
	run *demomodel.ScenarioRun,
	eventType, phase, actorType, actor, status, evidenceRef, actionFingerprint string,
	recoverySignal bool,
) error {
	if u.archiveWriter == nil {
		return nil
	}
	tenantID := strings.TrimSpace(u.archiveTenantID)
	if tenantID == "" {
		tenantID = strconv.FormatUint(run.TenantID, 10)
	}
	event := incidentcontrol.Event{
		ID:       demoArchiveEventID(run.IdempotencyKey, eventType),
		TenantID: tenantID, IncidentID: strconv.FormatUint(run.IncidentID, 10),
		OccurredAt: u.clock.Now(), Phase: phase, EventType: eventType,
		ActorType: actorType, Actor: actor, Status: status,
		ActionFingerprint: actionFingerprint, EvidenceRef: evidenceRef,
		TraceID: "demo:" + run.IdempotencyKey, RecoverySignal: recoverySignal,
	}
	if err := u.archiveWriter.Append(ctx, event); err != nil && !errors.Is(err, incidentcontrol.ErrDuplicateEvent) {
		return err
	}
	return nil
}

func demoArchiveEventID(idempotencyKey, eventType string) string {
	namespace := uuid.NewSHA1(uuid.NameSpaceURL, []byte("opskeeper:demo-timeline"))
	return uuid.NewSHA1(namespace, []byte(idempotencyKey+"/"+eventType)).String()
}

func completePreviewMetrics(candidate repairpreview.Candidate) bool {
	return candidate.ResultChecksum != "" && candidate.WriteImpact != "" &&
		candidate.SampleCount > 0 && candidate.AverageLatencyMS > 0 &&
		candidate.MedianLatencyMS > 0 && candidate.P95LatencyMS > 0 && candidate.TPS > 0
}

func (u *Usecase) applyPreviewTransition(
	ctx context.Context, run *demomodel.ScenarioRun, decision *PreviewDecisionSummary,
) error {
	if decision == nil || (run.Status != demomodel.ScenarioStatusDiagnosisSent &&
		run.Status != demomodel.ScenarioStatusPreviewReady) {
		return nil
	}
	target := demomodel.ScenarioStatusPreviewReady
	if decision.EligibleForHITL {
		target = demomodel.ScenarioStatusAwaitingApproval
	}
	if run.Status == target {
		return nil
	}
	if run.Status != demomodel.ScenarioStatusPreviewReady {
		if err := u.transitionPreview(ctx, run, decision, demomodel.ScenarioStatusPreviewReady); err != nil {
			return err
		}
	}
	if target == demomodel.ScenarioStatusPreviewReady {
		return nil
	}
	return u.transitionPreview(ctx, run, decision, target)
}

func (u *Usecase) transitionPreview(
	ctx context.Context, run *demomodel.ScenarioRun, decision *PreviewDecisionSummary, target string,
) error {
	allowedCurrent := demomodel.ScenarioStatusDiagnosisSent
	if target == demomodel.ScenarioStatusAwaitingApproval {
		allowedCurrent = demomodel.ScenarioStatusPreviewReady
	}
	event := u.previewEvent(run, decision, target)
	if err := u.scenarios.UpdateStatusWithEvent(ctx, run.ID, target, event, allowedCurrent); err != nil {
		return err
	}
	run.Status = target
	if target == demomodel.ScenarioStatusAwaitingApproval {
		if err := u.appendArchiveEvent(
			ctx, run, incidentcontrol.EventRootCause, "diagnosis", "agent", "opskeeper-manager",
			"confirmed", "opskeeper://repair-preview/"+run.IdempotencyKey, "", false,
		); err != nil {
			return err
		}
	}
	u.publishWorkflow(ctx, run, target, decision)
	return nil
}

func (u *Usecase) previewEvent(
	run *demomodel.ScenarioRun, decision *PreviewDecisionSummary, status string,
) *alertmodel.Event {
	snapshot, err := json.Marshal(map[string]any{
		"replay_profile_id": decision.ReplayProfileID, "candidate_a": decision.CandidateA,
		"candidate_b": decision.CandidateB, "eligible_for_hitl": decision.EligibleForHITL,
		"boundary": decision.BoundaryText,
	})
	if err != nil {
		return &alertmodel.Event{IncidentID: run.IncidentID, EventType: status}
	}
	message := "Repair preview evidence is ready."
	if status == demomodel.ScenarioStatusAwaitingApproval {
		message = "Repair preview passed; human approval is required before execution."
	}
	return &alertmodel.Event{
		IncidentID: run.IncidentID, EventType: status, StatusAfter: alertmodel.IncidentStatusOpen,
		Severity: "critical", Title: "Repair preview gate", Message: &message,
		ActorType: alertmodel.ActorTypeSystem, SnapshotJSON: string(snapshot),
		Reason: "Preview evidence creates HITL eligibility only", OccurredAt: u.clock.Now(),
	}
}

func (u *Usecase) previewTenantIDs(tenantID uint64) []string {
	numericTenantID := strconv.FormatUint(tenantID, 10)
	if u.archiveTenantID == "" || u.archiveTenantID == numericTenantID {
		return []string{numericTenantID}
	}
	return []string{u.archiveTenantID, numericTenantID}
}

func (u *Usecase) primaryPreviewTenantID(tenantID uint64) string {
	return u.previewTenantIDs(tenantID)[0]
}

func (u *Usecase) AdvanceWorkflow(
	ctx context.Context, tenantID uint64, scenarioID, key, stage string,
) (*ScenarioStatus, error) {
	if !idempotencyPattern.MatchString(key) {
		return nil, errs.ErrInvalid
	}
	run, err := u.scenarios.GetByIdempotencyKey(ctx, tenantID, scenarioID, key)
	if err != nil {
		return nil, err
	}
	if _, err := u.incidents.GetIncidentByID(ctx, run.IncidentID); err != nil {
		return nil, err
	}
	switch stage {
	case demomodel.ScenarioStatusRepairDispatched,
		demomodel.ScenarioStatusVerifying,
		demomodel.ScenarioStatusRecovered:
	default:
		return nil, errs.ErrInvalid
	}
	if run.Status == stage {
		return statusFromRun(run), nil
	}
	decision := u.previewDecision(ctx, tenantID, run)
	if err := u.advanceLoadedWorkflow(ctx, run, stage, decision); err != nil {
		return nil, err
	}
	status := statusFromRun(run)
	status.PreviewDecision = decision
	return status, nil
}

func (u *Usecase) advanceLoadedWorkflow(
	ctx context.Context, run *demomodel.ScenarioRun, stage string, decision *PreviewDecisionSummary,
) error {
	allowedCurrent := ""
	switch stage {
	case demomodel.ScenarioStatusPreviewReady:
		allowedCurrent = demomodel.ScenarioStatusDiagnosisSent
	case demomodel.ScenarioStatusAwaitingApproval:
		allowedCurrent = demomodel.ScenarioStatusPreviewReady
	case demomodel.ScenarioStatusRepairDispatched:
		allowedCurrent = demomodel.ScenarioStatusAwaitingApproval
	case demomodel.ScenarioStatusVerifying:
		allowedCurrent = demomodel.ScenarioStatusRepairDispatched
	case demomodel.ScenarioStatusRecovered:
		allowedCurrent = demomodel.ScenarioStatusVerifying
	default:
		return errs.ErrInvalid
	}
	event := u.workflowEvent(run, stage)
	if err := u.scenarios.UpdateStatusWithEvent(ctx, run.ID, stage, event, allowedCurrent); err != nil {
		return err
	}
	run.Status = stage
	u.publishWorkflow(ctx, run, stage, decision)
	return nil
}

func (u *Usecase) workflowEvent(run *demomodel.ScenarioRun, stage string) *alertmodel.Event {
	message := "Manager recorded authoritative demo workflow stage " + stage + "."
	snapshot, _ := json.Marshal(map[string]any{
		"scenario_id": run.ScenarioID, "idempotency_key": run.IdempotencyKey,
		"incident_id": run.IncidentID, "target_fingerprint": run.TargetFingerprint,
		"pool_manifest_id": run.PoolManifestID, "stage": stage,
	})
	return &alertmodel.Event{
		IncidentID: run.IncidentID, EventType: stage, StatusAfter: alertmodel.IncidentStatusOpen,
		Severity: "critical", Title: "Final demo workflow", Message: &message,
		ActorType: alertmodel.ActorTypeSystem, SnapshotJSON: string(snapshot),
		Reason: "OpsKeeper Manager authoritative transition", OccurredAt: u.clock.Now(),
	}
}

func (u *Usecase) diagnosisEvent(run *demomodel.ScenarioRun) *alertmodel.Event {
	snapshot, _ := json.Marshal(map[string]any{
		"scenario_id": run.ScenarioID, "idempotency_key": run.IdempotencyKey,
		"incident_id": run.IncidentID, "target_fingerprint": run.TargetFingerprint,
		"source_event_type": alertmodel.EventTypeAIInitialDiagnosis,
		"preview_run_id": DeterministicPreviewRunID(
			run.TenantID, run.ScenarioID, run.IdempotencyKey, run.TargetFingerprint, run.IncidentID,
		),
	})
	message := "Initial diagnosis evidence was accepted; controlled repair preview dispatch follows."
	return &alertmodel.Event{
		IncidentID: run.IncidentID, EventType: demomodel.ScenarioStatusDiagnosisSent,
		StatusAfter: alertmodel.IncidentStatusOpen, Severity: "critical", Title: "Diagnosis dispatched",
		Message: &message, ActorType: alertmodel.ActorTypeSystem, SnapshotJSON: string(snapshot),
		Reason: "Manager-authoritative diagnosis progression", OccurredAt: u.clock.Now(),
	}
}

func (u *Usecase) expiryEvent(run *demomodel.ScenarioRun) *alertmodel.Event {
	snapshot, _ := json.Marshal(map[string]any{
		"scenario_id": run.ScenarioID, "idempotency_key": run.IdempotencyKey,
		"incident_id": run.IncidentID, "expired_at": run.ExpiresAt,
	})
	message := "Scenario expired before human approval; no approval or repair was fabricated."
	return &alertmodel.Event{
		IncidentID: run.IncidentID, EventType: demomodel.ScenarioStatusClosed,
		StatusAfter: alertmodel.IncidentStatusOpen, Severity: "critical", Title: "Scenario expired",
		Message: &message, ActorType: alertmodel.ActorTypeSystem, SnapshotJSON: string(snapshot),
		Reason: "Honest TTL boundary before HITL", OccurredAt: u.clock.Now(),
	}
}

func (u *Usecase) publishWorkflow(
	ctx context.Context, run *demomodel.ScenarioRun, stage string, decision *PreviewDecisionSummary,
) {
	if u.workflowPublisher == nil {
		return
	}
	_ = u.workflowPublisher.PublishWorkflow(ctx, run, stage, decision)
}

func ValidateStart(input StartScenarioInput) error {
	if input.ScenarioID != ScenarioID || input.Target != ScenarioTarget {
		return errs.ErrInvalid
	}
	if input.DurationSeconds < 60 || input.DurationSeconds > 600 {
		return errs.ErrInvalid
	}
	if !idempotencyPattern.MatchString(input.IdempotencyKey) ||
		!validFingerprint(input.TargetFingerprint) || !validFingerprint(input.AlertFingerprint) {
		return errs.ErrInvalid
	}
	return nil
}

func validFingerprint(value string) bool {
	return hexPattern.MatchString(value) || sha256Pattern.MatchString(value)
}

func validBusinessSection(section string) bool {
	return section == "orders" || section == "inventory" || section == "audit"
}

func statusFromRun(run *demomodel.ScenarioRun) *ScenarioStatus {
	return &ScenarioStatus{
		IncidentID: run.IncidentID, ScenarioID: run.ScenarioID, Status: run.Status,
		PoolManifestID: run.PoolManifestID, TargetFingerprint: run.TargetFingerprint,
		AlertFingerprint: run.AlertFingerprint, UpdatedAt: run.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

type FixtureStartInput struct {
	CaseID          string `json:"case_id"`
	IncidentID      string `json:"incident_id"`
	InitialCapacity int    `json:"initial_capacity"`
	TargetCapacity  int    `json:"target_capacity"`
	TTLSeconds      int    `json:"ttl_seconds"`
}

type FixtureStartResult struct{ ManifestID string }
type FixtureStatus struct{ State string }

type FixtureError struct {
	HTTPStatus int
	Code       string
}

func (e *FixtureError) Error() string { return "pool fixture request failed" }

type PoolFixtureClient struct {
	endpoint string
	token    string
	client   *http.Client
}

func NewPoolFixtureClient(endpoint, token string) *PoolFixtureClient {
	return &PoolFixtureClient{
		endpoint: strings.TrimRight(endpoint, "/"), token: token,
		client: &http.Client{},
	}
}

func (c *PoolFixtureClient) Start(ctx context.Context, input FixtureStartInput) (FixtureStartResult, error) {
	var output struct {
		ManifestID string `json:"manifest_id"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/pool-fixtures", input, &output, requestTimeout); err != nil {
		return FixtureStartResult{}, err
	}
	if output.ManifestID == "" {
		return FixtureStartResult{}, &FixtureError{HTTPStatus: http.StatusBadGateway}
	}
	return FixtureStartResult{ManifestID: output.ManifestID}, nil
}

func (c *PoolFixtureClient) Status(ctx context.Context, manifestID string) (FixtureStatus, error) {
	var output struct {
		Status string `json:"status"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/pool-fixtures/"+manifestID, nil, &output, requestTimeout); err != nil {
		return FixtureStatus{}, err
	}
	return FixtureStatus{State: output.Status}, nil
}

func (c *PoolFixtureClient) Recover(ctx context.Context, manifestID, reason string) error {
	var probeOutput struct {
		ErrorCode string `json:"error_code"`
	}
	if err := c.do(
		ctx, http.MethodPost, "/v1/pool-fixtures/"+manifestID+"/probe",
		map[string]any{"timeout_milliseconds": 250}, &probeOutput, requestTimeout,
	); err != nil {
		var fixtureErr *FixtureError
		if !errors.As(err, &fixtureErr) || (fixtureErr.Code != "pool_exhausted" && probeOutput.ErrorCode != "pool_exhausted") {
			return err
		}
	}
	var recoveryOutput json.RawMessage
	return c.do(
		ctx, http.MethodPost, "/v1/pool-fixtures/"+manifestID+"/recover",
		map[string]any{"reason": reason}, &recoveryOutput, recoveryRequestTimeout,
	)
}

func (c *PoolFixtureClient) BusinessSnapshot(ctx context.Context, section string) (json.RawMessage, error) {
	var output json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/v1/business-snapshots/"+section, nil, &output, businessTimeout); err != nil {
		return nil, err
	}
	return output, nil
}

func (c *PoolFixtureClient) do(ctx context.Context, method, path string, input any, output any, timeout time.Duration) error {
	var body []byte
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = encoded
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return &FixtureError{HTTPStatus: http.StatusBadGateway}
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("X-Opskeeper-Version", "v1")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return &FixtureError{HTTPStatus: http.StatusServiceUnavailable}
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxFixtureBytes))
	if err != nil {
		return &FixtureError{HTTPStatus: http.StatusBadGateway}
	}
	var envelope struct {
		Code      int             `json:"code"`
		Message   string          `json:"message"`
		Data      json.RawMessage `json:"data"`
		ErrorCode string          `json:"error_code"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return &FixtureError{HTTPStatus: http.StatusBadGateway}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if len(envelope.Data) > 0 {
			_ = json.Unmarshal(envelope.Data, output)
		}
		return &FixtureError{HTTPStatus: response.StatusCode, Code: envelope.ErrorCode}
	}
	if len(envelope.Data) == 0 {
		return &FixtureError{HTTPStatus: http.StatusBadGateway}
	}
	if err := json.Unmarshal(envelope.Data, output); err != nil {
		return &FixtureError{HTTPStatus: http.StatusBadGateway}
	}
	return nil
}
