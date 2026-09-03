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

	"github.com/vincent-wuhan/opskeeper/internal/pkg/logquery"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/promquery"

	loop "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
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
	log     *slog.Logger
	now     func() time.Time
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
		if logCount > 0 {
			options = append(options, remediation("pg.kill_backend", target, "mutating", false))
		}
	case "redis":
		if hasMetric && metricValue >= 0.90 {
			options = append(options, remediation("redis.failover", target, "mutating", false))
		}
		if logCount > 0 {
			options = append(options, remediation("redis.client_kill", target, "mutating", false))
		}
	case "k8s":
		if hasMetric && metricValue >= 0.10 {
			options = append(options,
				remediation("k8s.rolling_restart", target, "mutating", false),
				remediation("k8s.scale_up", target, "mutating", false),
			)
		}
		if logCount >= 10 {
			options = append(options, remediation("k8s.evict_pod", target, "mutating", false))
		}
	case "host":
		if hasMetric && metricValue >= 0.80 {
			options = append(options, remediation("host.restart_service", target, "mutating", false))
		}
	case "mq":
		if hasMetric && metricValue >= 1000 {
			options = append(options, remediation("mq.drain_queue", target, "mutating", false))
		}
		if logCount > 0 {
			options = append(options, remediation("mq.replay_messages", target, "mutating", false))
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
