package investigatorreal

// RemediationActions is every action this investigator is capable of
// emitting, across all resource types and all evidence-conditional
// branches.
//
// Why this list exists. The actions are written inline as string literals
// in the switch statements above, which is fine for running them and
// useless for checking them: nothing in the codebase can answer "can the
// system do pg.kill_session?" without a human reading three switch blocks.
// The evaluation harness needs exactly that answer — a golden case that
// expects an action the investigator cannot produce will score zero for a
// reason that has nothing to do with whether the agent reasoned well, and a
// zero that reads like a verdict is worse than no number.
//
// So the capability is declared here, and TestTheDeclaredVocabularyMatchesThe
// Code scrapes this package's own AST for the literals actually passed to
// remediation(...) and requires set equality in both directions. Adding a
// branch without declaring it fails the test; declaring one that no code
// path can produce fails it too. The list cannot rot into a stale comment,
// because a comment has no test and this does.
//
// What it is not: this is the investigator's vocabulary, not the whole
// platform's. Plugin packages and control-plane adapters register their own
// tools elsewhere; a golden case covered by one of those is servable even
// when the symbol is absent here.
var RemediationActions = []string{
	"host.garbage_collect",
	"host.kill_process",
	"host.remove_old_logs",
	"host.restart_service",
	"k8s.cleanup_logs",
	"k8s.drain",
	"k8s.evict_pod",
	"k8s.resize_pvc",
	"k8s.rolling_restart",
	"k8s.rollout_status",
	"k8s.rollout_undo",
	"k8s.scale",
	"k8s.uncordon",
	"kafka.repartition",
	"mq.drain_queue",
	"mq.inspect_consumer_lag",
	"mq.replay_messages",
	"pg.connection_pause",
	"pg.explain_query",
	"pg.kill_session",
	"pg.terminate_long_tx",
	"pg.vacuum_analyze",
	"pg.vacuum_table",
	"redis.client_kill",
	"redis.config_set",
	"redis.failover",
	"redis.flushdb",
	"redis.memory_purge",
	"redis.scan_and_delete",
}
