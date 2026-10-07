// Package investigatorreal provides the evidence-aware production implementation
// of loop.InvestigatorToolset. It collects Prometheus and Loki observations and
// uses them to narrow remediation candidates.
package investigatorreal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/logquery"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"

	loop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

const maxEvidencePayloadBytes = 16 * 1024

// MetricQuerier is the narrow Prometheus surface consumed by this package.
// The concrete promquery.Client and aiops test fakes satisfy it structurally.
type MetricQuerier interface {
	QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) (*promquery.InstantResult, error)
}

// LogQuerier is the narrow Loki surface consumed by this package.
type LogQuerier interface {
	QueryRange(ctx context.Context, opts logquery.QueryRangeOptions) (*logquery.QueryRangeResult, error)
}

// InvestigatorToolset collects multi-source evidence and generates remediation
// candidates from that evidence.
type InvestigatorToolset struct {
	metrics MetricQuerier
	logs    LogQuerier
	labels  AlertLabelsProvider
	probes  loop.ToolCaller
	log     *slog.Logger
	now     func() time.Time
}

// AlertLabelsProvider returns the labels the firing alert carried.
//
// Why the toolset needs this. Every write action in the remediation
// vocabulary acts on a named object — a pod, a queue, a systemd unit, a
// redis client address — and a RemediationOption carries only a resource
// locator, never those values. The loop's argument resolvers read them from
// the evidence chain, so the chain has to carry them. An alert already names
// its object: that is how the rule selected what to fire on. Without this
// provider the chain holds only a metric and a log line, and every such
// action refuses at dispatch for want of a name that was known all along.
//
// It is optional. A toolset without one still investigates; it just records
// no subject, and the resolvers refuse with a message saying so. That is the
// correct failure — a refusal naming the missing evidence — rather than a
// dispatch that invents a pod.
type AlertLabelsProvider interface {
	AlertLabels(ctx context.Context, alertID string) (map[string]string, error)
}

// NewWithLabels constructs the toolset with a subject source wired. Passing
// a nil provider is equivalent to New.
func NewWithLabels(metrics MetricQuerier, logs LogQuerier, labels AlertLabelsProvider, log *slog.Logger) *InvestigatorToolset {
	t := New(metrics, logs, log)
	t.labels = labels
	return t
}

// WithProbes returns a copy of the toolset that also asks the domain what
// state it is in, through read-only tools.
//
// Without it the toolset sees a metric and a log line, which cannot tell a
// bloated table from a stuck vacuum from a slow query — three faults with
// three different fixes. See domain_probe.go for why the answer is to
// observe rather than to threshold. Passing a nil ToolCaller is equivalent
// to not calling this.
func (t *InvestigatorToolset) WithProbes(tools loop.ToolCaller) *InvestigatorToolset {
	clone := *t
	clone.probes = tools
	return &clone
}

// New constructs the real toolset. At least one querier must be non-nil.
func New(metrics MetricQuerier, logs LogQuerier, log *slog.Logger) *InvestigatorToolset {
	if metrics == nil && logs == nil {
		panic("investigatorreal: New requires at least one querier")
	}
	if log == nil {
		log = slog.Default()
	}
	return &InvestigatorToolset{
		metrics: metrics,
		logs:    logs,
		log:     log.With(slog.String("comp", "loop.investigator.real")),
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// Compile-time interface checks.
var (
	_ loop.InvestigatorToolset              = (*InvestigatorToolset)(nil)
	_ loop.EvidenceAwareInvestigatorToolset = (*InvestigatorToolset)(nil)
)

// Investigate returns resource_alert plus every successful observability query.
// Query errors are logged and skipped so a partial outage does not block the loop.
func (t *InvestigatorToolset) Investigate(ctx context.Context, resourceType, alertID string, timeWindow loop.TimeWindow) ([]loop.EvidenceItem, error) {
	if strings.TrimSpace(resourceType) == "" {
		return nil, fmt.Errorf("investigatorreal: Investigate requires resourceType")
	}
	if strings.TrimSpace(alertID) == "" {
		return nil, fmt.Errorf("investigatorreal: Investigate requires alertID")
	}
	window, err := normalizeWindow(timeWindow, t.now)
	if err != nil {
		return nil, err
	}

	evidence := []loop.EvidenceItem{{
		Tool: "resource_alert",
		Query: fmt.Sprintf("resource_type=%s alert_id=%s window=[%s,%s]",
			resourceType, alertID, window.Start.Format(time.RFC3339), window.End.Format(time.RFC3339)),
		Value:     alertID,
		Count:     1,
		Timestamp: window.End,
	}}

	if subject := t.subject(ctx, resourceType, alertID); subject != nil {
		evidence = append(evidence, *subject)
	}
	// Domain probes run after the subject and before the observability
	// queries so a slow database is bounded by the same context as
	// everything else rather than holding the chain open.
	evidence = append(evidence, probe(ctx, t.probes, resourceType, window.End)...)

	metricExpr := metricExpression(resourceType, alertID)
	logExpr := logExpression(resourceType, alertID)

	var collectors sync.WaitGroup
	results := make(chan loop.EvidenceItem, 2)
	if t.metrics != nil {
		collectors.Add(1)
		go func() {
			defer collectors.Done()
			result, queryErr := t.metrics.QueryRange(ctx, metricExpr, window.Start, window.End, stepFor(window))
			if queryErr != nil {
				t.log.WarnContext(ctx, "prometheus evidence failed",
					slog.String("resource_type", resourceType),
					slog.String("alert_id", alertID),
					slog.Any("err", queryErr))
				return
			}
			raw, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				t.log.WarnContext(ctx, "prometheus evidence marshal failed", slog.Any("err", marshalErr))
				return
			}
			results <- loop.EvidenceItem{
				Tool:      "query_promql",
				Query:     metricExpr,
				Value:     sanitizeEvidence(raw),
				Count:     1,
				Timestamp: window.End,
			}
		}()
	}
	if t.logs != nil {
		collectors.Add(1)
		go func() {
			defer collectors.Done()
			result, queryErr := t.logs.QueryRange(ctx, logquery.QueryRangeOptions{
				Query:     logExpr,
				Start:     window.Start,
				End:       window.End,
				Limit:     200,
				Direction: "backward",
			})
			if queryErr != nil {
				t.log.WarnContext(ctx, "loki evidence failed",
					slog.String("resource_type", resourceType),
					slog.String("alert_id", alertID),
					slog.Any("err", queryErr))
				return
			}
			raw, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				t.log.WarnContext(ctx, "loki evidence marshal failed", slog.Any("err", marshalErr))
				return
			}
			cleaned := sanitizeEvidence(raw)
			results <- loop.EvidenceItem{
				Tool:      "query_logql",
				Query:     logExpr,
				Value:     cleaned,
				Count:     countLogEntries(cleaned),
				Timestamp: window.End,
			}
		}()
	}

	collectors.Wait()
	close(results)
	for item := range results {
		evidence = append(evidence, item)
	}
	return evidence, nil
}

// subject records the object the alert fired about, when a source is wired.
//
// A lookup failure is logged and skipped rather than returned: the incident
// is still investigable on metrics and logs, and a subject that cannot be
// read must not block a root cause. The consequence is that the resolvers
// refuse the write actions with a message naming the missing subject, which
// is the honest outcome — the alternative, dispatching a write with no
// argument, is the failure this whole mechanism exists to prevent.
func (t *InvestigatorToolset) subject(ctx context.Context, resourceType, alertID string) *loop.EvidenceItem {
	if t.labels == nil {
		return nil
	}
	labels, err := t.labels.AlertLabels(ctx, alertID)
	if err != nil {
		t.log.WarnContext(ctx, "alert labels unavailable; the evidence chain will carry no subject",
			slog.String("resource_type", resourceType),
			slog.String("alert_id", alertID),
			slog.Any("err", err))
		return nil
	}
	if len(labels) == 0 {
		return nil
	}
	item := loop.NewSubjectEvidenceItem(alertID, resourceType+":"+alertID, labels)
	return &item
}

// ListRemediations returns a conservative baseline when evidence is unavailable.
func (t *InvestigatorToolset) ListRemediations(ctx context.Context, resourceType string) ([]loop.RemediationOption, error) {
	return t.ListRemediationsWithEvidence(ctx, resourceType, "unknown", nil)
}

// ListRemediationsWithEvidence narrows actions using metric and log evidence.
func (t *InvestigatorToolset) ListRemediationsWithEvidence(_ context.Context, resourceType, alertID string, evidence []loop.EvidenceItem) ([]loop.RemediationOption, error) {
	if strings.TrimSpace(resourceType) == "" {
		return nil, fmt.Errorf("investigatorreal: ListRemediations requires resourceType")
	}
	if strings.TrimSpace(alertID) == "" {
		alertID = "unknown"
	}

	metricValue, hasMetric := numericMetric(evidence)
	logCount := logHitCount(evidence)
	target := resourceType + ":" + alertID
	options := baselineRemediations(resourceType, target)

	switch resourceType {
	case "pg":
		if hasMetric && metricValue >= 30 {
			options = append(options, remediation("pg.terminate_long_tx", target, "mutating", false))
		}
		if hasMetric && metricValue >= 100 {
			options = append(options, remediation("pg.connection_pause", target, "mutating", false))
		}
		// A table the catalog reports as mostly dead tuples is the
		// evidence for a VACUUM, and the catalog is the only thing that
		// can name which table. The connection-pool metric above cannot
		// tell this from a slow query, which is why the remediation axis
		// was stuck: both incidents produced the same single number.
		if len(loop.BloatedTableRows(evidence)) > 0 {
			options = append(options, remediation("pg.vacuum_table", target, "mutating", false))
		}
		// pg_stat_statements is ordered by mean execution time, so the
		// first row is the query actually costing the most. EXPLAIN does
		// not execute it, so proposing it commits to nothing.
		if len(loop.ProbeRows(evidence, "pg.slow_log")) > 0 {
			options = append(options, remediation("pg.explain_query", target, "safe", false))
		}
		if logCount > 0 {
			// pg.kill_session is the name the pg adapter actually registers. The
			// previous pg.kill_backend was a rename nobody reconciled: PostgreSQL
			// calls the same thing a backend in pg_terminate_backend and a session
			// in pg_stat_activity, so the two names describe one operation — and
			// only one of them resolves to something that can run. Writing the
			// unregistered name into a contract put a fix in front of a human that
			// the system could not carry out.
			options = append(options, remediation("pg.kill_session", target, "mutating", false))
		}
	case "redis":
		if hasMetric && metricValue >= 0.90 {
			options = append(options, remediation("redis.failover", target, "mutating", false))
		}
		if logCount > 0 {
			options = append(options, remediation("redis.client_kill", target, "mutating", false))
		}
		// Memory ratio says the instance is near its ceiling and nothing
		// about what is filling it. big_keys is what names the key, and
		// the deletion floor the dispatch needs comes from that same
		// observation rather than from a constant written here.
		if len(loop.OversizedKeyRows(evidence)) > 0 {
			options = append(options, remediation("redis.scan_and_delete", target, "mutating", false))
		}
		// The two remedies below are proposed but deliberately have no
		// extractor. Their required arguments — which configuration
		// parameter, to what value, and the operator's confirmation word —
		// are decisions a human makes, not facts the evidence records. The
		// gate is what makes the proposal legitimate: an instance under
		// noeviction at 95% will start refusing writes, and one at 99%
		// is about to stop entirely. Naming the remedy is the platform's
		// job; choosing the value is not, and an extractor that guessed
		// one would be the exact failure the rest of this file refuses.
		if ratio, known := loop.RedisFillRatio(evidence); known {
			if loop.RedisRefusesWrites(evidence) && ratio >= 0.90 {
				options = append(options, remediation("redis.config_set", target, "mutating", false))
			}
			if ratio >= 0.99 {
				options = append(options, remediation("redis.flushdb", target, "dangerous", false))
			}
		}
	case "k8s":
		if hasMetric && metricValue >= 0.10 {
			options = append(options,
				remediation("k8s.rolling_restart", target, "mutating", false),
				remediation("k8s.scale", target, "mutating", false),
			)
		}
		if logCount >= 10 {
			options = append(options, remediation("k8s.evict_pod", target, "mutating", false))
		}
		// The metric above counts container restarts, which is the same
		// number whether a rollout is wedged or a pod was OOM-killed. The
		// probe is what separates them: a deployment whose rollout is not
		// completing is a different fault with a different fix, and a
		// rollback is only a reasonable thing to put in front of an
		// operator when something observed says the rollout is the problem.
		if len(loop.StuckRolloutRows(evidence)) > 0 {
			options = append(options, remediation("k8s.rollout_undo", target, "mutating", false))
		}
		// A NotReady node is its own evidence; the restart rate says
		// nothing about which node or whether it is the node at all.
		if len(loop.NotReadyNodeRows(evidence)) > 0 {
			options = append(options,
				remediation("k8s.uncordon", target, "mutating", false),
				remediation("k8s.drain", target, "mutating", false),
			)
		}
		// A full volume is the one fault in this branch that no metric
		// above can see: the restart rate is flat, the pods are Running,
		// and the node has room. What records it is k8s.pvc_usage, which
		// exists because the Kubernetes API does not report filesystem
		// usage and something has to go and measure it.
		//
		// Two remedies, and the difference between them is what one of
		// them is allowed to know. resize_pvc is the durable fix and its
		// subject is the measurement, so it resolves: the extractor reads
		// the claim the `df` actually ran against. Its SIZE does not
		// resolve, because how much to grow a volume is a budget decision
		// with a recurring bill attached, not a fact about a filesystem —
		// so the claim is named and the size is left to the approval.
		//
		// cleanup_logs is the immediate one, and it is recorded as
		// `dangerous` rather than `mutating` because what it destroys is
		// not recoverable: truncating logs cannot be undone by growing
		// the volume afterwards. Its subject is NOT resolved either, and
		// for a different reason than the size above — which directory
		// inside the volume holds logs is a fact about somebody's
		// application layout, and no filesystem reading in the evidence
		// chain knows it. The tool answers that question itself, safely:
		// it dry-runs by default and reports what it would reclaim, so
		// the path can be confirmed against reality before anything is
		// destroyed.
		if len(loop.FullClaimRows(evidence)) > 0 {
			options = append(options,
				remediation("k8s.resize_pvc", target, "mutating", false),
				remediation("k8s.cleanup_logs", target, "dangerous", false),
			)
		}
	case "host":
		if hasMetric && metricValue >= 0.80 {
			options = append(options, remediation("host.restart_service", target, "mutating", false))
		}
		// A load average says the box is busy and nothing about why. The
		// process list is what turns "busy" into a pid, and it is also
		// what tells us when there is no single culprit — a host at full
		// load with two hundred ordinary processes has nothing to kill,
		// and the proposal must not appear.
		if len(loop.BusyProcessRows(evidence)) > 0 {
			options = append(options, remediation("host.kill_process", target, "mutating", false))
		}
		// Likewise a full disk is a percentage; the old log list is what
		// says there is anything reclaimable under it.
		if len(loop.ReclaimableLogRows(evidence)) > 0 {
			options = append(options, remediation("host.remove_old_logs", target, "mutating", false))
		}
	case "mq":
		if hasMetric && metricValue >= 1000 {
			options = append(options, remediation("mq.drain_queue", target, "mutating", false))
		}
		if logCount > 0 {
			options = append(options, remediation("mq.replay_messages", target, "mutating", false))
		}
		// A consumer group can be behind because one partition is carrying
		// everything or because every partition is carrying its share, and
		// the per-group total is the same number in both cases. They are
		// different faults: the first is a hot key and the fix is to move
		// the partition, the second is insufficient throughput and the fix
		// is to add consumers. The skew read is what separates them, and
		// without it this branch cannot tell a hot key from a slow
		// consumer.
		//
		// `to_brokers` is deliberately not extracted — see
		// extractKafkaRepartition. The partition is named because the
		// measurement named it; where to put it is a placement decision
		// that needs the live broker list and the racks, and the tool
		// refuses a list that would silently change the replica factor.
		if len(loop.SkewedPartitionRows(evidence)) > 0 {
			options = append(options, remediation("kafka.repartition", target, "mutating", false))
		}
	default:
		t.log.Warn("investigatorreal: unknown resource type", slog.String("resource_type", resourceType))
		return []loop.RemediationOption{}, nil
	}
	return uniqueRemediations(options), nil
}

func uniqueRemediations(options []loop.RemediationOption) []loop.RemediationOption {
	seen := make(map[string]struct{}, len(options))
	unique := make([]loop.RemediationOption, 0, len(options))
	for _, option := range options {
		if _, exists := seen[option.Action]; exists {
			continue
		}
		seen[option.Action] = struct{}{}
		unique = append(unique, option)
	}
	return unique
}

func normalizeWindow(window loop.TimeWindow, now func() time.Time) (loop.TimeWindow, error) {
	if window.Start.IsZero() || window.End.IsZero() || !window.End.After(window.Start) {
		end := now().UTC()
		return loop.TimeWindow{Start: end.Add(-5 * time.Minute), End: end}, nil
	}
	return loop.TimeWindow{Start: window.Start.UTC(), End: window.End.UTC()}, nil
}

func stepFor(window loop.TimeWindow) time.Duration {
	duration := window.End.Sub(window.Start)
	switch {
	case duration <= 5*time.Minute:
		return 15 * time.Second
	case duration <= time.Hour:
		return time.Minute
	case duration <= 6*time.Hour:
		return 5 * time.Minute
	case duration <= 24*time.Hour:
		return 15 * time.Minute
	default:
		return time.Hour
	}
}

func metricExpression(resourceType, alertID string) string {
	switch resourceType {
	case "pg":
		return fmt.Sprintf(`max(pg_stat_activity_max_tx_duration_seconds{alert_id=%q})`, alertID)
	case "redis":
		return fmt.Sprintf(`max(redis_memory_used_bytes{alert_id=%q} / redis_memory_max_bytes{alert_id=%q})`, alertID, alertID)
	case "k8s":
		return fmt.Sprintf(`sum(rate(kube_pod_container_restarts_total{alert_id=%q}[5m]))`, alertID)
	case "host":
		return fmt.Sprintf(`max(rate(node_cpu_seconds_total{mode!="idle",alert_id=%q}[5m]))`, alertID)
	case "mq":
		return fmt.Sprintf(`sum(rate(mq_consumer_lag{alert_id=%q}[5m]))`, alertID)
	default:
		return fmt.Sprintf(`vector(0)`)
	}
}

func logExpression(resourceType, alertID string) string {
	return fmt.Sprintf(`{resource_type=%q} |= %q`, resourceType, alertID)
}

var sensitivePattern = regexp.MustCompile(`(?i)("(?:password|passwd|token|secret|authorization|api_key|access_key)"\s*:\s*")([^"]*)(")`)

func sanitizeEvidence(raw []byte) string {
	redacted := sensitivePattern.ReplaceAll(raw, []byte(`$1[REDACTED]$3`))
	if len(redacted) <= maxEvidencePayloadBytes {
		return string(redacted)
	}
	cut := redacted[:maxEvidencePayloadBytes]
	if index := bytesLastRuneBoundary(cut); index >= 0 {
		cut = cut[:index]
	}
	return string(cut) + `...[TRUNCATED]`
}

func bytesLastRuneBoundary(data []byte) int {
	for index := len(data) - 1; index >= 0 && index >= len(data)-utf8Lookback; index-- {
		if data[index] < 0x80 || data[index]&0xC0 != 0x80 {
			return index + 1
		}
	}
	return len(data)
}

const utf8Lookback = 4

func countLogEntries(value string) int {
	return strings.Count(value, `"timestamp"`) + strings.Count(value, `"ts"`)
}

func numericMetric(evidence []loop.EvidenceItem) (float64, bool) {
	pattern := regexp.MustCompile(`"([0-9]+(?:\.[0-9]+)?)"`)
	for i := len(evidence) - 1; i >= 0; i-- {
		item := evidence[i]
		if item.Tool != "query_promql" {
			continue
		}
		raw, ok := item.Value.(string)
		if !ok {
			continue
		}
		matches := pattern.FindAllStringSubmatch(raw, -1)
		if len(matches) == 0 {
			continue
		}
		last := matches[len(matches)-1][1]
		value, err := strconv.ParseFloat(last, 64)
		if err == nil {
			return value, true
		}
	}
	return 0, false
}

func logHitCount(evidence []loop.EvidenceItem) int {
	for _, item := range evidence {
		if item.Tool == "query_logql" && item.Count > 0 {
			return item.Count
		}
	}
	return 0
}

func baselineRemediations(resourceType, target string) []loop.RemediationOption {
	switch resourceType {
	case "pg":
		return []loop.RemediationOption{
			remediation("pg.vacuum_analyze", target, "safe", true),
			remediation("pg.terminate_long_tx", target, "mutating", false),
		}
	case "redis":
		return []loop.RemediationOption{
			remediation("redis.memory_purge", target, "safe", true),
			remediation("redis.failover", target, "mutating", false),
		}
	case "k8s":
		return []loop.RemediationOption{
			remediation("k8s.rollout_status", target, "safe", true),
			remediation("k8s.rolling_restart", target, "mutating", false),
		}
	case "host":
		return []loop.RemediationOption{
			remediation("host.garbage_collect", target, "safe", true),
			remediation("host.restart_service", target, "mutating", false),
		}
	case "mq":
		return []loop.RemediationOption{
			remediation("mq.inspect_consumer_lag", target, "safe", true),
			remediation("mq.drain_queue", target, "mutating", false),
		}
	default:
		return nil
	}
}

func remediation(action, target, risk string, autoApprove bool) loop.RemediationOption {
	return loop.RemediationOption{Action: action, Target: target, Risk: risk, AutoApprove: autoApprove}
}
