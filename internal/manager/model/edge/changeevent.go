// Package edge contains persistence models for edge-related entities
// (e.g. edge agent change events reported over the tunnel).
package edge

import "time"

// ChangeEventRow is one externally-observed change event captured by
// the edge agent's changewatcher (journald / dockerd / packagemgr)
// and pushed to the manager over the tunnel.
//
// One row per event. The edge side buffers and batches via
// changewatcher.TunnelSink; the manager side batch-inserts via
// data/edge/store/changeevent.go.
//
// Retention: a background cleanup goroutine (biz/edge/changeevent/cleanup.go)
// deletes rows older than 90 days. Default is overridable via config.
type ChangeEventRow struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement;column:id"        json:"id"`
	EdgeID    uint64    `gorm:"not null;index;column:edge_id"             json:"edge_id"`
	Source    string    `gorm:"not null;size:32;column:source"            json:"source"`
	Kind      string    `gorm:"not null;size:64;index;column:kind"        json:"kind"`
	Subject   string    `gorm:"not null;size:255;column:subject"          json:"subject"`
	Action    string    `gorm:"not null;size:32;column:action"            json:"action"`
	Timestamp time.Time `gorm:"not null;index;column:ts"                  json:"timestamp"`
	Severity  string    `gorm:"not null;size:16;column:severity"          json:"severity"`
	Labels    string    `gorm:"not null;type:text;column:labels"          json:"labels"` // JSON-encoded map[string]string
	CreatedAt time.Time `gorm:"not null;column:created_at;autoCreateTime" json:"created_at"`
}

// TableName returns the table name for ChangeEventRow.
func (ChangeEventRow) TableName() string {
	return "edge_change_events"
}
