package loop

// Extractors for the actions whose subject is a domain observation rather
// than a set of alert labels.
//
// The subject resolvers in subject.go answer "which object did the alert
// fire about". These answer a sharper question: "which object did the
// domain say is in trouble". The difference matters when an alert covers a
// whole database and the answer is one table inside it — the alert's labels
// name the database, and only `pg.table_bloat` names the table that is
// bloating.
//
// Same rule as everywhere else in this file: one candidate resolves, several
// refuse with the candidates named, none refuses naming the probe that would
// have supplied it. A VACUUM is not a read: running it on the wrong table
// holds a lock and does real work.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// bloatFloor is the dead-tuple share above which a table is treated as
// bloating.
//
// pg.table_bloat already filters to n_dead_tup > 0, which is a share of
// almost nothing — a table with one dead row passes that. This floor is the
// point where reclaiming the space is worth a lock, and it is stated as a
// number rather than tuned per deployment: a threshold that is adjusted
// until the golden cases pass is not a threshold, it is the answer key.
const bloatFloor = 20.0

// The three filters below are exported because both halves of the closed
// loop need them and neither may hold a private copy: the investigator
// proposes an action because a filter matched, and the resolver later
// refuses to name a target unless the same filter matches. Two copies of a
// threshold would be two chances to disagree about what the evidence said.

var (
	// BloatedTableRows keeps the tables whose dead share is at or above the
	// floor, most bloated first so a refusal names the worst offender.
	BloatedTableRows = func(evidence []EvidenceItem) []map[string]any {
		rows := ProbeRows(evidence, "pg.table_bloat")
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if pct, ok := RowFloat(row, "dead_pct"); ok && pct >= bloatFloor {
				out = append(out, row)
			}
		}
		return out
	}

	// StuckRolloutRows keeps the deployments whose rollout is not completing.
	// rollout_status was probed with only_stuck, but the filter is applied
	// again here because an adapter that ignored the argument would
	// otherwise put a healthy deployment in front of a rollback proposal.
	StuckRolloutRows = func(evidence []EvidenceItem) []map[string]any {
		rows := ProbeRows(evidence, "k8s.rollout_status")
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if complete, ok := RowBool(row, "rollout_complete"); ok && !complete {
				out = append(out, row)
			}
		}
		return out
	}

	// NotReadyNodeRows keeps the nodes the cluster reports as NotReady.
	NotReadyNodeRows = func(evidence []EvidenceItem) []map[string]any {
		rows := ProbeRows(evidence, "k8s.node_list")
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if status, ok := rowString(row, "status"); ok && status != "" && status != "Ready" {
				out = append(out, row)
			}
		}
		return out
	}
)

// probeExtractor builds an extractor that takes the single candidate from a
// filtered probe result.
//
// describe renders a row for the refusal message, and required names the
// argument so the message can say what was missing when the probe never ran.
func probeExtractor(
	tool, arg, required string,
	rows func([]EvidenceItem) []map[string]any,
	key string,
	describe func(map[string]any) string,
) evidenceArgExtractor {
	return func(_ RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
		candidates := rows(evidence)
		if len(candidates) == 0 {
			return nil, fmt.Errorf("no recorded subject names the %s this action acts on: the evidence chain "+
				"carries no usable %s result, so the %s it would act on was never observed. This is a "+
				"question for the operator or a missing probe, not something to guess",
				arg, tool, required)
		}
		value, ok := SingleCandidate(candidates, key, describe)
		if !ok {
			return nil, ambiguousCandidate(arg, candidates, describe)
		}
		return map[string]any{arg: value}, nil
	}
}

func ambiguousCandidate(arg string, candidates []map[string]any, describe func(map[string]any) string) error {
	rendered := make([]string, 0, len(candidates))
	for _, row := range candidates {
		if text := strings.TrimSpace(describe(row)); text != "" {
			rendered = append(rendered, text)
		}
	}
	return fmt.Errorf("the evidence describes %d candidates for %s (%s) and does not say which one to act on; "+
		"acting on one of them by guess is not a remediation, so this is refused",
		len(candidates), arg, strings.Join(rendered, ", "))
}

func extractPGBloatTable(_ RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	return probeExtractor(
		"pg.table_bloat", "table", "VACUUM", BloatedTableRows, "table",
		func(row map[string]any) string {
			pct, _ := RowFloat(row, "dead_pct")
			schema, _ := rowString(row, "schemaname")
			table, _ := rowString(row, "table")
			if schema != "" {
				table = schema + "." + table
			}
			return fmt.Sprintf("%s (%.1f%% dead)", table, pct)
		})(RemediationRequest{}, evidence)
}

func extractPGSlowQuery(_ RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	rows := ProbeRows(evidence, "pg.slow_log")
	if len(rows) == 0 {
		return nil, fmt.Errorf("pg.slow_log recorded no slow queries, so there is nothing to EXPLAIN")
	}
	// pg_stat_statements is ordered by mean_exec_time, so the first row is
	// the slowest. The plan an operator wants is the plan for the query
	// that is actually costing the most, and picking the worst is not a
	// guess — it is the ordering the database itself reported.
	query, ok := rowString(rows[0], "query")
	if !ok || query == "" {
		return nil, fmt.Errorf("the slowest recorded query carries no query text, so there is nothing to EXPLAIN")
	}
	return map[string]any{"query": query}, nil
}

func extractK8sStuckRollout(_ RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	return probeExtractor(
		"k8s.rollout_status", "deployment", "rollback", StuckRolloutRows, "name",
		func(row map[string]any) string {
			ready, _ := RowFloat(row, "ready")
			desired, _ := RowFloat(row, "desired")
			name, _ := rowString(row, "name")
			ns, _ := rowString(row, "namespace")
			return fmt.Sprintf("%s/%s (%.0f/%.0f ready)", ns, name, ready, desired)
		})(RemediationRequest{}, evidence)
}

func extractK8sNotReadyNode(_ RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	return probeExtractor(
		"k8s.node_list", "node", "node operation", NotReadyNodeRows, "name",
		func(row map[string]any) string {
			name, _ := rowString(row, "name")
			status, _ := rowString(row, "status")
			pressure, _ := rowString(row, "pressure")
			if pressure != "" {
				return fmt.Sprintf("%s (%s, %s)", name, status, pressure)
			}
			return fmt.Sprintf("%s (%s)", name, status)
		})(RemediationRequest{}, evidence)
}

// The host filters, in the same file and for the same reason as the ones
// above: the investigator proposes because a filter matched, and the
// resolver must reach the same verdict or a proposal the loop made would
// refuse at dispatch.

var (
	// BusyProcessRows keeps the processes consuming real CPU.
	BusyProcessRows = func(evidence []EvidenceItem) []map[string]any {
		return ProbeRows(evidence, "host.top_processes")
	}

	// ReclaimableLogRows keeps the old log files that can be removed.
	ReclaimableLogRows = func(evidence []EvidenceItem) []map[string]any {
		return ProbeRows(evidence, "host.old_log_files")
	}
)

func extractHostHotProcess(_ RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	rows := BusyProcessRows(evidence)
	if len(rows) == 0 {
		return nil, fmt.Errorf("host.top_processes recorded no process using real CPU, so there is no single " +
			"process to terminate. A host can be at full load with hundreds of ordinary processes and no " +
			"culprit, and killing one of them would be arbitrary")
	}
	// Several processes above the floor is the normal shape of a busy
	// host, and choosing among them is a judgement about which workload
	// matters — which is what the approval step is for.
	byPID := map[int64][]string{}
	for _, row := range rows {
		pid, ok := rowInt(row, "pid")
		if !ok || pid <= 0 {
			continue
		}
		byPID[pid] = append(byPID[pid], describeProcess(row))
	}
	if len(byPID) == 0 {
		return nil, fmt.Errorf("the %d recorded process(es) carry no usable pid", len(rows))
	}
	if len(byPID) == 1 {
		for pid := range byPID {
			return map[string]any{"pid": pid}, nil
		}
	}
	candidates := make([]string, 0, len(byPID))
	for pid, descriptors := range byPID {
		candidates = append(candidates, fmt.Sprintf("pid %d (%s)", pid, strings.Join(descriptors, "; ")))
	}
	sort.Strings(candidates)
	return nil, fmt.Errorf("the evidence describes %d processes using CPU (%s) and does not say which one to "+
		"terminate; killing one of them by guess is not a remediation, so this is refused",
		len(byPID), strings.Join(candidates, ", "))
}

func describeProcess(row map[string]any) string {
	parts := []string{}
	if comm, ok := rowString(row, "comm"); ok && comm != "" {
		parts = append(parts, comm)
	}
	if user, ok := rowString(row, "user"); ok && user != "" {
		parts = append(parts, "user="+user)
	}
	if pcpu, ok := RowFloat(row, "pcpu"); ok {
		parts = append(parts, fmt.Sprintf("%.1f%% cpu", pcpu))
	}
	return strings.Join(parts, " ")
}

// extractHostLogPath names the directory to reclaim from.
//
// The path is the *subject*, not a probe result: the alert knows which
// filesystem filled up, and only the alert knows that. The probe's role is
// to establish that there is something old enough under it to remove —
// which is what gates the proposal, not what names the target. Splitting it
// this way keeps "which filesystem" (an identity, from the alert) separate
// from "is there anything to reclaim" (a condition, from the domain).
func extractHostLogPath(_ RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	if len(ReclaimableLogRows(evidence)) == 0 {
		return nil, fmt.Errorf("no recorded subject names the log directory to reclaim from, and " +
			"host.old_log_files found nothing old enough under the default path to act on")
	}
	labels, ok := subjectLabels(evidence)
	if !ok {
		return nil, missingSubject("host.remove_old_logs", "path", hostPathLabelKeys)
	}
	value, _, err := subjectValue(labels, hostPathLabelKeys)
	if err != nil {
		return nil, fmt.Errorf("host.remove_old_logs cannot resolve path: %w", err)
	}
	if value == "" {
		return nil, missingSubject("host.remove_old_logs", "path", hostPathLabelKeys)
	}
	return map[string]any{"path": value}, nil
}

// hostPathLabelKeys are the alert labels that name a filesystem or
// directory. A mount is what fills up, and the alert that noticed it is
// the thing that knows which one.
var hostPathLabelKeys = []string{"path", "mount", "mountpoint", "mount_point", "dir", "directory", "filesystem"}

// The big-key floor, in bytes.
//
// This is an *observation* threshold, not an action one: it decides when a
// key is worth reporting as a suspect, never when it may be deleted. What
// gets deleted is decided by the caller's min_bytes, which is required for
// exactly that reason. Ten megabytes is the conventional line for "this key
// is a problem" in Redis practice; it is written as a number here rather
// than tuned, because a floor adjusted until the golden cases pass is the
// answer key wearing a constant's clothes.
const bigKeyFloor = 10 * 1024 * 1024

// OversizedKeyRows keeps the keys big enough to be a suspect.
var OversizedKeyRows = func(evidence []EvidenceItem) []map[string]any {
	rows := ProbeRows(evidence, "redis.big_keys")
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if size, ok := RowFloat(row, "bytes"); ok && size >= bigKeyFloor {
			out = append(out, row)
		}
	}
	return out
}

func extractRedisMinBytes(_ RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	rows := OversizedKeyRows(evidence)
	if len(rows) == 0 {
		return nil, fmt.Errorf("redis.big_keys recorded no key at or above %s, so there is nothing oversized to delete",
			humanBytesForMessage(bigKeyFloor))
	}
	// One key resolves to its own size, which deletes it and nothing else.
	// Several do not: choosing the smallest of them sets a floor that
	// sweeps up every key of that size across the keyspace, and the
	// evidence did not observe them. The refusal names the keys and their
	// sizes so the caller can state the floor it means.
	if len(rows) > 1 {
		described := make([]string, 0, len(rows))
		for _, row := range rows {
			key, _ := rowString(row, "key")
			size, _ := RowFloat(row, "bytes")
			described = append(described, fmt.Sprintf("%s (%s)", key, humanBytesForMessage(int64(size))))
		}
		sort.Strings(described)
		return nil, fmt.Errorf("the evidence describes %d oversized keys (%s); the deletion floor is a decision about "+
			"how much of the keyspace to sweep, and picking the smallest of them would delete every key of that "+
			"size rather than the ones observed — state min_bytes to say which you mean",
			len(rows), strings.Join(described, ", "))
	}
	size, ok := RowFloat(rows[0], "bytes")
	if !ok || size <= 0 {
		return nil, fmt.Errorf("the oversized key carries no usable size, so no deletion floor can be read from it")
	}
	return map[string]any{"min_bytes": int64(size)}, nil
}

// humanBytesForMessage renders a byte count for a refusal, keeping the
// exact figure so the reader can match it against what the tool reported.
func humanBytesForMessage(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB (%d bytes)", float64(n)/float64(div), "KMGTP"[exp], n)
}

// The Redis memory ceiling facts, read from one INFO row.
//
// A single row carries the whole diagnosis: how full the instance is, what
// it will do when it gets fuller, and how many keys it has already thrown
// away. They are read together rather than separately because none of the
// three means anything on its own — 90% full under allkeys-lru is a normal
// Friday, and 90% full under noeviction is an incident.
func redisMemoryFacts(evidence []EvidenceItem) (used, limit int64, policy string, ok bool) {
	rows := ProbeRows(evidence, "redis.info")
	for _, row := range rows {
		u, hasUsed := RowFloat(row, "used_memory_bytes")
		l, hasLimit := RowFloat(row, "maxmemory_bytes")
		p, hasPolicy := rowString(row, "maxmemory_policy")
		if !hasUsed || !hasPolicy {
			continue
		}
		policy = p
		ok = true
		if hasUsed {
			used = int64(u)
		}
		if hasLimit {
			limit = int64(l)
		}
		return
	}
	return 0, 0, "", false
}

// RedisFillRatio is how much of maxmemory is in use, and whether it is
// knowable at all.
//
// maxmemory of zero means no limit is configured, in which case the ratio
// is not small — it is meaningless, and dividing by it would report an
// instance as infinitely full. That is reported as "not knowable" so the
// proposals that depend on it simply do not appear.
func RedisFillRatio(evidence []EvidenceItem) (float64, bool) {
	used, limit, _, ok := redisMemoryFacts(evidence)
	if !ok || limit <= 0 {
		return 0, false
	}
	return float64(used) / float64(limit), true
}

// RedisRefusesWrites reports whether the instance is configured to reject
// commands rather than to evict keys.
//
// It is the single fact that separates "full but coping" from "full and
// about to start returning OOM". Under an eviction policy the keyspace
// shrinks and the service keeps answering; under noeviction the service
// starts returning errors to its callers, which is the version of this
// alert that pages somebody.
func RedisRefusesWrites(evidence []EvidenceItem) bool {
	_, _, policy, ok := redisMemoryFacts(evidence)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "noeviction":
		return true
	default:
		return false
	}
}

// ── PersistentVolumeClaims ─────────────────────────────────────────────

// pvFullFloor is the used share of a volume at which reclaiming or growing
// it becomes worth proposing.
//
// Ninety percent is a statement about headroom, not about the golden cases.
// A filesystem is not a percentage that degrades gracefully: past high-water
// the writes fail, and for most storage drivers they fail for the whole
// volume rather than for the file that ran out. Ninety leaves enough margin
// for the approval round-trip and the ordered rollout of a resize to finish
// before the volume is unusable, which is the whole reason to propose before
// the incident rather than during it. It is written as a number rather than
// tuned per deployment, for the reason every other floor in this file is.
const pvFullFloor = 90.0

// FullClaimRows keeps the volumes that were actually measured AND measured
// as nearly full.
//
// Both conditions are load-bearing and they are different failures. A row
// with `measured: false` is the adapter saying it could not read the
// filesystem — no pod mounts the claim, a distroless image has no `df`, the
// claim never bound. Reading its absence of a `used_percent` as "not full"
// would be reading an unknown as a negative, and a proposal gated on it
// would fire for every volume nobody could measure. A row that WAS measured
// and came back at 40% is a real observation of a healthy volume and is
// correctly left out.
var FullClaimRows = func(evidence []EvidenceItem) []map[string]any {
	rows := ProbeRows(evidence, "k8s.pvc_usage")
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		measured, ok := RowBool(row, "measured")
		if !ok || !measured {
			continue
		}
		if pct, ok := RowFloat(row, "used_percent"); ok && pct >= pvFullFloor {
			out = append(out, row)
		}
	}
	return out
}

// extractK8sResizePVC names the one nearly-full claim to grow.
//
// It does not use the shared SingleCandidate helper, because a claim's
// identity is its namespace AND its name, and `data` exists in every
// namespace in most clusters. Collapsing on the bare name would either merge
// two different volumes into one "agreement" and resolve to a claim nobody
// measured, or — worse — let a cluster with one `data` per namespace report
// ambiguity for a cluster where only one of them is full. The composite key
// is the claim's real identity, so it is the thing counted.
func extractK8sResizePVC(_ RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	rows := FullClaimRows(evidence)
	if len(rows) == 0 {
		return nil, fmt.Errorf("no recorded evidence measures a volume at or above %s%%: the chain carries no usable "+
			"k8s.pvc_usage result, so no claim was ever observed to be full. A volume that could not be measured is "+
			"not a healthy volume, and this refuses to propose growing one",
			humanPercent(pvFullFloor))
	}
	type claim struct {
		namespace string
		name      string
		used      float64
	}
	byKey := map[string]claim{}
	var order []string
	for _, row := range rows {
		name, nameOK := rowString(row, "name")
		namespace, _ := rowString(row, "namespace")
		used, _ := RowFloat(row, "used_percent")
		if !nameOK || name == "" {
			continue
		}
		// A row with no namespace came from a cluster-wide list. It is
		// still a real claim, and the resize needs a namespace to address
		// it — the adapter will refuse an ambiguous name rather than pick
		// one, so the empty string is passed through and the refusal
		// happens at the tool, where the message can list what it found.
		key := namespace + "/" + name
		if _, seen := byKey[key]; !seen {
			order = append(order, key)
		}
		byKey[key] = claim{namespace: namespace, name: name, used: used}
	}
	switch len(order) {
	case 0:
		return nil, fmt.Errorf("the volume rows at or above %s%% carry no claim name, so there is nothing to grow", humanPercent(pvFullFloor))
	case 1:
		found := byKey[order[0]]
		return map[string]any{"pvc": found.name, "namespace": found.namespace}, nil
	default:
		described := make([]string, 0, len(order))
		for _, key := range order {
			described = append(described, fmt.Sprintf("%s (%s%% full)", key, humanPercent(byKey[key].used)))
		}
		sort.Strings(described)
		return nil, fmt.Errorf("the evidence measures %d volumes at or above %s%% (%s); growing the wrong one costs money "+
			"and does not fix the one that is full, so this is refused rather than resolved to the fullest",
			len(order), humanPercent(pvFullFloor), strings.Join(described, ", "))
	}
}

func humanPercent(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// ── Kafka partition skew ───────────────────────────────────────────────

// partitionSkewFloor is how many times the even share a partition must be
// holding to be called skewed.
//
// Three is a statement about arithmetic rather than about the golden cases.
// A perfectly even topic has every partition at `vs_average` 1.0, so the
// floor is "this one partition is carrying at least three times what an even
// split would give it" — which is what a hot key actually looks like, and
// which a topic with a merely unlucky distribution does not reach. It is
// written as a number rather than tuned, for the reason every other floor in
// this file is.
const partitionSkewFloor = 3.0

// SkewedPartitionRows keeps the partitions the distribution says are the
// problem.
//
// Two independent observations qualify, and both are real:
//
//   - `vs_average` at or above the floor: this partition is carrying
//     disproportionate outstanding work.
//   - `under_replicated`: its in-sync replica set is short of its replica
//     set, so a broker failure makes this partition unavailable. The load
//     argument does not apply and the HA one does, and the fix — moving the
//     replica set — is the same operation.
//
// Rows that report `vs_average` as zero because the topic has no outstanding
// work are not skewed, and a row that carries no `vs_average` at all is not
// treated as zero: a row missing the field is a row from a tool that did not
// measure it.
var SkewedPartitionRows = func(evidence []EvidenceItem) []map[string]any {
	rows := ProbeRows(evidence, "kafka.partition_skew")
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if under, ok := RowBool(row, "under_replicated"); ok && under {
			out = append(out, row)
			continue
		}
		if ratio, ok := RowFloat(row, "vs_average"); ok && ratio >= partitionSkewFloor {
			out = append(out, row)
		}
	}
	return out
}

// extractKafkaRepartition names the one partition to move.
//
// Its target list of brokers is deliberately NOT extracted. Which brokers
// should carry a partition is a placement decision: it needs the cluster's
// live broker list, the racks, and the existing load, and none of those are
// recorded in the evidence chain. The partition is named because the
// measurement named it; the destination is left to whoever approves, and the
// tool refuses a list whose length would change the replica factor unless
// that is said out loud.
func extractKafkaRepartition(_ RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	rows := SkewedPartitionRows(evidence)
	if len(rows) == 0 {
		return nil, fmt.Errorf("no recorded evidence marks a partition as skewed: the chain carries no usable " +
			"kafka.partition_skew result, so no partition was ever observed carrying a disproportionate share. " +
			"Moving a partition nobody measured is not a remediation")
	}
	type candidate struct {
		topic     string
		partition int
		reason    string
	}
	byKey := map[string]candidate{}
	var order []string
	for _, row := range rows {
		topic, ok := rowString(row, "topic")
		if !ok || topic == "" {
			continue
		}
		partition, ok := RowFloat(row, "partition")
		if !ok {
			continue
		}
		key := fmt.Sprintf("%s-%d", topic, int(partition))
		reason := ""
		if under, _ := RowBool(row, "under_replicated"); under {
			reason = "fewer in-sync replicas than replicas"
		} else if ratio, _ := RowFloat(row, "vs_average"); ratio > 0 {
			reason = fmt.Sprintf("%sx the even share", humanPercent(ratio))
		}
		if _, seen := byKey[key]; !seen {
			order = append(order, key)
		}
		byKey[key] = candidate{topic: topic, partition: int(partition), reason: reason}
	}
	switch len(order) {
	case 0:
		return nil, fmt.Errorf("the skewed partition rows carry no topic and partition, so there is nothing to move")
	case 1:
		found := byKey[order[0]]
		return map[string]any{"topic": found.topic, "partition": found.partition}, nil
	default:
		described := make([]string, 0, len(order))
		for _, key := range order {
			described = append(described, fmt.Sprintf("%s (%s)", key, byKey[key].reason))
		}
		sort.Strings(described)
		return nil, fmt.Errorf("the evidence marks %d partitions as skewed (%s); moving the wrong one is a "+
			"partition copy the cluster has to make and a placement that fixes nothing, so this is refused rather "+
			"than resolved to the worst",
			len(order), strings.Join(described, ", "))
	}
}
