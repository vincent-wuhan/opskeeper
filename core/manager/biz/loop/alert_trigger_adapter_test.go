package loop

import (
	"context"
	"testing"
)

func ruleWith(t *testing.T, conds string) *AlertRule {
	t.Helper()
	return &AlertRule{ConditionsJSON: conds}
}

// TestTriggerFromRuleReadsTheOneComparison: a runbook's "when" is the
// comparison the operator wrote, read verbatim.
func TestTriggerFromRuleReadsTheOneComparison(t *testing.T) {
	got, ok := triggerFromRule(ruleWith(t, `[{"metric":"node_disk_used_ratio","operator":">","threshold":0.92}]`))
	if !ok {
		t.Fatal("triggerFromRule refused a single above-comparison")
	}
	if got.Metric != "node_disk_used_ratio" || got.Threshold != 0.92 {
		t.Fatalf("trigger = %+v, want node_disk_used_ratio > 0.92", got)
	}
}

// TestTriggerFromRuleDeclinesSeveralConditions: choosing one of several
// comparisons would fire on a condition narrower than what was reviewed.
func TestTriggerFromRuleDeclinesSeveralConditions(t *testing.T) {
	_, ok := triggerFromRule(ruleWith(t, `[{"metric":"a","operator":">","threshold":1},{"metric":"b","operator":">","threshold":2}]`))
	if ok {
		t.Fatal("triggerFromRule accepted a two-condition rule, want a refusal")
	}
}

// TestTriggerFromRuleDeclinesABelowOperator: the node evaluates metric_above,
// and flipping "<" into ">" would fire at the opposite moment.
func TestTriggerFromRuleDeclinesABelowOperator(t *testing.T) {
	_, ok := triggerFromRule(ruleWith(t, `[{"metric":"node_filesystem_avail_bytes","operator":"<","threshold":1000}]`))
	if ok {
		t.Fatal("triggerFromRule accepted a below-comparison, want a refusal")
	}
}

// TestTriggerFromRuleDeclinesMalformed: unreadable conditions are a refusal,
// not an invented trigger.
func TestTriggerFromRuleDeclinesMalformed(t *testing.T) {
	for _, conds := range []string{"", "not json", `[{"operator":">","threshold":1}]`} {
		if _, ok := triggerFromRule(ruleWith(t, conds)); ok {
			t.Fatalf("triggerFromRule accepted %q, want a refusal", conds)
		}
	}
	if _, ok := triggerFromRule(nil); ok {
		t.Fatal("triggerFromRule accepted a nil rule")
	}
}

// TestAutonomyTriggerForDeclinesANonNumericIncidentID: harness and
// chat-promoted runs carry their own ids, and a rule lookup miss is not an
// error.
func TestAutonomyTriggerForDeclinesANonNumericIncidentID(t *testing.T) {
	a := &AlertTriggerAdapter{}
	if _, ok := a.AutonomyTriggerFor(context.Background(), "", "inc-pg-lrtx-001"); ok {
		t.Fatal("a non-numeric incident id produced a trigger")
	}
}
