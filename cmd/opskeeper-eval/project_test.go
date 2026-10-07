package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const contractJSON = `{
  "schema_version": "v1",
  "root_cause_object": {"kind": "pg_lock", "summary": "orders has 17 waiters"},
  "confidence": 0.86,
  "evidence_chain": [
    {"tool": "query_promql", "query": "pg_locks_waiting", "value": 17, "count": 3, "timestamp": "2026-10-01T10:00:00Z"},
    {"tool": "query_logql", "query": "lock_timeout", "value": "3 hits", "timestamp": "2026-10-01T10:00:30Z"}
  ],
  "time_window": {"start": "2026-10-01T10:00:00Z", "end": "2026-10-01T10:05:00Z"},
  "remediation_options": [
    {"action": "pg.terminate_long_tx", "target": "pg:alert-1", "risk": "mutating", "auto_approve": false}
  ]
}`

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func runProjectIn(t *testing.T, f projectFlags) (string, error) {
	t.Helper()
	dir := t.TempDir()
	if f.contractPath == "" {
		f.contractPath = writeFile(t, dir, "contract.json", contractJSON)
	}
	path := filepath.Join(dir, "stdout.txt")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	err = runProject(f, file)
	body, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(body), err
}

// A contract whose root cause has no mapping would produce a response with
// an empty root_cause_matched, which the judge scores as a diagnosis that
// found nothing. Refusing is the only reading that cannot be mistaken for a
// verdict.
func TestProjectRefusesToWriteAResponseWithNoRootCause(t *testing.T) {
	_, err := runProjectIn(t, projectFlags{})
	if err == nil {
		t.Fatal("a response with no root cause was written")
	}
	for _, want := range []string{"pg_lock", "--kind-map", "--allow-unmapped-root-cause"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestTheKindMapSuppliesTheRootCauseAndTheEvidenceSurvives(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "resp.json")
	_, err := runProjectIn(t, projectFlags{
		kindMapPath:    writeFile(t, dir, "kinds.json", `{"pg_lock": ["pg.lock_waits", "pg.active_sessions"]}`),
		detectedAt:     "2026-10-01T09:59:19Z",
		investigatedAt: "2026-10-01T10:00:00Z",
		recoveredAt:    "2026-10-01T10:01:28Z",
		out:            out,
	})
	if err != nil {
		t.Fatalf("runProject: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Response struct {
			ToolCalls []struct {
				Name string `json:"name"`
			} `json:"tool_calls"`
			RootCause    []string `json:"root_cause_matched"`
			Remediations []string `json:"remediations_matched"`
			DetectMs     int64    `json:"detect_ms"`
			RemediateMs  int64    `json:"remediate_ms"`
			ResponseHash string   `json:"response_hash"`
		} `json:"response"`
		ContractKind string `json:"contract_kind"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("envelope is not the documented shape: %v\n%s", err, raw)
	}
	if len(got.Response.RootCause) != 2 || got.Response.RootCause[0] != "pg.lock_waits" {
		t.Errorf("root_cause_matched = %v, want the kind map's symbols", got.Response.RootCause)
	}
	if len(got.Response.ToolCalls) != 2 {
		t.Errorf("tool_calls = %d, want both evidence items", len(got.Response.ToolCalls))
	}
	if len(got.Response.Remediations) != 1 || got.Response.Remediations[0] != "pg.terminate_long_tx" {
		t.Errorf("remediations_matched = %v, want the contract's own action", got.Response.Remediations)
	}
	if got.Response.DetectMs != 41_000 || got.Response.RemediateMs != 88_000 {
		t.Errorf("durations = %d/%d, want 41000/88000", got.Response.DetectMs, got.Response.RemediateMs)
	}
	if got.Response.ResponseHash == "" {
		t.Error("response_hash is empty; the projection is not directly scorable")
	}
	if got.ContractKind != "pg_lock" {
		t.Errorf("contract_kind = %q, want pg_lock", got.ContractKind)
	}
}

// --bare has to produce exactly what judge --response decodes, because that
// is the composition the whole package exists to make possible.
func TestBareOutputIsExactlyAJudgeAgentResponse(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "bare.json")
	if _, err := runProjectIn(t, projectFlags{
		kindMapPath: writeFile(t, dir, "kinds.json", `{"pg_lock": ["pg.lock_waits"]}`),
		bare:        true,
		out:         out,
	}); err != nil {
		t.Fatalf("runProject: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// judge decodes strictly; anything extra here is a parse error there.
	var round map[string]any
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	if _, wrapped := round["response"]; wrapped {
		t.Errorf("--bare still emitted the diagnostic envelope:\n%s", raw)
	}
	if _, err := loadAgentResponse(out); err != nil {
		t.Errorf("judge could not read the --bare output: %v", err)
	}
}

func TestAllowUnmappedStampsTheKindIntoTheArtifact(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "resp.json")
	if _, err := runProjectIn(t, projectFlags{allowUnmapped: true, out: out}); err != nil {
		t.Fatalf("runProject: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		UnmappedRootCause string `json:"unmapped_root_cause"`
		UnmappedReason    string `json:"unmapped_root_cause_reason"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.UnmappedRootCause != "pg_lock" {
		t.Errorf("unmapped_root_cause = %q, want pg_lock", got.UnmappedRootCause)
	}
	if !strings.Contains(got.UnmappedReason, "not a symbol the judge") {
		t.Errorf("the reason does not explain the consequence: %q", got.UnmappedReason)
	}
}

// A field the build does not know is a contract it cannot project. Decoding
// leniently would drop the field and emit a response that looks complete
// while missing whatever it carried.
func TestAFieldThisBuildDoesNotKnowIsRejected(t *testing.T) {
	dir := t.TempDir()
	body := strings.Replace(contractJSON, `"confidence": 0.86`, `"confidence": 0.86, "foresight": 3`, 1)
	_, err := runProjectIn(t, projectFlags{contractPath: writeFile(t, dir, "c.json", body)})
	if err == nil {
		t.Fatal("a contract with an unknown field was projected anyway")
	}
	if !strings.Contains(err.Error(), "foresight") {
		t.Errorf("error %q does not name the offending field", err)
	}
}

// A mistyped clock would otherwise become "not observed", which the judge
// scores as a perfect time_efficiency.
func TestAMistypedTimestampIsAnErrorRatherThanAMissingMark(t *testing.T) {
	_, err := runProjectIn(t, projectFlags{
		kindMapPath: writeFile(t, t.TempDir(), "k.json", `{"pg_lock": ["pg.lock_waits"]}`),
		detectedAt:  "yesterday",
	})
	if err == nil {
		t.Fatal("a mistyped timestamp was accepted")
	}
	if !strings.Contains(err.Error(), "--detected-at") {
		t.Errorf("error %q does not name the flag at fault", err)
	}
}

func TestAMalformedKindMapIsAnError(t *testing.T) {
	dir := t.TempDir()
	_, err := runProjectIn(t, projectFlags{kindMapPath: writeFile(t, dir, "k.json", `["pg_lock"]`)})
	if err == nil {
		t.Fatal("a kind map that is not an object was accepted")
	}
}
