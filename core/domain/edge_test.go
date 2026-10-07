package domain

import (
	"reflect"
	"testing"
)

// TestEdgePresenceCarriesExactlyTheSixColumnsTheTwoCallersRead is the guard the
// projection never had.
//
// Decision 233 measured that `alert` and `systemhealth` together touch six
// columns of a fifteen-column table, and this type is the thing that replaced
// the other nine being reachable. A projection is a promise about what a
// consumer can see, and a promise written only as a struct definition decays
// the first time somebody adds a field because it was convenient — at which
// point the nine that were deliberately left behind start leaking back in one
// at a time, and nobody can tell from the diff which one was the mistake.
//
// So the field set is pinned, by name and by count. Adding a seventh column
// fails here, and the failure is the point: the new column has to be argued
// for rather than slipped in.
func TestEdgePresenceCarriesExactlyTheSixColumnsTheTwoCallersRead(t *testing.T) {
	typ := reflect.TypeOf(EdgePresence{})
	if typ.NumField() != 6 {
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			got = append(got, typ.Field(i).Name)
		}
		t.Fatalf("EdgePresence has %d fields %v, want the 6 the alert staleness gauge and the "+
			"system-health edge probe actually read; every extra column is a column the edge "+
			"domain's credentials, soft-delete and version-self-report stop being able to keep "+
			"to themselves", typ.NumField(), got)
	}
	want := map[string]bool{
		"ID": true, "Name": true, "Status": true,
		"DeviceID": true, "LastSeenAt": true, "CreatedAt": true,
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if !want[name] {
			t.Errorf("EdgePresence.%s is not one of the six columns the two callers read; "+
				"if it was added on purpose, update this list in the same commit and say why "+
				"a gauge or a health probe needs it", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("EdgePresence is missing %s, which both callers read; dropping it breaks a "+
			"caller that compiles fine against a zero value", name)
	}
}

// TestTheEdgeStatusConstantsAreTheOnesTheColumnIsConstrainedTo keeps the
// duplicated vocabulary honest.
//
// The edges table carries a CHECK constraint listing 'online' and 'offline',
// and this file repeats both as plain constants so a consumer that only counts
// them does not have to import the model package. That duplication is the
// thing this test exists for: it is a second spelling, and a second spelling
// that drifts is a probe that counts an unreachable status as neither online
// nor offline and reports a fleet as healthier than it is.
func TestTheEdgeStatusConstantsAreTheOnesTheColumnIsConstrainedTo(t *testing.T) {
	if EdgeStatusOnline != "online" {
		t.Errorf("EdgeStatusOnline = %q, want \"online\"; the edges table's CHECK constraint "+
			"allows no other value, so a different constant counts nothing", EdgeStatusOnline)
	}
	if EdgeStatusOffline != "offline" {
		t.Errorf("EdgeStatusOffline = %q, want \"offline\"; same CHECK constraint, same "+
			"consequence", EdgeStatusOffline)
	}
	if EdgeStatusOnline == EdgeStatusOffline {
		t.Error("the two presence states are the same string, so a probe counting them would " +
			"count every node in both buckets")
	}
}

// TestEdgeStatusQueryAnswersOneQuestionAndReturnsAValue is the guard for the
// second edge port, and it is the guard the previous shape could not have had.
//
// The webshell used to declare its own `EdgeStatusLookup` whose single method
// returned `*model.Edge`. A method-count assertion is satisfied by that, so
// the count alone never objected to the row — what the row cost was a
// `(nil, nil)` state the holder had to carry a branch for and no repository
// in this tree can produce. So the return type is measured here, on the type,
// where nobody can satisfy it by changing a comment.
func TestEdgeStatusQueryAnswersOneQuestionAndReturnsAValue(t *testing.T) {
	typ := reflect.TypeOf((*EdgeStatusQuery)(nil)).Elem()
	if typ.NumMethod() != 1 {
		var got []string
		for i := 0; i < typ.NumMethod(); i++ {
			got = append(got, typ.Method(i).Name)
		}
		t.Fatalf("EdgeStatusQuery has %d methods %v; it answers one question — is this node "+
			"online — and a second method is how List comes back, and List is what made a "+
			"fleet of a thousand and one look like a dead host", typ.NumMethod(), got)
	}
	if name := typ.Method(0).Name; name != "PresenceStatus" {
		t.Errorf("EdgeStatusQuery's one method is %s, want PresenceStatus", name)
	}
	out := typ.Method(0).Type.Out(0)
	if out.Kind() != reflect.String {
		t.Errorf("PresenceStatus answers with %s, want string; anything that can be nil brings "+
			"the absent-row branch back, and that branch is what this port exists to delete", out)
	}
}

// TestTheTwoEdgePortsAreNotTheSameType pins a decision rather than a shape.
//
// EdgeQuery and EdgeStatusQuery could be one interface with two methods, and
// that would be shorter. It would also hand the alert staleness gauge and the
// system-health probe a point lookup neither asked for, which is the accretion
// the EdgeQuery doc warns about in its own words. Merging them is the kind of
// edit that looks like tidying and hands two consumers a method they never
// justified, so it is refused here by name.
func TestTheTwoEdgePortsAreNotTheSameType(t *testing.T) {
	q := reflect.TypeOf((*EdgeQuery)(nil)).Elem()
	s := reflect.TypeOf((*EdgeStatusQuery)(nil)).Elem()
	if q == s {
		t.Fatal("EdgeQuery and EdgeStatusQuery are the same type; merging them gives the " +
			"alert gauge and the health probe a point lookup they never asked for, which is " +
			"the accretion EdgeQuery's own doc comment rules out")
	}
}
