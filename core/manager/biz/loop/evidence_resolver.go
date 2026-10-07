package loop

// The production ArgResolver: it reads the arguments a remediation acts on
// out of the evidence the investigation already recorded.
//
// Why this exists rather than defaults. A RemediationOption carries an
// action name and a resource locator ("pg:alert-17") — never the pid, role
// or session the action targets. The tempting bridge is a default: kill the
// oldest backend, pause the role that looks busiest. That is how "terminate
// the long transaction" silently becomes "terminate a transaction nobody
// chose", and it is irreversible.
//
// So every extractor here is evidence-backed, and every ambiguity is a
// refusal. When the investigation recorded one candidate the extractor
// returns it; when it recorded several it lists them and declines, because
// choosing between them is a judgement the evidence does not make.
//
// The map covers every action whose arguments are recorded somewhere the
// investigation can be held to. An action with no entry returns (nil, nil),
// which leaves the invoker's own missing-argument refusal in charge.
// Declaring an extractor that guesses would be worse than declaring none —
// so the rule for adding one is that it must be able to say no.
//
// Two sources of truth feed it. The pg resolvers read pg_stat_activity rows,
// which the investigator queried. The seven object-identity resolvers read
// the recorded subject (see subject.go), which is the firing alert's own
// labels. Both are observations rather than inferences, and both refuse when
// the evidence is absent or contradicts itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// EvidenceArgResolver resolves tool arguments from an incident's recorded
// evidence chain.
type EvidenceArgResolver struct {
	// Causes reads the RootCauseJSON for the incident. Required for any
	// action that has a declared extractor; a nil loader makes those
	// actions refuse with a reason instead of dispatching without
	// arguments.
	Causes RootCauseLoader
}

// evidenceArgExtractor turns evidence items into the arguments of one
// action. It receives the option as well, so an extractor may use the
// target; none currently does, and that is a fact about the evidence rather
// than a design preference.
type evidenceArgExtractor func(req RemediationRequest, evidence []EvidenceItem) (map[string]any, error)

// evidenceArgExtractors is the closed set of actions whose arguments are
// recoverable from evidence.
//
// Keyed by the exact tool name, like every other vocabulary in this package:
// an action that is not in this map is not resolved, and a name that drifts
// stops resolving rather than resolves wrongly.
var evidenceArgExtractors = map[string]evidenceArgExtractor{
	"pg.kill_session":       extractPGSessionPID,
	"pg.connection_pause":   extractPGRole,
	"pg.vacuum_table":       extractPGBloatTable,
	"pg.explain_query":      extractPGSlowQuery,
	"k8s.evict_pod":         extractK8sPod,
	"k8s.rolling_restart":   extractK8sDeployment,
	"k8s.rollout_undo":      extractK8sStuckRollout,
	"k8s.uncordon":          extractK8sNotReadyNode,
	"k8s.drain":             extractK8sNotReadyNode,
	"k8s.scale":             extractK8sScale,
	"k8s.resize_pvc":        extractK8sResizePVC,
	"kafka.repartition":     extractKafkaRepartition,
	"mq.drain_queue":        extractMQQueue,
	"mq.replay_messages":    extractMQQueue,
	"host.restart_service":  extractHostUnit,
	"host.kill_process":     extractHostHotProcess,
	"host.remove_old_logs":  extractHostLogPath,
	"redis.client_kill":     extractRedisAddr,
	"redis.scan_and_delete": extractRedisMinBytes,
}

// Accepted alert-label keys per subject field.
//
// An alert names its object under whichever key its rule was written with,
// so each field accepts the spellings that appear in real rules rather than
// one canonical name. These are the ONLY keys an extractor will read: a
// label that is not listed here cannot become an argument, so adding an
// unrelated label to the platform does not widen what a write action can act
// on.
var (
	podLabelKeys        = []string{"pod", "k8s_pod", "pod_name", "exported_pod", "kubernetes_pod_name"}
	deploymentLabelKeys = []string{"deployment", "k8s_deployment", "deploy", "deployment_name", "workload"}
	replicasLabelKeys   = []string{"replicas", "desired_replicas", "spec_replicas"}
	queueLabelKeys      = []string{"queue", "mq_queue", "queue_name", "kafka_topic", "topic"}
	unitLabelKeys       = []string{"unit", "systemd_unit", "service", "service_name"}
	addrLabelKeys       = []string{"addr", "client_addr", "client", "address"}
)

// subjectArg reads one identity argument from the recorded subject.
func subjectArg(req RemediationRequest, evidence []EvidenceItem, action, arg string, accepted []string) (map[string]any, error) {
	labels, ok := subjectLabels(evidence)
	if !ok {
		return nil, missingSubject(action, arg, accepted)
	}
	value, _, err := subjectValue(labels, accepted)
	if err != nil {
		return nil, fmt.Errorf("%s cannot resolve %s: %w", action, arg, err)
	}
	if value == "" {
		return nil, missingSubject(action, arg, accepted)
	}
	return map[string]any{arg: value}, nil
}

func extractK8sPod(req RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	return subjectArg(req, evidence, "k8s.evict_pod", "pod", podLabelKeys)
}

func extractK8sDeployment(req RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	return subjectArg(req, evidence, "k8s.rolling_restart", "deployment", deploymentLabelKeys)
}

func extractK8sScale(req RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	deployment, _, err := subjectDeployment(evidence)
	if err != nil {
		return nil, err
	}
	replicas, _, err := subjectReplicas(evidence)
	if err != nil {
		return nil, err
	}
	return map[string]any{"deployment": deployment, "replicas": replicas}, nil
}

func subjectDeployment(evidence []EvidenceItem) (string, []string, error) {
	labels, ok := subjectLabels(evidence)
	if !ok {
		return "", nil, missingSubject("k8s.scale", "deployment", deploymentLabelKeys)
	}
	value, keys, err := subjectValue(labels, deploymentLabelKeys)
	if err != nil {
		return "", keys, fmt.Errorf("k8s.scale cannot resolve deployment: %w", err)
	}
	if value == "" {
		return "", nil, missingSubject("k8s.scale", "deployment", deploymentLabelKeys)
	}
	return value, keys, nil
}

func subjectReplicas(evidence []EvidenceItem) (int, []string, error) {
	labels, ok := subjectLabels(evidence)
	if !ok {
		return 0, nil, missingSubject("k8s.scale", "replicas", replicasLabelKeys)
	}
	value, keys, err := subjectValue(labels, replicasLabelKeys)
	if err != nil {
		return 0, keys, fmt.Errorf("k8s.scale cannot resolve replicas: %w", err)
	}
	if value == "" {
		return 0, nil, missingSubject("k8s.scale", "replicas", replicasLabelKeys)
	}
	n, convErr := strconv.Atoi(value)
	if convErr != nil || n < 0 {
		return 0, keys, fmt.Errorf("k8s.scale cannot resolve replicas: the alert records %q (%s), "+
			"which is not a replica count; scaling to it would be a guess", value, strings.Join(keys, "/"))
	}
	return n, keys, nil
}

func extractMQQueue(req RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	action := strings.TrimSpace(req.Option.Action)
	return subjectArg(req, evidence, action, "queue", queueLabelKeys)
}

func extractHostUnit(req RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	return subjectArg(req, evidence, "host.restart_service", "unit", unitLabelKeys)
}

func extractRedisAddr(req RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	return subjectArg(req, evidence, "redis.client_kill", "addr", addrLabelKeys)
}

// Resolve implements ArgResolver.
func (r EvidenceArgResolver) Resolve(ctx context.Context, req RemediationRequest, spec ToolSpec) (map[string]any, error) {
	action := strings.TrimSpace(req.Option.Action)
	extract, ok := evidenceArgExtractors[action]
	if !ok {
		// Not declared: the invoker's missing-argument check already
		// speaks for this action, and it names the arguments rather
		// than the missing extractor, which is the more useful of the
		// two messages.
		return nil, nil
	}
	if len(spec.RequiredArgs) == 0 {
		// The tool wants nothing this resolver could supply.
		return nil, nil
	}
	if r.Causes == nil {
		return nil, errors.New("no root-cause loader is wired, so the evidence cannot be read")
	}
	rc, err := r.Causes.LoadRootCause(ctx, req.TenantID, req.IncidentID)
	if err != nil {
		return nil, fmt.Errorf("read the evidence for incident %s: %w", req.IncidentID, err)
	}
	if rc == nil {
		return nil, fmt.Errorf("incident %s has no recorded root cause, so %s has no evidence to act on",
			req.IncidentID, action)
	}
	return extract(req, rc.EvidenceChain)
}

var _ ArgResolver = EvidenceArgResolver{}

// ActionsResolvableFromEvidence is every action for which a production
// extractor is declared.
//
// It is exported because it is a claim about this build that nothing else
// can see: the evaluation gate lists the actions whose required arguments a
// RemediationOption cannot carry, and without this list it cannot tell the
// ones that have somewhere to get them (a pid, a role) from the ones that
// have nowhere at all (a pod name no evidence records). Both are refusals
// today, and they need different work.
func ActionsResolvableFromEvidence() []string {
	out := make([]string, 0, len(evidenceArgExtractors))
	for action := range evidenceArgExtractors {
		out = append(out, action)
	}
	sort.Strings(out)
	return out
}

// extractPGSessionPID returns the pid of the one session the evidence
// describes.
func extractPGSessionPID(_ RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	rows := pgActivityRows(evidence)
	if len(rows) == 0 {
		return nil, errors.New("no pg_stat_activity evidence was recorded, so there is no session to terminate")
	}
	byPID := map[int64][]string{}
	for _, row := range rows {
		pid, ok := rowInt(row, "pid")
		if !ok || pid <= 0 {
			continue
		}
		byPID[pid] = append(byPID[pid], describeSession(row))
	}
	switch len(byPID) {
	case 0:
		return nil, fmt.Errorf("the evidence names %d session(s) but none carries a usable pid", len(rows))
	case 1:
		for pid := range byPID {
			return map[string]any{"pid": pid}, nil
		}
	}
	return nil, fmt.Errorf("the evidence describes %d sessions and does not say which one to terminate; "+
		"terminating one of them at random is not a remediation, so this is refused — candidates: %s",
		len(byPID), strings.Join(sortedPIDs(byPID), ", "))
}

// extractPGRole returns the role the evidence shows as the source of the
// load.
func extractPGRole(_ RemediationRequest, evidence []EvidenceItem) (map[string]any, error) {
	rows := pgActivityRows(evidence)
	if len(rows) == 0 {
		return nil, errors.New("no pg_stat_activity evidence was recorded, so there is no role to pause")
	}
	roles := map[string]struct{}{}
	for _, row := range rows {
		role, ok := rowString(row, "usename")
		if !ok || role == "" {
			continue
		}
		roles[role] = struct{}{}
	}
	switch len(roles) {
	case 0:
		return nil, fmt.Errorf("the evidence names %d session(s) but none carries a usename", len(rows))
	case 1:
		for role := range roles {
			return map[string]any{"role": role}, nil
		}
	}
	return nil, fmt.Errorf("the evidence spans %d roles (%s) and pausing all of them is not what was approved",
		len(roles), strings.Join(sortedStrings(roles), ", "))
}

// pgActivityRows pulls the pg_stat_activity rows out of an evidence chain.
//
// The tool label is not what selects them: the real PostgreSQL investigator
// writes "pg_stat_activity", the golden corpus names the same fact
// "pg.active_sessions", and a resolver keyed on either one would go blind
// the day the other is used. A row that carries a pid is an activity row,
// so that is the test.
func pgActivityRows(evidence []EvidenceItem) []map[string]any {
	var rows []map[string]any
	for _, item := range evidence {
		for _, row := range decodeRows(item.Value) {
			if _, ok := row["pid"]; ok {
				rows = append(rows, row)
			}
		}
	}
	return rows
}

// decodeRows accepts every shape an evidence value arrives in: the typed
// slice the investigator built, the []any a JSON round trip through the
// contract table produces, and a raw JSON string. The contract stores
// evidence as interface{}, so all three are real.
func decodeRows(value any) []map[string]any {
	switch v := value.(type) {
	case []map[string]any:
		return v
	case []any:
		rows := make([]map[string]any, 0, len(v))
		for _, entry := range v {
			if row, ok := entry.(map[string]any); ok {
				rows = append(rows, row)
			}
		}
		return rows
	case string:
		var rows []map[string]any
		if err := json.Unmarshal([]byte(v), &rows); err != nil {
			return nil
		}
		return rows
	case json.RawMessage:
		var rows []map[string]any
		if err := json.Unmarshal(v, &rows); err != nil {
			return nil
		}
		return rows
	default:
		return nil
	}
}

// rowInt reads an integer field. A JSON round trip turns every number into
// a float64, so a resolver that only accepted int would fail on exactly the
// path it was written for.
func rowInt(row map[string]any, key string) (int64, bool) {
	raw, ok := row[key]
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		if v != float64(int64(v)) {
			return 0, false
		}
		return int64(v), true
	case json.Number:
		i, err := v.Int64()
		return i, err == nil
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

func rowString(row map[string]any, key string) (string, bool) {
	raw, ok := row[key]
	if !ok {
		return "", false
	}
	s, ok := raw.(string)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(s), true
}

// describeSession renders one candidate for a refusal message. It names the
// fields an operator would use to decide between candidates, and it does not
// include the query text: evidence is sanitized before it is stored, but a
// refusal is printed in more places than the evidence is.
func describeSession(row map[string]any) string {
	pid, _ := rowInt(row, "pid")
	parts := []string{fmt.Sprintf("pid %d", pid)}
	if role, ok := rowString(row, "usename"); ok && role != "" {
		parts = append(parts, "user="+role)
	}
	if app, ok := rowString(row, "application_name"); ok && app != "" {
		parts = append(parts, "app="+app)
	}
	if state, ok := rowString(row, "state"); ok && state != "" {
		parts = append(parts, "state="+state)
	}
	return strings.Join(parts, " ")
}

func sortedPIDs(byPID map[int64][]string) []string {
	out := make([]string, 0, len(byPID))
	for pid, descriptors := range byPID {
		out = append(out, fmt.Sprintf("pid %d (%s)", pid, strings.Join(descriptors, "; ")))
	}
	sort.Strings(out)
	return out
}

func sortedStrings(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
