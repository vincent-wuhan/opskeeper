package loop

import (
	"encoding/json"
	"testing"
)

func TestTheRootCauseEnumIsTheOneTheModelIsGiven(t *testing.T) {
	kinds, err := RootCauseKinds()
	if err != nil {
		t.Fatalf("RootCauseKinds: %v", err)
	}
	// Spot-check against the literal values the prompt offers, so a schema
	// that parses to something structurally valid but semantically wrong
	// (an empty enum, a nested array) still fails.
	for _, want := range []string{"pg_lock", "pg_long_tx", "redis_memory", "k8s_oom", "host_cpu", "unknown"} {
		found := false
		for _, k := range kinds {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("RootCauseKinds is missing %q; got %v", want, kinds)
		}
	}
}

// The derivation must not be doing something clever with a different part
// of the document. Re-parsing the same constant independently and comparing
// is redundant with the test above, so this one checks the shape instead:
// the enum has to be a list of non-empty strings.
func TestTheRootCauseEnumIsAListOfNonEmptyStrings(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(investigatedOutputSchema), &schema); err != nil {
		t.Fatal(err)
	}
	props := schema["properties"].(map[string]any)
	rco := props["root_cause_object"].(map[string]any)["properties"].(map[string]any)
	kind := rco["kind"].(map[string]any)
	enum, ok := kind["enum"].([]any)
	if !ok || len(enum) == 0 {
		t.Fatalf("kind.enum is not a non-empty list: %#v", kind["enum"])
	}
	for _, e := range enum {
		s, ok := e.(string)
		if !ok || s == "" {
			t.Errorf("kind.enum contains a non-string or empty entry: %#v", e)
		}
	}
}
