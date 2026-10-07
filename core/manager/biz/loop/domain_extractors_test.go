package loop

import (
	"context"
	"strings"
	"testing"
)

func probeItem(tool string, rows ...map[string]any) EvidenceItem {
	return EvidenceItem{Tool: tool, Value: map[string]any{"rows": rows, "count": len(rows)}}
}

func resolveProbe(t *testing.T, action string, required []string, evidence ...EvidenceItem) (map[string]any, error) {
	t.Helper()
	r := EvidenceArgResolver{Causes: scriptedRootCauseLoader{rc: causeWithEvidence(evidence...)}}
	return r.Resolve(context.Background(), RemediationRequest{
		IncidentID: "inc-1", Option: opt(action, "mutating", false),
	}, ToolSpec{Name: action, RequiredArgs: required})
}

func TestDomainExtractors_NamesTheObjectTheDomainFlagged(t *testing.T) {
	cases := []struct {
		name     string
		action   string
		required []string
		evidence []EvidenceItem
		wantKey  string
		wantVal  string
	}{
		{
			name: "the bloating table", action: "pg.vacuum_table", required: []string{"table"},
			evidence: []EvidenceItem{probeItem("pg.table_bloat",
				map[string]any{"schemaname": "public", "table": "order_events", "dead_pct": 61.0})},
			wantKey: "table", wantVal: "order_events",
		},
		{
			name: "the stuck deployment", action: "k8s.rollout_undo", required: []string{"deployment"},
			evidence: []EvidenceItem{probeItem("k8s.rollout_status",
				map[string]any{"name": "order-svc", "namespace": "prod", "rollout_complete": false})},
			wantKey: "deployment", wantVal: "order-svc",
		},
		{
			name: "the NotReady node", action: "k8s.drain", required: []string{"node"},
			evidence: []EvidenceItem{probeItem("k8s.node_list",
				map[string]any{"name": "worker-1", "status": "Ready"},
				map[string]any{"name": "worker-3", "status": "NotReady"})},
			wantKey: "node", wantVal: "worker-3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, err := resolveProbe(t, tc.action, tc.required, tc.evidence...)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got := args[tc.wantKey]; got != tc.wantVal {
				t.Fatalf("%s = %#v, want %q", tc.wantKey, got, tc.wantVal)
			}
		})
	}
}

// EXPLAIN is proposed for the slowest query, and pg_stat_statements already
// ordered them. Picking the worst is reading the order the database
// reported, not choosing.
func TestDomainExtractors_ExplainsTheSlowestRecordedQuery(t *testing.T) {
	args, err := resolveProbe(t, "pg.explain_query", []string{"query"},
		probeItem("pg.slow_log",
			map[string]any{"queryid": 7, "mean_exec_time": 4200.0, "query": "SELECT count(*) FROM order_events"},
			map[string]any{"queryid": 8, "mean_exec_time": 12.0, "query": "SELECT 1"},
		))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := args["query"]; got != "SELECT count(*) FROM order_events" {
		t.Fatalf("query = %#v, want the slowest one", got)
	}
}

// Two tables above the floor means choosing between a 61% dead table and a
// 25% dead one — a judgement about blast radius, which is what the approval
// step exists for.
func TestDomainExtractors_RefusesToChooseBetweenTwoBloatingTables(t *testing.T) {
	_, err := resolveProbe(t, "pg.vacuum_table", []string{"table"},
		probeItem("pg.table_bloat",
			map[string]any{"schemaname": "public", "table": "order_events", "dead_pct": 61.0},
			map[string]any{"schemaname": "public", "table": "sessions", "dead_pct": 25.0},
		))
	if err == nil {
		t.Fatal("two bloating tables must not resolve to one")
	}
	for _, want := range []string{"order_events", "sessions"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name both candidates; %q does not mention %q", err.Error(), want)
		}
	}
}

func TestDomainExtractors_RefusesBetweenTwoNotReadyNodes(t *testing.T) {
	_, err := resolveProbe(t, "k8s.drain", []string{"node"},
		probeItem("k8s.node_list",
			map[string]any{"name": "worker-3", "status": "NotReady"},
			map[string]any{"name": "worker-7", "status": "NotReady"},
		))
	if err == nil {
		t.Fatal("draining one of two NotReady nodes by guess is not a remediation")
	}
}

// The message must say which probe would have supplied the value, so the
// operator knows the difference between "the node is fine" and "nobody
// asked".
func TestDomainExtractors_RefusalNamesTheProbeThatWouldHaveAnswered(t *testing.T) {
	_, err := resolveProbe(t, "pg.vacuum_table", []string{"table"},
		EvidenceItem{Tool: "query_promql", Value: `[{"value":"3"}]`})
	if err == nil {
		t.Fatal("a metric-only chain cannot name a table")
	}
	if !strings.Contains(err.Error(), "pg.table_bloat") {
		t.Errorf("the refusal must name the probe: %q", err.Error())
	}
}

// A rollout_status probe that ignored only_stuck would otherwise put a
// healthy deployment in front of a rollback proposal.
func TestDomainExtractors_IgnoresCompletedRolloutsEvenIfTheProbeDidNot(t *testing.T) {
	_, err := resolveProbe(t, "k8s.rollout_undo", []string{"deployment"},
		probeItem("k8s.rollout_status",
			map[string]any{"name": "order-svc", "namespace": "prod", "rollout_complete": true},
		))
	if err == nil {
		t.Fatal("a completed rollout must not produce a rollback dispatch")
	}
}

func TestDomainExtractors_IgnoresReadyNodesEvenIfTheProbeDidNot(t *testing.T) {
	_, err := resolveProbe(t, "k8s.uncordon", []string{"node"},
		probeItem("k8s.node_list", map[string]any{"name": "worker-1", "status": "Ready"}))
	if err == nil {
		t.Fatal("uncordoning a Ready node is not a remediation")
	}
}

// The contract round-trips evidence through JSON, so the rows arrive as
// []any with float64 numbers on the production path.
func TestDomainExtractors_SurviveAJSONRoundTrip(t *testing.T) {
	// A JSON round trip turns the rows slice into []any; the envelope around
	// it survives as a map. This is the shape a contract read back from the
	// database actually has.
	roundTripped := EvidenceItem{
		Tool: "k8s.node_list",
		Value: map[string]any{
			"rows":  []any{map[string]any{"name": "worker-3", "status": "NotReady"}},
			"count": float64(1),
		},
	}
	r := EvidenceArgResolver{Causes: scriptedRootCauseLoader{rc: causeWithEvidence(roundTripped)}}
	args, err := r.Resolve(context.Background(), RemediationRequest{
		Option: opt("k8s.drain", "mutating", false),
	}, ToolSpec{Name: "k8s.drain", RequiredArgs: []string{"node"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := args["node"]; got != "worker-3" {
		t.Fatalf("node = %#v", got)
	}
}

// Some contract writers store the evidence value as the raw JSON text it
// arrived as. A reader that only understood the decoded map would fail on
// exactly that path.
func TestDomainExtractors_ReadsRowsStoredAsRawJSON(t *testing.T) {
	raw := EvidenceItem{
		Tool:  "pg.table_bloat",
		Value: `[{"schemaname":"public","table":"order_events","dead_pct":61}]`,
	}
	r := EvidenceArgResolver{Causes: scriptedRootCauseLoader{rc: causeWithEvidence(raw)}}
	args, err := r.Resolve(context.Background(), RemediationRequest{
		Option: opt("pg.vacuum_table", "mutating", false),
	}, ToolSpec{Name: "pg.vacuum_table", RequiredArgs: []string{"table"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := args["table"]; got != "order_events" {
		t.Fatalf("table = %#v", got)
	}
}

// The two halves of the closed loop must agree about what the evidence
// said. The investigator proposes because a filter matched; the resolver
// refuses to name a target unless the same filter matches.
func TestDomainFiltersAgreeBetweenProposalAndDispatch(t *testing.T) {
	evidence := []EvidenceItem{
		probeItem("pg.table_bloat", map[string]any{"table": "order_events", "dead_pct": 61.0}),
		probeItem("k8s.rollout_status", map[string]any{"name": "order-svc", "rollout_complete": false}),
		probeItem("k8s.node_list", map[string]any{"name": "worker-3", "status": "NotReady"}),
	}
	for action, rows := range map[string][]map[string]any{
		"pg.vacuum_table":  BloatedTableRows(evidence),
		"k8s.rollout_undo": StuckRolloutRows(evidence),
		"k8s.uncordon":     NotReadyNodeRows(evidence),
		"k8s.drain":        NotReadyNodeRows(evidence),
	} {
		if len(rows) == 0 {
			t.Errorf("%s has an extractor whose filter the proposal path never satisfies", action)
		}
		if _, err := resolveProbe(t, action, nil, evidence...); err != nil {
			t.Errorf("%s: the proposal was made but the dispatch refuses: %v", action, err)
		}
	}
}

func TestDomainExtractors_NamesTheSingleHotProcess(t *testing.T) {
	args, err := resolveProbe(t, "host.kill_process", []string{"pid"},
		probeItem("host.top_processes",
			map[string]any{"pid": 4211, "comm": "runaway-worker", "user": "root", "pcpu": 98.5},
		))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, ok := args["pid"].(int64); !ok || got != 4211 {
		t.Fatalf("pid = %#v, want 4211", args["pid"])
	}
}

// Two processes above the floor is the normal shape of a busy host, and
// choosing between them is a judgement about which workload matters — which
// is what the approval step is for.
func TestDomainExtractors_RefusesToChooseBetweenTwoHotProcesses(t *testing.T) {
	_, err := resolveProbe(t, "host.kill_process", []string{"pid"},
		probeItem("host.top_processes",
			map[string]any{"pid": 4211, "comm": "runaway-worker", "pcpu": 98.5},
			map[string]any{"pid": 4300, "comm": "batch-importer", "pcpu": 61.0},
		))
	if err == nil {
		t.Fatal("two hot processes must not resolve to one")
	}
	for _, want := range []string{"4211", "4300", "runaway-worker", "batch-importer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name the candidates; %q does not mention %q", err.Error(), want)
		}
	}
}

func TestDomainExtractors_RefusesWhenNoProcessIsHot(t *testing.T) {
	_, err := resolveProbe(t, "host.kill_process", []string{"pid"}, probeItem("host.top_processes"))
	if err == nil {
		t.Fatal("an empty process list cannot name a process to kill")
	}
	if !strings.Contains(err.Error(), "no single process") {
		t.Errorf("the refusal must say the load is not one process: %q", err.Error())
	}
}

// The directory to reclaim from is the *subject*: the alert knows which
// filesystem filled up. The probe only establishes that there is something
// old enough under it to remove.
func TestDomainExtractors_ReclaimPathComesFromTheAlertNotTheProbe(t *testing.T) {
	subject := NewSubjectEvidenceItem("alert-9", "host:alert-9", map[string]string{"mount": "/data"})
	args, err := resolveProbe(t, "host.remove_old_logs", []string{"path"},
		subject, probeItem("host.old_log_files",
			map[string]any{"path": "/data/logs/app-2024.log", "size_bytes": 1048576.0},
		))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := args["path"]; got != "/data" {
		t.Fatalf("path = %#v, want the mount the alert named", got)
	}
}

func TestDomainExtractors_RefusesReclaimWithoutBothTheMountAndTheFiles(t *testing.T) {
	// The mount is missing.
	_, err := resolveProbe(t, "host.remove_old_logs", []string{"path"},
		probeItem("host.old_log_files", map[string]any{"path": "/data/app.log", "size_bytes": 1.0}))
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("without a recorded mount the directory is unknown; got %v", err)
	}
	// The files are missing.
	_, err = resolveProbe(t, "host.remove_old_logs", []string{"path"},
		NewSubjectEvidenceItem("alert-9", "host:alert-9", map[string]string{"mount": "/data"}))
	if err == nil {
		t.Fatal("a mount with nothing old on it must not be dispatched")
	}
}

// The two halves of the closed loop must agree about what the evidence said.
func TestDomainFiltersAgreeForHost(t *testing.T) {
	evidence := []EvidenceItem{
		probeItem("host.top_processes", map[string]any{"pid": 4211, "comm": "runaway", "pcpu": 98.5}),
		probeItem("host.old_log_files", map[string]any{"path": "/data/app.log", "size_bytes": 10.0}),
		NewSubjectEvidenceItem("a", "host:a", map[string]string{"mount": "/data"}),
	}
	for action, rows := range map[string][]map[string]any{
		"host.kill_process":    BusyProcessRows(evidence),
		"host.remove_old_logs": ReclaimableLogRows(evidence),
	} {
		if len(rows) == 0 {
			t.Errorf("%s has an extractor whose filter the proposal path never satisfies", action)
		}
		if _, err := resolveProbe(t, action, nil, evidence...); err != nil {
			t.Errorf("%s: the proposal was made but the dispatch refuses: %v", action, err)
		}
	}
}

func TestDomainExtractors_UsesTheObservedSizeAsTheDeletionFloor(t *testing.T) {
	args, err := resolveProbe(t, "redis.scan_and_delete", []string{"min_bytes"},
		probeItem("redis.big_keys",
			map[string]any{"key": "test:bigkey:hot", "type": "hash", "bytes": float64(50 * 1024 * 1024)},
		))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, ok := args["min_bytes"].(int64); !ok || got != 50*1024*1024 {
		t.Fatalf("min_bytes = %#v, want the size the scan reported", args["min_bytes"])
	}
}

// Picking the smallest of several oversized keys would set a floor that
// sweeps up every key of that size across the keyspace, and the evidence
// did not observe them.
func TestDomainExtractors_RefusesToPickAFloorAmongSeveralOversizedKeys(t *testing.T) {
	_, err := resolveProbe(t, "redis.scan_and_delete", []string{"min_bytes"},
		probeItem("redis.big_keys",
			map[string]any{"key": "a:big", "bytes": float64(60 * 1024 * 1024)},
			map[string]any{"key": "b:big", "bytes": float64(20 * 1024 * 1024)},
		))
	if err == nil {
		t.Fatal("several oversized keys must not resolve to one floor")
	}
	for _, want := range []string{"a:big", "b:big", "min_bytes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q: %q", want, err.Error())
		}
	}
}

// The floor is an observation threshold; below it nothing is a suspect and
// there is nothing to delete.
func TestDomainExtractors_RefusesWhenNoKeyIsOversized(t *testing.T) {
	_, err := resolveProbe(t, "redis.scan_and_delete", []string{"min_bytes"},
		probeItem("redis.big_keys", map[string]any{"key": "session:1", "bytes": float64(4096)}))
	if err == nil {
		t.Fatal("a 4KB key must not produce a deletion floor")
	}
}

// ── PersistentVolumeClaim resize ───────────────────────────────────────

// One full volume names itself. The namespace travels with it because a
// claim name is only unique inside a namespace, and the resize would address
// the wrong disk without it.
func TestExtractResizePVC_NamesTheOneVolumeThatWasMeasuredFull(t *testing.T) {
	args, err := resolveProbe(t, "k8s.resize_pvc", []string{"pvc", "size"},
		probeItem("k8s.pvc_usage", map[string]any{
			"name": "data-pvc", "namespace": "test", "measured": true, "used_percent": 99.0,
		}),
	)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if args["pvc"] != "data-pvc" {
		t.Errorf("pvc = %v, want data-pvc", args["pvc"])
	}
	if args["namespace"] != "test" {
		t.Errorf("namespace = %v, want test — a claim name is not unique across namespaces", args["namespace"])
	}
	// The size is deliberately absent. How much to grow a volume is a
	// budget decision with a recurring bill, and the evidence chain does not
	// record what anyone decided to spend.
	if _, present := args["size"]; present {
		t.Error("size must not be extracted from evidence; it is a decision, not an observation")
	}
}

// Two full volumes is a refusal that names both, not a pick. Growing the
// fuller one is a judgement about which bill to raise.
func TestExtractResizePVC_RefusesWhenTwoVolumesAreFull(t *testing.T) {
	_, err := resolveProbe(t, "k8s.resize_pvc", []string{"pvc", "size"},
		probeItem("k8s.pvc_usage",
			map[string]any{"name": "data-pvc", "namespace": "test", "measured": true, "used_percent": 99.0},
			map[string]any{"name": "logs-pvc", "namespace": "test", "measured": true, "used_percent": 96.0},
		),
	)
	if err == nil {
		t.Fatal("two full volumes must refuse rather than resolve to one of them")
	}
	for _, want := range []string{"test/data-pvc", "test/logs-pvc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name %s so the operator can choose; got %q", want, err)
		}
	}
}

// Two claims with the same name in different namespaces are two volumes, so
// this is the ambiguity case above rather than an agreement on one.
func TestExtractResizePVC_TreatsSameNameInTwoNamespacesAsAmbiguous(t *testing.T) {
	_, err := resolveProbe(t, "k8s.resize_pvc", []string{"pvc", "size"},
		probeItem("k8s.pvc_usage",
			map[string]any{"name": "data", "namespace": "team-a", "measured": true, "used_percent": 99.0},
			map[string]any{"name": "data", "namespace": "team-b", "measured": true, "used_percent": 99.0},
		),
	)
	if err == nil {
		t.Fatal("the same claim name in two namespaces is two volumes, and neither may be chosen")
	}
	if !strings.Contains(err.Error(), "team-a") || !strings.Contains(err.Error(), "team-b") {
		t.Errorf("refusal must name both namespaces; got %q", err)
	}
}

// The measurement never running is not permission to guess. This is the
// refusal that says so.
func TestExtractResizePVC_RefusesWhenNoVolumeWasMeasured(t *testing.T) {
	_, err := resolveProbe(t, "k8s.resize_pvc", []string{"pvc", "size"},
		probeItem("k8s.pvc_usage", map[string]any{
			"name": "data-pvc", "namespace": "test", "measured": false,
			"unmeasured_reason": "no running pod in namespace test mounts this claim",
		}),
	)
	if err == nil {
		t.Fatal("a volume nobody could measure must not be resized")
	}
	if !strings.Contains(err.Error(), "k8s.pvc_usage") {
		t.Errorf("the refusal must name the probe that would have answered this; got %q", err)
	}
}

// A volume measured at 40% is a real observation of a healthy disk and is
// not a candidate.
func TestExtractResizePVC_IgnoresAVolumeMeasuredBelowTheFloor(t *testing.T) {
	_, err := resolveProbe(t, "k8s.resize_pvc", []string{"pvc", "size"},
		probeItem("k8s.pvc_usage", map[string]any{
			"name": "data-pvc", "namespace": "test", "measured": true, "used_percent": 40.0,
		}),
	)
	if err == nil {
		t.Fatal("40%% full is not a resize")
	}
}

// ── Kafka partition reassignment ───────────────────────────────────────

func skewItem(topic string, partition int, ratio float64) map[string]any {
	return map[string]any{
		"topic": topic, "partition": partition,
		"vs_average": ratio, "under_replicated": false,
	}
}

// The partition is named because the measurement named it. The destination
// is not: which brokers should carry it needs the live broker list, the
// racks and the existing load, and the evidence chain records none of those.
func TestExtractKafkaRepartition_NamesThePartitionAndNotTheBrokers(t *testing.T) {
	args, err := resolveProbe(t, "kafka.repartition", []string{"topic", "partition", "to_brokers"},
		probeItem("kafka.partition_skew",
			skewItem("order.events", 0, 8.0),
			skewItem("order.events", 1, 1.0),
		),
	)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if args["topic"] != "order.events" {
		t.Errorf("topic = %v, want order.events", args["topic"])
	}
	if args["partition"] != 0 {
		t.Errorf("partition = %v, want 0", args["partition"])
	}
	if _, present := args["to_brokers"]; present {
		t.Error("to_brokers must not be extracted; a placement decision is not an observation")
	}
}

// Two skewed partitions is a refusal naming both, with the reason for each.
// Moving the worst one is a judgement about which copy the cluster should
// spend, and the tool's own guard would then refuse the replica list.
func TestExtractKafkaRepartition_RefusesWhenTwoPartitionsAreSkewed(t *testing.T) {
	_, err := resolveProbe(t, "kafka.repartition", []string{"topic", "partition", "to_brokers"},
		probeItem("kafka.partition_skew",
			skewItem("order.events", 0, 8.0),
			skewItem("order.events", 3, 4.0),
		),
	)
	if err == nil {
		t.Fatal("two skewed partitions must refuse rather than resolve to one")
	}
	for _, want := range []string{"order.events-0", "order.events-3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name %s; got %q", want, err)
		}
	}
	if !strings.Contains(err.Error(), "x the even share") {
		t.Errorf("refusal must carry each partition's reason; got %q", err)
	}
}

// A partition short of in-sync replicas qualifies on that ground alone, and
// the refusal message says so rather than quoting a load ratio it does not
// have.
func TestExtractKafkaRepartition_ShortOfReplicasIsNamedAsSuch(t *testing.T) {
	args, err := resolveProbe(t, "kafka.repartition", []string{"topic", "partition", "to_brokers"},
		probeItem("kafka.partition_skew",
			map[string]any{"topic": "order.events", "partition": 2, "under_replicated": true},
		),
	)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if args["partition"] != 2 {
		t.Errorf("partition = %v, want 2", args["partition"])
	}
}

// The skew read never running is not permission to move something.
func TestExtractKafkaRepartition_RefusesWithoutAMeasurement(t *testing.T) {
	_, err := resolveProbe(t, "kafka.repartition", []string{"topic", "partition", "to_brokers"},
		probeItem("kafka.partition_skew"),
	)
	if err == nil {
		t.Fatal("no measurement must refuse")
	}
	if !strings.Contains(err.Error(), "kafka.partition_skew") {
		t.Errorf("the refusal must name the probe that would have answered this; got %q", err)
	}
}
