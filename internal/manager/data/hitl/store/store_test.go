package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestCreateAndGet(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()

	p := &model.Proposal{
		Kind:        "shell_command",
		Title:       "rm -rf /tmp/danger",
		Summary:     "test",
		PayloadJSON: `{"cmd":"rm -rf /tmp/x"}`,
		Source:      model.SourceAgent,
		Severity:    model.SeverityDangerous,
		Sensitivity: model.SensitivityTopSecret,
		ProposedBy:  42,
		State:       model.StatePending,
	}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatalf("Create error: %v", err)
	}
	if p.ID == "" {
		t.Fatal("ID not auto-filled")
	}

	got, err := repo.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if got.Kind != "shell_command" || got.Title != p.Title {
		t.Errorf("Get returned mismatched row: %+v", got)
	}
	if got.State != model.StatePending {
		t.Errorf("state = %s, want pending", got.State)
	}
	if got.PauseStateVersion != 0 {
		t.Errorf("default pause_state_version = %d, want 0", got.PauseStateVersion)
	}
}

func TestCreateAgentTeamsIdempotentRejectsPayloadReplacement(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	key := "agentteams_hitl:9:request-1"

	newProposal := func(payload string) *model.Proposal {
		idempotencyKey := key
		return &model.Proposal{
			Kind: model.KindAgentTeams, PayloadJSON: payload, SessionID: "incident-1",
			MessageID: "request-1", IdempotencyKey: &idempotencyKey,
			Severity: model.SeverityDangerous, Sensitivity: model.SensitivityRestricted,
			State: model.StatePending,
		}
	}
	first := newProposal(`{"parameters":{"command":"restart_service"}}`)
	if err := repo.CreateAgentTeamsIdempotent(ctx, first); err != nil {
		t.Fatalf("first create: %v", err)
	}
	retry := newProposal(first.PayloadJSON)
	if err := repo.CreateAgentTeamsIdempotent(ctx, retry); err != nil {
		t.Fatalf("retry create: %v", err)
	}
	if retry.ID != first.ID {
		t.Fatalf("retry ID = %s; want %s", retry.ID, first.ID)
	}
	replacement := newProposal(`{"parameters":{"command":"noop"}}`)
	err := repo.CreateAgentTeamsIdempotent(ctx, replacement)
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("replacement err = %v; want conflict", err)
	}
}

func TestGet_NotFound(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepo(db)
	_, err := repo.Get(context.Background(), "nonexistent-id")
	if err == nil {
		t.Fatal("Get(nonexistent) should error")
	}
	if err != errs.ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestList_FilterAndPagination(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_ = repo.Create(ctx, &model.Proposal{
			Kind: "test", PayloadJSON: "{}", ProposedBy: uint64(i),
			Severity: model.SeverityMutating, State: model.StatePending,
		})
	}
	for i := 0; i < 3; i++ {
		_ = repo.Create(ctx, &model.Proposal{
			Kind: "test", PayloadJSON: "{}", ProposedBy: uint64(i + 100),
			Severity: model.SeverityMutating, State: model.StateApproved,
		})
	}

	all, total, err := repo.List(ctx, "", 100, 0)
	if err != nil {
		t.Fatalf("List err: %v", err)
	}
	if total != 8 {
		t.Errorf("total = %d, want 8", total)
	}
	if len(all) != 8 {
		t.Errorf("len = %d, want 8", len(all))
	}

	pending, totalPending, err := repo.List(ctx, model.StatePending, 100, 0)
	if err != nil {
		t.Fatalf("List pending err: %v", err)
	}
	if totalPending != 5 || len(pending) != 5 {
		t.Errorf("pending = %d (total %d), want 5", len(pending), totalPending)
	}

	page1, _, err := repo.List(ctx, "", 2, 0)
	if err != nil {
		t.Fatalf("page1 err: %v", err)
	}
	if len(page1) != 2 {
		t.Errorf("page1 len = %d, want 2", len(page1))
	}
}

func TestTransition_HappyPath(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()

	p := &model.Proposal{
		Kind: "test", PayloadJSON: "{}", Severity: model.SeverityMutating,
		State: model.StatePending, ProposedBy: 1,
	}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	by := uint64(99)
	r := "approved for batch"
	if err := repo.Transition(ctx, p.ID, model.StatePending, model.TransitionFields{
		ToState:    model.StateApproved,
		ApprovedBy: &by,
		Reason:     &r,
		DecidedAt:  ptrTime(time.Now()),
	}); err != nil {
		t.Fatalf("Transition err: %v", err)
	}

	got, _ := repo.Get(ctx, p.ID)
	if got.State != model.StateApproved {
		t.Errorf("state = %s, want approved", got.State)
	}
	if got.ApprovedBy == nil || *got.ApprovedBy != 99 {
		t.Errorf("approved_by = %v, want 99", got.ApprovedBy)
	}
	if got.Reason == nil || *got.Reason != "approved for batch" {
		t.Errorf("reason = %v, want 'approved for batch'", got.Reason)
	}
}

func TestTransition_OptimisticLockConflict(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()

	p := &model.Proposal{Kind: "test", PayloadJSON: "{}", Severity: model.SeverityMutating}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	by := uint64(1)
	// 第一次 approve 应成功
	if err := repo.Transition(ctx, p.ID, model.StatePending, model.TransitionFields{
		ToState:    model.StateApproved,
		ApprovedBy: &by,
	}); err != nil {
		t.Fatalf("first transition err: %v", err)
	}
	// 第二次再以 expectedFrom=pending 迁移应 ErrStateConflict（实际状态已非 pending）
	err := repo.Transition(ctx, p.ID, model.StatePending, model.TransitionFields{
		ToState:    model.StateRejected,
		RejectedBy: &by,
	})
	if err != ErrStateConflict {
		t.Errorf("err = %v, want ErrStateConflict", err)
	}
}

func TestTransition_InvalidTransition(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()

	p := &model.Proposal{Kind: "test", PayloadJSON: "{}", Severity: model.SeverityMutating}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	// pending 不能直接 → executed（必须先 approved）
	err := repo.Transition(ctx, p.ID, model.StatePending, model.TransitionFields{
		ToState: model.StateExecuted,
	})
	if err == nil {
		t.Fatal("invalid transition should error")
	}
}

func TestTransition_PauseIncrementsStateVersion(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()

	p := &model.Proposal{Kind: "test", PayloadJSON: "{}", Severity: model.SeverityMutating}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	initialVersion := p.PauseStateVersion

	by := uint64(2)
	if err := repo.Transition(ctx, p.ID, model.StatePending, model.TransitionFields{
		ToState:                    model.StatePaused,
		PausedBy:                   &by,
		IncrementPauseStateVersion: true,
		PausedAt:                   ptrTime(time.Now()),
	}); err != nil {
		t.Fatalf("pause err: %v", err)
	}
	got, _ := repo.Get(ctx, p.ID)
	if got.PauseStateVersion != initialVersion+1 {
		t.Errorf("pause_state_version = %d, want %d", got.PauseStateVersion, initialVersion+1)
	}
}

func TestProposalState_LoadUpsertDelete(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()

	p := &model.Proposal{Kind: "test", PayloadJSON: "{}", Severity: model.SeverityMutating}
	if err := repo.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	st := &model.ProposalState{
		ProposalID: p.ID, StateVersion: 1, TokenBlob: []byte("token-bytes"),
	}
	if err := repo.UpsertState(ctx, st); err != nil {
		t.Fatalf("UpsertState err: %v", err)
	}

	loaded, err := repo.LoadState(ctx, p.ID)
	if err != nil {
		t.Fatalf("LoadState err: %v", err)
	}
	if string(loaded.TokenBlob) != "token-bytes" {
		t.Errorf("token_blob = %q, want %q", loaded.TokenBlob, "token-bytes")
	}
	if loaded.StateVersion != 1 {
		t.Errorf("state_version = %d, want 1", loaded.StateVersion)
	}

	// 二次 upsert（同 PK）应覆盖
	st2 := &model.ProposalState{ProposalID: p.ID, StateVersion: 2, TokenBlob: []byte("v2")}
	if err := repo.UpsertState(ctx, st2); err != nil {
		t.Fatalf("UpsertState v2 err: %v", err)
	}
	loaded2, _ := repo.LoadState(ctx, p.ID)
	if loaded2.StateVersion != 2 || string(loaded2.TokenBlob) != "v2" {
		t.Errorf("upsert did not overwrite: state_version=%d blob=%q", loaded2.StateVersion, loaded2.TokenBlob)
	}

	// delete
	if err := repo.DeleteState(ctx, p.ID); err != nil {
		t.Fatalf("DeleteState err: %v", err)
	}
	if _, err := repo.LoadState(ctx, p.ID); err != errs.ErrNotFound {
		t.Errorf("LoadState after delete err = %v, want ErrNotFound", err)
	}
}

func TestReserveAndCompleteRecoveryApproval(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	requestedAt := time.Now().UTC().Add(-time.Minute)
	expiresAt := time.Now().UTC().Add(10 * time.Minute)
	execution := model.RecoveryExecutionParameters{
		Command: "restart_service", DeviceID: 7, Service: "nginx", Reason: "exact restart",
	}
	payload, err := json.Marshal(model.AgentTeamsPayload{
		Fingerprint: "fingerprint",
		RequestID:   "request-1",
		IncidentID:  "incident-1",
		Action:      "restart_service",
		BlastRadius: "single_device",
		Resource:    "host:worker-1",
		RoomID:      "!room:opskeeper",
		Parameters:  execution,
		RequestedAt: requestedAt,
		ExpiresAt:   expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal := &model.Proposal{
		Kind:        model.KindAgentTeams,
		PayloadJSON: string(payload),
		SessionID:   "incident-1",
		State:       model.StateApproved,
		ExpiresAt:   &expiresAt,
	}
	if err := repo.Create(ctx, proposal); err != nil {
		t.Fatal(err)
	}

	query := RecoveryApprovalQuery{
		ProposalID: proposal.ID,
		SessionID:  "incident-1",
		Kind:       model.KindAgentTeams,
		Action:     "restart_service",
		Resource:   "host:worker-1",
		Execution:  execution,
		Now:        time.Now().UTC(),
	}
	if err := repo.ReserveApprovedForRecovery(ctx, query); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := repo.ReserveApprovedForRecovery(ctx, query); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("second reserve err = %v, want forbidden", err)
	}
	if err := repo.CompleteRecoveryExecution(ctx, proposal.ID, true, `{"ok":true}`, time.Now().UTC()); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, err := repo.Get(ctx, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateExecuted || got.ResultJSON == nil || *got.ResultJSON != `{"ok":true}` {
		t.Fatalf("proposal = state %s result %+v; want executed result", got.State, got.ResultJSON)
	}
}

func TestReserveRecoveryApprovalBindsKillProcessTarget(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	expiresAt := time.Now().UTC().Add(time.Minute)
	execution := model.RecoveryExecutionParameters{
		Command: model.RecoveryActionKillProcess, IncidentID: "host-cpu",
		FixtureManifestID: "f4b1c0a19d3e5f7a", Reason: "terminate fixture",
	}
	payload, _ := json.Marshal(model.AgentTeamsPayload{
		Fingerprint: "fingerprint", RequestID: "request-kill", IncidentID: "host-cpu",
		Action: model.RecoveryActionKillProcess, BlastRadius: "single_device", Resource: "host:fixture",
		RoomID: "!room:opskeeper", RequestedAt: time.Now().UTC().Add(-time.Minute),
		ExpiresAt: expiresAt, Parameters: execution,
	})
	proposal := &model.Proposal{
		Kind: model.KindAgentTeams, PayloadJSON: string(payload), SessionID: "host-cpu",
		State: model.StateApproved, ExpiresAt: &expiresAt,
	}
	if err := repo.Create(ctx, proposal); err != nil {
		t.Fatal(err)
	}
	base := RecoveryApprovalQuery{
		ProposalID: proposal.ID, SessionID: "host-cpu", Kind: model.KindAgentTeams,
		Action: model.RecoveryActionKillProcess, Resource: "host:fixture", Execution: execution,
		Now: time.Now().UTC(),
	}
	wrongIncident := base
	wrongIncident.Execution.IncidentID = "other-incident"
	if err := repo.ReserveApprovedForRecovery(ctx, wrongIncident); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("wrong incident err = %v, want forbidden", err)
	}
	wrongManifest := base
	wrongManifest.Execution.FixtureManifestID = "0123456789abcdef"
	if err := repo.ReserveApprovedForRecovery(ctx, wrongManifest); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("wrong manifest err = %v, want forbidden", err)
	}
	if err := repo.ReserveApprovedForRecovery(ctx, base); err != nil {
		t.Fatalf("exact reserve: %v", err)
	}
}

func TestReserveRecoveryApprovalRejectsMismatchAndExpiry(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	expiresAt := time.Now().UTC().Add(time.Minute)
	execution := model.RecoveryExecutionParameters{
		Command: "restart_service", DeviceID: 7, Service: "nginx", Reason: "exact restart",
	}
	payload, _ := json.Marshal(model.AgentTeamsPayload{
		Fingerprint: "fingerprint", RequestID: "request-1", IncidentID: "incident-1",
		Action: "restart_service", BlastRadius: "single_device", Resource: "host:worker-1",
		RoomID: "!room:opskeeper", RequestedAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: expiresAt,
		Parameters: execution,
	})
	proposal := &model.Proposal{
		Kind: model.KindAgentTeams, PayloadJSON: string(payload), SessionID: "incident-1",
		State: model.StateApproved, ExpiresAt: &expiresAt,
	}
	if err := repo.Create(ctx, proposal); err != nil {
		t.Fatal(err)
	}
	base := RecoveryApprovalQuery{
		ProposalID: proposal.ID, SessionID: "incident-1", Kind: model.KindAgentTeams,
		Action: "restart_service", Resource: "host:worker-1", Now: time.Now().UTC(),
		Execution: execution,
	}
	wrongAction := base
	wrongAction.Action = "kill_process"
	if err := repo.ReserveApprovedForRecovery(ctx, wrongAction); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("wrong action err = %v, want forbidden", err)
	}
	wrongExecution := base
	wrongExecution.Execution.Reason = "changed reason"
	if err := repo.ReserveApprovedForRecovery(ctx, wrongExecution); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("wrong execution err = %v, want forbidden", err)
	}
	expired := base
	expired.Now = expiresAt.Add(time.Second)
	if err := repo.ReserveApprovedForRecovery(ctx, expired); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("expired err = %v, want forbidden", err)
	}
	got, err := repo.Get(ctx, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateApproved {
		t.Fatalf("state = %s; mismatch must not consume approved proposal", got.State)
	}
}

func TestRecoverExpiredRecoveryExecution(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()
	now := time.Now().UTC()
	expiresAt := now.Add(10 * time.Minute)
	execution := model.RecoveryExecutionParameters{
		Command: "restart_service", DeviceID: 7, Service: "nginx", Reason: "exact restart",
	}
	payload, _ := json.Marshal(model.AgentTeamsPayload{
		Fingerprint: "fingerprint", RequestID: "request-1", IncidentID: "incident-1",
		Action: "restart_service", BlastRadius: "single_device", Resource: "host:worker-1",
		RoomID: "!room:opskeeper", Parameters: execution,
		RequestedAt: now.Add(-time.Minute), ExpiresAt: expiresAt,
	})
	proposal := &model.Proposal{
		Kind: model.KindAgentTeams, PayloadJSON: string(payload), SessionID: "incident-1",
		State: model.StateApproved, ExpiresAt: &expiresAt,
	}
	if err := repo.Create(ctx, proposal); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReserveApprovedForRecovery(ctx, RecoveryApprovalQuery{
		ProposalID: proposal.ID, SessionID: "incident-1", Kind: model.KindAgentTeams,
		Action: "restart_service", Resource: "host:worker-1", Execution: execution, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecoverExpiredRecoveryExecution(ctx, proposal.ID, now.Add(6*time.Minute)); err != nil {
		t.Fatalf("recover expired execution: %v", err)
	}
	got, err := repo.Get(ctx, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateFailed {
		t.Fatalf("state = %s; want failed after expired lease", got.State)
	}
}

func TestCountByStateAndLegacyKind(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()

	_ = repo.Create(ctx, &model.Proposal{Kind: "k1", PayloadJSON: "{}", Severity: model.SeverityMutating, State: model.StatePending, LegacyKind: "approval_legacy"})
	_ = repo.Create(ctx, &model.Proposal{Kind: "k2", PayloadJSON: "{}", Severity: model.SeverityMutating, State: model.StatePending, LegacyKind: "approval_legacy"})
	_ = repo.Create(ctx, &model.Proposal{Kind: "k3", PayloadJSON: "{}", Severity: model.SeverityMutating, State: model.StateApproved, LegacyKind: "mutating_legacy"})

	pending, err := repo.CountByState(ctx, model.StatePending)
	if err != nil || pending != 2 {
		t.Errorf("CountByState(pending) = %d, want 2", pending)
	}
	approval, err := repo.CountByLegacyKind(ctx, "approval_legacy")
	if err != nil || approval != 2 {
		t.Errorf("CountByLegacyKind(approval_legacy) = %d, want 2", approval)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
