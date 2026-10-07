package projection

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func doc() *Doc {
	start := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	return &Doc{
		SchemaVersion:   "v1",
		RootCauseObject: &RootCauseObject{Kind: "pg_lock", Summary: "table lock on orders"},
		Confidence:      0.82,
		EvidenceChain: []EvidenceItem{
			{Tool: "query_promql", Query: "pg_locks", Value: 17, Timestamp: start},
			{Tool: "query_logql", Query: "lock_timeout", Value: "3 hits", Timestamp: start},
		},
		TimeWindow: TimeWindow{Start: start, End: start.Add(5 * time.Minute)},
		RemediationOptions: []RemediationOption{
			{Action: "pg.terminate_long_tx", Target: "pg:alert-1", Risk: "mutating"},
			{Action: "pg.kill_backend", Target: "pg:alert-1", Risk: "mutating"},
		},
	}
}

var base = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func timing() Timing {
	return Timing{
		DetectedAt:     base,
		InvestigatedAt: base.Add(41 * time.Second),
		RecoveredAt:    base.Add(41*time.Second + 88*time.Second),
	}
}

// The unmapped-kind case is the whole reason this package exists: a contract
// that names its root cause in a namespace the judge cannot read must not be
// projected into a fabricated "no root cause found".
func TestAnUnmappableKindIsReportedRatherThanTurnedIntoAWrongAnswer(t *testing.T) {
	res, err := FromContract(doc(), timing(), nil)
	if err != nil {
		t.Fatalf("FromContract: %v", err)
	}
	if len(res.Response.RootCause) != 0 {
		t.Errorf("RootCause = %v, want empty: no resolver ran, so nothing was known", res.Response.RootCause)
	}
	if res.UnmappedKind != "pg_lock" {
		t.Errorf("UnmappedKind = %q, want pg_lock", res.UnmappedKind)
	}
	if res.RootCauseIsScorable() {
		t.Error("RootCauseIsScorable() = true for a response carrying no root cause")
	}
	if !strings.Contains(res.UnmappedReason, "pg_lock") {
		t.Errorf("UnmappedReason does not name the kind: %q", res.UnmappedReason)
	}
	// A caller reading only the response would see a diagnosis that found
	// nothing. The report is what distinguishes that from a real miss.
	if res.Response.ResponseHash == "" {
		t.Error("the response hash was not computed; the projection is not directly scorable")
	}
}

func TestAResolverSuppliesTheRootCause(t *testing.T) {
	resolve := func(kind string) ([]string, bool) {
		if kind != "pg_lock" {
			return nil, false
		}
		return []string{"pg.lock_waits", "pg.active_sessions"}, true
	}
	res, err := FromContract(doc(), timing(), resolve)
	if err != nil {
		t.Fatalf("FromContract: %v", err)
	}
	if got := res.Response.RootCause; len(got) != 2 || got[0] != "pg.lock_waits" {
		t.Errorf("RootCause = %v, want the resolver's symbols", got)
	}
	if res.UnmappedKind != "" {
		t.Errorf("UnmappedKind = %q, want empty when the resolver answered", res.UnmappedKind)
	}
	if !res.RootCauseIsScorable() {
		t.Error("RootCauseIsScorable() = false after a successful resolve")
	}
}

// A resolver that returns ok but nothing is still no answer.
func TestAResolverThatReturnsNothingIsTreatedAsUnmapped(t *testing.T) {
	res, err := FromContract(doc(), timing(), func(string) ([]string, bool) { return nil, true })
	if err != nil {
		t.Fatal(err)
	}
	if res.UnmappedKind == "" {
		t.Error("a resolver returning no symbols was taken as an answer")
	}
	if res.RootCauseIsScorable() {
		t.Error("RootCauseIsScorable() = true with no symbols")
	}
}

func TestRemediationActionsAreCarriedThroughInOrderWithoutDuplicates(t *testing.T) {
	d := doc()
	d.RemediationOptions = append(d.RemediationOptions, RemediationOption{Action: "pg.terminate_long_tx"})
	res, err := FromContract(d, timing(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Response.Remediations
	if len(got) != 2 || got[0] != "pg.terminate_long_tx" || got[1] != "pg.kill_backend" {
		t.Errorf("Remediations = %v, want both actions once, in order", got)
	}
}

// The evidence chain is what an LLM judge reasons over, so dropping it would
// leave the model scoring a bare conclusion.
func TestTheEvidenceChainBecomesToolCalls(t *testing.T) {
	res, err := FromContract(doc(), timing(), nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := res.Response.ToolCalls
	if len(calls) != 2 {
		t.Fatalf("got %d tool calls, want 2", len(calls))
	}
	if calls[0].Name != "query_promql" {
		t.Errorf("calls[0].Name = %q, want query_promql", calls[0].Name)
	}
	var args map[string]string
	if err := json.Unmarshal(calls[0].Args, &args); err != nil {
		t.Fatalf("args are not an object: %v", err)
	}
	if args["query"] != "pg_locks" {
		t.Errorf("args = %v, want the contract's query preserved", args)
	}
	if len(calls[0].Result) == 0 {
		t.Error("the evidence value was dropped")
	}
}

func TestTimingBecomesTheTwoDurationsTheJudgeScores(t *testing.T) {
	res, err := FromContract(doc(), timing(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Response.DetectMs != 41_000 {
		t.Errorf("DetectMs = %d, want 41000", res.Response.DetectMs)
	}
	if res.Response.RemediateMs != 88_000 {
		t.Errorf("RemediateMs = %d, want 88000", res.Response.RemediateMs)
	}
}

// An unobserved mark must not read as "instantly fast": time_efficiency
// scores 1.0 for a zero duration, so a missing clock would hand out a full
// mark for a run nobody timed.
func TestAnUnobservedOrBackwardsIntervalScoresZeroRatherThanFullMarks(t *testing.T) {
	for name, c := range map[string]struct {
		timing                    Timing
		wantDetect, wantRemediate int64
	}{
		"no marks at all":       {timing: Timing{}},
		"detected only":         {timing: Timing{DetectedAt: base}},
		"recovered only":        {timing: Timing{RecoveredAt: base.Add(time.Minute)}},
		"investigated only":     {timing: Timing{InvestigatedAt: base}},
		"backwards remediation": {timing: Timing{DetectedAt: base, InvestigatedAt: base.Add(time.Minute), RecoveredAt: base}, wantDetect: 60_000},
		"backwards detection":   {timing: Timing{DetectedAt: base.Add(time.Minute), InvestigatedAt: base, RecoveredAt: base.Add(2 * time.Minute)}, wantRemediate: 120_000},
	} {
		res, err := FromContract(doc(), c.timing, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Response.DetectMs != c.wantDetect || res.Response.RemediateMs != c.wantRemediate {
			t.Errorf("%s: got DetectMs=%d RemediateMs=%d, want %d and %d",
				name, res.Response.DetectMs, res.Response.RemediateMs, c.wantDetect, c.wantRemediate)
		}
	}
}

// The negative-duration guard is the reason the previous test is worth
// having: a negative elapsed time would be scored by the judge as
// infinitely fast, so time_efficiency would hand out a full mark to a run
// whose clock is broken.
func TestNoTimingEverProducesANegativeDuration(t *testing.T) {
	marks := []time.Time{{}, base, base.Add(time.Minute), base.Add(-time.Hour)}
	for _, d := range marks {
		for _, i := range marks {
			for _, r := range marks {
				res, err := FromContract(doc(), Timing{DetectedAt: d, InvestigatedAt: i, RecoveredAt: r}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if res.Response.DetectMs < 0 || res.Response.RemediateMs < 0 {
					t.Fatalf("d=%v i=%v r=%v produced DetectMs=%d RemediateMs=%d",
						d, i, r, res.Response.DetectMs, res.Response.RemediateMs)
				}
			}
		}
	}
}

// A contract with no conclusion is not a response to score. Returning a
// blank AgentResponse here would be scored as a total failure, which reads
// as a verdict on an agent that may never have run.
func TestAContractWithNoConclusionIsAnErrorNotABlankResponse(t *testing.T) {
	cases := map[string]*Doc{
		"nil document":     nil,
		"no root cause":    {RemediationOptions: []RemediationOption{{Action: "pg.kill_backend"}}},
		"blank kind":       {RootCauseObject: &RootCauseObject{}, RemediationOptions: []RemediationOption{{Action: "pg.kill_backend"}}},
		"nothing proposed": {RootCauseObject: &RootCauseObject{Kind: "pg_lock"}},
	}
	for name, d := range cases {
		if _, err := FromContract(d, timing(), nil); err == nil {
			t.Errorf("%s: a contract with no conclusion was projected anyway", name)
		}
	}
}

// Options with a blank action are dropped rather than becoming empty strings
// that can never match anything and only dilute the comparison.
func TestABlankActionIsDropped(t *testing.T) {
	d := doc()
	d.RemediationOptions = []RemediationOption{{Action: ""}, {Action: "pg.kill_backend"}}
	res, err := FromContract(d, timing(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Response.Remediations) != 1 || res.Response.Remediations[0] != "pg.kill_backend" {
		t.Errorf("Remediations = %v, want just the named action", res.Response.Remediations)
	}
}
