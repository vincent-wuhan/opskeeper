package axes

import (
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
)

// The derivation moved out of cmd/opskeeper-eval together with the code, so its
// tests moved with it. A library whose behaviour is only reachable through one
// command's test file has a coverage gap shaped exactly like the bug this move
// fixes: the second caller (core/harness/runner) had no way to see any of it.

func TestTheLocusComesFromIdentityParametersNotKnobs(t *testing.T) {
	c := &schema.Case{
		ID: "pg/lock-waits",
		Inject: []schema.InjectStep{{
			Type: "pg.inject_lock_chain",
			Params: map[string]interface{}{
				"table": "orders",
				// Knobs must not become loci: no correct answer repeats "4",
				// so requiring it would fail answers for saying the right
				// thing.
				"cores":       4,
				"target_load": 98,
			},
		}},
		Expect: schema.Expect{RootCauseLines: []string{"pg.lock_waits"}},
	}
	got := Of(c)
	if !containsToken(got.Locus, "orders") || !containsToken(got.Locus, "pg") {
		t.Fatalf("locus = %v, want the table and the family", got.Locus)
	}
	if containsToken(got.Locus, "4") || containsToken(got.Locus, "98") {
		t.Errorf("a knob became a locus: %v", got.Locus)
	}
	if got.Thin {
		t.Errorf("locus with a named table reported as coarsened: %v", got.Locus)
	}
}

func TestATwoCharacterFamilyIsNotDropped(t *testing.T) {
	// "pg" and "mq" are exactly the tokens the minimum-length rule exists to
	// drop from prose, and dropping the family would leave those five cases
	// with no locus at all.
	for _, id := range []string{"pg/lock-waits", "mq/broker-down"} {
		family := strings.SplitN(id, "/", 2)[0]
		got := Of(&schema.Case{
			ID:     id,
			Expect: schema.Expect{RootCauseLines: []string{family + ".something"}},
		})
		if !containsToken(got.Locus, family) {
			t.Errorf("%s: family %q missing from locus %v", id, family, got.Locus)
		}
	}
}

func TestACoarsenedLocusIsReportedRatherThanFabricated(t *testing.T) {
	got := Of(&schema.Case{
		ID: "redis/memory-burst",
		Inject: []schema.InjectStep{{
			Type:   "redis.inject_memory_burst",
			Params: map[string]interface{}{"maxmemory_mb": 1024, "fill_percent": 99},
		}},
		Expect: schema.Expect{RootCauseLines: []string{"redis.memory_usage"}},
	})
	if len(got.Locus) != 1 || got.Locus[0] != "redis" {
		t.Fatalf("locus = %v, want the family alone", got.Locus)
	}
	if !got.Thin {
		t.Error("a family-only locus was not reported as coarsened")
	}
}

func TestAFlowListParameterIsALocus(t *testing.T) {
	got := Of(&schema.Case{
		ID: "pg/long-running-tx",
		Inject: []schema.InjectStep{{
			Type:   "pg.begin_txn_hold",
			Params: map[string]interface{}{"tables": []interface{}{"orders"}},
		}},
		Expect: schema.Expect{RootCauseLines: []string{"pg.long_running_txns"}},
	})
	if !containsToken(got.Locus, "orders") {
		t.Errorf("locus = %v, want the table from the inline list", got.Locus)
	}
}
func containsToken(tokens []string, want string) bool {
	for _, token := range tokens {
		if token == want {
			return true
		}
	}
	return false
}
