package redis

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// The policy classifier decides whether Redis is counting access frequency
// at all, and it is the one piece of this diagnostic that decides between
// "here is a ranking" and "there is nothing to rank". The spellings below
// are the ones that actually appear in the wild, plus the two that must not
// be mistaken for each other.
func TestLFUPolicyRecognisesTheSpellingsThatCount(t *testing.T) {
	for _, tc := range []struct {
		policy string
		want   bool
		why    string
	}{
		{"allkeys-lfu", true, "the canonical LFU policy"},
		{"volatile-lfu", true, "LFU restricted to keys with a TTL"},
		{"allkeys-lfu", true, "the default spelling of the same policy"},
		{"ALLKEYS-LFU", true, "Redis echoes the policy verbatim, so case is not a signal"},
		{"  allkeys-lfu  ", true, "surrounding whitespace must not change the answer"},
		{"noeviction", false, "counts nothing — reporting a ranking here would be invented"},
		{"allkeys-lru", false, "LRU tracks recency, not frequency; it is not a weaker lfu"},
		{"volatile-lru", false, "same, for TTL keys"},
		{"allkeys-random", false, "counts nothing"},
		{"", false, "an unreadable policy is not evidence of counting"},
	} {
		if got := lfuPolicy(tc.policy); got != tc.want {
			t.Errorf("lfuPolicy(%q) = %v, want %v — %s", tc.policy, got, tc.want, tc.why)
		}
	}
}

// A server that cannot report access frequency must not make this
// diagnostic fail, and — the part that matters — it must not come back
// with an empty ranking. An empty list of hot keys reads as "nothing is
// hot", which is a finding an operator would act on. The honest answer is
// that the question could not be asked, and it has to be distinguishable
// from a genuinely quiet instance.
func TestDiagnose_HotKeys_SaysItCannotReadFrequencyRatherThanReportingNothingIsHot(t *testing.T) {
	a, mr := connected(t)
	for i := 0; i < 5; i++ {
		mr.Set(string(rune('a'+i)), "v")
	}
	r, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: catHotKeys})
	if err != nil {
		t.Fatalf("a server that does not implement OBJECT FREQ must not fail the diagnostic: %v", err)
	}
	if len(r.Findings) != 1 {
		t.Fatalf("expected one finding saying frequency is unreadable, got %d: %v", len(r.Findings), r.Findings)
	}
	row := r.Findings[0]
	if readable, ok := row["frequency_readable"].(bool); !ok || readable {
		t.Errorf("the finding does not say frequency_readable=false, so an empty ranking would read as "+
			"\"nothing is hot\": %v", row)
	}
	// The note is what the model reads and paraphrases, so the reason has
	// to be in it rather than only in a field.
	if !strings.Contains(r.Summary, "does not report it") {
		t.Errorf("Summary must say the frequency could not be read: %q", r.Summary)
	}
}

// Whatever comes back, the sampling caveat travels with it. This is the
// same promise big_keys makes, and it is the reason the diagnostic is not
// allowed to present itself as a ranking.
func TestDiagnose_HotKeys_NeverClaimsToBeAGlobalRanking(t *testing.T) {
	a, mr := connected(t)
	mr.Set("k", "v")
	r, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: catHotKeys})
	if err != nil {
		t.Fatalf("Diagnose(hot_keys): %v", err)
	}
	if strings.Contains(r.Summary, "the hottest key") && !strings.Contains(r.Summary, "not a global ranking") {
		t.Errorf("Summary presents a sample as a ranking: %q", r.Summary)
	}
}

// The tool is registered, and it is graded as a read. The upcall channel
// re-derives each tool's class before dispatching, so a mislabelled tool
// here is not cosmetic: it decides whether a node's agent can reach it at
// all, and a write-graded read would be refused on the only channel a node
// has.
func TestHotKeysIsRegisteredAsAReadOnlyDiagnostic(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	tool, ok := reg.GetTool("redis.hot_keys")
	if !ok {
		t.Fatal("redis.hot_keys is not registered, so no package ships it and the redis/hot-key case stays uncoverable")
	}
	if tool.RiskLevel != adapter.RiskL1Diagnostic {
		t.Errorf("risk level = %s, want %s", tool.RiskLevel, adapter.RiskL1Diagnostic)
	}
	// L0 and L1 are the grades the upcall channel carries; anything from L2
	// up is refused there because that channel has no approval queue. The
	// canonical read-classification lives in the toolset package, which
	// imports this one, so the check is spelled out here rather than
	// imported — and the toolset generator is the other half of the gate,
	// since it ships only L0/L1 tools into a node's package.
	if tool.RiskLevel != adapter.RiskL0ReadOnly && tool.RiskLevel != adapter.RiskL1Diagnostic {
		t.Errorf("risk level %s is not a read grade, so the agent upcall channel would refuse it", tool.RiskLevel)
	}
	// A registered read on an adapter with no client says so, rather than
	// returning empty rows that read as "no key is hot".
	if _, err := tool.Handler(context.Background(), map[string]interface{}{}); !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("redis.hot_keys on an unconnected adapter = %v, want ErrNotConnected", err)
	}
}
