// Package version — deployment.go
//
// Deployment composition surfaced for operations review:
// Manager binary version, TeamHarness plugin
// version, per-skill versions, plugin → Manager contract, server
// health-check summary, deploy dependencies, and one worked
// recovery operation example (the PG connection-pool main scenario).
//
// The handler is intentionally read-only and unauthenticated so the
// `/admin/runtime` Element panel can poll it without holding a
// bearer token. The endpoint never returns secrets, credentials,
// private hosts, or internal-only topology.
//
// Routes:
//
//	GET /v1/version/deployment
//	  → DeploymentResponse (see below)
//
// The handler is wired in cmd/opskeeper/main.go next to the
// existing `/v1/version` route. We deliberately do NOT extend the
// `/v1/version` shape (kept stable for the AgentTeams drift
// detector); the richer view lives under `/v1/version/deployment`
// so the new fields don't break AgentTeams.
package version

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// hashError returns a short, stable fingerprint of err.Error() so
// version-page consumers can correlate health-summary notes with the
// real error in server logs without leaking the raw message (which
// can carry DSNs, hostnames, file paths, or stack traces). Pair the
// fingerprint with a coarse classification so the SPA can still
// colour the badge without seeing the payload.
func hashError(err error) string {
	if err == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(err.Error()))
	return hex.EncodeToString(sum[:6])
}

// classifyHealthError maps an error to a safe coarse-grained label
// the version page can render. We deliberately avoid surfacing the
// raw message: DSN strings, host names, and stack frames would all
// land in the public /v1/version/deployment payload otherwise.
func classifyHealthError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "not wired"):
		return "seam_not_wired"
	case strings.Contains(msg, "context deadline"):
		return "timeout"
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "dial"):
		return "unreachable"
	case strings.Contains(msg, "permission denied"),
		strings.Contains(msg, "unauthorized"),
		strings.Contains(msg, "forbidden"):
		return "auth_or_perm"
	}
	return "degraded"
}

// ManagerVersion is the build-time-stamped version of the opskeeper
// binary. cmd/opskeeper/main.go passes the same `version` variable
// (already ldflag-overridable) into the handler at construction time.
type ManagerVersion string

// PluginManifest is the JSON shape of
// plugins/opskeeper-teamharness/dashboard/plugin.json. We only read
// the few fields we need for the version page; everything else is
// passed through verbatim so the panel can render the full manifest
// if asked.
type PluginManifest struct {
	APIVersion       string            `json:"apiVersion"`
	Kind             string            `json:"kind"`
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Version          string            `json:"version"`
	Description      string            `json:"description,omitempty"`
	Author           string            `json:"author,omitempty"`
	Entry            map[string]string `json:"entry,omitempty"`
	DashboardVersion string            `json:"dashboardVersion,omitempty"`
	MinVersion       string            `json:"min_version,omitempty"`
	ExtensionPoints  []string          `json:"extensionPoints,omitempty"`
	Permissions      []string          `json:"permissions,omitempty"`
	Dependencies     []string          `json:"dependencies,omitempty"`
}

// SkillVersion is one worker's skill_meta.yaml content, flattened to
// the fields the deployment panel renders.
type SkillVersion struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Role    string `json:"role"`
	Path    string `json:"path"`
}

// HealthSummary is the trimmed systemhealth.Report the panel renders
// at the top of the version page. We don't reuse systemhealth.Report
// directly because the version page is meant to be self-contained —
// it should still render when systemhealth is not wired (e.g. dev
// mode without a DB).
type HealthSummary struct {
	Overall string `json:"overall"`
	DB      string `json:"db"`
	Prom    string `json:"prom"`
	Logs    string `json:"logs"`
	Traces  string `json:"traces"`
	LLM     string `json:"llm"`
	Note    string `json:"note,omitempty"`
}

// DeployDependency is one external service the Manager talks to.
// We intentionally do NOT include URLs / DSNs — only the service
// type and whether it's configured. The panel uses this to draw
// the topology chip strip ("DB ✓ · Prom ✓ · LLM ✗").
type DeployDependency struct {
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Required   bool   `json:"required"`
	Note       string `json:"note,omitempty"`
}

// RecoveryOperation is one worked example the panel renders below
// the dependency strip. We use the PG connection-pool main scenario
// as the canonical example because it covers the full
// 7-stage loop and exercises every audit kind (dispatch /
// approval / execution / verification).
type RecoveryOperation struct {
	IncidentID     string              `json:"incident_id"`
	Title          string              `json:"title"`
	FaultFamily    string              `json:"fault_family"`
	PhasesObserved int                 `json:"phases_observed"`
	PhasesExpected int                 `json:"phases_expected"`
	RecoverySignal bool                `json:"recovery_signal"`
	Closed         bool                `json:"closed"`
	DurationSec    int                 `json:"duration_sec"`
	AuditKinds     []string            `json:"audit_kinds"`
	Evidence       map[string]string   `json:"evidence,omitempty"`
	Phases         []TimelinePhaseLite `json:"phases,omitempty"`
}

// TimelinePhaseLite is a tiny phase-row shape (no tool calls / no
// audit rows) so the version panel can render the 7-stage sequence
// without duplicating the full ClosedLoopTimeline payload.
type TimelinePhaseLite struct {
	Phase      string `json:"phase"`
	PhaseLabel string `json:"phase_label"`
	Status     string `json:"status"`
	WorkerRole string `json:"worker_role"`
	SkillVer   string `json:"skill_version"`
	Duration   string `json:"duration,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// DeploymentResponse is the GET /v1/version/deployment body.
type DeploymentResponse struct {
	Manager      ManagerVersion     `json:"manager_version"`
	GoVersion    string             `json:"go_version"`
	OSArch       string             `json:"os_arch"`
	BuildAt      string             `json:"build_at,omitempty"`
	Plugin       PluginManifest     `json:"plugin"`
	Skills       []SkillVersion     `json:"skills"`
	Health       HealthSummary      `json:"health"`
	Dependencies []DeployDependency `json:"dependencies"`
	Recovery     RecoveryOperation  `json:"recovery_example"`
	GeneratedAt  string             `json:"generated_at"`
}

// Source describes how the handler resolves each piece of the
// deployment payload. The version page uses Source to colour the
// "data source" badge so reviewers can see whether a value came
// from the runtime, from disk, or from a fixture.
type Source struct {
	PluginPath    string
	SkillsDir     string
	HealthService HealthSource
}

// HealthSource is the narrow seam the handler uses to ask the
// systemhealth service for the trimmed health summary. Production
// wires *systemhealth.Handler.Check; tests pass a stub.
type HealthSource interface {
	Health(ctx context.Context) (HealthSummary, error)
}

// Handler serves the /v1/version/deployment route. It is read-only
// and requires no auth so the Element panel can poll it.
type Handler struct {
	managerVersion ManagerVersion
	source         Source
}

// NewHandler constructs the handler. The plugin path and skills
// dir are typically absolute paths resolved at process start.
func NewHandler(managerVersion ManagerVersion, source Source) (*Handler, error) {
	if source.PluginPath == "" {
		return nil, errors.New("version: PluginPath is required")
	}
	if source.SkillsDir == "" {
		return nil, errors.New("version: SkillsDir is required")
	}
	return &Handler{
		managerVersion: managerVersion,
		source:         source,
	}, nil
}

// Register mounts /v1/version/deployment on r (a chi.Router).
//
// We deliberately do NOT mount this on the auth-gated /api/v1
// group; the version page is meant to be reachable from any
// client (read-only, no secrets). If the platform requires auth
// on all /v1 endpoints, the caller should mount the route inside
// the auth-gated group instead — see cmd/opskeeper/main.go.
func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/version/deployment", h.deployment)
}

// deployment writes the DeploymentResponse body. Any failure to
// resolve a sub-section (plugin manifest, skill versions, health)
// degrades gracefully: the field is filled with a placeholder and
// a `note` field surfaces the cause. This avoids a 5xx when a
// part of the deployment is not yet wired (e.g. health check on
// first boot before the DB pool is ready).
func (h *Handler) deployment(w http.ResponseWriter, r *http.Request) {
	resp := DeploymentResponse{
		Manager:     h.managerVersion,
		GoVersion:   runtime.Version(),
		OSArch:      fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}

	// Plugin manifest (read from disk; tolerate missing file).
	manifest, err := readPluginManifest(h.source.PluginPath)
	if err != nil {
		manifest = PluginManifest{
			ID:      "opskeeper-teamharness",
			Name:    "Opskeeper TeamHarness",
			Version: "(manifest unreadable)",
		}
		// Never leak the raw error message into the public
		// /v1/version/deployment payload. Surface a stable
		// fingerprint the operator can match against server logs.
		manifest.Description = fmt.Sprintf("read_failed:%s", hashError(err))
	}
	resp.Plugin = manifest

	// Per-skill versions (read from skill_meta.yaml siblings).
	resp.Skills = readSkillVersions(h.source.SkillsDir)

	// Health summary — use the seam if wired, otherwise render a
	// "not yet wired" placeholder so the version page can still
	// load on dev / first-boot environments. The Note field
	// surfaces only a coarse classification + stable fingerprint,
	// never the raw error string (which can carry DSNs / hostnames
	// / file paths).
	if h.source.HealthService != nil {
		hs, herr := h.source.HealthService.Health(r.Context())
		if herr == nil {
			resp.Health = hs
		} else {
			resp.Health = HealthSummary{
				Overall: "unknown",
				Note:    fmt.Sprintf("seam_error:%s:%s", classifyHealthError(herr), hashError(herr)),
			}
		}
	} else {
		resp.Health = HealthSummary{Overall: "unknown", Note: "seam_not_wired"}
	}

	// Deploy dependencies — derived from the env vars the cmd/opskeeper
	// process already inspects. We mirror the same names so the
	// version page doesn't have to re-derive them.
	resp.Dependencies = deriveDependencies()

	// Worked recovery example — fixed PG connection-pool scenario.
	// The shapes match the Day 11 timeline aggregation so the panel
	// can reuse the renderer.
	resp.Recovery = pgPoolRecoveryExample()

	writeJSON(w, http.StatusOK, resp)
}

// readPluginManifest parses plugin.json (or plugin.yaml in a
// future iteration) from disk. We keep the parser minimal —
// only the fields the panel renders today.
func readPluginManifest(path string) (PluginManifest, error) {
	var m PluginManifest
	f, err := os.Open(path)
	if err != nil {
		return m, fmt.Errorf("open plugin manifest: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return m, fmt.Errorf("read plugin manifest: %w", err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse plugin manifest: %w", err)
	}
	if m.Version == "" {
		return m, fmt.Errorf("plugin manifest missing version field")
	}
	return m, nil
}

// readSkillVersions walks the skills/agent directory and reads
// each skill_meta.yaml sibling. We use a tiny YAML subset rather
// than importing gopkg.in/yaml.v3 to keep the deployment handler
// dependency-light (the panel only needs id + version + role).
//
// The skill directory layout is:
//
//	plugins/opskeeper-teamharness/skills/agent/<id>/SKILL.md
//	plugins/opskeeper-teamharness/skills/agent/<id>/skill_meta.yaml
//
// We derive role from the id prefix and the path; missing files
// surface as empty version strings so the panel renders "(unknown)"
// rather than crashing.
func readSkillVersions(dir string) []SkillVersion {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]SkillVersion, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		metaPath := filepath.Join(dir, id, "skill_meta.yaml")
		version := readSkillVersionField(metaPath, "version")
		out = append(out, SkillVersion{
			ID:      id,
			Version: version,
			Role:    roleFromID(id),
			Path:    metaPath,
		})
	}
	return out
}

// readSkillVersionField parses a single `key: value` line from
// a minimal YAML subset. The function is intentionally permissive:
// comments and blank lines are ignored, missing keys return "".
func readSkillVersionField(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	prefix := strings.TrimSpace(key) + ":"
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if strings.HasPrefix(line, prefix) {
			val := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			val = strings.Trim(val, `"'`)
			return val
		}
	}
	return ""
}

// roleFromID maps the worker's id to the role the closed-loop
// SKILL expects. The mapping is fixed (the agent role is the
// dispatch target, not the prompt name) so we encode it once
// here and reuse the answer from the timeline aggregation.
func roleFromID(id string) string {
	switch id {
	case "opskeeper-alerter":
		return "opskeeper-alerter"
	case "opskeeper-investigator":
		return "opskeeper-investigator"
	case "opskeeper-critic":
		return "opskeeper-critic"
	case "opskeeper-reviewer":
		return "opskeeper-reviewer"
	case "opskeeper-repairer":
		return "opskeeper-repairer"
	case "opskeeper-verifier":
		return "opskeeper-verifier"
	case "opskeeper-postmortem":
		return "opskeeper-postmortem"
	case "opskeeper-coordination":
		return "opskeeper-manager"
	}
	return id
}

// deriveDependencies reads env vars the cmd/opskeeper process
// already inspects and returns a stable list. We DO NOT inspect
// DSNs / URLs / credentials — the handler exposes only whether
// each dependency is configured. Required flags mirror the
// startup-config required/optional split.
//
// Env var names follow cmd/opskeeper/config.Load conventions.
// Adding a new dependency is a two-line edit here.
func deriveDependencies() []DeployDependency {
	return []DeployDependency{
		{Name: "postgres", Configured: envPresent("OPSKEEPER_DB_DIALECT") || envPresent("OPSKEEPER_DB_DSN"), Required: true, Note: "control-plane data layer"},
		{Name: "prometheus", Configured: envPresent("OPSKEEPER_PROM_URL") || envPresent("OPSKEEPER_PROM_QUERY_URL"), Required: false, Note: "metric evaluator source"},
		{Name: "loki", Configured: envPresent("OPSKEEPER_LOKI_URL"), Required: false, Note: "log query source"},
		{Name: "tempo", Configured: envPresent("OPSKEEPER_TEMPO_URL") || envPresent("OPSKEEPER_OTEL_ENDPOINT"), Required: false, Note: "trace query + OTel collector"},
		{Name: "qdrant", Configured: envPresent("OPSKEEPER_QDRANT_URL"), Required: false, Note: "knowledge vector index"},
		{Name: "llm", Configured: envPresent("OPSKEEPER_LLM_API_KEY") || envPresent("ANTHROPIC_API_KEY") || envPresent("OPSKEEPER_OPENAI_API_KEY"), Required: true, Note: "manager LLM inference"},
		{Name: "agentteams", Configured: envPresent("OPSKEEPER_AGENTTEAMS_GATEWAY_URL"), Required: false, Note: "Manager ↔ Worker dispatch"},
	}
}

// envPresent is a tiny helper that treats empty / unset the same
// way. Used by deriveDependencies to decide whether each service
// was configured. We intentionally do NOT distinguish "empty" from
// "unset" — both mean the dependency was not wired.
func envPresent(key string) bool {
	return strings.TrimSpace(os.Getenv(key)) != ""
}

// pgPoolRecoveryExample returns the worked example the
// demo references. The numbers / labels mirror the Day 11
// timeline aggregation so the panel and the closed-loop view
// render the same 7-stage sequence with the same roles.
//
// This is a STATIC example (the version page renders without
// DB access). The IncidentDetail page renders the live event log
// instead; the version page is the "at a glance" view for ops
// reviewers.
//
// The incident id uses the EXAMPLE- prefix so reviewers can tell
// the row is a static demo placeholder rather than a live
// production incident. The PG-pool scenario remains the canonical
// worked example; the prefix change is a docs/naming clarity
// fix requested by the 337 review (no semantic change).
func pgPoolRecoveryExample() RecoveryOperation {
	return RecoveryOperation{
		IncidentID:     "EXAMPLE-PG-POOL",
		Title:          "Application pool waits exhaust PostgreSQL connection budget",
		FaultFamily:    "capacity/connection_pool",
		PhasesObserved: 7,
		PhasesExpected: 7,
		RecoverySignal: true,
		Closed:         true,
		DurationSec:    360,
		AuditKinds:     []string{"dispatch", "approval", "execution", "verification", "close"},
		Evidence: map[string]string{
			"alert":      "evidence/incidents/EXAMPLE-PG-POOL/alert.json",
			"diagnosis":  "evidence/incidents/EXAMPLE-PG-POOL/diagnosis.json",
			"approval":   "evidence/incidents/EXAMPLE-PG-POOL/approval.json",
			"recovery":   "evidence/incidents/EXAMPLE-PG-POOL/recovery-check.json",
			"postmortem": "evidence/incidents/EXAMPLE-PG-POOL/postmortem.md",
			"timeline":   "evidence/incidents/EXAMPLE-PG-POOL/timeline.jsonl",
		},
		Phases: []TimelinePhaseLite{
			{Phase: "detected", PhaseLabel: "detect", Status: "success", WorkerRole: "opskeeper-alerter", SkillVer: "1.0.0", Duration: "8s", Summary: "alert dedup: 71 waiters, pg connections 116/120"},
			{Phase: "correlated", PhaseLabel: "correlate", Status: "success", WorkerRole: "opskeeper-investigator", SkillVer: "1.1.0", Duration: "30s", Summary: "postgres.analyze_status: active=116 waiters=71"},
			{Phase: "investigated", PhaseLabel: "diagnose", Status: "success", WorkerRole: "opskeeper-investigator", SkillVer: "1.1.0", Duration: "1m", Summary: "pool 90/90 saturated; pg 116/120"},
			{Phase: "critiqued", PhaseLabel: "critique", Status: "success", WorkerRole: "opskeeper-critic", SkillVer: "1.0.0", Duration: "55s", Summary: "evidence chain complete (replay/depth/consistency pass)"},
			{Phase: "approved", PhaseLabel: "approve", Status: "success", WorkerRole: "opskeeper-reviewer", SkillVer: "1.0.0", Duration: "55s", Summary: "HITL approved by dba-oncall + opskeeper-observer (bound target=pg:pool-fixture)"},
			{Phase: "recovered", PhaseLabel: "act", Status: "success", WorkerRole: "opskeeper-repairer", SkillVer: "1.0.0", Duration: "1m", Summary: "resize_pool 90→120 + recycle_idle (recovery_signal=true, verified_delta.passed=true)"},
			{Phase: "postmortem", PhaseLabel: "report", Status: "success", WorkerRole: "opskeeper-postmortem", SkillVer: "1.0.0", Duration: "55s", Summary: "postmortem.md written to knowledge vault"},
		},
	}
}

// writeJSON serialises body as JSON with the content-type header.
// We avoid importing the loop package's helper to keep this
// package dependency-free.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// CachedDeployment is a process-wide read-through cache. Tests
// can pass cacheTTL = 0 to bypass the cache.
func CachedDeployment(h *Handler, ctx context.Context) (DeploymentResponse, error) {
	deploymentCacheMu.Lock()
	defer deploymentCacheMu.Unlock()
	if deploymentCache != nil && time.Since(deploymentCacheAt) < deploymentCacheTTL {
		return *deploymentCache, nil
	}
	resp, err := h.build(ctx)
	if err != nil {
		return resp, err
	}
	deploymentCache = &resp
	deploymentCacheAt = time.Now()
	return resp, nil
}

// build assembles the DeploymentResponse without using writeJSON.
// Kept as a separate helper so the cached path doesn't pay the
// HTTP-write cost.
func (h *Handler) build(ctx context.Context) (DeploymentResponse, error) {
	resp := DeploymentResponse{
		Manager:     h.managerVersion,
		GoVersion:   runtime.Version(),
		OSArch:      fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	manifest, err := readPluginManifest(h.source.PluginPath)
	if err != nil {
		manifest = PluginManifest{ID: "opskeeper-teamharness", Name: "Opskeeper TeamHarness", Version: "(manifest unreadable)"}
		// Same redacted-summary contract as deployment() above.
		manifest.Description = fmt.Sprintf("read_failed:%s", hashError(err))
	}
	resp.Plugin = manifest
	resp.Skills = readSkillVersions(h.source.SkillsDir)
	resp.Dependencies = deriveDependencies()
	resp.Recovery = pgPoolRecoveryExample()
	if h.source.HealthService != nil {
		hs, herr := h.source.HealthService.Health(ctx)
		if herr == nil {
			resp.Health = hs
		} else {
			resp.Health = HealthSummary{
				Overall: "unknown",
				Note:    fmt.Sprintf("seam_error:%s:%s", classifyHealthError(herr), hashError(herr)),
			}
		}
	} else {
		resp.Health = HealthSummary{Overall: "unknown", Note: "seam_not_wired"}
	}
	return resp, nil
}

var (
	deploymentCacheMu  sync.Mutex
	deploymentCache    *DeploymentResponse
	deploymentCacheAt  time.Time
	deploymentCacheTTL = 30 * time.Second
)
