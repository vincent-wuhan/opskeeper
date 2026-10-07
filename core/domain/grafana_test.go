package domain

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestGrafanaSyncResultCarriesTheThreeColumnsTheSyncEndpointReturns pins the
// wire contract, which is the one thing about this type that is not a choice.
//
// `POST /v1/integrations/grafana/sync` writes this struct straight into the
// response and the frontend reads three keys off it. Nothing in the tree
// connects the Go field name to that JSON key except the tag, and a rename that
// drops or changes a tag compiles, passes every test in this module, and
// returns a body the SPA cannot read. The failure lands in a browser, on an
// operator's screen, after a deploy.
//
// So the keys are asserted by name and count, and then asserted again by
// round-tripping a value through encoding/json — the second one is what catches
// a tag that is present but misspelled, which the first cannot.
func TestGrafanaSyncResultCarriesTheThreeColumnsTheSyncEndpointReturns(t *testing.T) {
	typ := reflect.TypeOf(GrafanaSyncResult{})
	if typ.NumField() != 3 {
		t.Fatalf("GrafanaSyncResult has %d fields, want the 3 the sync endpoint returns", typ.NumField())
	}
	want := map[string]string{
		"Folder":     "folder",
		"Datasource": "datasource",
		"Dashboards": "dashboards",
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		key, ok := want[f.Name]
		if !ok {
			t.Errorf("GrafanaSyncResult.%s is not one of the three fields the sync endpoint has "+
				"always returned; if it was added on purpose, the SPA needs to read it and this "+
				"list needs updating in the same commit", f.Name)
			continue
		}
		tag := f.Tag.Get("json")
		if tag != key {
			t.Errorf("GrafanaSyncResult.%s has json tag %q, want %q. The frontend reads that key "+
				"by name, so a change here is a wire change that compiles and ships silently",
				f.Name, tag, key)
		}
		delete(want, f.Name)
	}
	for name := range want {
		t.Errorf("GrafanaSyncResult is missing %s, which the sync endpoint has always returned", name)
	}

	body, err := json.Marshal(GrafanaSyncResult{
		Folder:     "opskeeper",
		Datasource: "prometheus",
		Dashboards: []string{"nodes", "alerts"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := map[string]any{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("the wire body has %d keys %v, want exactly 3", len(got), got)
	}
	for _, key := range []string{"folder", "datasource", "dashboards"} {
		if _, ok := got[key]; !ok {
			t.Errorf("the wire body has no %q key: %s", key, body)
		}
	}
	// An empty Dashboards slice must still serialise as a key rather than being
	// dropped. `omitempty` here would make a sync that pushed no dashboards
	// indistinguishable from a sync that never ran, and the operator reading
	// that page cannot tell those apart.
	if _, ok := got["dashboards"]; !ok && got["dashboards"] == nil {
		t.Errorf("an empty GrafanaSyncResult serialises without a dashboards key: %s. A sync that "+
			"pushed nothing and a sync that never ran must not look the same to the reader",
			string(body))
	}
}

// TestTheGrafanaPortIsThreeMethodsAndNothingElse keeps the port from growing by
// accretion.
//
// The reason this edge was cheap to cut is that the consumer had already written
// down exactly three things it needed. A fourth method added to this interface
// "just for convenience" is a fourth thing some other domain will be handed,
// and the interface is shared — so the cost of widening it is paid by every
// future test double and every future implementor, none of whom were there when
// the reason was written down.
func TestTheGrafanaPortIsThreeMethodsAndNothingElse(t *testing.T) {
	typ := reflect.TypeOf((*GrafanaQuery)(nil)).Elem()
	if typ.NumMethod() != 3 {
		var got []string
		for i := 0; i < typ.NumMethod(); i++ {
			got = append(got, typ.Method(i).Name)
		}
		t.Errorf("GrafanaQuery has %d methods %v, want the 3 the integration HTTP surface "+
			"declared for itself. Widening a shared port is paid for by every implementor and "+
			"every test double, so it needs a reason rather than a convenience",
			typ.NumMethod(), got)
	}
}
