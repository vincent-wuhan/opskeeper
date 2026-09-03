package loop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	ToolNameCorrelate   = "loop.correlate"
	ToolNameInvestigate = "loop.investigate"
	ToolNameVerify      = "recovery.verify"
)

var ErrMCPToolNotFound = errors.New("loop: MCP tool not found")

type MCPCorrelateAlert struct {
	AlertID    string            `json:"alert_id"`
	Severity   string            `json:"severity"`
	Resource   string            `json:"resource"`
	DetectedAt string            `json:"detected_at"`
	Summary    string            `json:"summary,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

type MCPCorrelateInput struct {
	RawAlerts []MCPCorrelateAlert `json:"raw_alerts"`
	Window    string              `json:"window"`
}

type MCPInvestigateInput struct {
	IncidentID       string         `json:"incident_id"`
	AlertGroup       []string       `json:"alert_group"`
	CorrelationHints map[string]any `json:"correlation_hints"`
}

type MCPVerifyInput struct {
	IncidentID     string   `json:"incident_id"`
	BaselineWindow string   `json:"baseline_window,omitempty"`
	CompareWindow  string   `json:"compare_window,omitempty"`
	Tolerance      *float64 `json:"tolerance,omitempty"`
	Metrics        []string `json:"metrics,omitempty"`
}

type MCPRecoveryContext struct {
	SkillID       string
	Target        string
	ResourceType  string
	Tolerance     float64
	VerifyMetrics []string
}

type MCPRecoveryContextLoader interface {
	LoadMCPRecoveryContext(ctx context.Context, tenantID, incidentID string) (*MCPRecoveryContext, error)
}

type ContractMCPRecoveryContextLoader struct {
	contracts ContractRepo
}

func NewContractMCPRecoveryContextLoader(contracts ContractRepo) ContractMCPRecoveryContextLoader {
	return ContractMCPRecoveryContextLoader{contracts: contracts}
}

type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type MCPInvestigateOutput struct {
	SchemaVersion      string                 `json:"schema_version"`
	RootCauseObject    *RootCauseObject       `json:"root_cause_object"`
	Confidence         float64                `json:"confidence"`
	EvidenceChain      []MCPEvidence          `json:"evidence_chain"`
	TimeWindow         TimeWindow             `json:"time_window"`
	RemediationOptions []MCPRemediationOption `json:"remediation_options"`
}

type MCPEvidence struct {
	Type       string    `json:"type"`
	Ref        string    `json:"ref"`
	ObservedAt time.Time `json:"observed_at"`
	Snippet    string    `json:"snippet,omitempty"`
}

type MCPRemediationOption struct {
	Action       string  `json:"action"`
	Target       string  `json:"target"`
	BlastRadius  string  `json:"blast_radius"`
	RollbackPlan string  `json:"rollback_plan"`
	Confidence   float64 `json:"confidence"`
}

type MCPVerifyOutput struct {
	SchemaVersion       string             `json:"schema_version"`
	Passed              bool               `json:"passed"`
	MetricsCompared     []string           `json:"metrics_compared"`
	Delta               map[string]float64 `json:"delta"`
	FailedMetrics       []string           `json:"failed_metrics,omitempty"`
	RollbackRecommended bool               `json:"rollback_recommended"`
	SampleSize          int                `json:"sample_size"`
	Tolerance           float64            `json:"tolerance"`
	RetryCount          int                `json:"retry_count"`
	WarningLevel        string             `json:"warning_level"`
}

type MCPAdapter struct {
	orchestrator    Orchestrator
	verify          VerifyRecoveryCaller
	recoveryContext MCPRecoveryContextLoader
}

func NewMCPAdapter(orchestrator Orchestrator, verify VerifyRecoveryCaller, recoveryLoaders ...MCPRecoveryContextLoader) *MCPAdapter {
	adapter := &MCPAdapter{orchestrator: orchestrator, verify: verify}
	if len(recoveryLoaders) > 0 {
		adapter.recoveryContext = recoveryLoaders[0]
	}
	return adapter
}

func (a *MCPAdapter) Tools(context.Context) []MCPTool {
	return []MCPTool{
		{
			Name:        ToolNameCorrelate,
			Description: "Correlate and deduplicate alerts",
			InputSchema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["raw_alerts","window"],"properties":{"raw_alerts":{"type":"array","minItems":1,"items":{"type":"object","required":["alert_id","severity","resource","detected_at"],"additionalProperties":true,"properties":{"alert_id":{"type":"string","minLength":1},"severity":{"type":"string","enum":["critical","warn","info","noise"]},"resource":{"type":"string","minLength":1},"detected_at":{"type":"string","format":"date-time"},"summary":{"type":"string"},"labels":{"type":"object","additionalProperties":{"type":"string"}}}}},"window":{"type":"string","pattern":"^[1-9][0-9]*(m|h|s)$"}}}`),
		},
		{
			Name:        ToolNameInvestigate,
			Description: "Return a structured root cause",
			InputSchema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["incident_id","alert_group","correlation_hints"],"properties":{"incident_id":{"type":"string","minLength":1},"alert_group":{"type":"array","minItems":1,"items":{"type":"string","minLength":1}},"correlation_hints":{"type":"object","additionalProperties":true,"properties":{"fingerprint":{"type":"string"},"resource_type":{"type":"string"},"target":{"type":"string"},"suspected_causes":{"type":"array","items":{"type":"string"}}}}}}`),
		},
		{
			Name:        ToolNameVerify,
			Description: "Compare recovery against adaptive baseline",
			InputSchema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["incident_id"],"properties":{"incident_id":{"type":"string","minLength":1},"baseline_window":{"type":"string","default":"5m","pattern":"^[1-9][0-9]*(m|h|s)$"},"compare_window":{"type":"string","default":"2m","pattern":"^[1-9][0-9]*(m|h|s)$"},"tolerance":{"type":"number","exclusiveMinimum":0,"maximum":1,"default":0.15},"metrics":{"type":"array","uniqueItems":true,"items":{"type":"string","enum":["cpu","mem","disk_io","net_in","net_out","conn_count","request_rate"]}}}}`),
		},
	}
}

func (a *MCPAdapter) Invoke(ctx context.Context, tenantID string, name string, arguments json.RawMessage) (any, error) {
	switch name {
	case ToolNameCorrelate:
		var input MCPCorrelateInput
		if err := strictUnmarshal(arguments, &input); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if err := input.validate(); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		return CorrelateAlerts(input)
	case ToolNameInvestigate:
		var input MCPInvestigateInput
		if err := strictUnmarshal(arguments, &input); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if input.IncidentID == "" || len(input.AlertGroup) == 0 || input.CorrelationHints == nil {
			return nil, errors.New("invalid arguments: incident_id, alert_group, and correlation_hints are required")
		}
		if err := input.validate(); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		result, err := a.orchestrator.Run(ctx, RunOptions{
			IncidentID:  input.IncidentID,
			TenantID:    tenantID,
			TriggeredBy: "mcp",
			// 真实链路里 critiqued 还要走 LLM 评审，输出 RootCauseJSON
			// 给到调用方后由 demo runner 继续推进 approved / recovered
			// / postmortem；MCP 接口先返回已确认的 RootCauseJSON。
			StopAfterPhase:   PhaseInvestigated,
			AlertGroup:       input.AlertGroup,
			CorrelationHints: input.CorrelationHints,
		})
		if err != nil {
			return nil, err
		}
		if result.RootCause == nil {
			return nil, errors.New("loop: investigation did not produce RootCauseJSON")
		}
		return newMCPInvestigateOutput(result.RootCause), nil
	case ToolNameVerify:
		var input MCPVerifyInput
		if err := strictUnmarshal(arguments, &input); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if err := rejectNullVerifyFields(arguments); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if input.IncidentID == "" {
			return nil, errors.New("invalid arguments: incident_id is required")
		}
		if err := input.validate(); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		recoveryContext, err := a.loadRecoveryContext(ctx, tenantID, input.IncidentID)
		if err != nil {
			return nil, err
		}
		internalArgs := input.toVerifyRecoveryArgs(recoveryContext)
		raw, err := json.Marshal(internalArgs)
		if err != nil {
			return nil, fmt.Errorf("marshal recovery.verify arguments: %w", err)
		}
		output, err := a.verify.InvokeVerifyRecovery(ctx, string(raw))
		if err != nil {
			return nil, err
		}
		var verified VerifiedDelta
		if err := json.Unmarshal([]byte(output), &verified); err != nil {
			return nil, fmt.Errorf("decode VerifiedDelta: %w", err)
		}
		return newMCPVerifyOutput(verified, input.Metrics), nil
	default:
		return nil, ErrMCPToolNotFound
	}
}

func newMCPInvestigateOutput(rootCause *RootCauseJSON) MCPInvestigateOutput {
	output := MCPInvestigateOutput{
		SchemaVersion:   "v1",
		RootCauseObject: rootCause.RootCauseObject,
		Confidence:      rootCause.Confidence,
		TimeWindow:      rootCause.TimeWindow,
	}
	for _, evidence := range rootCause.EvidenceChain {
		ref := evidence.Tool
		if evidence.Query != "" {
			ref += ":" + evidence.Query
		}
		if ref == "" {
			ref = "investigation"
		}
		snippet := evidence.Query
		if snippet == "" && evidence.Value != nil {
			snippet = fmt.Sprint(evidence.Value)
		}
		output.EvidenceChain = append(output.EvidenceChain, MCPEvidence{
			Type:       evidenceType(evidence.Tool),
			Ref:        ref,
			ObservedAt: evidence.Timestamp,
			Snippet:    snippet,
		})
	}
	for _, option := range rootCause.RemediationOptions {
		output.RemediationOptions = append(output.RemediationOptions, MCPRemediationOption{
			Action:       option.Action,
			Target:       option.Target,
			BlastRadius:  blastRadius(option.Risk),
			RollbackPlan: fmt.Sprintf("Operator must review and reverse %s on %s before retrying.", option.Action, option.Target),
			Confidence:   rootCause.Confidence,
		})
	}
	return output
}

func evidenceType(tool string) string {
	switch {
	case strings.Contains(tool, "log"):
		return "log"
	case strings.Contains(tool, "trace"):
		return "trace"
	case strings.Contains(tool, "alert"):
		return "alert"
	case strings.Contains(tool, "git"):
		return "git_commit"
	case strings.Contains(tool, "harness"):
		return "harness_case"
	case strings.Contains(tool, "query"):
		return "query_result"
	default:
		return "metric"
	}
}

func blastRadius(risk string) string {
	switch risk {
	case "dangerous":
		return "tenant_wide"
	case "mutating":
		return "cluster"
	default:
		return "single_device"
	}
}

func newMCPVerifyOutput(verified VerifiedDelta, requestedMetrics []string) MCPVerifyOutput {
	metrics := make([]string, 0, len(verified.Deltas))
	usedMetrics := make(map[string]struct{}, len(verified.Deltas))
	if len(requestedMetrics) > 0 {
		for _, metric := range requestedMetrics {
			mapped := mapProtocolVerifyMetric(metric, metric)
			if _, ok := verified.Deltas[mapped]; ok {
				metrics = append(metrics, metric)
				usedMetrics[metric] = struct{}{}
			}
		}
	} else {
		for internalMetric := range verified.Deltas {
			protocolMetric := internalVerifyMetricToProtocol(internalMetric)
			metrics = append(metrics, protocolMetric)
			usedMetrics[protocolMetric] = struct{}{}
		}
	}
	sort.Strings(metrics)
	delta := make(map[string]float64, len(metrics))
	for _, metric := range metrics {
		delta[metric] = verified.Deltas[mapProtocolVerifyMetric(metric, metric)]
	}
	failedMetrics := make([]string, 0, len(verified.FailedMetrics))
	for _, metric := range metrics {
		for _, failed := range verified.FailedMetrics {
			if failed == mapProtocolVerifyMetric(metric, metric) {
				failedMetrics = append(failedMetrics, metric)
			}
		}
	}
	return MCPVerifyOutput{
		SchemaVersion:       "v1",
		Passed:              verified.Passed,
		MetricsCompared:     metrics,
		Delta:               delta,
		FailedMetrics:       failedMetrics,
		RollbackRecommended: !verified.Passed,
		SampleSize:          verified.SampleSize,
		Tolerance:           verified.Tolerance,
		RetryCount:          verified.RetryCount,
		WarningLevel:        verified.WarningLevel,
	}
}

func internalVerifyMetricToProtocol(metric string) string {
	switch metric {
	case "cpu_usage":
		return "cpu"
	case "mem_usage":
		return "mem"
	case "latency_p99":
		return "disk_io"
	case "qps":
		return "request_rate"
	default:
		return metric
	}
}

func (l ContractMCPRecoveryContextLoader) LoadMCPRecoveryContext(ctx context.Context, tenantID, incidentID string) (*MCPRecoveryContext, error) {
	if l.contracts == nil {
		return nil, errors.New("loop: recovery context contract repository is required")
	}
	contract, err := l.contracts.ReadContract(ctx, tenantID, incidentID, PhaseApproved, "ApprovalDecision")
	if err != nil {
		return nil, fmt.Errorf("loop: load ApprovalDecision for recovery.verify: %w", err)
	}
	if contract != nil {
		var approval ApprovalDecision
		if err := json.Unmarshal([]byte(contract.Payload), &approval); err != nil {
			return nil, fmt.Errorf("loop: decode ApprovalDecision for recovery.verify: %w", err)
		}
		if approval.SkillID == "" || approval.Target == "" || approval.ResourceType == "" {
			return nil, errors.New("loop: ApprovalDecision lacks skill_id, target, or resource_type")
		}
		return &MCPRecoveryContext{
			SkillID:       approval.SkillID,
			Target:        approval.Target,
			ResourceType:  approval.ResourceType,
			Tolerance:     approval.Tolerance,
			VerifyMetrics: approval.VerifyMetrics,
		}, nil
	}

	contract, err = l.contracts.ReadContract(ctx, tenantID, incidentID, PhaseInvestigated, "root_cause_json")
	if err != nil {
		return nil, fmt.Errorf("loop: load RootCauseJSON for recovery.verify: %w", err)
	}
	if contract == nil {
		return nil, errors.New("loop: recovery.verify requires an ApprovalDecision or RootCauseJSON")
	}
	var rootCause RootCauseJSON
	if err := json.Unmarshal([]byte(contract.Payload), &rootCause); err != nil {
		return nil, fmt.Errorf("loop: decode RootCauseJSON for recovery.verify: %w", err)
	}
	if rootCause.RootCauseObject == nil || len(rootCause.RemediationOptions) == 0 {
		return nil, errors.New("loop: RootCauseJSON lacks root_cause_object or remediation_options")
	}
	target := rootCause.RemediationOptions[0].Target
	if target == "" {
		return nil, errors.New("loop: RootCauseJSON remediation target is empty")
	}
	return &MCPRecoveryContext{
		SkillID:      rootCause.RootCauseObject.Kind,
		Target:       target,
		ResourceType: resourceType(target),
	}, nil
}

func (a *MCPAdapter) loadRecoveryContext(ctx context.Context, tenantID, incidentID string) (*MCPRecoveryContext, error) {
	if a.recoveryContext != nil {
		return a.recoveryContext.LoadMCPRecoveryContext(ctx, tenantID, incidentID)
	}
	return nil, errors.New("loop: recovery context loader is required")
}

type verifyRecoveryInvocation struct {
	SkillID        string   `json:"skill_id"`
	Target         string   `json:"target"`
	ResourceType   string   `json:"resource_type"`
	BaselineWindow string   `json:"baseline_window,omitempty"`
	CompareWindow  string   `json:"compare_window,omitempty"`
	Tolerance      float64  `json:"tolerance,omitempty"`
	Metrics        []string `json:"metrics"`
}

func (in MCPVerifyInput) toVerifyRecoveryArgs(context *MCPRecoveryContext) verifyRecoveryInvocation {
	baselineWindow := "5m"
	if in.BaselineWindow != "" {
		baselineWindow = in.BaselineWindow
	}
	compareWindow := "2m"
	if in.CompareWindow != "" {
		compareWindow = in.CompareWindow
	}
	tolerance := context.Tolerance
	if in.Tolerance != nil {
		tolerance = *in.Tolerance
	} else if tolerance == 0 {
		tolerance = 0.15
	}
	protocolMetrics := in.Metrics
	if len(protocolMetrics) == 0 {
		protocolMetrics = context.VerifyMetrics
	}
	metrics := make([]string, 0, len(protocolMetrics))
	for _, metric := range protocolMetrics {
		metrics = append(metrics, mapProtocolVerifyMetric(metric, metric))
	}
	if len(metrics) == 0 {
		metrics = defaultInternalVerifyMetrics(context.ResourceType)
	}
	return verifyRecoveryInvocation{
		SkillID:        context.SkillID,
		Target:         context.Target,
		ResourceType:   context.ResourceType,
		BaselineWindow: baselineWindow,
		CompareWindow:  compareWindow,
		Tolerance:      tolerance,
		Metrics:        metrics,
	}
}

func mapProtocolVerifyMetric(metric, fallback string) string {
	switch metric {
	case "cpu":
		return "cpu_usage"
	case "mem":
		return "mem_usage"
	case "disk_io":
		return "latency_p99"
	case "net_in", "net_out", "conn_count", "request_rate":
		return "qps"
	default:
		return fallback
	}
}

func defaultInternalVerifyMetrics(resourceType string) []string {
	switch resourceType {
	case "redis":
		return []string{"mem_usage", "qps", "latency_p99"}
	case "app":
		return []string{"qps", "latency_p99"}
	case "pg":
		return []string{"cpu_usage", "mem_usage", "qps", "latency_p99"}
	default:
		return []string{"cpu_usage", "mem_usage"}
	}
}

func (in *MCPVerifyInput) validate() error {
	if err := validateProtocolWindow(in.BaselineWindow); in.BaselineWindow != "" && err != nil {
		return fmt.Errorf("baseline_window: %w", err)
	}
	if err := validateProtocolWindow(in.CompareWindow); in.CompareWindow != "" && err != nil {
		return fmt.Errorf("compare_window: %w", err)
	}
	if in.Tolerance != nil && (*in.Tolerance <= 0 || *in.Tolerance > 1) {
		return errors.New("tolerance must be greater than 0 and at most 1")
	}
	seen := make(map[string]struct{}, len(in.Metrics))
	for _, metric := range in.Metrics {
		if !validProtocolVerifyMetric(metric) {
			return fmt.Errorf("metrics contains unsupported value %q", metric)
		}
		if _, ok := seen[metric]; ok {
			return fmt.Errorf("metrics contains duplicate value %q", metric)
		}
		seen[metric] = struct{}{}
	}
	return nil
}

func rejectNullVerifyFields(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, name := range []string{"baseline_window", "compare_window", "tolerance", "metrics"} {
		if value, ok := fields[name]; ok && string(value) == "null" {
			return fmt.Errorf("%s must not be null", name)
		}
	}
	return nil
}

func (in *MCPCorrelateInput) validate() error {
	for _, alert := range in.RawAlerts {
		if alert.AlertID == "" || alert.Resource == "" {
			return errors.New("raw_alerts entries require alert_id and resource")
		}
		if !validSeverity(alert.Severity) {
			return fmt.Errorf("raw_alerts contains unsupported severity %q", alert.Severity)
		}
		if _, err := time.Parse(time.RFC3339, alert.DetectedAt); err != nil {
			return fmt.Errorf("raw_alerts contains invalid detected_at %q", alert.DetectedAt)
		}
	}
	return validateProtocolWindow(in.Window)
}

func (in *MCPInvestigateInput) validate() error {
	seen := make(map[string]struct{}, len(in.AlertGroup))
	for _, alertID := range in.AlertGroup {
		if alertID == "" {
			return errors.New("alert_group entries must not be empty")
		}
		if _, ok := seen[alertID]; ok {
			return fmt.Errorf("alert_group contains duplicate value %q", alertID)
		}
		seen[alertID] = struct{}{}
	}
	return nil
}

var protocolWindowPattern = regexp.MustCompile(`^[1-9][0-9]*(m|h|s)$`)

func validateProtocolWindow(value string) error {
	if !protocolWindowPattern.MatchString(value) {
		return errors.New("window must match ^[1-9][0-9]*(m|h|s)$")
	}
	return nil
}

func validProtocolVerifyMetric(metric string) bool {
	switch metric {
	case "cpu", "mem", "disk_io", "net_in", "net_out", "conn_count", "request_rate":
		return true
	default:
		return false
	}
}

func CorrelateAlerts(in MCPCorrelateInput) (any, error) {
	if len(in.RawAlerts) == 0 {
		return nil, errors.New("invalid arguments: raw_alerts is required")
	}
	window, err := parsePositiveWindow(in.Window)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: window: %w", err)
	}
	groups := make(map[string][]MCPCorrelateAlert)
	for _, alert := range in.RawAlerts {
		if alert.AlertID == "" || alert.Resource == "" || alert.DetectedAt == "" {
			return nil, errors.New("invalid arguments: alert_id, resource, and detected_at are required")
		}
		if !validSeverity(alert.Severity) {
			return nil, fmt.Errorf("invalid arguments: unsupported severity %q", alert.Severity)
		}
		if _, err := time.Parse(time.RFC3339, alert.DetectedAt); err != nil {
			return nil, fmt.Errorf("invalid arguments: detected_at: %w", err)
		}
		detectedAt, _ := time.Parse(time.RFC3339, alert.DetectedAt)
		bucket := int64(detectedAt.Unix()) / int64(window.Seconds())
		key := strings.ToLower(alert.Resource) + "\x00" + fmt.Sprint(bucket) + "\x00" + semanticKey(alert.Summary)
		groups[key] = append(groups[key], alert)
	}

	outputGroups := make([]map[string]any, 0, len(groups))
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		alerts := groups[key]
		sort.Slice(alerts, func(i, j int) bool { return alerts[i].DetectedAt < alerts[j].DetectedAt })
		alertIDs := make([]string, 0, len(alerts))
		for _, alert := range alerts {
			alertIDs = append(alertIDs, alert.AlertID)
		}
		first, last := alerts[0], alerts[len(alerts)-1]
		digest := sha256.Sum256([]byte(key))
		outputGroups = append(outputGroups, map[string]any{
			"fingerprint":     hex.EncodeToString(digest[:16]),
			"alert_ids":       alertIDs,
			"root_hypothesis": first.Summary,
			"confidence":      0.75,
			"resource_type":   resourceType(first.Resource),
			"target":          first.Resource,
			"time_window":     map[string]string{"start": first.DetectedAt, "end": last.DetectedAt},
		})
	}
	for _, group := range outputGroups {
		if hypothesis, _ := group["root_hypothesis"].(string); hypothesis == "" {
			group["root_hypothesis"] = "alerts share resource and time window"
		}
	}
	return map[string]any{
		"correlated_groups": outputGroups,
		"dedup_reason":      fmt.Sprintf("same resource and normalized summary within %s", window),
		"severity":          highestSeverity(in.RawAlerts),
	}, nil
}

func semanticKey(summary string) string {
	fields := strings.FieldsFunc(strings.ToLower(summary), func(r rune) bool {
		return r == ' ' || r == '\t' || r == ',' || r == ';' || r == ':' || r == '-'
	})
	noise := map[string]struct{}{"a": {}, "an": {}, "the": {}, "is": {}, "are": {}, "was": {}, "were": {}}
	keywords := make([]string, 0, len(fields))
	for _, field := range fields {
		if _, ok := noise[field]; ok {
			continue
		}
		keywords = append(keywords, field)
	}
	sort.Strings(keywords)
	return strings.Join(keywords, " ")
}

func resourceType(resource string) string {
	prefix, _, _ := strings.Cut(resource, ":")
	switch strings.ToLower(prefix) {
	case "pg", "postgres", "postgresql":
		return "pg"
	case "redis":
		return "redis"
	case "k8s", "kubernetes":
		return "k8s"
	case "app":
		return "app"
	default:
		return "host"
	}
}

func validSeverity(severity string) bool {
	switch severity {
	case "critical", "warn", "info", "noise":
		return true
	default:
		return false
	}
}

func highestSeverity(alerts []MCPCorrelateAlert) string {
	rank := map[string]int{"noise": 0, "info": 1, "warn": 2, "critical": 3}
	result := "noise"
	for _, alert := range alerts {
		if rank[alert.Severity] > rank[result] {
			result = alert.Severity
		}
	}
	return result
}

func parsePositiveWindow(value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, errors.New("must be a positive duration such as 5m")
	}
	return duration, nil
}

func strictUnmarshal(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
