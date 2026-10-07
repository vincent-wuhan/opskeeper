package investigatorreal

import (
	"testing"

	loop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

func skewRow(topic string, partition int, ratio float64) map[string]any {
	return map[string]any{
		"topic": topic, "partition": partition,
		"outstanding": 900, "outstanding_share_pct": 75.0,
		"vs_average": ratio, "under_replicated": false,
	}
}

// A group can be behind because one partition is carrying everything or
// because every partition is carrying its share. The per-group total is the
// same number in both cases, so without the skew read this branch cannot
// tell a hot key from a slow consumer — and the two have opposite fixes.
func TestProposals_MoveAPartitionTheDistributionSaysIsHot(t *testing.T) {
	actions := proposedActions(t, "mq", probeEvidence("kafka.partition_skew",
		skewRow("order.events", 0, 8.0),
		skewRow("order.events", 1, 1.0),
	))
	option, ok := actions["kafka.repartition"]
	if !ok {
		t.Fatalf("a partition at 8x the even share must produce kafka.repartition, got %v", actions)
	}
	if option.Risk != "mutating" {
		t.Errorf("risk = %q, want mutating", option.Risk)
	}
}

// An evenly distributed topic is a different incident — insufficient
// throughput — and its fix is consumers, not moving a partition. Moving one
// here would copy a partition across the cluster and change nothing.
func TestProposals_DoNotMoveAPartitionOnAnEvenlyDistributedTopic(t *testing.T) {
	actions := proposedActions(t, "mq", probeEvidence("kafka.partition_skew",
		skewRow("order.events", 0, 1.0),
		skewRow("order.events", 1, 1.1),
		skewRow("order.events", 2, 0.9),
	))
	if _, ok := actions["kafka.repartition"]; ok {
		t.Fatal("an evenly distributed topic is a throughput problem, not a hot partition")
	}
}

// The second, independent observation: a partition whose in-sync replica
// set is short is a partition a broker failure takes away. The load
// argument does not apply and the fix — moving the replica set — is the same
// operation.
func TestProposals_MoveAPartitionThatIsShortOfInSyncReplicas(t *testing.T) {
	row := skewRow("order.events", 3, 1.0)
	row["under_replicated"] = true
	row["in_sync_replicas"] = 2
	actions := proposedActions(t, "mq", probeEvidence("kafka.partition_skew", row))
	if _, ok := actions["kafka.repartition"]; !ok {
		t.Fatalf("a partition below its replica count must produce kafka.repartition, got %v", actions)
	}
}

// A row with no vs_average is a row from a tool that did not measure it, and
// treating it as zero would read "unknown" as "evenly distributed".
func TestProposals_DoNotMoveAPartitionNobodyMeasured(t *testing.T) {
	actions := proposedActions(t, "mq", probeEvidence("kafka.partition_skew",
		map[string]any{"topic": "order.events", "partition": 0},
	))
	if _, ok := actions["kafka.repartition"]; ok {
		t.Fatal("a row with no measurement is not evidence of even distribution")
	}
}

func TestSkewedPartitionRows_QualifiesOnEitherObservation(t *testing.T) {
	evidence := []loop.EvidenceItem{
		probeEvidence("kafka.partition_skew",
			skewRow("order.events", 0, 5.0), // hot
			skewRow("order.events", 1, 1.0), // even
			map[string]any{"topic": "order.events", "partition": 2, "under_replicated": true}, // short of ISR
			map[string]any{"topic": "order.events", "partition": 3},                           // unmeasured
		),
	}
	rows := loop.SkewedPartitionRows(evidence)
	if len(rows) != 2 {
		t.Fatalf("qualified %d partitions, want the hot one and the short one: %#v", len(rows), rows)
	}
	got := map[int]bool{}
	for _, row := range rows {
		partition, _ := loop.RowFloat(row, "partition")
		got[int(partition)] = true
	}
	if !got[0] || !got[2] {
		t.Errorf("qualified %v, want partitions 0 and 2", got)
	}
}
