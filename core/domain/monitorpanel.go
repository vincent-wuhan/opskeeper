package domain

// This file is the answer to a measurement. `domaincheck -edges` priced the
// `grafana -> monitor` edge at four types and one method, and the four types
// were an eleven-field GORM entity plus three string constants. The consumer
// was `core/domains/biz/grafana`, whose entire relationship with the monitor
// domain is "render these panels as a dashboard".
//
// It read six of the eleven columns: ID, Type, Title, Unit, PromQL, Legend.
// It never read Ordinal (that is the SPA's row order, and the Grafana layout
// is computed from slice position instead), never read LastSyncError or
// LastSyncAt (those are how the monitor domain records whether the mirror
// worked — the mirror cannot care), and never read UpdatedAt or CreatedAt.
//
// So the entity crossing the boundary was four times the width of the thing
// the boundary is for, and three of the extra columns are timestamps whose
// only meaning is bookkeeping on the far side of it.

// The panel types are declared here rather than in the monitor model because
// two domains now need them and the mirror is the one that maps them onto
// Grafana's own vocabulary. Keeping one definition is the point; the monitor
// model aliases these, so nothing downstream changes spelling.
const (
	// MonitorPanelTypeTimeseries is a line over time.
	MonitorPanelTypeTimeseries = "timeseries"
	// MonitorPanelTypeStat is a single number.
	MonitorPanelTypeStat = "stat"
	// MonitorPanelTypeGauge is a single number with a threshold arc.
	MonitorPanelTypeGauge = "gauge"
)

// MonitorPanelSpec is one panel as a dashboard writer needs to see it.
//
// It is deliberately not the entity: no gorm tags, no timestamps, no sync
// bookkeeping. The five columns the mirror does not read are the five whose
// only consumer is the row it was loaded from, and a projection that carries
// them is a projection that will be carried forever — the next person to add
// a column to the entity will add it here too, and the boundary will widen
// back to eleven without anybody deciding to widen it.
//
// Values rather than pointers throughout: a nil panel has no meaning to a
// renderer, and `[]*Panel` invited a nil check at three call sites that
// disappeared when the slice became `[]MonitorPanelSpec`.
type MonitorPanelSpec struct {
	// ID is the Grafana panel id. The core fleet panels use 9001+ so they
	// never collide with the auto-increment row ids of user panels.
	ID uint64
	// Title is what the operator sees on the panel.
	Title string
	// Type is one of the MonitorPanelType* constants.
	Type string
	// PromQL is the query Grafana evaluates.
	PromQL string
	// Legend is the series label format, e.g. "{{device_id}}".
	Legend string
	// Unit is Grafana's field unit, e.g. "percent" or "Bps".
	Unit string
}
