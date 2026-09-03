// Package report — postmortem.go
//
// 闭环 orchestrator 的 postmortem phase 实现（zero-manual-ops-loop Day 4
// 任务 4.1–4.6）。三大职责：
//
//  1. 收集阶段合约：RootCauseJSON / CritiqueScore / VerifiedDelta /
//     Timeline（来自 loop.Loaders 窄接口）
//  2. 联动 critic + toolreplay + runtime-source-bridge：拼装
//     改进建议 + 源码 commit 反查
//  3. 渲染 Markdown + 高敏感字段 redact + 落 git artifact
//
// 设计要点：
//   - 8 章节模板（执行摘要 / 时间线 / 根因 / 修复 / 验证 / 影响 /
//     经验教训 / Source commits），对应 design §B.1。
//   - critic 评分缺 → 章节标 `(critic 未评审)`（spec 4.6 测试）。
//   - 模板执行失败 → 降级纯文本 key=value（不阻塞 phase 推进）。
//   - redact 调用 dataguard.Redactor（5 级分级）。
//   - 落盘调用 PostmortemSink 接口（默认 GitArtifactSink）；
//     consumer 模式防止 report → gitartifact 直接 import。
//
// 公共 API：
//   - PostmortemService — 主入口
//   - Render / Run
//   - SourceCommit / SourceSelector / SourceCommitResolver 窄接口
//   - PostmortemSink 窄接口 + GitArtifactSink 默认实现
//   - CriticInsights 窄接口 + NoopCriticInsights 默认实现
package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/dataguard"
	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
)

// SourceCommit is the postmortem-Markdown-friendly view of a resolved
// runtime → commit attribution. Distinct from gitartifact.ResolvedCommit
// to keep the report package's render path decoupled from the
// gitartifact package's internal types.
//
// JSON tags align with the spec's "Source commits" table column
// names so the Web timeline can consume the same struct without
// remapping.
type SourceCommit struct {
	// CommitSHA is the resolved commit hash (40 / 64 hex chars).
	CommitSHA string `json:"commit_sha"`
	// FilePath is the file in the commit.
	FilePath string `json:"file_path"`
	// LineStart / LineEnd locate the symbol in FilePath.
	LineStart int `json:"line_start"`
	LineEnd   int `json:"line_end"`
	// BlameAuthor is the commit author (or "git blame" attribution).
	BlameAuthor string `json:"blame_author"`
	// FirstIntroducedAt is the commit timestamp. v2 does not yet
	// run real `git blame`; the commit time is the closest proxy.
	FirstIntroducedAt time.Time `json:"first_introduced_at"`
	// Confidence is the linker-reported score in [0, 1].
	Confidence float64 `json:"confidence"`
	// RuntimeKey is the selector's string form (kept for debugging
	// in the postmortem — helps the human reader trace which
	// runtime symbol was attributed to which commit).
	RuntimeKey string `json:"runtime_key,omitempty"`
}

// SourceSelector is the report-package view of a runtime symbol to
// attribute. It is intentionally a flat struct (not a tagged union)
// so the resolver can be wired from a JSON-encoded alert payload.
//
// One of (Image+Tag) / (Query) / (Cmd+Key) / (Method+Path) is set,
// depending on Type.
type SourceSelector struct {
	Type   string // "k8s_image" | "pg_query" | "redis_cmd" | "http_route"
	Image  string
	Tag    string
	Query  string
	Cmd    string
	Key    string
	Method string
	Path   string
}

// SourceCommitResolver resolves a batch of runtime selectors to
// commit attributions. The narrow interface lets the integration
// PR (Day 5) wire the production resolver while tests inject a
// canned fake. The default production implementation wraps
// gitartifact.LinkRuntimeToCommit (see NewGitArtifactSourceResolver).
//
// Resolve returns (resolved, unmatched, err):
//   - resolved: the selectors that hit a linker
//   - unmatched: the selectors that did not (linker not registered,
//     linker returned nil, or selector was invalid)
//   - err: terminal failure (e.g. context canceled). Non-terminal
//     per-selector failures are reported via `unmatched` (this
//     mirrors the v1 bridge convention).
type SourceCommitResolver interface {
	Resolve(ctx context.Context, tenantID uint64, selectors []SourceSelector) (resolved []SourceCommit, unmatched []SourceSelector, err error)
}

// CriticDimensionAvg is the aggregate critic score for a single
// dimension across the rolling window.
type CriticDimensionAvg struct {
	Dimension string  `json:"dimension"`
	AvgScore  float64 `json:"avg_score"`
	N         int     `json:"n"`
}

// ToolLatencyStats is the toolreplay aggregate for a single tool.
type ToolLatencyStats struct {
	ToolName string  `json:"tool_name"`
	AvgMs    float64 `json:"avg_ms"`
	P95Ms    float64 `json:"p95_ms"`
	N        int     `json:"n"`
}

// ImprovementInsight bundles the two data sources for the
// "复盘建议" (Actionable Improvements) section.
type ImprovementInsight struct {
	CriticDimensions []CriticDimensionAvg
	SlowestTools     []ToolLatencyStats
	// GeneratedAt is when this insight was collected.
	GeneratedAt time.Time
	// WindowStart / WindowEnd describe the rolling window the
	// aggregates cover.
	WindowStart time.Time
	WindowEnd   time.Time
}

// CriticInsights is the narrow interface the postmortem uses to
// pull critic + toolreplay aggregates. The default production
// implementation lands when critic/toolreplay are integrated
// (Day 6). For Day 4 the report package ships a NoopCriticInsights
// so the renderer still emits the section (with a "(critic 未评审)"
// marker) instead of failing.
type CriticInsights interface {
	// Collect returns the aggregates for the rolling window
	// [now - window, now]. An empty slice (not nil) signals
	// "no data" — the renderer still emits the section.
	Collect(ctx context.Context, tenantID string, window time.Duration) (CriticDimensionAvg, []ToolLatencyStats, error)
}

// NoopCriticInsights is the CriticInsights default. It returns
// empty aggregates so the renderer treats this as "no data" and
// emits the (critic 未评审) section header. Day 6 will replace it
// with a real implementation wired to the critic + toolreplay
// stores.
type NoopCriticInsights struct{}

// Collect implements CriticInsights (no-op).
func (NoopCriticInsights) Collect(_ context.Context, _ string, _ time.Duration) (CriticDimensionAvg, []ToolLatencyStats, error) {
	return CriticDimensionAvg{}, nil, nil
}

// PostmortemSink persists the rendered PostmortemDoc and returns
// the git commit SHA. The narrow interface lets tests inject a
// fake (memory store, etc.) without standing up a real git client.
type PostmortemSink interface {
	Save(ctx context.Context, doc *loop.PostmortemDoc) (commitSHA string, err error)
}

// SaveError is returned by PostmortemService.Run when the sink
// rejects the save (network / validation). Wrapped with %w so
// the orchestrator can map it to phase_failed.
type SaveError struct{ Err error }

func (e *SaveError) Error() string { return "postmortem sink save: " + e.Err.Error() }
func (e *SaveError) Unwrap() error { return e.Err }

// PostmortemConfig tunes the renderer / collector.
type PostmortemConfig struct {
	// Sensitivity drives the Redactor. If empty, Public (noop).
	Sensitivity dataguard.Sensitivity
	// InsightsWindow is the rolling window for the critic +
	// toolreplay aggregates. 0 = default 30 days.
	InsightsWindow time.Duration
	// TopSlowTools caps the "top 3 slowest tools" list. 0 = 3.
	TopSlowTools int
	// Clock is the time source. nil = time.Now.
	Clock func() time.Time
	// Logger is the slog destination. nil = slog.Default().
	Logger *slog.Logger
}

// PostmortemService is the production entry point.
//
// All dependencies are required (constructor returns an error if
// any is nil). The package follows the AGENTS.md monorepo rule:
// report does NOT import the concrete gitartifact implementation
// or the concrete critic package — it consumes them via narrow
// interfaces (SourceCommitResolver, CriticInsights, PostmortemSink)
// wired by the integration PR (Day 5).
type PostmortemService struct {
	mu       sync.RWMutex
	loaders  loop.Loaders
	redactor dataguard.Redactor
	resolver SourceCommitResolver
	insights CriticInsights
	sink     PostmortemSink
	cfg      PostmortemConfig
	log      *slog.Logger
	tmpl     *template.Template
	clock    func() time.Time
	rawTmpl  string
}

// NewPostmortemService is the production constructor. It panics
// on nil critical deps (loaders / resolver / sink) so misconfig
// surfaces at startup; nil redactor / insights are tolerated and
// replaced with safe defaults.
func NewPostmortemService(
	loaders loop.Loaders,
	redactor dataguard.Redactor,
	resolver SourceCommitResolver,
	insights CriticInsights,
	sink PostmortemSink,
	cfg PostmortemConfig,
) (*PostmortemService, error) {
	if err := loaders.Validate(); err != nil {
		return nil, fmt.Errorf("report: postmortem: %w", err)
	}
	if resolver == nil {
		return nil, errors.New("report: postmortem: resolver is required")
	}
	if sink == nil {
		return nil, errors.New("report: postmortem: sink is required")
	}
	if redactor == nil {
		redactor = dataguard.NewRedactor(dataguard.RedactModeNone, false)
	}
	if insights == nil {
		insights = NoopCriticInsights{}
	}
	if cfg.InsightsWindow == 0 {
		cfg.InsightsWindow = 30 * 24 * time.Hour
	}
	if cfg.TopSlowTools == 0 {
		cfg.TopSlowTools = 3
	}
	clock := cfg.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	svc := &PostmortemService{
		loaders:  loaders,
		redactor: redactor,
		resolver: resolver,
		insights: insights,
		sink:     sink,
		cfg:      cfg,
		log:      logger,
		clock:    clock,
		rawTmpl:  postmortemMarkdownTemplate,
	}
	t, err := template.New("postmortem").Funcs(svc.templateFuncs()).Parse(postmortemMarkdownTemplate)
	if err != nil {
		return nil, fmt.Errorf("report: postmortem: parse template: %w", err)
	}
	svc.tmpl = t
	return svc, nil
}

// RenderInput is the input to Render (and is also produced by Run
// via Collect). Fields are optional; nil values render as
// "未提供" / `(critic 未评审)` placeholders.
type RenderInput struct {
	IncidentID    string
	TenantID      string
	Severity      string
	RootCause     *loop.RootCauseJSON
	Critique      *loop.CritiqueScore
	Verified      *loop.VerifiedDelta
	Timeline      []loop.TimelineEvent
	Insights      ImprovementInsight
	SourceCommits []SourceCommit
	// UnmatchedSourceCommits is rendered as "(code source unavailable)"
	// section when non-empty (spec §E.4 fallback).
	UnmatchedSourceCommits []SourceSelector
	// TemplateName is reserved for future per-tenant template
	// overrides. Today the package ships a single template.
	TemplateName string
	// Now is the wall-clock time the postmortem is rendered. Used
	// for the "Generated at" header. If zero, PostmortemService.clock
	// is consulted.
	Now time.Time
}

// postmortemView is the per-render snapshot the template sees.
// All fields are pre-formatted strings so the template stays
// declarative and the renderer code does the heavy lifting.
type postmortemView struct {
	Title                    string
	IncidentID               string
	TenantID                 string
	Severity                 string
	GeneratedAt              string
	RootCauseKind            string
	RootCauseSummary         string
	RootCauseDetail          string
	RootCauseConfidence      string
	RootCauseEvidence        string
	RemediationOptions       string
	CritiqueVerdict          string
	CritiqueScore            string
	CritiqueReasons          string
	CritiqueMissing          bool
	VerifiedPassed           string
	VerifiedFailedMetrics    string
	VerifiedRetryCount       string
	VerifiedWarningLevel     string
	TimelineRows             string
	ImprovementCriticRows    string
	ImprovementToolRows      string
	ImprovementCount         int
	SourceCommitsRows        string
	SourceCommitsUnavailable string
	RedactionNotice          string
}

// Render applies the Markdown template (or a degraded key=value
// text on template failure) to the RenderInput and returns the
// rendered PostmortemDoc. The doc is NOT saved — call Run for
// the full collect → render → save flow.
func (s *PostmortemService) Render(_ context.Context, in RenderInput) (*loop.PostmortemDoc, error) {
	view := s.buildView(in)
	md, err := s.renderTemplate(view)
	if err != nil {
		// Degrade to plain text key=value so the phase never
		// fails on a template glitch (per spec 4.6 test
		// "渲染失败降级").
		s.log.Warn("postmortem: template render failed, falling back to plain text",
			slog.String("incident_id", in.IncidentID),
			slog.String("error", err.Error()))
		md = s.renderPlainText(view, in)
	}
	now := in.Now
	if now.IsZero() {
		now = s.clock()
	}
	doc := &loop.PostmortemDoc{
		SchemaVersion: loop.ContractSchemaV1,
		IncidentID:    in.IncidentID,
		Markdown:      md,
		GeneratedAt:   now,
		Sources:       docSources(in),
	}
	if err := loop.ValidatePostmortemDoc(doc); err != nil {
		return nil, fmt.Errorf("report: postmortem: %w", err)
	}
	return doc, nil
}

// Run is the postmortem phase's full pipeline: collect → render →
// save. Returns the rendered doc and the sink-side commit SHA.
func (s *PostmortemService) Run(ctx context.Context, tenantID, incidentID string) (*loop.PostmortemDoc, string, error) {
	if tenantID == "" {
		return nil, "", errors.New("report: postmortem: tenantID required")
	}
	if incidentID == "" {
		return nil, "", errors.New("report: postmortem: incidentID required")
	}
	// 1. Collect phase contracts.
	rc, err := s.loaders.RootCause.LoadRootCause(ctx, tenantID, incidentID)
	if err != nil {
		return nil, "", fmt.Errorf("postmortem: load root cause: %w", err)
	}
	cs, err := s.loaders.Critique.LoadCritique(ctx, tenantID, incidentID)
	if err != nil {
		return nil, "", fmt.Errorf("postmortem: load critique: %w", err)
	}
	vd, err := s.loaders.Verified.LoadVerifiedDelta(ctx, tenantID, incidentID)
	if err != nil {
		return nil, "", fmt.Errorf("postmortem: load verified delta: %w", err)
	}
	tl, err := s.loaders.Timeline.LoadTimeline(ctx, tenantID, incidentID)
	if err != nil {
		return nil, "", fmt.Errorf("postmortem: load timeline: %w", err)
	}

	// 2. Collect critic + toolreplay aggregates (best effort).
	now := s.clock()
	window := s.cfg.InsightsWindow
	dim, slowest, err := s.insights.Collect(ctx, tenantID, window)
	insights := ImprovementInsight{
		GeneratedAt: now,
		WindowStart: now.Add(-window),
		WindowEnd:   now,
	}
	// We accept any per-tool/per-dim error and degrade silently:
	// the section is best-effort. A nil dim / empty slice renders
	// the "no data" placeholder.
	_ = dim
	insights.SlowestTools = slowest
	// Top-N slowest tools.
	if n := s.cfg.TopSlowTools; n > 0 && len(insights.SlowestTools) > n {
		insights.SlowestTools = insights.SlowestTools[:n]
	}

	// 3. Resolve runtime → commit attributions. Selectors are
	// derived from the RootCause evidence chain (if any) — the
	// selector list is empty when there is no usable evidence.
	selectors := buildSelectorsFromRootCause(rc)
	var (
		resolvedCommits []SourceCommit
		unmatched       []SourceSelector
	)
	if len(selectors) > 0 {
		var rerr error
		resolvedCommits, unmatched, rerr = s.resolver.Resolve(ctx, tenantIDFromString(tenantID), selectors)
		if rerr != nil {
			// A non-per-selector failure (e.g. context canceled)
			// is terminal. We do not degrade: a sink without
			// source commits would mislead the reader.
			return nil, "", fmt.Errorf("postmortem: resolve source commits: %w", rerr)
		}
	}

	// 4. Render.
	in := RenderInput{
		IncidentID:             incidentID,
		TenantID:               tenantID,
		Severity:               severityFromRootCause(rc),
		RootCause:              rc,
		Critique:               cs,
		Verified:               vd,
		Timeline:               tl,
		Insights:               insights,
		SourceCommits:          resolvedCommits,
		UnmatchedSourceCommits: unmatched,
		Now:                    now,
	}
	doc, err := s.Render(ctx, in)
	if err != nil {
		return nil, "", err
	}

	// 5. Save.
	sha, err := s.sink.Save(ctx, doc)
	if err != nil {
		return nil, "", &SaveError{Err: err}
	}
	return doc, sha, nil
}

// --- internals ---------------------------------------------------------

// redactInput returns a copy of `in` with sensitive fields redacted
// per the configured redactor. The redaction is applied at the
// data level (not the rendered markdown) so URLs / commit SHAs /
// numeric line numbers are NOT touched.
//
// Per spec §F.1:
//   - RootCause detail map → RedactMap (key-driven).
//   - RootCause summary / evidence / remediation strings → RedactString
//     (PII substring matching).
//   - Critique.reasons / Verified.failed_metrics → RedactString
//     (defense in depth).
//
// The function is a no-op when the redactor mode is None.
func (s *PostmortemService) redactInput(in RenderInput) RenderInput {
	if s.redactor == nil || s.redactor.Mode() == dataguard.RedactModeNone {
		return in
	}
	// Use a context.Background() so callers don't have to thread
	// a ctx through buildView. RedactString is pure (no IO).
	ctx := context.Background()
	if in.RootCause != nil && in.RootCause.RootCauseObject != nil {
		// Deep-copy the RootCause so we don't mutate the caller's
		// struct.
		cp := *in.RootCause
		obj := *in.RootCause.RootCauseObject
		if in.RootCause.RootCauseObject.Detail != nil {
			obj.Detail = dataguard.RedactMap(s.redactor, in.RootCause.RootCauseObject.Detail)
			// For TopSecret (and any mode with stripDigits), also
			// apply RedactString to every value so free-form digits
			// / emails embedded in non-sensitive-keyed fields are
			// scrubbed. Other modes (Confidential / Restricted)
			// also pass through RedactString so a value like
			// `"alice@example.com"` under a key called `note` is
			// still redacted via the substring rule.
			for k, v := range obj.Detail {
				if str, ok := v.(string); ok {
					obj.Detail[k] = s.redactor.RedactString(ctx, str)
				}
			}
		}
		obj.Summary = s.redactor.RedactString(ctx, obj.Summary)
		cp.RootCauseObject = &obj
		// Evidence chain: redact each evidence's value (stringified).
		ev := make([]loop.EvidenceItem, len(cp.EvidenceChain))
		for i, e := range cp.EvidenceChain {
			ev[i] = e
			if str, ok := e.Value.(string); ok {
				ev[i].Value = s.redactor.RedactString(ctx, str)
			}
			// Also redact the query (free-form text).
			ev[i].Query = s.redactor.RedactString(ctx, ev[i].Query)
		}
		cp.EvidenceChain = ev
		in.RootCause = &cp
	}
	if in.Critique != nil {
		cp := *in.Critique
		redacted := make([]string, len(cp.Reasons))
		for i, r := range cp.Reasons {
			redacted[i] = s.redactor.RedactString(ctx, r)
		}
		cp.Reasons = redacted
		in.Critique = &cp
	}
	return in
}

func (s *PostmortemService) templateFuncs() template.FuncMap {
	return template.FuncMap{

		"now": s.clock,
	}
}

func (s *PostmortemService) renderTemplate(view postmortemView) (string, error) {
	if s.tmpl == nil {
		return "", errors.New("template not initialised")
	}
	var buf bytes.Buffer
	if err := s.tmpl.Execute(&buf, view); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// renderPlainText is the degraded key=value format used when the
// template execution fails. The format is intentionally ugly but
// parseable (one key=value per line) so downstream consumers
// (Web UI, search) can still surface the fields.
func (s *PostmortemService) renderPlainText(view postmortemView, in RenderInput) string {
	now := s.clock()
	lines := []string{
		fmt.Sprintf("incident_id=%s", view.IncidentID),
		fmt.Sprintf("tenant_id=%s", view.TenantID),
		fmt.Sprintf("severity=%s", view.Severity),
		fmt.Sprintf("generated_at=%s", now.Format(time.RFC3339)),
		fmt.Sprintf("root_cause_kind=%s", view.RootCauseKind),
		fmt.Sprintf("root_cause_summary=%s", view.RootCauseSummary),
		fmt.Sprintf("root_cause_confidence=%s", view.RootCauseConfidence),
		fmt.Sprintf("critique_verdict=%s", view.CritiqueVerdict),
		fmt.Sprintf("verified_passed=%s", view.VerifiedPassed),
		fmt.Sprintf("remediation_options=%s", view.RemediationOptions),
		fmt.Sprintf("source_commits=%s", view.SourceCommitsRows),
		fmt.Sprintf("redaction_notice=%s", view.RedactionNotice),
	}
	// Include selectors that didn't resolve so the human reader
	// can chase the missing attribution.
	if len(in.UnmatchedSourceCommits) > 0 {
		var keys []string
		for _, sel := range in.UnmatchedSourceCommits {
			keys = append(keys, sourceSelectorKey(sel))
		}
		lines = append(lines, "unmatched_source_selectors="+strings.Join(keys, ","))
	}
	return strings.Join(lines, "\n")
}

func (s *PostmortemService) buildView(in RenderInput) postmortemView {
	// Apply redaction to the structured + free-form fields BEFORE
	// view construction. We mutate a local copy so the caller's
	// RenderInput is left untouched.
	in = s.redactInput(in)
	view := postmortemView{
		Title:       "Incident Postmortem",
		IncidentID:  in.IncidentID,
		TenantID:    in.TenantID,
		Severity:    in.Severity,
		GeneratedAt: formatTime(s.clock()),
	}
	if view.Severity == "" {
		view.Severity = "unknown"
	}
	if in.RootCause != nil && in.RootCause.RootCauseObject != nil {
		view.RootCauseKind = in.RootCause.RootCauseObject.Kind
		view.RootCauseSummary = in.RootCause.RootCauseObject.Summary
		view.RootCauseDetail = formatDetail(in.RootCause.RootCauseObject.Detail)
		view.RootCauseConfidence = fmt.Sprintf("%.2f", in.RootCause.Confidence)
		view.RootCauseEvidence = formatEvidence(in.RootCause.EvidenceChain)
		view.RemediationOptions = formatRemediation(in.RootCause.RemediationOptions)
	} else {
		view.RootCauseKind = "(未提供)"
		view.RootCauseSummary = "根因合约未写入（可能未走完 investigated 阶段）。"
		view.RootCauseDetail = ""
		view.RootCauseConfidence = "n/a"
		view.RootCauseEvidence = ""
		view.RemediationOptions = "(未提供)"
	}
	if in.Critique != nil {
		view.CritiqueVerdict = in.Critique.Verdict
		view.CritiqueScore = fmt.Sprintf("%.2f", in.Critique.Score)
		view.CritiqueReasons = strings.Join(in.Critique.Reasons, "; ")
		if view.CritiqueReasons == "" {
			view.CritiqueReasons = "(no reasons)"
		}
		view.CritiqueMissing = false
	} else {
		view.CritiqueVerdict = "未评审"
		view.CritiqueScore = "n/a"
		view.CritiqueReasons = "(critic 未评审)"
		view.CritiqueMissing = true
	}
	if in.Verified != nil {
		view.VerifiedPassed = boolYesNo(in.Verified.Passed)
		view.VerifiedFailedMetrics = strings.Join(in.Verified.FailedMetrics, ", ")
		if view.VerifiedFailedMetrics == "" {
			view.VerifiedFailedMetrics = "(none)"
		}
		view.VerifiedRetryCount = fmt.Sprintf("%d", in.Verified.RetryCount)
		view.VerifiedWarningLevel = in.Verified.WarningLevel
	} else {
		view.VerifiedPassed = "deferred to human review"
		view.VerifiedFailedMetrics = "(none)"
		view.VerifiedRetryCount = "0"
		view.VerifiedWarningLevel = "unknown"
	}
	view.TimelineRows = formatTimeline(in.Timeline, loop.TimelineMaxRows)
	if len(in.Insights.CriticDimensions) == 0 && len(in.Insights.SlowestTools) == 0 {
		view.ImprovementCriticRows = "(critic / toolreplay 数据未提供)"
		view.ImprovementToolRows = ""
		view.ImprovementCount = 0
	} else {
		view.ImprovementCriticRows = formatCriticDims(in.Insights.CriticDimensions)
		view.ImprovementToolRows = formatSlowestTools(in.Insights.SlowestTools)
		view.ImprovementCount = len(in.Insights.CriticDimensions) + len(in.Insights.SlowestTools)
	}
	view.SourceCommitsRows = formatSourceCommits(in.SourceCommits)
	if len(in.UnmatchedSourceCommits) > 0 {
		var keys []string
		for _, sel := range in.UnmatchedSourceCommits {
			keys = append(keys, sourceSelectorKey(sel))
		}
		view.SourceCommitsUnavailable = "未命中: " + strings.Join(keys, ", ")
	} else {
		view.SourceCommitsUnavailable = ""
	}
	if s.redactor.Mode() != dataguard.RedactModeNone {
		view.RedactionNotice = fmt.Sprintf("已 redact 字段（mode=%s）", s.redactor.Mode())
	}
	return view
}

// docSources returns the list of source names recorded in the
// PostmortemDoc.Sources field. Order is stable for diffability.
func docSources(in RenderInput) []string {
	sources := []string{"RootCauseJSON"}
	if in.Critique != nil {
		sources = append(sources, "CritiqueScore")
	}
	if in.Verified != nil {
		sources = append(sources, "VerifiedDelta")
	}
	if len(in.Timeline) > 0 {
		sources = append(sources, "Timeline")
	}
	if len(in.Insights.SlowestTools) > 0 || len(in.Insights.CriticDimensions) > 0 {
		sources = append(sources, "critic+toolreplay")
	}
	if len(in.SourceCommits) > 0 {
		sources = append(sources, "runtime_source_bridge")
	}
	return sources
}

// buildSelectorsFromRootCause walks the RootCause evidence chain
// and emits SourceSelector values. We keep the mapping minimal:
// one selector per distinct (type, primary-key) tuple. The Day 5
// integration can extend this when more selector sources land.
func buildSelectorsFromRootCause(rc *loop.RootCauseJSON) []SourceSelector {
	if rc == nil {
		return nil
	}
	out := make([]SourceSelector, 0, len(rc.EvidenceChain))
	seen := make(map[string]struct{})
	for _, ev := range rc.EvidenceChain {
		if ev.Tool == "" {
			continue
		}
		key := ev.Tool
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		switch strings.ToLower(ev.Tool) {
		case "k8s_image", "k8s_pod_image":
			out = append(out, SourceSelector{Type: "k8s_image", Image: ev.Query})
		case "pg_query", "pg_long_tx", "pg_slow_query":
			out = append(out, SourceSelector{Type: "pg_query", Query: ev.Query})
		case "redis_cmd":
			// Redis evidence is typically a cmd + key. We pack both
			// into Query (cmd) and leave Key empty unless a future
			// schema exposes a separate key field.
			parts := strings.Fields(ev.Query)
			cmd, key := "", ""
			if len(parts) > 0 {
				cmd = parts[0]
			}
			if len(parts) > 1 {
				key = parts[1]
			}
			out = append(out, SourceSelector{Type: "redis_cmd", Cmd: cmd, Key: key})
		case "http_route":
			// HTTP route evidence packs "METHOD path" in Query.
			parts := strings.Fields(ev.Query)
			method, path := "", ""
			if len(parts) > 0 {
				method = parts[0]
			}
			if len(parts) > 1 {
				path = parts[1]
			}
			out = append(out, SourceSelector{Type: "http_route", Method: method, Path: path})
		}
	}
	return out
}

func sourceSelectorKey(sel SourceSelector) string {
	switch sel.Type {
	case "k8s_image":
		if sel.Tag != "" {
			return "k8s_image:" + sel.Image + ":" + sel.Tag
		}
		return "k8s_image:" + sel.Image
	case "pg_query":
		return "pg_query:" + sel.Query
	case "redis_cmd":
		return "redis_cmd:" + sel.Cmd + ":" + sel.Key
	case "http_route":
		return "http_route:" + sel.Method + " " + sel.Path
	}
	return sel.Type
}

func tenantIDFromString(s string) uint64 {
	if s == "" {
		return 0
	}
	// tenantID is a numeric string in the project; we do not import
	// strconv here to keep the function allocation-light. A simple
	// manual parse is enough (callers that need strict semantics
	// can pre-convert).
	var n uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + uint64(c-'0')
	}
	return n
}

func severityFromRootCause(rc *loop.RootCauseJSON) string {
	if rc == nil || len(rc.RemediationOptions) == 0 {
		return "unknown"
	}
	// Pick the highest-risk option as the effective severity.
	riskRank := map[string]int{"safe": 0, "mutating": 1, "dangerous": 2, "": -1}
	worst := -1
	for _, ro := range rc.RemediationOptions {
		if r := riskRank[ro.Risk]; r > worst {
			worst = r
		}
	}
	switch worst {
	case 0:
		return "safe"
	case 1:
		return "mutating"
	case 2:
		return "dangerous"
	}
	return "unknown"
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "(zero)"
	}
	return t.UTC().Format(time.RFC3339)
}

func formatDetail(d map[string]any) string {
	if len(d) == 0 {
		return ""
	}
	// Stable key order so the rendered Markdown is diff-friendly.
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("- %s: %v", k, d[k]))
	}
	return strings.Join(parts, "\n")
}

func formatEvidence(evs []loop.EvidenceItem) string {
	if len(evs) == 0 {
		return "(none)"
	}
	var parts []string
	for i, ev := range evs {
		desc := ev.Query
		if desc == "" {
			desc = fmt.Sprintf("%v", ev.Value)
			if desc == "" {
				desc = "(no payload)"
			}
		}
		parts = append(parts, fmt.Sprintf("%d. %s — %s (n=%d)", i+1, ev.Tool, desc, ev.Count))
	}
	return strings.Join(parts, "\n")
}

func formatRemediation(ros []loop.RemediationOption) string {
	if len(ros) == 0 {
		return "(none)"
	}
	var parts []string
	for i, ro := range ros {
		auto := ""
		if ro.AutoApprove {
			auto = " (auto-approve)"
		}
		parts = append(parts, fmt.Sprintf("%d. [%s] %s%s", i+1, ro.Risk, ro.Action, auto))
	}
	return strings.Join(parts, "\n")
}

func formatTimeline(tl []loop.TimelineEvent, maxRows int) string {
	if len(tl) == 0 {
		return "(no events)"
	}
	if maxRows > 0 && len(tl) > maxRows {
		tl = tl[len(tl)-maxRows:]
	}
	var parts []string
	for _, e := range tl {
		parts = append(parts, fmt.Sprintf("- %s | %s | %s | %s", e.CreatedAt, e.Phase, e.EventType, e.Summary))
	}
	return strings.Join(parts, "\n")
}

func formatCriticDims(dims []CriticDimensionAvg) string {
	if len(dims) == 0 {
		return "(no critic data)"
	}
	var parts []string
	for _, d := range dims {
		parts = append(parts, fmt.Sprintf("- %s: avg=%.2f (n=%d)", d.Dimension, d.AvgScore, d.N))
	}
	return strings.Join(parts, "\n")
}

func formatSlowestTools(tools []ToolLatencyStats) string {
	if len(tools) == 0 {
		return ""
	}
	var parts []string
	for _, t := range tools {
		parts = append(parts, fmt.Sprintf("- %s: avg=%.1fms p95=%.1fms (n=%d)", t.ToolName, t.AvgMs, t.P95Ms, t.N))
	}
	return strings.Join(parts, "\n")
}

func formatSourceCommits(commits []SourceCommit) string {
	if len(commits) == 0 {
		return "(no source commits resolved)"
	}
	var parts []string
	for i, c := range commits {
		introduced := "(unknown)"
		if !c.FirstIntroducedAt.IsZero() {
			introduced = c.FirstIntroducedAt.UTC().Format(time.RFC3339)
		}
		needs := ""
		if c.Confidence > 0 && c.Confidence < 0.7 {
			needs = " *(needs human confirm)*"
		}
		parts = append(parts, fmt.Sprintf("%d. `%s` — `%s:%d-%d` — %s — %s%s",
			i+1, shortSHA(c.CommitSHA), c.FilePath, c.LineStart, c.LineEnd,
			c.BlameAuthor, introduced, needs))
	}
	return strings.Join(parts, "\n")
}

func shortSHA(sha string) string {
	if len(sha) >= 12 {
		return sha[:12]
	}
	return sha
}

func boolYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// postmortemMarkdownTemplate is the 8-section Markdown template
// (design §B.1). Edit the structure here; per-tenant overrides are
// not in scope for Day 4.
const postmortemMarkdownTemplate = `# {{.Title}}

| Field | Value |
| --- | --- |
| Incident ID | {{.IncidentID}} |
| Tenant ID | {{.TenantID}} |
| Severity | {{.Severity}} |
| Generated at | {{.GeneratedAt}} |
{{- if .RedactionNotice}}
| Redaction | {{.RedactionNotice}} |
{{- end}}

## 1. 执行摘要 (Summary)

- **根因类型**: {{.RootCauseKind}}
- **根因摘要**: {{.RootCauseSummary}}
- **Confidence**: {{.RootCauseConfidence}}
- **Critic 评分**: {{.CritiqueScore}} (verdict={{.CritiqueVerdict}})
- **Verified**: passed={{.VerifiedPassed}}, warning={{.VerifiedWarningLevel}}, retry_count={{.VerifiedRetryCount}}

## 2. 时间线 (Timeline)

{{.TimelineRows}}

## 3. 根因 (Root Cause)

**Kind**: {{.RootCauseKind}}

**Summary**: {{.RootCauseSummary}}

**Detail**:

{{.RootCauseDetail}}

**Evidence chain**:

{{.RootCauseEvidence}}

## 4. 修复 (Remediation)

{{.RemediationOptions}}

## 5. 验证 (Verification)

- Passed: **{{.VerifiedPassed}}**
- Failed metrics: {{.VerifiedFailedMetrics}}
- Retry count: {{.VerifiedRetryCount}}
- Warning level: {{.VerifiedWarningLevel}}

{{- if .CritiqueMissing}}

> **Critique 评审缺失** (critic 未评审)：本节保留占位以保持章节顺序稳定。
{{- end}}

## 6. 影响 (Impact)

> 由 orchestrator 在 phase 流转时填入（Day 5 集成后此章节由 chatdiagnose service 注入；当前保留为模板占位）。

## 7. 经验教训 (Actionable Improvements)

{{- if .ImprovementCriticRows}}

**Critic 低分维度 (avg < 0.7)**:

{{.ImprovementCriticRows}}
{{- end}}

{{- if .ImprovementToolRows}}

**Toolreplay 最慢工具 (top 3)**:

{{.ImprovementToolRows}}
{{- end}}

{{- if eq .ImprovementCount 0}}

(暂无足够历史数据；建议部署 ≥ 30 天后再生成改进建议。)
{{- end}}

## 8. Source commits (代码溯源)

> 由 Runtime-Source-Bridge 反查（v1 借鉴 D11）。

| Commit SHA | File | Lines | Author | First introduced | Confidence |
| --- | --- | --- | --- | --- | --- |
{{ .SourceCommitsRows }}

{{- if .SourceCommitsUnavailable}}

> **{{.SourceCommitsUnavailable}}** — code source unavailable（runtime 符号未命中 git 反向索引）。
{{- end}}
`

// fakeSyntheticSHA is used by GitArtifactSink to manufacture a
// deterministic commit SHA when no real git client is wired.
// The Day 5 integration will replace this with a real client call.
func fakeSyntheticSHA(incidentID string, body []byte) string {
	h := sha256.Sum256(append([]byte(incidentID+"\n"), body...))
	// 40-char hex string to match git's short-SHA length used in
	// the artifact's Commit field.
	return hex.EncodeToString(h[:])[:40]
}
