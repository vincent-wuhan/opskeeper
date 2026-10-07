package wire

import (
	"encoding/json"
	"testing"
)

// Two processes used to derive the display target independently and their key
// orders disagreed: the control plane's kernel looked for `target` first, the
// packaged courier looked for `service` first. So `{"target":"prod-cluster",
// "service":"orders-api"}` produced "prod-cluster" on the control plane and
// "orders-api" on the node — the same approval, described two ways, depending
// on which process asked. The first case below is that disagreement, pinned.

func TestToolTargetPrefersTheKeyTheCallerMeantByTarget(t *testing.T) {
	args := json.RawMessage(`{"service":"orders-api","target":"prod-cluster"}`)
	if got := ToolTarget(args); got != "prod-cluster" {
		t.Fatalf("ToolTarget = %q, want %q — an argument literally called target is the one "+
			"the caller meant, and the two copies of this rule used to disagree here", got, "prod-cluster")
	}
	if got, want := ToolSummary("host_restart_service", args), "host_restart_service on prod-cluster"; got != want {
		t.Errorf("ToolSummary = %q, want %q", got, want)
	}
}

// Every key both former implementations looked for, plus the ones only one of
// them did. A key list that quietly loses half its vocabulary is how the two
// copies drifted in the first place.
func TestToolTargetCoversBothVocabularies(t *testing.T) {
	for _, key := range []string{
		"target", "resource", "host", "node", "namespace", "service", "path", // the kernel's list
		"pod", "name", "query", // the courier's additions
	} {
		args := json.RawMessage(`{"` + key + `":"value-of-` + key + `"}`)
		if got := ToolTarget(args); got != "value-of-"+key {
			t.Errorf("ToolTarget(%s) = %q, want value-of-%s", key, got, key)
		}
	}
}

// A non-string value is not a target. Reading one would print a Go value into
// an approval card, and the console is the wrong place to discover a type.
func TestToolTargetSkipsNonStringValues(t *testing.T) {
	args := json.RawMessage(`{"target":42,"service":"orders-api"}`)
	if got := ToolTarget(args); got != "orders-api" {
		t.Errorf("ToolTarget = %q, want orders-api: the numeric target is not a name, and the "+
			"next key is", got)
	}
}

// Arguments the host cannot read have no target, and the summary falls back to
// the tool's own name — the one part that is always true.
func TestToolTargetOfUnreadableArgumentsIsEmpty(t *testing.T) {
	if got := ToolTarget(json.RawMessage(`{not json`)); got != "" {
		t.Errorf("ToolTarget = %q, want empty", got)
	}
	if got, want := ToolSummary("pg_kill_session", json.RawMessage(`{not json`)), "pg_kill_session"; got != want {
		t.Errorf("ToolSummary = %q, want %q", got, want)
	}
	if got, want := ToolSummary("pg_kill_session", json.RawMessage(`{}`)), "pg_kill_session"; got != want {
		t.Errorf("ToolSummary = %q, want %q", got, want)
	}
}

// The map and JSON entry points must not become two rules again: the courier
// holds a decoded object, the two host paths hold raw bytes, and the same call
// has to read the same either way.
func TestTheMapAndJSONFormsAgree(t *testing.T) {
	for _, raw := range []string{
		`{"target":"web-1"}`,
		`{"service":"orders-api"}`,
		`{"name":"web-1","query":"select 1"}`,
		`{"unrelated":"x"}`,
		`{}`,
	} {
		rawArgs := json.RawMessage(raw)
		var decoded map[string]any
		if err := json.Unmarshal(rawArgs, &decoded); err != nil {
			t.Fatalf("fixture %s: %v", raw, err)
		}
		if got, want := ToolTargetMap(decoded), ToolTarget(rawArgs); got != want {
			t.Errorf("%s: ToolTargetMap = %q, ToolTarget = %q", raw, got, want)
		}
		if got, want := ToolSummaryMap("t", decoded), ToolSummary("t", rawArgs); got != want {
			t.Errorf("%s: ToolSummaryMap = %q, ToolSummary = %q", raw, got, want)
		}
	}
}

// The key order is part of the contract, not an implementation detail: it is
// what makes two processes describe one call the same way. Guarding the list
// itself means a key cannot be dropped without this failing.
func TestTheTargetKeyOrderIsFixed(t *testing.T) {
	want := []string{"target", "resource", "host", "node", "pod", "service", "namespace", "path", "name", "query"}
	if len(toolTargetKeys) != len(want) {
		t.Fatalf("the key list has %d entries, want %d: %v", len(toolTargetKeys), len(want), toolTargetKeys)
	}
	for i, key := range want {
		if toolTargetKeys[i] != key {
			t.Fatalf("toolTargetKeys[%d] = %q, want %q — the order decides which argument an "+
				"approval card names, and two copies of this list with different orders is how "+
				"one call got described two ways", i, toolTargetKeys[i], key)
		}
	}
}
