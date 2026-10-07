package domain

import (
	"reflect"
	"testing"
)

// TestMonitorPanelSpecHasExactlySixFields is the guard on the width of this
// projection, and it exists because a projection that is not pinned is a
// projection that widens.
//
// The rule it enforces is one-directional on purpose. Adding a seventh field
// here is a decision somebody should have to write down, because the six
// fields are precisely the ones the dashboard renderer reads and the five that
// were left behind are precisely the ones it does not — and the natural
// reflex when somebody adds a column to a panel row is to add it here too,
// which would put `last_sync_at` back across a boundary whose only job is to
// render. A type whose field set is not asserted grows one field per column
// anybody adds anywhere in the system, and it does so silently.
//
// The other direction — a column the renderer *should* read, added to the
// entity and not to this struct — cannot be caught from here, and the guard
// that does catch it lives in the consumer:
// TestPanelSpecsSetsEveryFieldOfTheSpec.
func TestMonitorPanelSpecHasExactlySixFields(t *testing.T) {
	want := map[string]string{
		"ID":     "uint64",
		"Title":  "string",
		"Type":   "string",
		"PromQL": "string",
		"Legend": "string",
		"Unit":   "string",
	}
	typ := reflect.TypeOf(MonitorPanelSpec{})
	if typ.NumField() != len(want) {
		t.Errorf("MonitorPanelSpec has %d fields (%s); it should have exactly %d: "+
			"the dashboard renderer reads six columns of the monitor panel and the five "+
			"that were dropped are the five whose only meaning is bookkeeping on the far "+
			"side of the boundary. A seventh field is somebody re-widening it",
			typ.NumField(), fieldList(typ), len(want))
	}
	for name, kind := range want {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Errorf("MonitorPanelSpec has no %s field; the renderer reads it", name)
			continue
		}
		if got := f.Type.String(); got != kind {
			t.Errorf("MonitorPanelSpec.%s is %s, want %s", name, got, kind)
		}
	}
	// And nothing that would drag an import behind it. A value type with a
	// time.Time or a gorm-tagged field is a field set that has started
	// re-growing from the other end.
	for i := 0; i < typ.NumField(); i++ {
		if tag, tagged := typ.Field(i).Tag.Lookup("gorm"); tagged {
			t.Errorf("MonitorPanelSpec.%s carries a gorm tag (%q). This is a value passed "+
				"between two domains, not a row; if it needs gorm tags it is the entity again",
				typ.Field(i).Name, tag)
		}
	}
}

// TestThePanelTypeConstantsAreTheThreeTheRendererMaps guards the other half of
// the move. The values are declared in core/domain and aliased by the monitor
// model, so there is one definition — but a constant's value is also its wire
// format: they go into the SPA's request bodies and out to Grafana as panel
// type names. Renaming one is a silent contract change.
func TestThePanelTypeConstantsAreTheThreeTheRendererMaps(t *testing.T) {
	cases := map[string]string{
		MonitorPanelTypeTimeseries: "timeseries",
		MonitorPanelTypeStat:       "stat",
		MonitorPanelTypeGauge:      "gauge",
	}
	if len(cases) != 3 {
		t.Fatalf("there are %d panel type constants, want 3", len(cases))
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("a MonitorPanelType constant is %q, want %q. These strings are what the "+
				"SPA sends and what Grafana is told; a rename is a contract change that no "+
				"test in this repository can see through", got, want)
		}
	}
}

func fieldList(typ reflect.Type) string {
	out := ""
	for i := 0; i < typ.NumField(); i++ {
		if i > 0 {
			out += ", "
		}
		out += typ.Field(i).Name
	}
	return out
}
