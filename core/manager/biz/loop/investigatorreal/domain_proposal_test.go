package investigatorreal

import (
	"testing"

	loop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

func probeEvidence(tool string, rows ...map[string]any) loop.EvidenceItem {
	return loop.EvidenceItem{Tool: tool, Value: map[string]any{"rows": rows, "count": len(rows)}}
}

func proposedActions(t *testing.T, resourceType string, evidence ...loop.EvidenceItem) map[string]loop.RemediationOption {
	t.Helper()
	toolset := New(&fakeMetricQuerier{}, nil, nil)
	options, err := toolset.ListRemediationsWithEvidence(t.Context(), resourceType, "alert-17", evidence)
	if err != nil {
		t.Fatalf("ListRemediationsWithEvidence: %v", err)
	}
	out := map[string]loop.RemediationOption{}
	for _, option := range options {
		out[option.Action] = option
	}
	return out
}

// A table the catalog reports as mostly dead tuples is the evidence for a
// VACUUM. The connection-pool metric cannot make this proposal, which is
// the whole reason the probe exists.
func TestProposals_VacuumATableTheCatalogReportsAsBloating(t *testing.T) {
	actions := proposedActions(t, "pg", probeEvidence("pg.table_bloat",
		map[string]any{"schemaname": "public", "table": "order_events", "dead_pct": 61.0},
	))
	if _, ok := actions["pg.vacuum_table"]; !ok {
		t.Fatalf("a 61%% dead table must produce pg.vacuum_table, got %v", actions)
	}
}

// Below the floor the dead tuples are not worth a lock, and proposing a
// VACUUM for them would train operators to ignore the proposal.
func TestProposals_DoNotVacuumATableThatIsNotActuallyBloating(t *testing.T) {
	actions := proposedActions(t, "pg", probeEvidence("pg.table_bloat",
		map[string]any{"schemaname": "public", "table": "order_events", "dead_pct": 3.0},
	))
	if _, ok := actions["pg.vacuum_table"]; ok {
		t.Fatal("3% dead tuples is not a reason to take a VACUUM lock")
	}
}

// pg_stat_statements is ordered by mean execution time, so the recorded
// slow log is the evidence that there is something to explain.
func TestProposals_ExplainTheSlowestRecordedQuery(t *testing.T) {
	actions := proposedActions(t, "pg", probeEvidence("pg.slow_log",
		map[string]any{"queryid": 1, "mean_exec_time": 2000.0, "query": "SELECT 1"},
	))
	option, ok := actions["pg.explain_query"]
	if !ok {
		t.Fatalf("a recorded slow query must produce pg.explain_query, got %v", actions)
	}
	// EXPLAIN without ANALYZE does not run the statement, so this commits
	// to nothing and must not be gated as a mutation.
	if option.Risk != "safe" {
		t.Errorf("Risk = %q, want safe: EXPLAIN does not execute the query", option.Risk)
	}
	if option.AutoApprove {
		t.Error("nothing in this proposal should auto-approve")
	}
}

// A deployment whose rollout is not completing is a different fault from a
// pod that was OOM-killed, and they produce the same restart rate.
func TestProposals_RollBackOnlyWhenARolloutIsActuallyStuck(t *testing.T) {
	stuck := proposedActions(t, "k8s", probeEvidence("k8s.rollout_status",
		map[string]any{"name": "order-svc", "namespace": "prod", "rollout_complete": false, "ready": 1.0, "desired": 3.0},
	))
	if _, ok := stuck["k8s.rollout_undo"]; !ok {
		t.Fatalf("a wedged rollout must produce k8s.rollout_undo, got %v", stuck)
	}

	healthy := proposedActions(t, "k8s", probeEvidence("k8s.rollout_status",
		map[string]any{"name": "order-svc", "namespace": "prod", "rollout_complete": true, "ready": 3.0, "desired": 3.0},
	))
	if _, ok := healthy["k8s.rollout_undo"]; ok {
		t.Fatal("a completed rollout must not produce a rollback proposal")
	}
}

// A NotReady node is its own evidence. The restart rate says nothing about
// which node, or whether a node is the problem at all.
func TestProposals_ActOnANodeTheClusterReportsNotReady(t *testing.T) {
	actions := proposedActions(t, "k8s", probeEvidence("k8s.node_list",
		map[string]any{"name": "worker-3", "status": "NotReady", "pressure": "DiskPressure"},
		map[string]any{"name": "worker-1", "status": "Ready"},
	))
	for _, want := range []string{"k8s.uncordon", "k8s.drain"} {
		if _, ok := actions[want]; !ok {
			t.Fatalf("a NotReady node must produce %s, got %v", want, actions)
		}
	}

	healthy := proposedActions(t, "k8s", probeEvidence("k8s.node_list",
		map[string]any{"name": "worker-1", "status": "Ready"},
	))
	if _, ok := healthy["k8s.drain"]; ok {
		t.Fatal("draining a healthy node is not a remediation")
	}
}

// With no domain evidence the toolset must fall back to what it always had,
// not to a guess.
func TestProposals_WithoutProbeEvidenceNothingNewIsProposed(t *testing.T) {
	actions := proposedActions(t, "k8s", loop.EvidenceItem{Tool: "query_promql", Value: `[{"value":"0"}]`})
	for _, unwanted := range []string{"k8s.rollout_undo", "k8s.uncordon", "k8s.drain"} {
		if _, ok := actions[unwanted]; ok {
			t.Errorf("%s was proposed with no evidence of the condition it acts on", unwanted)
		}
	}
}

// A load average says the box is busy and nothing about why. The process
// list is what turns "busy" into a pid.
func TestProposals_KillTheOneProcessActuallyUsingTheCPU(t *testing.T) {
	actions := proposedActions(t, "host", probeEvidence("host.top_processes",
		map[string]any{"pid": 4211, "comm": "runaway-worker", "user": "root", "pcpu": 98.5},
	))
	if _, ok := actions["host.kill_process"]; !ok {
		t.Fatalf("a runaway process must produce host.kill_process, got %v", actions)
	}
}

// A host at full load with two hundred ordinary processes has nothing to
// kill, and the probe returns nothing above the floor precisely so that this
// proposal does not appear.
func TestProposals_DoNotKillAnythingWhenNoProcessIsTheCulprit(t *testing.T) {
	actions := proposedActions(t, "host", probeEvidence("host.top_processes"))
	if _, ok := actions["host.kill_process"]; ok {
		t.Fatal("no busy process must mean no kill proposal")
	}
}

// A full disk is a percentage; the old-log list is what says there is
// anything reclaimable.
func TestProposals_ReclaimSpaceOnlyWhenThereIsSomethingOld(t *testing.T) {
	full := proposedActions(t, "host", probeEvidence("host.old_log_files",
		map[string]any{"path": "/var/log/app-2024.log", "size_bytes": 1048576.0},
	))
	if _, ok := full["host.remove_old_logs"]; !ok {
		t.Fatalf("reclaimable logs must produce host.remove_old_logs, got %v", full)
	}
	empty := proposedActions(t, "host", probeEvidence("host.old_log_files"))
	if _, ok := empty["host.remove_old_logs"]; ok {
		t.Fatal("a disk with nothing old on it must not produce a removal proposal")
	}
}

// A memory ratio says the instance is near its ceiling and nothing about
// what is filling it. big_keys is what names the key.
func TestProposals_DeleteOversizedKeysOnlyWhenOneExists(t *testing.T) {
	big := proposedActions(t, "redis", probeEvidence("redis.big_keys",
		map[string]any{"key": "test:bigkey:hot", "type": "hash", "bytes": 50 * 1024 * 1024},
	))
	if _, ok := big["redis.scan_and_delete"]; !ok {
		t.Fatalf("an oversized key must produce redis.scan_and_delete, got %v", big)
	}
	small := proposedActions(t, "redis", probeEvidence("redis.big_keys",
		map[string]any{"key": "session:1", "type": "string", "bytes": 4096},
	))
	if _, ok := small["redis.scan_and_delete"]; ok {
		t.Fatal("a 4KB key is not oversized and must not produce a deletion proposal")
	}
}

func memoryInfo(used, max int64, policy string) loop.EvidenceItem {
	return probeEvidence("redis.info", map[string]any{
		"used_memory_bytes": float64(used), "maxmemory_bytes": float64(max),
		"maxmemory_policy": policy, "evicted_keys": float64(0),
	})
}

// Under an eviction policy a full instance shrinks its keyspace and keeps
// answering. Under noeviction it starts returning errors to its callers,
// and that is the version of this alert that pages somebody.
func TestProposals_ConfigChangeOnlyWhenTheServerWillStartRefusingWrites(t *testing.T) {
	refusing := proposedActions(t, "redis", memoryInfo(95, 100, "noeviction"))
	if _, ok := refusing["redis.config_set"]; !ok {
		t.Fatalf("a noeviction instance at 95%% must produce redis.config_set, got %v", refusing)
	}
	coping := proposedActions(t, "redis", memoryInfo(95, 100, "allkeys-lru"))
	if _, ok := coping["redis.config_set"]; ok {
		t.Fatal("an instance that evicts under pressure does not need its policy changed")
	}
}

// maxmemory of zero means no limit is configured. Dividing by it would
// report the instance as infinitely full.
func TestProposals_NoMemoryRemediesWhenMaxmemoryIsUnset(t *testing.T) {
	actions := proposedActions(t, "redis", memoryInfo(1_000_000, 0, "noeviction"))
	for _, unwanted := range []string{"redis.config_set", "redis.flushdb"} {
		if _, ok := actions[unwanted]; ok {
			t.Errorf("%s was proposed with no memory ceiling to be near", unwanted)
		}
	}
}

func TestProposals_FlushOnlyAtThePointWhereTheInstanceIsAboutToStop(t *testing.T) {
	near := proposedActions(t, "redis", memoryInfo(99, 100, "allkeys-lru"))
	if _, ok := near["redis.flushdb"]; !ok {
		t.Fatalf("an instance at 99%% of its ceiling must surface flushdb, got %v", near)
	}
	room := proposedActions(t, "redis", memoryInfo(80, 100, "allkeys-lru"))
	if _, ok := room["redis.flushdb"]; ok {
		t.Fatal("emptying a database that is 80% full is not a remediation")
	}
}

// The remedy is proposed; the value is not. That asymmetry is the point —
// the report shows these as un-dispatchable, which is the honest state for
// an action whose arguments only a human may supply.
func TestProposals_TheMemoryRemediesDeclareNoExtractor(t *testing.T) {
	declared := map[string]struct{}{}
	for _, action := range loop.ActionsResolvableFromEvidence() {
		declared[action] = struct{}{}
	}
	for _, action := range []string{"redis.config_set", "redis.flushdb"} {
		if _, ok := declared[action]; ok {
			t.Errorf("%s declares an evidence extractor, but its arguments are a human's decision", action)
		}
	}
}
