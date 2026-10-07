package tunnel

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestChangeEventWire_RoundTrip(t *testing.T) {
	original := ChangeEventWire{
		Source:    "journald",
		Kind:      "ssh_login",
		Subject:   "alice",
		Action:    "login",
		Timestamp: time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC),
		Severity:  "info",
		Labels:    map[string]string{"from": "10.0.0.5"},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ChangeEventWire
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Source != original.Source {
		t.Errorf("Source = %q, want %q", decoded.Source, original.Source)
	}
	if decoded.Subject != original.Subject {
		t.Errorf("Subject = %q, want %q", decoded.Subject, original.Subject)
	}
	if !decoded.Timestamp.Equal(original.Timestamp) {
		t.Errorf("Timestamp = %v, want %v", decoded.Timestamp, original.Timestamp)
	}
	if decoded.Labels["from"] != "10.0.0.5" {
		t.Errorf("Labels[from] = %q", decoded.Labels["from"])
	}
}

func TestChangeEventWire_NoLabels(t *testing.T) {
	ev := ChangeEventWire{Source: "dockerd", Kind: "container_start", Subject: "web", Timestamp: time.Now()}
	data, _ := json.Marshal(ev)
	if strings.Contains(string(data), "labels") {
		t.Errorf("expected omitempty for empty Labels, got: %s", data)
	}
}

func TestPushChangeEventsRequest_DefaultEdgeID(t *testing.T) {
	req := PushChangeEventsRequest{Events: []ChangeEventWire{{Source: "x", Kind: "y"}}}
	data, _ := json.Marshal(req)
	if strings.Contains(string(data), "edge_id") {
		t.Errorf("EdgeID=0 should be omitted, got: %s", data)
	}
}

func TestPushChangeEventsRequest_RoundTrip(t *testing.T) {
	req := PushChangeEventsRequest{
		EdgeID: 42,
		Events: []ChangeEventWire{
			{Source: "journald", Kind: "ssh_login", Subject: "alice", Timestamp: time.Now()},
			{Source: "packagemgr", Kind: "package_install", Subject: "nginx", Timestamp: time.Now()},
		},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded PushChangeEventsRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.EdgeID != 42 {
		t.Errorf("EdgeID = %d, want 42", decoded.EdgeID)
	}
	if len(decoded.Events) != 2 {
		t.Fatalf("len(Events) = %d, want 2", len(decoded.Events))
	}
	if decoded.Events[1].Subject != "nginx" {
		t.Errorf("Events[1].Subject = %q, want nginx", decoded.Events[1].Subject)
	}
}

func TestPushChangeEventsResponse_Fields(t *testing.T) {
	resp := PushChangeEventsResponse{Accepted: 50, Rejected: 2}
	data, _ := json.Marshal(resp)
	var decoded PushChangeEventsResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Accepted != 50 || decoded.Rejected != 2 {
		t.Errorf("got %+v, want accepted=50 rejected=2", decoded)
	}
}

func TestMethodPushChangeEvents_Constant(t *testing.T) {
	if MethodPushChangeEvents != "push_change_events" {
		t.Errorf("MethodPushChangeEvents = %q, want push_change_events", MethodPushChangeEvents)
	}
}

// TestExecuteSkillWireKeys pins the two keys of each execute_skill message,
// which is the whole contract.
//
// Both types were declared here and used by nobody while the manager and the
// node each hand-rolled an identical anonymous struct, so the declared tags
// had drifted from the executed ones without anything noticing — the tags were
// not what was on the wire, they were what somebody had written down and never
// checked. The four literals are gone; these four keys are now the only place
// the shape is written.
//
// The node and the manager are in different modules and cannot import each
// other, so neither side's test can compare the two directly. Pinning the tags
// here is what lets those two tests mean anything: if a key changes, this goes
// red in the module both sides already depend on, and the two side tests go red
// on top of it.
func TestExecuteSkillWireKeys(t *testing.T) {
	for _, c := range []struct {
		name string
		got  string
		want string
	}{
		{"request key", mustJSON(t, ExecuteSkillRequest{Key: "k"}), `{"key":"k"}`},
		{"request params omitted", mustJSON(t, ExecuteSkillRequest{Key: "k"}), `{"key":"k"}`},
		{"response error only", mustJSON(t, ExecuteSkillResponse{Error: "boom"}), `{"error":"boom"}`},
		{"response result only", mustJSON(t, ExecuteSkillResponse{Result: json.RawMessage(`{}`)}), `{"result":{}}`},
		{"response empty", mustJSON(t, ExecuteSkillResponse{}), `{}`},
	} {
		if c.got != c.want {
			t.Errorf("%s marshalled to %s, want %s. Both ends of the execute_skill RPC are "+
				"in different modules and neither can import the other, so these keys are the "+
				"only thing keeping the two sides in agreement", c.name, c.got, c.want)
		}
	}
	// Params is omitempty on purpose: a skill with no parameters must not send
	// "params":null, because the node decodes into the same struct and a null
	// is not the same as an absent key to anything that later asks whether the
	// caller supplied one.
	if body := mustJSON(t, ExecuteSkillRequest{Key: "k", Params: json.RawMessage(`{}`)}); body != `{"key":"k","params":{}}` {
		t.Errorf("an explicitly empty params marshalled to %s; omitempty drops a zero-length "+
			"RawMessage but must keep an explicit empty object", body)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return string(b)
}

// TestBothEndsOfTheSkillRPCUseTheDeclaredShapes is the guard for the defect
// this file's other test cannot see.
//
// The two tests above pin the four JSON keys, and a mutation that renames a key
// goes red on them. But the mutation that matters most is the one that put the
// bug here in the first place: replacing `tunnel.ExecuteSkillRequest` with a
// hand-rolled anonymous struct that happens to carry the same four keys. That
// mutation compiles, the bytes on the wire are identical, and **every other test
// in the repository stays green** — measured, not assumed: both of those
// mutations were tried and both came back green.
//
// So the invariant has to be about the declaration, not about the bytes. If
// either side stops naming these two types, they become dead code and this
// fails. The count is two packages per type rather than one on purpose: the
// defect needed *both* sides to stop using them, and a guard that only required
// one would have stayed green through half of it.
//
// The shell messages further down this file are the same shape of finding and
// are deliberately not in this list — they have no users at all, which is a
// different question (is the feature meant to exist?) and is recorded in the
// ledger rather than answered here.
func TestBothEndsOfTheSkillRPCUseTheDeclaredShapes(t *testing.T) {
	for _, c := range []struct {
		sym string
		why string
	}{
		{"tunnel.ExecuteSkillRequest", "the manager marshals the body with it"},
		{"tunnel.ExecuteSkillResponse", "the manager decodes the reply with it, and the node writes the reply with it"},
	} {
		users := packagesUsing(c.sym)
		if len(users) < 2 {
			t.Errorf("%s is used by %d package(s) (%v); it needs the sending side and the "+
				"receiving side, and fewer than two means at least one of them is talking to "+
				"the node through a hand-rolled struct instead. %s",
				c.sym, len(users), users, c.why)
		}
	}
}

// packagesUsing counts the non-test packages outside this one that name a type
// through the tunnel package qualifier.
//
// Two things are deliberate here, and the first one is this repository's
// sixth version of the same mistake.
//
// **It reads the tree, not the text.** The first version of this function did
// `strings.Contains(raw, sym)`, and it reported two users for a shape the
// manager had stopped using — because the word appears in a *comment* in that
// file, in the sentence explaining why the anonymous struct was removed. A
// comment that documents the fix was counted as the fix. Decision 235 removed
// a comment-shaped false positive from the edge report for exactly this reason
// ("a call inside a comment is not a call"), and decision 237 then hit it
// again from the other direction. So the walk here goes through go/parser and
// looks for a real selector expression.
//
// **It requires the qualifier.** `ExecuteSkillRequest` also appears in this
// file's own declaration and in this file's tests, and a search that matched
// those would report users for a type nobody calls.
func packagesUsing(sym string) []string {
	name := strings.TrimPrefix(sym, "tunnel.")
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, root := range []string{"../../manager", "../../edge", "../../domains"} {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil //nolint:nilerr // an unreadable subtree is not a reason to pass
			}
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				return nil //nolint:nilerr // a file that does not parse is a build failure elsewhere
			}
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != name {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "tunnel" {
					seen[filepath.Dir(path)] = true
				}
				return true
			})
			return nil
		})
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
