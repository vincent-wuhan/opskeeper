package loop

import (
	"reflect"
	"strings"
	"testing"

	alertmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/alert"
)

// Cutting loop -> alert means two declarations of the same facts: the
// labels column's parse, and the JSON keys of a stored rule condition.
// Both fail silently rather than loudly, so both are held here against
// alert's own implementation rather than assumed to match.

// The fixture set is chosen for the cases where the two could disagree,
// not for the cases where they obviously agree. The empty column is first
// because it is the one rule that is not obvious: alert returns an empty
// map, and a loop that returned nil instead would make every downstream
// resolver refuse with a message blaming the evidence.
func TestTheLoopLabelParseAgreesWithAlertsOnEveryFixture(t *testing.T) {
	fixtures := []string{
		"",
		`{}`,
		`{"service":"order-api","host":"host-1"}`,
		`null`,
		`{`,
		`{"port":"not-a-number"}`,
		`["a"]`,
	}
	for _, raw := range fixtures {
		theirs, theirErr := alertmodel.Incident{LabelsJSON: raw}.Labels()
		ours, ourErr := parseIncidentLabels(raw)
		if (theirErr == nil) != (ourErr == nil) {
			t.Errorf("labels %q: alert err=%v, loop err=%v", raw, theirErr, ourErr)
			continue
		}
		if theirErr != nil {
			continue
		}
		if !reflect.DeepEqual(map[string]string(ours), map[string]string(theirs)) {
			t.Errorf("labels %q: loop got %v, alert got %v", raw, ours, theirs)
		}
		// The empty-column case specifically must not come back nil: a nil
		// map and an empty map are equal to DeepEqual but not to a caller
		// that writes into the result.
		if raw == "" && ours == nil {
			t.Error(`labels "": loop returned a nil map, alert returns an empty one`)
		}
	}
}

// A renamed JSON tag on either side does not fail to compile and does not
// fail to parse: it parses, reads as the zero value, and the trigger
// adapter declines to fire. That is a self-healing action quietly switching
// itself off, which is the failure this test exists for.
func TestEveryRuleConditionKeyTheTriggerReadsIsAKeyAlertStillWrites(t *testing.T) {
	theirs := reflect.TypeOf(alertmodel.RuleCondition{})
	mine := reflect.TypeOf(ruleCondition{})
	read := map[string]bool{}
	for i := 0; i < mine.NumField(); i++ {
		f := mine.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "" {
			t.Fatalf("field %s has no json tag", f.Name)
		}
		read[tag] = true
	}
	theirTags := map[string]bool{}
	for i := 0; i < theirs.NumField(); i++ {
		tag := strings.Split(theirs.Field(i).Tag.Get("json"), ",")[0]
		theirTags[tag] = true
	}
	for tag := range read {
		if !theirTags[tag] {
			t.Errorf("the trigger reads %q, which alert's RuleCondition no longer declares", tag)
		}
	}
	// The field types have to line up too: a threshold that becomes a
	// string parses out as zero and the comparison silently never fires.
	byName := map[string]reflect.StructField{}
	for i := 0; i < theirs.NumField(); i++ {
		byName[theirs.Field(i).Name] = theirs.Field(i)
	}
	for i := 0; i < mine.NumField(); i++ {
		f := mine.Field(i)
		their, ok := byName[f.Name]
		if !ok {
			t.Errorf("field %s does not exist on alert's RuleCondition", f.Name)
			continue
		}
		if f.Type != their.Type {
			t.Errorf("field %s: loop %s, alert %s", f.Name, f.Type, their.Type)
		}
	}
}

// The incident projection declares a subset of alert's row, so the count
// differs by design. What must not differ is the type of any field that
// exists on both sides: a Severity that became an int, or a FirstFiredAt
// that became a string, would compile and convert and then be wrong.
func TestEveryProjectedIncidentFieldKeepsAlertsType(t *testing.T) {
	theirs := reflect.TypeOf(alertmodel.Incident{})
	mine := reflect.TypeOf(AlertIncident{})
	byName := map[string]reflect.StructField{}
	for i := 0; i < theirs.NumField(); i++ {
		byName[theirs.Field(i).Name] = theirs.Field(i)
	}
	if mine.NumField() >= theirs.NumField() {
		t.Errorf("the projection has %d fields and the row has %d; a projection that keeps everything is a copy",
			mine.NumField(), theirs.NumField())
	}
	for i := 0; i < mine.NumField(); i++ {
		f := mine.Field(i)
		their, ok := byName[f.Name]
		if !ok {
			t.Errorf("projected field %s does not exist on alert's Incident", f.Name)
			continue
		}
		if f.Type != their.Type {
			t.Errorf("field %s: loop %s, alert %s", f.Name, f.Type, their.Type)
		}
	}
}
