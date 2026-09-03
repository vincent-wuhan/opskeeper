package incident

import (
	"database/sql"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const incidentTimelineTraceEventIndex = "uq_incident_timeline_trace_event"

// Migrate creates the timeline table and enforces one event per trace and event
// type inside one tenant incident. Existing duplicate groups abort startup
// instead of being silently discarded by the unique index.
func Migrate(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("incident migration: nil db")
	}
	for _, model := range []any{&eventRow{}, &runbookRow{}, &recallLogRow{}} {
		if db.Migrator().HasTable(model) {
			continue
		}
		if err := db.AutoMigrate(model); err != nil {
			return fmt.Errorf("incident migration: auto migrate missing table: %w", err)
		}
	}
	if err := rejectDuplicateTimelineEvents(db); err != nil {
		return err
	}
	if err := ensureIncidentTimelineTraceEventIndex(db); err != nil {
		return fmt.Errorf("incident migration: ensure trace event index: %w", err)
	}
	return nil
}

func rejectDuplicateTimelineEvents(db *gorm.DB) error {
	var duplicates []struct {
		TenantID   string
		IncidentID string
		TraceID    sql.NullString
		EventType  string
		Count      int64
	}
	err := db.Model(&eventRow{}).
		Select("tenant_id, incident_id, trace_id, event_type, COUNT(*) AS count").
		Where("trace_id IS NOT NULL AND trace_id <> ''").
		Group("tenant_id, incident_id, trace_id, event_type").
		Having("COUNT(*) > 1").
		Limit(11).
		Find(&duplicates).Error
	if err != nil {
		return fmt.Errorf("incident migration: find duplicate events: %w", err)
	}
	if len(duplicates) == 0 {
		return nil
	}

	groups := make([]string, 0, len(duplicates))
	for _, duplicate := range duplicates {
		groups = append(groups, fmt.Sprintf(
			"%s/%s/%s/%s=%d",
			duplicate.TenantID, duplicate.IncidentID, duplicate.TraceID.String, duplicate.EventType, duplicate.Count,
		))
	}
	suffix := ""
	if len(duplicates) == 11 {
		duplicates = duplicates[:10]
		suffix = " (and more)"
	}
	return fmt.Errorf(
		"incident migration: duplicate trace events must be resolved before startup: %s%s",
		strings.Join(groups[:len(duplicates)], "; "), suffix,
	)
}

func ensureIncidentTimelineTraceEventIndex(db *gorm.DB) error {
	if db.Migrator().HasIndex(&eventRow{}, incidentTimelineTraceEventIndex) {
		return nil
	}
	switch db.Dialector.Name() {
	case "postgres", "sqlite":
		return db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS ` + incidentTimelineTraceEventIndex + `
			ON incident_timeline (tenant_id, incident_id, trace_id, event_type)
			WHERE trace_id IS NOT NULL AND trace_id <> ''`).Error
	case "mysql":
		if err := db.Exec("UPDATE incident_timeline SET trace_id = NULL WHERE trace_id = ''").Error; err != nil {
			return fmt.Errorf("normalize empty trace ids: %w", err)
		}
		return db.Exec(`CREATE UNIQUE INDEX ` + incidentTimelineTraceEventIndex + `
			ON incident_timeline (tenant_id(191), incident_id(191), trace_id(191), event_type(191))`).Error
	default:
		return fmt.Errorf("unsupported dialect %q", db.Dialector.Name())
	}
}
