package incident

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrDuplicateEvent = errors.New("incident: event already exists")

type Repository interface {
	Append(ctx context.Context, event Event) error
	ListIncident(ctx context.Context, tenantID, incidentID string) ([]Event, error)
	ListTenant(ctx context.Context, tenantID string) ([]Event, error)
	SaveRunbook(ctx context.Context, postmortem Postmortem) error
	ListRunbooks(ctx context.Context, tenantID, databaseType, faultFingerprint string) ([]Postmortem, error)
	AppendRecallLogs(ctx context.Context, logs []RecallLog) error
	ListRecallLogs(ctx context.Context, tenantID, incidentID string) ([]RecallLog, error)
}

type SQLRepository struct {
	db *gorm.DB
}

func NewSQLRepository(db *gorm.DB) *SQLRepository {
	return &SQLRepository{db: db}
}

func (repository *SQLRepository) Append(ctx context.Context, event Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	row := eventRowFromEvent(event)
	if event.TraceID != "" {
		var count int64
		err := repository.db.WithContext(ctx).Model(&eventRow{}).
			Where("tenant_id = ? AND incident_id = ? AND trace_id = ? AND event_type = ?",
				event.TenantID, event.IncidentID, event.TraceID, event.EventType).
			Count(&count).Error
		if err != nil {
			return fmt.Errorf("check incident event duplicates: %w", err)
		}
		if count > 0 {
			return ErrDuplicateEvent
		}
	}
	result := repository.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&row)
	if result.Error != nil {
		if isUniqueConstraintError(result.Error) {
			return ErrDuplicateEvent
		}
		return fmt.Errorf("append incident event: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrDuplicateEvent
	}
	return nil
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "duplicate key value") ||
		strings.Contains(message, "duplicate entry")
}

func (repository *SQLRepository) ListIncident(ctx context.Context, tenantID, incidentID string) ([]Event, error) {
	if tenantID == "" || incidentID == "" {
		return nil, errors.New("incident: tenant and incident ids are required")
	}
	var rows []eventRow
	err := repository.db.WithContext(ctx).
		Where("tenant_id = ? AND incident_id = ?", tenantID, incidentID).
		Order("occurred_at ASC, id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list incident timeline: %w", err)
	}
	return eventsFromRows(rows), nil
}

func (repository *SQLRepository) ListTenant(ctx context.Context, tenantID string) ([]Event, error) {
	if tenantID == "" {
		return nil, errors.New("incident: tenant id is required")
	}
	var rows []eventRow
	err := repository.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("incident_id ASC, occurred_at ASC, id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list tenant timeline: %w", err)
	}
	return eventsFromRows(rows), nil
}

func (repository *SQLRepository) SaveRunbook(ctx context.Context, postmortem Postmortem) error {
	if err := postmortem.Validate(); err != nil {
		return err
	}
	if postmortem.ID == "" {
		namespace := uuid.NewSHA1(uuid.NameSpaceURL, []byte("opskeeper:runbook"))
		postmortem.ID = uuid.NewSHA1(namespace, []byte(postmortem.TenantID+"/"+postmortem.IncidentID)).String()
	}
	row, err := runbookRowFromPostmortem(postmortem)
	if err != nil {
		return err
	}
	err = repository.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "tenant_id"}, {Name: "incident_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"database_type", "database_version", "topology_fingerprint", "fault_fingerprint",
				"symptom", "diagnosis", "effective_actions", "ineffective_actions", "recovery_signals",
				"applicable_boundary", "source_evidence", "confirmed_by", "confirmed_at", "updated_at",
			}),
		}).Create(&row).Error
	if err != nil {
		return fmt.Errorf("save runbook memory: %w", err)
	}
	return nil
}

func (repository *SQLRepository) ListRunbooks(ctx context.Context, tenantID, databaseType, faultFingerprint string) ([]Postmortem, error) {
	if tenantID == "" || databaseType == "" {
		return nil, errors.New("incident: tenant id and database type are required")
	}
	query := repository.db.WithContext(ctx).Where("tenant_id = ? AND database_type = ?", tenantID, databaseType)
	if faultFingerprint != "" {
		query = query.Where("fault_fingerprint = ?", faultFingerprint)
	}
	var rows []runbookRow
	if err := query.Order("confirmed_at DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list runbook memory: %w", err)
	}
	postmortems := make([]Postmortem, 0, len(rows))
	for _, row := range rows {
		postmortem, err := row.toPostmortem()
		if err != nil {
			return nil, err
		}
		postmortems = append(postmortems, postmortem)
	}
	return postmortems, nil
}

func (repository *SQLRepository) AppendRecallLogs(ctx context.Context, logs []RecallLog) error {
	if len(logs) == 0 {
		return errors.New("incident: at least one recall log is required")
	}
	rows := make([]recallLogRow, 0, len(logs))
	for _, log := range logs {
		if err := log.Validate(); err != nil {
			return err
		}
		if log.ID == "" {
			namespace := uuid.NewSHA1(uuid.NameSpaceURL, []byte("opskeeper:rrf-recall"))
			log.ID = uuid.NewSHA1(namespace, []byte(log.TenantID+"/"+log.IncidentID+"/"+log.CandidateRef+"/"+log.RecalledAt.Format(time.RFC3339Nano))).String()
		}
		rows = append(rows, recallLogRowFromRecallLog(log))
	}
	return repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for index := range rows {
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows[index])
			if result.Error != nil {
				return fmt.Errorf("append recall log %d: %w", index, result.Error)
			}
		}
		return nil
	})
}

func (repository *SQLRepository) ListRecallLogs(ctx context.Context, tenantID, incidentID string) ([]RecallLog, error) {
	if tenantID == "" || incidentID == "" {
		return nil, errors.New("incident: tenant and incident ids are required")
	}
	var rows []recallLogRow
	err := repository.db.WithContext(ctx).
		Where("tenant_id = ? AND incident_id = ?", tenantID, incidentID).
		Order("recalled_at DESC, rrf_score DESC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list recall logs: %w", err)
	}
	logs := make([]RecallLog, 0, len(rows))
	for _, row := range rows {
		logs = append(logs, row.toRecallLog())
	}
	return logs, nil
}

type eventRow struct {
	ID                string         `gorm:"column:id;primaryKey"`
	TenantID          string         `gorm:"column:tenant_id"`
	IncidentID        string         `gorm:"column:incident_id"`
	OccurredAt        time.Time      `gorm:"column:occurred_at"`
	Phase             string         `gorm:"column:phase"`
	EventType         string         `gorm:"column:event_type"`
	ActorType         string         `gorm:"column:actor_type"`
	Actor             string         `gorm:"column:actor"`
	Status            string         `gorm:"column:status"`
	ActionFingerprint string         `gorm:"column:action_fingerprint"`
	Evidence          string         `gorm:"column:evidence"`
	EvidenceRef       string         `gorm:"column:evidence_ref"`
	TraceID           sql.NullString `gorm:"column:trace_id"`
	RecoverySignal    bool           `gorm:"column:recovery_signal"`
}

func (eventRow) TableName() string { return "incident_timeline" }

func eventRowFromEvent(event Event) eventRow {
	return eventRow{
		ID: event.ID, TenantID: event.TenantID, IncidentID: event.IncidentID,
		OccurredAt: event.OccurredAt.UTC(), Phase: event.Phase, EventType: event.EventType,
		ActorType: event.ActorType, Actor: event.Actor, Status: event.Status,
		ActionFingerprint: event.ActionFingerprint, Evidence: "{}",
		EvidenceRef:    event.EvidenceRef,
		TraceID:        sql.NullString{String: event.TraceID, Valid: event.TraceID != ""},
		RecoverySignal: event.RecoverySignal,
	}
}

func eventsFromRows(rows []eventRow) []Event {
	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		events = append(events, Event{
			ID: row.ID, TenantID: row.TenantID, IncidentID: row.IncidentID,
			OccurredAt: row.OccurredAt.UTC(), Phase: row.Phase, EventType: row.EventType,
			ActorType: row.ActorType, Actor: row.Actor, Status: row.Status,
			ActionFingerprint: row.ActionFingerprint, EvidenceRef: row.EvidenceRef,
			TraceID: traceIDFromRow(row), RecoverySignal: row.RecoverySignal,
		})
	}
	return events
}

func traceIDFromRow(row eventRow) string {
	if !row.TraceID.Valid {
		return ""
	}
	return row.TraceID.String
}

type runbookRow struct {
	ID                  string    `gorm:"column:id;primaryKey"`
	TenantID            string    `gorm:"column:tenant_id"`
	IncidentID          string    `gorm:"column:incident_id"`
	DatabaseType        string    `gorm:"column:database_type"`
	DatabaseVersion     string    `gorm:"column:database_version"`
	TopologyFingerprint string    `gorm:"column:topology_fingerprint"`
	FaultFingerprint    string    `gorm:"column:fault_fingerprint"`
	Symptom             string    `gorm:"column:symptom"`
	Diagnosis           string    `gorm:"column:diagnosis"`
	EffectiveActions    string    `gorm:"column:effective_actions"`
	IneffectiveActions  string    `gorm:"column:ineffective_actions"`
	RecoverySignals     string    `gorm:"column:recovery_signals"`
	ApplicableBoundary  string    `gorm:"column:applicable_boundary"`
	SourceEvidence      string    `gorm:"column:source_evidence"`
	ConfirmedBy         string    `gorm:"column:confirmed_by"`
	ConfirmedAt         time.Time `gorm:"column:confirmed_at"`
	CreatedAt           time.Time `gorm:"column:created_at"`
	UpdatedAt           time.Time `gorm:"column:updated_at"`
}

func (runbookRow) TableName() string { return "runbook_memory" }

func runbookRowFromPostmortem(postmortem Postmortem) (runbookRow, error) {
	empty := runbookRow{}
	fields := []struct {
		value  any
		target *string
	}{
		{postmortem.Symptom, &empty.Symptom}, {postmortem.Diagnosis, &empty.Diagnosis},
		{postmortem.EffectiveActions, &empty.EffectiveActions},
		{postmortem.IneffectiveActions, &empty.IneffectiveActions},
		{postmortem.RecoverySignals, &empty.RecoverySignals},
		{postmortem.ApplicableBoundary, &empty.ApplicableBoundary},
		{postmortem.SourceEvidence, &empty.SourceEvidence},
	}
	for _, field := range fields {
		encoded, err := mustJSON(field.value)
		if err != nil {
			return empty, err
		}
		*field.target = encoded
	}
	now := time.Now().UTC()
	empty.ID = postmortem.ID
	empty.TenantID = postmortem.TenantID
	empty.IncidentID = postmortem.IncidentID
	empty.DatabaseType = postmortem.DatabaseType
	empty.DatabaseVersion = postmortem.DatabaseVersion
	empty.TopologyFingerprint = postmortem.TopologyFingerprint
	empty.FaultFingerprint = postmortem.FaultFingerprint
	empty.ConfirmedBy = postmortem.ConfirmedBy
	empty.ConfirmedAt = postmortem.ConfirmedAt.UTC()
	empty.CreatedAt = now
	empty.UpdatedAt = now
	return empty, nil
}

func (row runbookRow) toPostmortem() (Postmortem, error) {
	postmortem := Postmortem{
		ID: row.ID, TenantID: row.TenantID, IncidentID: row.IncidentID,
		DatabaseType: row.DatabaseType, DatabaseVersion: row.DatabaseVersion,
		TopologyFingerprint: row.TopologyFingerprint, FaultFingerprint: row.FaultFingerprint,
		ConfirmedBy: row.ConfirmedBy, ConfirmedAt: row.ConfirmedAt,
	}
	if err := json.Unmarshal([]byte(row.Symptom), &postmortem.Symptom); err != nil {
		return Postmortem{}, fmt.Errorf("decode runbook symptom: %w", err)
	}
	if err := json.Unmarshal([]byte(row.Diagnosis), &postmortem.Diagnosis); err != nil {
		return Postmortem{}, fmt.Errorf("decode runbook diagnosis: %w", err)
	}
	if err := json.Unmarshal([]byte(row.EffectiveActions), &postmortem.EffectiveActions); err != nil {
		return Postmortem{}, fmt.Errorf("decode effective actions: %w", err)
	}
	if err := json.Unmarshal([]byte(row.IneffectiveActions), &postmortem.IneffectiveActions); err != nil {
		return Postmortem{}, fmt.Errorf("decode ineffective actions: %w", err)
	}
	if err := json.Unmarshal([]byte(row.RecoverySignals), &postmortem.RecoverySignals); err != nil {
		return Postmortem{}, fmt.Errorf("decode recovery signals: %w", err)
	}
	if err := json.Unmarshal([]byte(row.ApplicableBoundary), &postmortem.ApplicableBoundary); err != nil {
		return Postmortem{}, fmt.Errorf("decode applicable boundary: %w", err)
	}
	if err := json.Unmarshal([]byte(row.SourceEvidence), &postmortem.SourceEvidence); err != nil {
		return Postmortem{}, fmt.Errorf("decode source evidence: %w", err)
	}
	return postmortem, nil
}

type RecallLog struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	IncidentID     string    `json:"incident_id"`
	QueryText      string    `json:"query_text"`
	CandidateRef   string    `json:"candidate_ref"`
	VectorRank     int       `json:"vector_rank"`
	KeywordRank    int       `json:"keyword_rank"`
	RRFScore       float64   `json:"rrf_score"`
	Selected       bool      `json:"selected"`
	RejectedReason string    `json:"rejected_reason,omitempty"`
	RecalledAt     time.Time `json:"recalled_at"`
}

func (log RecallLog) Validate() error {
	switch {
	case log.TenantID == "", log.IncidentID == "":
		return errors.New("incident recall log: tenant and incident ids are required")
	case log.QueryText == "", log.CandidateRef == "":
		return errors.New("incident recall log: query and candidate reference are required")
	case log.VectorRank < 0, log.KeywordRank < 0:
		return errors.New("incident recall log: ranks cannot be negative")
	case log.RRFScore < 0:
		return errors.New("incident recall log: RRF score cannot be negative")
	case log.RecalledAt.IsZero():
		return errors.New("incident recall log: recalled_at is required")
	case log.Selected && log.RejectedReason != "":
		return errors.New("incident recall log: selected candidate cannot have a rejected reason")
	case !log.Selected && log.RejectedReason == "":
		return errors.New("incident recall log: rejected candidate requires a reason")
	}
	return nil
}

type recallLogRow struct {
	ID             string    `gorm:"column:id;primaryKey"`
	TenantID       string    `gorm:"column:tenant_id"`
	IncidentID     string    `gorm:"column:incident_id"`
	QueryText      string    `gorm:"column:query_text"`
	CandidateRef   string    `gorm:"column:candidate_ref"`
	VectorRank     int       `gorm:"column:vector_rank"`
	KeywordRank    int       `gorm:"column:keyword_rank"`
	RRFScore       float64   `gorm:"column:rrf_score"`
	Selected       bool      `gorm:"column:selected"`
	RejectedReason string    `gorm:"column:rejected_reason"`
	RecalledAt     time.Time `gorm:"column:recalled_at"`
	CreatedAt      time.Time `gorm:"column:created_at"`
}

func (recallLogRow) TableName() string { return "rrf_recall_log" }

func recallLogRowFromRecallLog(log RecallLog) recallLogRow {
	return recallLogRow{
		ID: log.ID, TenantID: log.TenantID, IncidentID: log.IncidentID,
		QueryText: log.QueryText, CandidateRef: log.CandidateRef,
		VectorRank: log.VectorRank, KeywordRank: log.KeywordRank, RRFScore: log.RRFScore,
		Selected: log.Selected, RejectedReason: log.RejectedReason, RecalledAt: log.RecalledAt.UTC(),
		CreatedAt: time.Now().UTC(),
	}
}

func (row recallLogRow) toRecallLog() RecallLog {
	return RecallLog{
		ID: row.ID, TenantID: row.TenantID, IncidentID: row.IncidentID,
		QueryText: row.QueryText, CandidateRef: row.CandidateRef,
		VectorRank: row.VectorRank, KeywordRank: row.KeywordRank, RRFScore: row.RRFScore,
		Selected: row.Selected, RejectedReason: row.RejectedReason, RecalledAt: row.RecalledAt,
	}
}
