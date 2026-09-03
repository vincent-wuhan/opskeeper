// chatdiagnose/kb_lookup.go — KB-first lookup for the chat diagnose
// entry point (D15 of zero-manual-ops-loop).
//
// Two KB sources, queried serially:
//
//  1. incident_pattern table (internal/manager/model/chatdiagnose).
//     Tenant-scoped signature retrieval. Populated by postmortem
//     completion; vector similarity comes from Qdrant and lexical recall
//     uses tenant-filtered BM25 candidates.
//
//  2. gitartifact linker (internal/manager/biz/knowledge/gitartifact
//     — Day 2 spike, not yet present). Resolves "@pg-prod-01"-style
//     resource references to historical commit / doc hits.
//
// Failure mode philosophy: a KB miss MUST NOT block the chat. Both
// sources return their errors to the caller but the caller (Service.
// Diagnose) treats them as advisory — the chat always falls through
// to the ReAct phase. Errors are logged + audit-logged but not
// surfaced to the SPA user as "the KB is broken".
//
// Feature flag: the whole KB-first phase is gated behind
// feature.kb_first (default off until 30+ postmortems have seeded
// the pattern table — see Q-δ in the design doc). When the flag is
// off, Service.Diagnose skips the Lookup call entirely.
package chatdiagnose

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

// KBLookupRequest is the input to KBLookup.Lookup. Signature is the
// precomputed text signature of the user message — the caller is
// responsible for extracting it (Service.Diagnose uses
// extractSignature, a thin wrapper). Keeping the extraction outside
// the lookup makes the lookup easy to test with hand-built requests.
type KBLookupRequest struct {
	// TenantID is mandatory. The KB is tenant-scoped — see spec
	// §"跨租户 incident_pattern 行不可见".
	TenantID string

	// Message is the raw user message (used for signature fallback if
	// Signature is empty).
	Message string

	// Resources is the parsed @-resource list. Drives the gitartifact
	// source.
	Resources []ResourceRef

	// Signature is the precomputed signature text. Empty -> derive
	// from Message.
	Signature string

	// TopK bounds the result set. Default 5.
	TopK int

	// Threshold is the minimum similarity score to keep. Default
	// 0.85 (per D15).
	Threshold float64
}

// KBHit is one KB match. The wire shape is identical between pattern
// hits and gitartifact hits so the SPA can render them with one
// component.
type KBHit struct {
	// PatternID is set for pattern hits; 0 for gitartifact hits.
	PatternID int64

	// ResourceType is the canonical resource prefix.
	ResourceType string

	// Symptom / RootCause are populated for pattern hits; empty for
	// gitartifact hits (the doc title plays that role there).
	Symptom   string
	RootCause string

	// Similarity is the [0, 1] source relevance. Pattern hits use vector
	// cosine or lexical query-term coverage; RRF rank is used only for
	// ordering and is never exposed as this score.
	Similarity float64

	// HitCount / LastHitAt mirror the pattern row at lookup time.
	// Gitartifact hits leave both zero-valued.
	HitCount  int
	LastHitAt *time.Time

	// PostmortemID is the FK back to the source postmortem for
	// pattern hits.
	PostmortemID string

	// Summary is the human-readable one-liner the SPA renders in the
	// "参考历史事故" chip. Pattern hits use
	// "{symptom} → {root_cause}"; gitartifact hits use
	// "git:{short-sha}/{file-path}".
	Summary string
}

// KBLookup is the contract the biz layer depends on. Two impls ship
// in this package: KBLookupImpl (production) and a recording stub for
// tests (see service_test.go).
type KBLookup interface {
	// Lookup returns incident-pattern hits in fused retrieval order, followed by
	// git-artifact hits. Similarity is a source relevance score for thresholding.
	// Errors are returned to the caller but the caller decides
	// whether to surface them — see package doc.
	Lookup(ctx context.Context, req KBLookupRequest) ([]KBHit, error)

	// Write is the post-loop hook. The postmortem completion handler
	// calls this to seed / refresh an IncidentPattern row.
	Write(ctx context.Context, pattern chatdiagnosemodel.IncidentPattern) error
}

// PatternRepo is the persistence contract for incident_pattern. The
// data layer (internal/manager/data/chatdiagnose/store/repo.go) will
// implement it; tests use an in-memory fake.
type PatternRepo interface {
	// FindSimilar returns up to topK pattern rows whose tenant
	// matches AND whose cosine similarity to the supplied embedding
	// passes the vector-store threshold. Implementations populate
	// Relevance with that cosine score.
	//
	// vec must have the same dimensionality as the configured
	// embedder (OpenAI 1536 / BGE 512); the impl validates.
	FindSimilar(ctx context.Context, tenantID string, vec []float64, topK int) ([]chatdiagnosemodel.IncidentPattern, error)

	// IncHitCount atomically increments the hit counter. Called
	// asynchronously after a successful lookup so the chat latency
	// is not affected.
	IncHitCount(ctx context.Context, patternID int64) error

	// Save upserts a pattern row. Called by Write. Takes pointer to mutate id on insert.
	Save(ctx context.Context, pattern *chatdiagnosemodel.IncidentPattern) error
}

// PatternCandidateSearcher is an optional lexical-retrieval extension for
// PatternRepo. Implementations MUST restrict candidates to tenantID and use
// terms only as a bounded SQL prefilter; BM25 scoring stays in the business layer.
type PatternCandidateSearcher interface {
	SearchCandidates(ctx context.Context, tenantID string, terms []string, limit int) ([]chatdiagnosemodel.IncidentPattern, error)
}

// GitArtifactLinker is the contract for the gitartifact source. The
// knowledge/gitartifact package will implement it in a later PR; the
// interface lives here so this package compiles today.
type GitArtifactLinker interface {
	// LookupByResource resolves a (resource_type, resource_id) tuple
	// to historical gitartifact hits — commits, docs, postmortems
	// that touched the same resource. Returned hits are relevance-
	// scored; the impl is responsible for sorting.
	LookupByResource(ctx context.Context, tenantID, resourceType, resourceID string) ([]GitArtifactHit, error)
}

// GitArtifactHit is one resolved git reference.
type GitArtifactHit struct {
	// CommitSHA is the full 40-char git SHA. The biz layer truncates
	// to 8 chars for the wire Summary.
	CommitSHA string
	// FilePath is the path within the repo at that commit.
	FilePath string
	// Relevance is the [0, 1] score from the linker.
	Relevance float64
}

// Embedder is the contract for the embedding model. The chat runtime
// will implement it (Day 7 spike); this package only depends on the
// interface.
type Embedder interface {
	// Embed returns a fixed-dimension float slice for text. The biz
	// layer serialises to JSON for storage today; Day 7 swaps to
	// pgvector's vector(N) type.
	Embed(ctx context.Context, text string) ([]float64, error)
}

// KBLookupImpl is the production KBLookup. Composition root wires
// all four deps; the constructors do NOT establish connections —
// that's the data layer's job.
type KBLookupImpl struct {
	// repo is the incident_pattern store.
	repo PatternRepo
	// gitLinker is the gitartifact reverse lookup.
	gitLinker GitArtifactLinker
	// embedder is the embedding model client.
	embedder Embedder
	// threshold is the default similarity floor when the request
	// doesn't set one. Defaults to 0.85 (per D15).
	threshold float64
	// auditLogger is the optional audit sink for KB misses/hits.
	// Nil = no audit (test-friendly).
	auditLogger AuditLogger
}

// AuditLogger is the narrow contract the KB impl uses to emit audit
// events. The real audit package (internal/manager/biz/audit) will
// implement it in a later PR; tests can supply a fake.
type AuditLogger interface {
	Write(ctx context.Context, entry AuditEntry) error
}

// AuditEntry is one audit row the KB emits.
type AuditEntry struct {
	// TenantID is mandatory.
	TenantID string
	// Actor is the user_id (empty for system events).
	Actor string
	// Action is the audit action key (e.g. "kb.hit", "kb.miss").
	Action string
	// Resource is the resource identifier the audit entry refers to
	// (e.g. pattern ID, postmortem ID).
	Resource string
	// Payload is the free-form JSON payload.
	Payload map[string]any
}

// KBLookupOption configures a KBLookupImpl at construction time.
type KBLookupOption func(*KBLookupImpl)

// WithThreshold overrides the default similarity floor.
func WithThreshold(t float64) KBLookupOption {
	return func(k *KBLookupImpl) { k.threshold = t }
}

// WithAuditLogger wires an audit sink.
func WithAuditLogger(a AuditLogger) KBLookupOption {
	return func(k *KBLookupImpl) { k.auditLogger = a }
}

// NewKBLookup constructs a KBLookupImpl. All four deps are mandatory
// in production but tests can pass nil for repo / gitLinker /
// embedder (the corresponding lookup branches become no-ops).
func NewKBLookup(repo PatternRepo, gitLinker GitArtifactLinker, embedder Embedder, opts ...KBLookupOption) *KBLookupImpl {
	k := &KBLookupImpl{
		repo:      repo,
		gitLinker: gitLinker,
		embedder:  embedder,
		threshold: 0.85,
	}
	for _, o := range opts {
		o(k)
	}
	return k
}

// Lookup runs the dual-source KB query. Incident patterns retain reciprocal-rank
// fusion order; git-artifact hits follow them in linker order.
//
// Order of operations:
//  1. Pattern lookup (Jaccard placeholder). Each row that passes the
//     threshold is converted to a KBHit and the hit counter is
//     incremented asynchronously.
//  2. Gitartifact lookup per resource ref. Errors are swallowed at
//     this layer (logged via fmt.Fprintf to stderr in this skeleton —
//     Day 2 spike will route through the project logger).
//
// Similarity remains the bounded source relevance used for thresholding, not
// the fusion rank. Duplicate source tuples across the two sources are preserved.
func (k *KBLookupImpl) Lookup(ctx context.Context, req KBLookupRequest) ([]KBHit, error) {
	if req.TenantID == "" {
		return nil, fmt.Errorf("chatdiagnose: KB lookup requires tenant_id")
	}
	if req.Threshold == 0 {
		req.Threshold = k.threshold
	}
	if req.TopK <= 0 {
		req.TopK = 5
	}

	signature := req.Signature
	if signature == "" {
		signature = extractSignature(req.Message)
	}

	var hits []KBHit
	var queryVec []float64
	// Source 1: incident_pattern.
	// Step 1 — embed signature → vec (with timeout, Maxwell 建议).
	// embedder 为 nil 视为 KB miss（保留 NoopEmbedder 兼容），不阻塞。
	if k.repo != nil && k.embedder != nil {
		embedCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		vec, eErr := k.embedder.Embed(embedCtx, signature)
		cancel()
		if eErr != nil {
			k.auditKBError(ctx, req.TenantID, "kb.embedder.embed_failed", eErr)
		} else {
			queryVec = vec
		}
	}

	var vectorPatterns []chatdiagnosemodel.IncidentPattern
	if k.repo != nil && len(queryVec) > 0 {
		patterns, err := k.repo.FindSimilar(ctx, req.TenantID, queryVec, req.TopK)
		if err != nil {
			// Pattern lookup failure is non-fatal — log via the
			// audit sink if available and continue to gitartifact.
			k.auditKBError(ctx, req.TenantID, "kb.pattern.lookup_failed", err)
		} else {
			vectorPatterns = patterns
		}
	}

	var lexicalPatterns []chatdiagnosemodel.IncidentPattern
	if searcher, ok := k.repo.(PatternCandidateSearcher); ok && k.repo != nil {
		candidates, err := searcher.SearchCandidates(ctx, req.TenantID, lexicalSearchTerms(signature), lexicalCandidateLimit(req.TopK))
		if err != nil {
			k.auditKBError(ctx, req.TenantID, "kb.pattern.candidate_lookup_failed", err)
		} else {
			lexicalPatterns = rankBM25(signature, candidates, req.TopK)
		}
	}

	for _, ranked := range fusePatternRanks(vectorPatterns, lexicalPatterns) {
		p := ranked.pattern
		relevance := ranked.relevance
		if relevance < req.Threshold {
			continue
		}
		if len(hits) >= req.TopK {
			continue
		}
		hits = append(hits, KBHit{
			PatternID:    p.ID,
			ResourceType: p.ResourceType,
			Symptom:      p.Symptom,
			RootCause:    p.RootCauseObject,
			Similarity:   relevance,
			HitCount:     p.HitCount,
			LastHitAt:    p.LastHitAt,
			PostmortemID: p.SourcePostmortemID,
			Summary:      p.Symptom + " → " + p.RootCauseObject,
		})
		// Audit KB hit
		if k.auditLogger != nil {
			_ = k.auditLogger.Write(ctx, AuditEntry{
				TenantID: req.TenantID,
				Action:   "kb.pattern.hit",
				Resource: fmt.Sprintf("pattern:%d", p.ID),
				Payload: map[string]any{
					"pattern_id":    p.ID,
					"resource_type": p.ResourceType,
					"similarity":    relevance,
					"fingerprint":   p.Fingerprint,
				},
			})
		}
		// Async hit counter increment — uses a fresh
		// background context (Go 1.21+ context.WithoutCancel
		// 保留 trace_id) so the caller cancel doesn't cancel
		// the bookkeeping write.
		go func(parentCtx context.Context, patternID int64) {
			bgCtx := context.WithoutCancel(parentCtx)
			_ = k.repo.IncHitCount(bgCtx, patternID)
		}(ctx, p.ID)
	}

	// Source 2: gitartifact.
	if k.gitLinker != nil {
		for _, r := range req.Resources {
			ghits, err := k.gitLinker.LookupByResource(ctx, req.TenantID, r.Type, r.ID)
			if err != nil {
				k.auditKBError(ctx, req.TenantID, "kb.gitartifact.lookup_failed", err)
				continue
			}
			for _, gh := range ghits {
				shortSHA := gh.CommitSHA
				if len(shortSHA) > 8 {
					shortSHA = shortSHA[:8]
				}
				hits = append(hits, KBHit{
					ResourceType: r.Type,
					Similarity:   gh.Relevance,
					Summary:      fmt.Sprintf("git:%s/%s", shortSHA, gh.FilePath),
				})
			}
		}
	}

	return hits, nil
}

// Write seeds an incident pattern. Called by the postmortem
// completion hook.
//
// Steps:
//  1. Compute the embedding for Signature.
//  2. Serialise to JSON and stash in pattern.Embedding.
//  3. Upsert via repo.Save.
func (k *KBLookupImpl) Write(ctx context.Context, pattern chatdiagnosemodel.IncidentPattern) error {
	if pattern.TenantID == "" {
		return fmt.Errorf("chatdiagnose: KB Write requires tenant_id")
	}
	if k.embedder == nil {
		return fmt.Errorf("chatdiagnose: KB Write requires a configured embedder")
	}
	if pattern.Signature == "" {
		return fmt.Errorf("chatdiagnose: KB Write requires non-empty Signature")
	}
	vec, err := k.embedder.Embed(ctx, pattern.Signature)
	if err != nil {
		return fmt.Errorf("chatdiagnose: embed failed: %w", err)
	}
	encoded, err := json.Marshal(vec)
	if err != nil {
		return fmt.Errorf("chatdiagnose: marshal embedding: %w", err)
	}
	pattern.Embedding = string(encoded)
	if pattern.CreatedAt.IsZero() {
		pattern.CreatedAt = time.Now().UTC()
	}
	pattern.UpdatedAt = time.Now().UTC()
	if k.repo == nil {
		return fmt.Errorf("chatdiagnose: KB Write requires a configured PatternRepo")
	}
	return k.repo.Save(ctx, &pattern)
}

// auditKBError centralises the audit emission for KB lookup errors.
// Best-effort — never returns an error to the caller (the chat path
// MUST continue even when the KB is broken).
func (k *KBLookupImpl) auditKBError(ctx context.Context, tenantID, action string, cause error) {
	if k.auditLogger == nil {
		return
	}
	_ = k.auditLogger.Write(ctx, AuditEntry{
		TenantID: tenantID,
		Action:   action,
		Payload: map[string]any{
			"error": cause.Error(),
		},
	})
}

// extractSignature is the thin wrapper the chat entry point uses to
// derive a signature from the raw user message. Day 2 placeholder:
// the full message lowercased + whitespace-normalised. Day 7 will
// swap to an LLM-extracted symptom + resource fingerprint.
func extractSignature(message string) string {
	return strings.ToLower(strings.TrimSpace(message))
}
