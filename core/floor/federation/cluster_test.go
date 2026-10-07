package federation

import (
	"strings"
	"testing"
)

// TestClusterIDGrammar pins the identity rules, because every other field in
// this package ends up somewhere that assumes them: a staging directory, a log
// line, a console URL, a policy row.
func TestClusterIDGrammar(t *testing.T) {
	good := []string{
		"prod-cn-north",
		"a",
		"eu-1",
		"cluster-2026",
		"0",
		"abc",
		"a-b-c-0-1-2",
	}
	for _, raw := range good {
		if _, err := NewClusterID(raw); err != nil {
			t.Errorf("NewClusterID(%q) = %v, want accepted", raw, err)
		}
	}

	bad := map[string]string{
		"empty":         "",
		"blank":         "   ",
		"upper case":    "Prod",
		"underscore":    "prod_north",
		"dot":           "prod.north",
		"leading dash":  "-prod",
		"trailing dash": "prod-",
		"slash":         "prod/north",
		"space":         "prod north",
		// One byte over the limit, built from legal characters so that
		// it is refused for its length and not for its alphabet.
		"too long": "a" + strings.Repeat("b", MaxClusterIDLen) + "c",
	}
	for name, raw := range bad {
		if id, err := NewClusterID(raw); err == nil {
			t.Errorf("NewClusterID(%q) [%s] = %q, want refused", raw, name, id)
		}
	}
}

// TestAnOverlongIDIsRefusedForItsLength is separate because it is the one case
// where a test can pass for the wrong reason: an id built from NUL bytes would
// also be refused, by the alphabet rule, and the length check would never run.
// So the refusal has to name the limit.
func TestAnOverlongIDIsRefusedForItsLength(t *testing.T) {
	raw := "a" + strings.Repeat("b", MaxClusterIDLen) + "c"
	_, err := NewClusterID(raw)
	if err == nil {
		t.Fatalf("NewClusterID(%q) accepted %d characters", raw, len(raw))
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("refusal = %v, want it to name the length limit", err)
	}
	// One under the limit is accepted, so the check is the boundary and not
	// a blanket refusal of anything long.
	atLimit := "a" + strings.Repeat("b", MaxClusterIDLen-2) + "c"
	if len(atLimit) != MaxClusterIDLen {
		t.Fatalf("test bug: %q is %d characters, not %d", atLimit, len(atLimit), MaxClusterIDLen)
	}
	if _, err := NewClusterID(atLimit); err != nil {
		t.Errorf("NewClusterID(%q) at exactly the limit = %v, want accepted", atLimit, err)
	}
}

func TestClusterValidate(t *testing.T) {
	id, err := NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}

	ok := Cluster{ID: id, EdgeCount: 40}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate on a well-formed cluster: %v", err)
	}

	// A negative count is not a number anyone has, and accepting it would
	// put a nonsense figure into a capacity answer.
	negative := Cluster{ID: id, EdgeCount: -1}
	if err := negative.Validate(); err == nil {
		t.Errorf("Validate accepted a cluster claiming %d nodes", negative.EdgeCount)
	}

	// An identity that did not come from NewClusterID is caught here rather
	// than at the first place it is concatenated onto a path.
	bypass := Cluster{ID: ClusterID("Prod/North")}
	if err := bypass.Validate(); err == nil {
		t.Errorf("Validate accepted an identity built without the constructor")
	}
}

func TestClusterIDStringAndValid(t *testing.T) {
	id := ClusterID("prod-cn-north")
	if id.String() != "prod-cn-north" {
		t.Errorf("String() = %q", id.String())
	}
	if !id.Valid() {
		t.Errorf("Valid() = false for a well-formed id")
	}
	if ClusterID("").Valid() {
		t.Errorf("Valid() = true for the empty id")
	}
}
