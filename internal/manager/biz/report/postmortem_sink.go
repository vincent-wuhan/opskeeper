// Package report — postmortem_sink.go
//
// Default PostmortemSink (GitArtifactSink) and SourceCommitResolver
// (GitArtifactSourceResolver) implementations. Both wrap the
// gitartifact package via its public API; neither reaches into
// gitartifact internals. This keeps the report → gitartifact
// boundary clean (per the monorepo rule in AGENTS.md).
package report

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact"
	gitamodel "github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/model"
	gitastore "github.com/vincent-wuhan/opskeeper/internal/knowledge/gitartifact/store"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
)

// GitArtifactSink is the default PostmortemSink. It records the
// rendered Markdown as a model.Artifact in the gitartifact store
// and returns a deterministic synthetic commit SHA.
//
// Day 4 deviation from design §D.4: the design assumed a real
// `gitClient.CommitFile` is available. The opskeeper codebase does
// not ship a git client here (real git ops happen elsewhere —
// see openspec/changes/unified-platform-base-selection); the sink
// falls back to a SHA-256-derived synthetic commit so callers can
// still obtain a stable identifier for the postmortem commit. Day 5
// integration will swap in a real git client (open question
// E-Q1 in the design doc).
type GitArtifactSink struct {
	// Store is the gitartifact artifact store. Required.
	Store gitastore.Store

	// PublicIDPrefix prefixes the postmortem PublicID so a single
	// store can host multiple artifact types. Default: "postmortem-".
	PublicIDPrefix string

	// Now is the time source for BuildAt. nil = time.Now.
	Now func() time.Time

	// mu protects nothing today (Store is goroutine-safe per its
	// own contract) but is kept for future fields that need
	// synchronisation.
	mu sync.Mutex
}

// NewGitArtifactSink constructs the sink. Returns an error when
// Store is nil.
func NewGitArtifactSink(store gitastore.Store) (*GitArtifactSink, error) {
	if store == nil {
		return nil, fmt.Errorf("report: GitArtifactSink: store is required")
	}
	return &GitArtifactSink{
		Store:          store,
		PublicIDPrefix: "postmortem-",
		Now:            func() time.Time { return time.Now().UTC() },
	}, nil
}

// Save implements PostmortemSink.
func (s *GitArtifactSink) Save(ctx context.Context, doc *loop.PostmortemDoc) (string, error) {
	if doc == nil {
		return "", fmt.Errorf("GitArtifactSink: nil doc")
	}
	if err := loop.ValidatePostmortemDoc(doc); err != nil {
		return "", fmt.Errorf("GitArtifactSink: %w", err)
	}
	publicID := s.PublicIDPrefix + doc.IncidentID
	now := s.Now()
	sha := fakeSyntheticSHA(doc.IncidentID, []byte(doc.Markdown))
	artifact := &gitamodel.Artifact{
		ID:          publicID,
		PublicID:    publicID,
		RepoURL:     "postmortem://opskeeper/incidents",
		Commit:      sha,
		Branch:      "main",
		ArtifactURL: fmt.Sprintf("postmortem://opskeeper/incidents/%s.md", doc.IncidentID),
		Meta: map[string]interface{}{
			"build_id":            publicID,
			"loop_version":        "v1",
			"template_version":    "2026-08-10.v1",
			"redacted":            s.isRedacted(doc),
			"redaction_notice":    s.redactionNotice(doc),
			"generated_at":        doc.GeneratedAt,
			"sources":             doc.Sources,
			"schema_version":      doc.SchemaVersion,
			"conversation_id":     doc.ConversationID,
			"incident_id":         doc.IncidentID,
			"severity":            s.severityFromDoc(doc),
			"root_cause_kind":     s.rootCauseKindFromDoc(doc),
			"markdown_size_bytes": len(doc.Markdown),
		},
		BuildAt:     now,
		IndexedAt:   &now,
		IndexStatus: gitamodel.IndexStatusCompleted,
		IndexError:  "",
		TenantID:    0, // postmortem is tenant-agnostic at the artifact level;
		//  multi-tenant filtering happens at the postmortem loader
		//  (loop.Loaders).
	}
	if err := s.Store.Put(ctx, artifact); err != nil {
		return "", fmt.Errorf("GitArtifactSink: store put: %w", err)
	}
	return sha, nil
}

// isRedacted inspects the postmortem Markdown for the redact marker.
func (s *GitArtifactSink) isRedacted(doc *loop.PostmortemDoc) bool {
	return contains(doc.Markdown, "<redacted:")
}

// redactionNotice returns a short human-readable tag for the
// commit metadata. Empty when no redaction was applied.
func (s *GitArtifactSink) redactionNotice(doc *loop.PostmortemDoc) string {
	if !s.isRedacted(doc) {
		return ""
	}
	return "[REDACTED]"
}

// severityFromDoc / rootCauseKindFromDoc scan the Markdown for
// the Summary table (the first occurrence). They are best-effort
// lookups; the postmortem Markdown is machine-emitted and the
// patterns are stable.
func (s *GitArtifactSink) severityFromDoc(doc *loop.PostmortemDoc) string {
	return scanMarkdownField(doc.Markdown, "Severity")
}

func (s *GitArtifactSink) rootCauseKindFromDoc(doc *loop.PostmortemDoc) string {
	// Look for the literal "**根因类型**:" header (zh) or
	// "**Root cause kind**:" if localised. The current template
	// ships the zh form so we match that.
	return scanMarkdownField(doc.Markdown, "根因类型")
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// scanMarkdownField returns the value following a `| **<key>** |
// <value> |` table row. Used to surface the Severity / root cause
// in the artifact's commit metadata.
func scanMarkdownField(md, key string) string {
	needle := "**" + key + "**"
	i := indexOf(md, needle)
	if i < 0 {
		return ""
	}
	// Walk past the closing `|` to the value.
	j := indexOf(md[i:], " | ")
	if j < 0 {
		return ""
	}
	start := i + j + 3
	// Find the next `|` (end of value cell).
	end := indexOf(md[start:], " |")
	if end < 0 {
		return trim(md[start:])
	}
	return trim(md[start : start+end])
}

func indexOf(s, sub string) int {
	if sub == "" {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func trim(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// GitArtifactSourceResolver is the default SourceCommitResolver.
// It translates report.SourceSelector to gitartifact.RuntimeSelector
// and back, via the public gitartifact.LinkRuntimeToCommit API.
type GitArtifactSourceResolver struct {
	// Registry is the gitartifact linker registry. Required.
	Registry *gitartifact.LinkerRegistry
}

// NewGitArtifactSourceResolver constructs the resolver.
func NewGitArtifactSourceResolver(reg *gitartifact.LinkerRegistry) (*GitArtifactSourceResolver, error) {
	if reg == nil {
		return nil, fmt.Errorf("report: GitArtifactSourceResolver: registry is required")
	}
	return &GitArtifactSourceResolver{Registry: reg}, nil
}

// Resolve implements SourceCommitResolver.
func (r *GitArtifactSourceResolver) Resolve(
	ctx context.Context,
	tenantID uint64,
	selectors []SourceSelector,
) ([]SourceCommit, []SourceSelector, error) {
	if r.Registry == nil {
		return nil, selectors, fmt.Errorf("GitArtifactSourceResolver: registry not set")
	}
	if len(selectors) == 0 {
		return nil, nil, nil
	}
	rtSelectors := make([]gitartifact.RuntimeSelector, 0, len(selectors))
	for _, sel := range selectors {
		rt, ok := toGitArtifactSelector(sel)
		if !ok {
			// Unsupported type — count as unmatched.
			continue
		}
		rtSelectors = append(rtSelectors, rt)
	}
	res := gitartifact.LinkRuntimeToCommit(ctx, r.Registry, gitartifact.LinkRuntimeToCommitInput{
		TenantID:  tenantID,
		Selectors: rtSelectors,
	})
	resolved := make([]SourceCommit, 0, len(res.ResolvedCommits))
	for _, rc := range res.ResolvedCommits {
		resolved = append(resolved, SourceCommit{
			CommitSHA:         rc.CommitSHA,
			FilePath:          rc.FilePath,
			LineStart:         rc.LineStart,
			LineEnd:           rc.LineEnd,
			BlameAuthor:       rc.BlameAuthor,
			FirstIntroducedAt: rc.FirstIntroducedAt,
			Confidence:        rc.Confidence,
			RuntimeKey:        rc.RuntimeKey,
		})
	}
	// Map unmatched gitartifact.RuntimeSelector back to report.SourceSelector.
	unmatched := make([]SourceSelector, 0, len(res.UnmatchedRuntime))
	for _, sel := range res.UnmatchedRuntime {
		unmatched = append(unmatched, fromGitArtifactSelector(sel))
	}
	return resolved, unmatched, nil
}

// toGitArtifactSelector converts a report.SourceSelector to a
// gitartifact.RuntimeSelector. Returns (zero, false) for types
// the gitartifact bridge does not support (so the caller can count
// them as unmatched).
func toGitArtifactSelector(sel SourceSelector) (gitartifact.RuntimeSelector, bool) {
	switch sel.Type {
	case "k8s_image":
		return gitartifact.NewK8sSelector(sel.Image, sel.Tag), true
	case "pg_query":
		return gitartifact.NewPGSelector(sel.Query, ""), true
	case "redis_cmd":
		return gitartifact.NewRedisSelector(sel.Cmd, sel.Key), true
	case "http_route":
		return gitartifact.NewHTTPSelector(sel.Method, sel.Path, ""), true
	}
	return gitartifact.RuntimeSelector{}, false
}

// fromGitArtifactSelector converts a gitartifact.RuntimeSelector
// back to a report.SourceSelector. We round-trip via the public
// constructors to keep the field mapping in one place.
func fromGitArtifactSelector(sel gitartifact.RuntimeSelector) SourceSelector {
	switch sel.Type {
	case gitartifact.SymbolTypeK8sImage:
		out := SourceSelector{Type: "k8s_image"}
		if sel.K8s != nil {
			out.Image = sel.K8s.Image
			out.Tag = sel.K8s.Tag
		}
		return out
	case gitartifact.SymbolTypePGQuery:
		out := SourceSelector{Type: "pg_query"}
		if sel.PG != nil {
			out.Query = sel.PG.Query
		}
		return out
	case gitartifact.SymbolTypeRedisCmd:
		out := SourceSelector{Type: "redis_cmd"}
		if sel.Redis != nil {
			out.Cmd = sel.Redis.Cmd
			out.Key = sel.Redis.Key
		}
		return out
	case gitartifact.SymbolTypeHTTPRoute:
		out := SourceSelector{Type: "http_route"}
		if sel.HTTP != nil {
			out.Method = sel.HTTP.Method
			out.Path = sel.HTTP.Path
		}
		return out
	}
	return SourceSelector{Type: string(sel.Type)}
}
