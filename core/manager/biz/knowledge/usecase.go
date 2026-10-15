// Package knowledge bundles the user-facing knowledge base + git-repo
// integration. Two flows:
//
//  1. Manual docs: user pastes markdown via the SPA's /knowledge page
//     (or POST /v1/knowledge/docs). We embed and upsert into qdrant.
//  2. Repo sync: user registers a git URL; Sync() shells `git clone
//     --depth=1` (or `git pull` when the dir already exists) into
//     /var/lib/opskeeper/repos/<id>, walks the tree for .md / .txt /
//     .rst / .yaml / .yml / .toml / .json files, embeds each, replaces
//     the qdrant point set for that repo.
//
// The repo registrations themselves live in MySQL (knowledge_repos —
// small relational table; not search target). Every doc body lives
// only in qdrant — payload + vector. No double-write.
package knowledge

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/embedding"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/qdrantx"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	incidentcontrol "github.com/vincent-wuhan/opskeeper/core/domains/control/incident"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/knowledge"
	"github.com/vincent-wuhan/opskeeper/core/manager/observability/otelgenai"
	"go.opentelemetry.io/otel/attribute"
)

// RepoStore is the narrow relational-data surface. *store.Repo
// satisfies it.
type RepoStore interface {
	ListRepos(ctx context.Context) ([]*model.Repository, error)
	GetRepo(ctx context.Context, id uint64) (*model.Repository, error)
	GetRepoByURL(ctx context.Context, url string) (*model.Repository, error)
	CreateRepo(ctx context.Context, repo *model.Repository) error
	UpdateRepoSync(ctx context.Context, id uint64, fileCount int, syncErr string) error
	MarkRepoSyncFailed(ctx context.Context, id uint64, syncErr string) error
	DeleteRepo(ctx context.Context, id uint64) error

	// SSH identities — managed in the same data layer for
	// transactional locality (one DB connection covers both repo +
	// identity reads at sync time).
	ListSSHIdentities(ctx context.Context) ([]*model.SSHIdentity, error)
	GetSSHIdentity(ctx context.Context, id uint64) (*model.SSHIdentity, error)
	CreateSSHIdentity(ctx context.Context, id *model.SSHIdentity) error
	UpdateSSHIdentity(ctx context.Context, id uint64, name, hostsJSON, knownHosts string) error
	TouchSSHIdentityUsage(ctx context.Context, id uint64) error
	DeleteSSHIdentity(ctx context.Context, id uint64) error
}

// QdrantClient is the vector-store surface this usecase needs. It is an
// alias for the port in qdrantx so both the HTTP qdrant client and the
// embedded backend satisfy it — and tests can inject a fake.
type QdrantClient = qdrantx.Store

type payloadMigrator interface {
	SetPayloadByFilter(
		ctx context.Context,
		collection string,
		payload map[string]any,
		mustMatch map[string]any,
	) error
}

// CollectionName is the single qdrant collection opskeeper writes to.
const CollectionName = "opskeeper_knowledge"

const globalTenantScope = "global"

// Re-exports so external callers stay on the biz alias.
type (
	ListDocsFilter = model.ListDocsFilter
	SearchOptions  = model.SearchOptions
	SearchHit      = model.SearchHit
)

// Usecase is the public service. Repo holds repo registrations,
// Vec holds the embedded docs, Embedder produces vectors. cloneDir
// is where git checkouts land.
//
// HTTPS git auth uses git's `credential.helper` protocol
// (P3, not yet shipped). The earlier GitHub-only PAT path was removed
// once SSH identities (P1+P2) covered the realistic use cases. Until
// P3 lands, private HTTPS repos cannot be cloned — use SSH (git@host:)
// for any non-public source.
type Usecase struct {
	repo     RepoStore
	vec      QdrantClient
	embed    embedding.Embedder
	cloneDir string
	log      *slog.Logger

	recallRepo RecallStore
}

type RecallStore interface {
	AppendRecallLogs(ctx context.Context, logs []incidentcontrol.RecallLog) error
	ListRunbooks(ctx context.Context, tenantID, databaseType, faultFingerprint string) ([]incidentcontrol.Postmortem, error)
}

func (u *Usecase) WithRecallRepository(repository RecallStore) *Usecase {
	u.recallRepo = repository
	return u
}

// New returns a Usecase. cloneDir defaults to /var/lib/opskeeper/repos.
// Synchronously calls EnsureCollection at boot when an embedder is
// configured — without one we accept reads against any pre-existing
// qdrant collection and reject writes (CreateManualDoc / Sync) with
// errs.ErrUnavailable. This lets /api/v1/knowledge/* list endpoints
// render on a fresh install (no LLM key) so operators can browse
// what's there before configuring the embedder.
func New(ctx context.Context, repo RepoStore, vec QdrantClient, embed embedding.Embedder, cloneDir string, log *slog.Logger) (*Usecase, error) {
	if cloneDir == "" {
		cloneDir = "/var/lib/opskeeper/repos"
	}
	if log == nil {
		log = slog.Default()
	}
	if vec == nil {
		return nil, errors.New("knowledge: qdrant client required")
	}
	// Pick the dim from the embedder when present, else fall back to the
	// configured OPSKEEPER_EMBEDDING_DIM (sniffed via env here so we don't
	// drag a config struct into the biz layer) so the collection exists
	// for read paths on a fresh install. The operator's later embedder
	// MUST match this dim — same env var is read by both code paths.
	dim := 1536
	if embed != nil {
		dim = embed.Dim()
	} else if v := os.Getenv("OPSKEEPER_EMBEDDING_DIM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			dim = n
		}
	}
	if err := vec.EnsureCollection(ctx, CollectionName, dim); err != nil {
		return nil, fmt.Errorf("knowledge: ensure collection: %w", err)
	}
	// Payload indexes for server-side filters. Without these qdrant
	// scans the whole collection for every filter. Idempotent.
	//
	// path = exact-match on full breadcrumb (e.g. "网络/DNS").
	// path_prefixes = array of every cumulative prefix; prefix queries
	//   are exact-match against this field. We avoided qdrant's text
	//   tokenizer because "网络" tokenizes loosely and would match
	//   "K8s/网络" by mistake.
	//
	// id_alias intentionally not indexed: doc IDs span the full uint64
	// range (md5-derived) and qdrant's filter parser rejects values
	// > int64, so we never filter by id (use GetPoints instead).
	for _, f := range []string{"source_type", "repo_id", "path", "path_prefixes", "tags", "tenant_scopes"} {
		if err := vec.EnsurePayloadIndex(ctx, CollectionName, f, "keyword"); err != nil {
			log.Warn("knowledge: payload index",
				slog.String("field", f),
				slog.Any("err", err))
		}
	}
	return &Usecase{repo: repo, vec: vec, embed: embed, cloneDir: cloneDir, log: log}, nil
}

// ----- Doc CRUD -----

// CreateManualDocInput is the form-shaped input for /knowledge POST.
type CreateManualDocInput struct {
	TenantID string
	Title    string
	// TitleEN is an optional English label shown when the operator's
	// locale is en-US. Empty = UI falls back to Title.
	TitleEN string
	Content string
	URL     string // optional source URL the user wants to remember
	Path    string // "/"-separated breadcrumb (e.g. "网络/DNS")
	Tags    []string
}

// CreateManualDoc validates, embeds, upserts to qdrant.
func (u *Usecase) CreateManualDoc(ctx context.Context, in CreateManualDocInput) (*model.Doc, error) {
	if u.embed == nil {
		return nil, fmt.Errorf("%w: embedder not configured (set OPSKEEPER_EMBEDDING_API_KEY)", errs.ErrNotWiredYet)
	}
	title := strings.TrimSpace(in.Title)
	content := strings.TrimSpace(in.Content)
	if title == "" {
		return nil, fmt.Errorf("%w: title required", errs.ErrInvalid)
	}
	if content == "" {
		return nil, fmt.Errorf("%w: content required", errs.ErrInvalid)
	}
	if len(title) > 256 {
		title = title[:256]
	}
	tenantID := normalizedTenantID(in.TenantID)
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id required", errs.ErrUnauthorized)
	}
	id := manualDocID(tenantID, title)
	now := time.Now().UTC()
	d := &model.Doc{
		ID:           id,
		SourceType:   model.SourceManual,
		TenantScopes: tenantScopes(tenantID),
		Title:        title,
		TitleEN:      strings.TrimSpace(in.TitleEN),
		Content:      content,
		URL:          strings.TrimSpace(in.URL),
		Path:         normalizePath(in.Path),
		Tags:         normalizeTags(in.Tags),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := u.upsertDoc(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

// UploadDocInput is one uploaded file (ADR-028). Filename is the logical
// identity (also the displayed url); re-uploading the same name overwrites.
type UploadDocInput struct {
	TenantID string
	Filename string // e.g. "runbooks/db-failover.md" or "db-failover.md"
	Title    string // optional; defaults to filename without extension
	Content  string // already-decoded UTF-8 text (md / txt phase-1)
	Path     string // optional folder breadcrumb; empty = root
	Tags     []string
}

// UploadDoc ingests an org-uploaded file into the 组织知识库 tree
// (source_type=upload). Chunk → embed → upsert, mirroring the vault/repo
// pipeline. Deterministic per-chunk ids + a delete-by-(source,url) sweep
// make re-uploads idempotent (handles a file that got shorter). A vault
// re-sync never touches these — its delete is scoped to source_type=vault.
func (u *Usecase) UploadDoc(ctx context.Context, in UploadDocInput) (*model.Doc, error) {
	if u.embed == nil {
		return nil, fmt.Errorf("%w: embedder not configured", errs.ErrNotWiredYet)
	}
	url := strings.TrimSpace(in.Filename)
	content := strings.TrimSpace(in.Content)
	if url == "" {
		return nil, fmt.Errorf("%w: filename required", errs.ErrInvalid)
	}
	if content == "" {
		return nil, fmt.Errorf("%w: empty file", errs.ErrInvalid)
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(url), filepath.Ext(url))
	}
	if len(title) > 256 {
		title = title[:256]
	}
	tenantID := normalizedTenantID(in.TenantID)
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenant_id required", errs.ErrUnauthorized)
	}
	now := time.Now().UTC()
	return u.ingestUpload(ctx, model.Doc{
		SourceType:   model.SourceUpload,
		TenantScopes: tenantScopes(tenantID),
		URL:          url,
		Title:        title,
		Content:      content,
		Path:         normalizePath(in.Path),
		Tags:         normalizeTags(in.Tags),
		CreatedAt:    now,
		UpdatedAt:    now,
	})
}

// ingestUpload (re)chunks one org-uploaded file into qdrant under
// source_type=upload, keyed on d.URL (the file's stable identity). It embeds
// first, then sweeps any prior version of the same url (so a now-shorter body
// leaves no stale high-index chunks behind), then upserts — an embed failure
// must not delete the doc before the replacement exists. Shared by
// UploadDoc (initial multipart ingest) and UpdateManualDoc's upload branch
// (in-place edit of an already-uploaded file, ADR-028 组织CRUD). The sweep is
// scoped to this exact (source_type=upload, url, tenant_scopes) — it never
// touches other docs, and a vault re-sync never touches these (its delete is
// scoped to source_type=vault). Returns the logical doc (head chunk id).
func (u *Usecase) ingestUpload(ctx context.Context, d model.Doc) (*model.Doc, error) {
	if uploadScope(d) == "" {
		return nil, fmt.Errorf("%w: tenant scope required", errs.ErrUnauthorized)
	}
	// Embed the whole body before clearing the prior chunks (same rationale as
	// the repo/vault paths): a transient embedder failure on an in-place edit
	// must not delete the doc the operator is editing.
	parts := splitForChunks(d.Content)
	const batch = 32
	vectors := make([][]float32, 0, len(parts))
	for i := 0; i < len(parts); i += batch {
		end := i + batch
		if end > len(parts) {
			end = len(parts)
		}
		texts := make([]string, 0, end-i)
		for j := i; j < end; j++ {
			body := parts[j]
			if j == 0 {
				body = d.Title + "\n\n" + parts[j]
			}
			texts = append(texts, truncateForEmbedding(body))
		}
		batchVectors, err := u.embed.Embed(ctx, texts)
		if err != nil {
			return nil, fmt.Errorf("knowledge: embed upload batch %d: %w", i, err)
		}
		vectors = append(vectors, batchVectors...)
	}

	// Sweep any prior version of the same url so a now-shorter body leaves no
	// stale high-index chunks behind (scoped to this exact url + tenant scope).
	if err := u.vec.DeleteByFilter(ctx, CollectionName, map[string]any{
		"source_type":   model.SourceUpload,
		"url":           d.URL,
		"tenant_scopes": d.TenantScopes,
	}); err != nil {
		return nil, fmt.Errorf("knowledge: clear prior upload: %w", err)
	}

	for i := 0; i < len(parts); i += batch {
		end := i + batch
		if end > len(parts) {
			end = len(parts)
		}
		points := make([]qdrantx.Point, 0, end-i)
		for j := i; j < end; j++ {
			body := parts[j]
			if j == 0 {
				body = d.Title + "\n\n" + parts[j]
			}
			points = append(points, uploadChunkPoint(d.URL, j, len(parts), vectors[j], d, body))
		}
		if err := u.vec.Upsert(ctx, CollectionName, points); err != nil {
			return nil, fmt.Errorf("knowledge: upsert upload batch %d: %w", i, err)
		}
	}
	out := d
	out.ID = uploadChunkDocID(uploadScope(d), d.URL, 0)
	return &out, nil
}

// UpdateManualDocInput captures every editable field on a manual doc.
// Tags is replaced wholesale (not merged) — same as title/content.
// Empty Path clears the field (= moves doc back to root).
type UpdateManualDocInput struct {
	TenantID string
	Title    string
	TitleEN  string
	Content  string
	Path     string
	Tags     []string
}

// UpdateManualDoc edits title + content + category + tags of an org-owned doc
// (ADR-028 组织CRUD): both manual-paste (single point) and uploaded files
// (multi-chunk). Vault/repo docs reject — they're regenerated on sync.
func (u *Usecase) UpdateManualDoc(ctx context.Context, id uint64, in UpdateManualDocInput) (*model.Doc, error) {
	if u.embed == nil {
		return nil, fmt.Errorf("%w: embedder not configured (set OPSKEEPER_EMBEDDING_API_KEY)", errs.ErrNotWiredYet)
	}
	existing, err := u.GetDoc(ctx, id)
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(in.Title)
	content := strings.TrimSpace(in.Content)
	if title == "" || content == "" {
		return nil, fmt.Errorf("%w: title + content required", errs.ErrInvalid)
	}
	if len(title) > 256 {
		title = title[:256]
	}
	switch existing.SourceType {
	case model.SourceManual:
		if !docVisibleToTenant(existing, normalizedTenantID(in.TenantID)) {
			return nil, errs.ErrForbidden
		}
		existing.Title = title
		existing.TitleEN = strings.TrimSpace(in.TitleEN)
		existing.Content = content
		existing.Path = normalizePath(in.Path)
		existing.Tags = normalizeTags(in.Tags)
		existing.UpdatedAt = time.Now().UTC()
		if err := u.upsertDoc(ctx, existing); err != nil {
			return nil, err
		}
		return existing, nil
	case model.SourceUpload:
		if !docVisibleToTenant(existing, normalizedTenantID(in.TenantID)) {
			return nil, errs.ErrForbidden
		}
		// Re-chunk + re-embed the edited file under its stable url identity.
		// url is the file's identity (not editable); title/title_en/content/
		// path/tags all are. CreatedAt is preserved across the re-ingest.
		return u.ingestUpload(ctx, model.Doc{
			SourceType:   model.SourceUpload,
			TenantScopes: existing.TenantScopes,
			URL:          existing.URL,
			Title:        title,
			TitleEN:      strings.TrimSpace(in.TitleEN),
			Content:      content,
			Path:         normalizePath(in.Path),
			Tags:         normalizeTags(in.Tags),
			CreatedAt:    existing.CreatedAt,
			UpdatedAt:    time.Now().UTC(),
		})
	default:
		return nil, fmt.Errorf("%w: %s docs are read-only — re-sync the source to refresh", errs.ErrInvalid, existing.SourceType)
	}
}

// normalizePath trims surrounding whitespace and slashes; collapses
// internal "//" to "/"; an all-blank input becomes "" (= root).
// Examples:
//
//	"网络/DNS" → "网络/DNS"
//	"/网络/DNS/" → "网络/DNS"
//	"网络//DNS" → "网络/DNS"
//	" " → ""
func normalizePath(in string) string {
	s := strings.Trim(strings.TrimSpace(in), "/")
	if s == "" {
		return ""
	}
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return s
}

// pathPrefixes returns every cumulative prefix of a "/"-separated
// breadcrumb. Used as a denormalized payload field so that prefix
// filters reduce to keyword exact-match (qdrant's text/fulltext
// tokenizer is too loose for strict path prefix). Examples:
//
//	"网络/DNS/排查" → ["网络", "网络/DNS", "网络/DNS/排查"]
//	"网络" → ["网络"]
//	"" → nil
//
// Note: this means "网络/DNS/排查" is searchable by path_prefix=网络
// (matches), path_prefix=网络/DNS (matches), but NOT by path_prefix=网
// (no exact-match). That's the intended folder-tree semantics.
func pathPrefixes(p string) []string {
	p = normalizePath(p)
	if p == "" {
		return nil
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for i := range parts {
		out = append(out, strings.Join(parts[:i+1], "/"))
	}
	return out
}

// normalizeTags trims, dedupes, drops empty. Order is preserved on first
// occurrence so a UI list keeps user intent.
func normalizeTags(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizedTenantID(in string) string {
	return strings.TrimSpace(in)
}

func tenantScopes(tenantID string) []string {
	return []string{"tenant:" + normalizedTenantID(tenantID)}
}

func uploadScope(d model.Doc) string {
	for _, scope := range d.TenantScopes {
		if strings.HasPrefix(scope, "tenant:") {
			return scope
		}
	}
	return ""
}

func docVisibleToTenant(d *model.Doc, tenantID string) bool {
	if d == nil {
		return false
	}
	scope := "tenant:" + normalizedTenantID(tenantID)
	for _, got := range d.TenantScopes {
		if got == scope || got == globalTenantScope {
			return true
		}
	}
	return false
}

// ListDocs streams via qdrant scroll filtered by source/repo/path/tag.
// Listings show one entry per file even when the file got chunked into
// multiple qdrant points.
//
// Historical wart: we used to MustMatch chunk_index=0 at the qdrant
// layer, but the manual-doc upsert path forgot to set chunk_index in
// payload (fixed now). Every manual doc inserted before the fix is
// invisible to that strict filter, even though the vector is fine
// and RAG search returns it. Operators interpreted this as "RAG 又没了".
//
// New behavior: scroll without the chunk_index filter, dedupe by
// id_alias in Go preferring the head chunk (chunk_index==0 or absent).
// Cost: the scroll returns chunks for chunked repo docs too; we bump
// the underlying limit to compensate so the caller's `Limit` still
// represents a count of logical docs, not points.
func (u *Usecase) ListDocs(ctx context.Context, f ListDocsFilter) ([]*model.Doc, error) {
	must := map[string]any{}
	if f.SourceType != "" {
		must["source_type"] = f.SourceType
	}
	if f.RepoID != nil {
		must["repo_id"] = *f.RepoID
	}
	if f.Path != "" {
		must["path"] = f.Path
	} else if f.PathPrefix != "" {
		// Strict folder-tree prefix via the denormalized path_prefixes
		// array (each doc carries every cumulative prefix); exact
		// keyword match here is "doc's path-tree contains this folder".
		must["path_prefixes"] = normalizePath(f.PathPrefix)
	}
	if f.Tag != "" {
		// On array-typed payload like tags qdrant's match.value still
		// works as "contains".
		must["tags"] = f.Tag
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	// Pad to make room for non-head chunks that the dedupe will drop.
	scanLimit := limit * 8
	if scanLimit > 10000 {
		scanLimit = 10000
	}
	res, err := u.vec.Scroll(ctx, CollectionName, qdrantx.ScrollOpts{
		MustMatch: must,
		Limit:     scanLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: scroll: %w", err)
	}
	return dedupeByIDAlias(res.Points, limit), nil
}

// dedupeByIDAlias collapses a flat list of qdrant points (some of
// which may be chunks of the same logical doc) to one entry per
// id_alias, preferring the head chunk (chunk_index == 0 or absent).
// Order is preserved by first occurrence. limit caps the result.
func dedupeByIDAlias(points []qdrantx.SearchHit, limit int) []*model.Doc {
	type slot struct {
		idx        int // position in `out` to overwrite if we find a better head
		isHead     bool
		isExplicit bool // chunk_index field actually present
	}
	out := make([]*model.Doc, 0, len(points))
	seen := make(map[uint64]*slot, len(points))
	for _, p := range points {
		alias := docIDAlias(p)
		ci, ciPresent := chunkIndexFromPayload(p.Payload)
		isHead := !ciPresent || ci == 0
		if s, ok := seen[alias]; ok {
			// Already have an entry for this doc. Upgrade if the new
			// point is a head chunk and the existing one wasn't.
			if isHead && !s.isHead {
				out[s.idx] = payloadToDoc(p.ID, p.Payload)
				s.isHead = true
				s.isExplicit = ciPresent
			}
			continue
		}
		out = append(out, payloadToDoc(p.ID, p.Payload))
		seen[alias] = &slot{idx: len(out) - 1, isHead: isHead, isExplicit: ciPresent}
		if limit > 0 && len(out) >= limit && limit < len(points) {
			// We've already pinned `limit` distinct docs; further
			// points can only upgrade existing entries, never add
			// new ones. Keep scanning so chunk-only-first-then-head
			// ordering still upgrades the right slot.
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// docIDAlias extracts the logical doc id from a qdrant point.
// Falls back to the point id when id_alias is absent (older data /
// non-chunked docs that never set the helper).
func docIDAlias(p qdrantx.SearchHit) uint64 {
	if v, ok := p.Payload["id_alias"]; ok {
		switch x := v.(type) {
		case float64:
			return uint64(x)
		case int64:
			return uint64(x)
		case uint64:
			return x
		case json.Number:
			if n, err := x.Int64(); err == nil {
				return uint64(n)
			}
		}
	}
	return p.ID
}

// chunkIndexFromPayload returns (index, present). Missing field means
// pre-chunking data (manual doc inserted before chunk_index was added
// to upsertDoc) — treat as head chunk.
func chunkIndexFromPayload(payload map[string]any) (int, bool) {
	v, ok := payload["chunk_index"]
	if !ok || v == nil {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case int64:
		return int(x), true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}

// GetDoc loads a single doc by qdrant point id. Uses POST /points
// rather than a filter, because doc IDs are full uint64 (md5-derived)
// and qdrant's filter parser rejects values > int64.
func (u *Usecase) GetDoc(ctx context.Context, id uint64) (*model.Doc, error) {
	pts, err := u.vec.GetPoints(ctx, CollectionName, []uint64{id})
	if err != nil {
		return nil, fmt.Errorf("knowledge: get point: %w", err)
	}
	for _, p := range pts {
		if p.ID == id {
			return payloadToDoc(p.ID, p.Payload), nil
		}
	}
	return nil, errs.ErrNotFound
}

// MoveDoc relocates an org-owned doc into a different folder (newPath) —
// the drag-and-drop target on the 组织 tree (ADR-029 point 3). Only path
// changes; title/content/tags are preserved. Manual docs re-upsert in place;
// uploaded files re-ingest under their stable url (path lives in every chunk
// payload, so it's a full re-embed — moves are user-initiated and rare, so
// the cost is acceptable). Vault/repo docs reject — they're read-only. Same
// tenant gate as UpdateManualDoc: you can only move a doc your tenant sees.
func (u *Usecase) MoveDoc(ctx context.Context, id uint64, newPath, tenantID string) (*model.Doc, error) {
	if u.embed == nil {
		return nil, fmt.Errorf("%w: embedder not configured (set OPSKEEPER_EMBEDDING_API_KEY)", errs.ErrNotWiredYet)
	}
	existing, err := u.GetDoc(ctx, id)
	if err != nil {
		return nil, err
	}
	path := normalizePath(newPath)
	switch existing.SourceType {
	case model.SourceManual:
		if !docVisibleToTenant(existing, tenantID) {
			return nil, errs.ErrForbidden
		}
		existing.Path = path
		existing.UpdatedAt = time.Now().UTC()
		if err := u.upsertDoc(ctx, existing); err != nil {
			return nil, err
		}
		return existing, nil
	case model.SourceUpload:
		if !docVisibleToTenant(existing, tenantID) {
			return nil, errs.ErrForbidden
		}
		return u.ingestUpload(ctx, model.Doc{
			SourceType:   model.SourceUpload,
			TenantScopes: existing.TenantScopes,
			URL:          existing.URL,
			Title:        existing.Title,
			TitleEN:      existing.TitleEN,
			Content:      existing.Content,
			Path:         path,
			Tags:         existing.Tags,
			CreatedAt:    existing.CreatedAt,
			UpdatedAt:    time.Now().UTC(),
		})
	default:
		return nil, fmt.Errorf("%w: %s docs can't be moved — they're read-only", errs.ErrInvalid, existing.SourceType)
	}
}

// DeleteDoc removes an org-owned doc (ADR-028 组织CRUD): a manual doc (single
// point) or an uploaded file (every chunk of its url). Vault/repo docs reject
// — unsync the whole source to drop them. Same tenant gate as
// UpdateManualDoc: you can only delete a doc your tenant can see, so a
// cross-tenant caller hitting ErrForbidden on edit can't bypass it by
// deleting instead.
func (u *Usecase) DeleteDoc(ctx context.Context, id uint64, tenantID string) error {
	d, err := u.scrollOneByID(ctx, id)
	if err != nil {
		return err
	}
	if d == nil {
		return errs.ErrNotFound
	}
	switch d.SourceType {
	case model.SourceManual:
		if !docVisibleToTenant(d, tenantID) {
			return errs.ErrForbidden
		}
		return u.vec.DeleteByID(ctx, CollectionName, id)
	case model.SourceUpload:
		if !docVisibleToTenant(d, tenantID) {
			return errs.ErrForbidden
		}
		// Uploaded files are multi-chunk; drop every chunk of this url.
		return u.vec.DeleteByFilter(ctx, CollectionName, map[string]any{
			"source_type": model.SourceUpload,
			"url":         d.URL,
		})
	default:
		return fmt.Errorf("%w: %s docs can't be deleted individually — unregister the source", errs.ErrInvalid, d.SourceType)
	}
}

// scrollOneByID is GetDoc minus the err for "not found" — returns nil.
func (u *Usecase) scrollOneByID(ctx context.Context, id uint64) (*model.Doc, error) {
	pts, err := u.vec.GetPoints(ctx, CollectionName, []uint64{id})
	if err != nil {
		return nil, fmt.Errorf("knowledge: get point: %w", err)
	}
	for _, p := range pts {
		if p.ID == id {
			return payloadToDoc(p.ID, p.Payload), nil
		}
	}
	return nil, nil
}

// ListPaths returns the distinct non-empty Path values across every
// doc, plus per-path doc counts. Backs the SPA's folder-tree view —
// the SPA splits each path on "/" to build the actual tree. Cheap
// (single Scroll over up to 5000 points; collection cap is ≤2000
// repo files + manual docs). Filters to chunk_index=0 so a chunked
// doc still counts as one entry in the folder tree.
//
// Path source: manual docs set `path` directly via the editor; repo
// docs never set it (the Sync path doesn't take a per-doc folder),
// so we fall back to the URL's directory portion — i.e. a repo doc
// at `reference/external/dns/foo.md` contributes to the `reference`
// / `reference/external` / `reference/external/dns` tree. This is
// what the SPA tree view always wanted; previously the tree was
// silently empty for repo docs.
func (u *Usecase) ListPaths(ctx context.Context) (map[string]int, error) {
	// Same chunk_index relaxation as ListDocs — see dedupeByIDAlias
	// comment. Scroll without the chunk_index filter; dedupe so each
	// logical doc contributes once to its path bucket.
	res, err := u.vec.Scroll(ctx, CollectionName, qdrantx.ScrollOpts{
		Limit: 10000,
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: scroll for paths: %w", err)
	}
	docs := dedupeByIDAlias(res.Points, 0)
	out := make(map[string]int)
	for _, d := range docs {
		if v := d.Path; v != "" {
			out[v]++
			continue
		}
		// Repo doc fallback — derive a folder from URL.
		if d.URL != "" {
			dir := filepath.Dir(d.URL)
			if dir != "" && dir != "." && dir != "/" {
				out[dir]++
			}
		}
	}
	return out, nil
}

// ----- Search -----

// Search embeds the query, computes vector and keyword candidate ranks,
// then returns docs in RRF order. When IncidentID is set, every candidate
// decision is persisted to rrf_recall_log before the caller receives data.
func (u *Usecase) Search(ctx context.Context, q string, opts SearchOptions) ([]SearchHit, error) {
	ctx, endSpan := otelgenai.StartRAG(ctx, "search", CollectionName)
	if u.embed == nil {
		err := fmt.Errorf("%w: embedder not configured (set OPSKEEPER_EMBEDDING_API_KEY)", errs.ErrNotWiredYet)
		endSpan(err)
		return nil, err
	}
	q = strings.TrimSpace(q)
	if q == "" {
		endSpan(nil, attribute.Int("opskeeper.genai.rag.hit_count", 0))
		return nil, nil
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	tenantID := normalizedTenantID(opts.TenantID)
	if tenantID == "" {
		if caller, ok := tenantctx.From(ctx); ok {
			if caller.AgentTeams != nil {
				tenantID = normalizedTenantID(caller.AgentTeams.TenantID)
			} else if caller.UserID != 0 {
				tenantID = fmt.Sprint(caller.UserID)
			}
		}
	}
	if tenantID == "" {
		err := fmt.Errorf("%w: tenant_id required", errs.ErrUnauthorized)
		endSpan(err)
		return nil, err
	}
	vecs, err := u.embed.Embed(ctx, []string{q})
	if err != nil {
		err = fmt.Errorf("knowledge: embed query: %w", err)
		endSpan(err)
		return nil, err
	}
	if len(vecs) != 1 {
		err = fmt.Errorf("knowledge: embedder returned %d vectors", len(vecs))
		endSpan(err)
		return nil, err
	}
	must := map[string]any{}
	if p := normalizePath(opts.Path); p != "" {
		must["path"] = p
	} else if p := normalizePath(opts.PathPrefix); p != "" {
		must["path_prefixes"] = p
	}
	if tags := normalizeTags(opts.Tags); len(tags) > 0 {
		must["tags"] = tags // []string → match.any in qdrantx.buildFilter
	}
	must["tenant_scopes"] = []string{globalTenantScope, "tenant:" + tenantID}
	// Over-fetch so that the dedup-by-parent step still returns `limit`
	// unique docs even when a long doc contributes multiple chunks at
	// the top of the result list. Cap at 5x to bound the upper end.
	overFetch := limit * 5
	if overFetch > 200 {
		overFetch = 200
	}
	hits, err := u.vec.Search(ctx, CollectionName, vecs[0], qdrantx.SearchOpts{
		Limit:     overFetch,
		MustMatch: must,
	})
	if err != nil {
		err = fmt.Errorf("knowledge: search: %w", err)
		endSpan(err)
		return nil, err
	}
	candidates := make(map[string]hybridCandidate)
	for _, hit := range hits {
		id := docIDAlias(hit)
		ref := knowledgeCandidateRef(id)
		if _, exists := candidates[ref]; exists {
			continue
		}
		candidates[ref] = hybridCandidate{
			ref:         ref,
			doc:         payloadToDoc(id, hit.Payload),
			vectorRank:  len(candidates) + 1,
			vectorScore: hit.Score,
		}
	}
	keywordCandidates, err := u.keywordCandidates(ctx, must, q, tenantID, opts)
	if err != nil {
		err = fmt.Errorf("knowledge: keyword search: %w", err)
		endSpan(err)
		return nil, err
	}
	for index, keyword := range keywordCandidates {
		ref := keyword.ref
		candidate, exists := candidates[ref]
		candidate.ref = ref
		candidate.doc = keyword.doc
		candidate.keywordRank = index + 1
		if !exists {
			candidate.vectorScore = 0
		}
		candidates[ref] = candidate
	}

	ranked := make([]incidentcontrol.RankedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		ranked = append(ranked, incidentcontrol.RankedCandidate{
			CandidateRef: candidate.ref,
			VectorRank:   candidate.vectorRank,
			KeywordRank:  candidate.keywordRank,
		})
	}
	selection, err := incidentcontrol.SelectTopRRF(ranked)
	if err != nil && len(ranked) > 0 {
		endSpan(err)
		return nil, err
	}
	if opts.IncidentID != "" && u.recallRepo == nil {
		err = errors.New("knowledge: incident recall repository not wired")
		endSpan(err)
		return nil, err
	}
	if opts.IncidentID != "" && len(selection) > 0 {
		recalledAt := time.Now().UTC()
		logs := make([]incidentcontrol.RecallLog, 0, len(selection))
		for _, candidate := range selection {
			logs = append(logs, incidentcontrol.RecallLog{
				TenantID: tenantID, IncidentID: opts.IncidentID, QueryText: q,
				CandidateRef: candidate.CandidateRef, VectorRank: candidate.VectorRank,
				KeywordRank: candidate.KeywordRank, RRFScore: candidate.RRFScore,
				Selected: candidate.Selected, RejectedReason: candidate.RejectedReason,
				RecalledAt: recalledAt,
			})
		}
		if err := u.recallRepo.AppendRecallLogs(ctx, logs); err != nil {
			err = fmt.Errorf("knowledge: append incident recall logs: %w", err)
			endSpan(err)
			return nil, err
		}
	}

	out := make([]SearchHit, 0, limit)
	for _, selected := range selection {
		candidate := candidates[selected.CandidateRef]
		if candidate.doc == nil {
			continue
		}
		out = append(out, SearchHit{Doc: candidate.doc, Score: candidate.vectorScore})
		if len(out) >= limit {
			break
		}
	}
	endSpan(nil,
		attribute.Int("opskeeper.genai.rag.hit_count", len(out)),
		attribute.Int("opskeeper.genai.rag.raw_hit_count", len(hits)),
		attribute.Int("opskeeper.genai.rag.keyword_candidate_count", len(keywordCandidates)),
	)
	return out, nil
}

type hybridCandidate struct {
	ref         string
	doc         *model.Doc
	vectorRank  int
	keywordRank int
	vectorScore float64
}

func knowledgeCandidateRef(id uint64) string {
	return fmt.Sprintf("knowledge:%d", id)
}

type keywordCandidate struct {
	ref      string
	doc      *model.Doc
	score    int
	priority int
}

func (u *Usecase) keywordCandidates(ctx context.Context, must map[string]any, query, tenantID string, opts SearchOptions) ([]keywordCandidate, error) {
	result, err := u.vec.Scroll(ctx, CollectionName, qdrantx.ScrollOpts{
		MustMatch: must,
		Limit:     1000,
	})
	if err != nil {
		return nil, err
	}
	terms := keywordTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	out := make([]keywordCandidate, 0)
	for _, doc := range dedupeByIDAlias(result.Points, 1000) {
		score := keywordScore(doc, terms)
		if score > 0 {
			out = append(out, keywordCandidate{ref: knowledgeCandidateRef(doc.ID), doc: doc, score: score, priority: 1})
		}
	}
	if opts.DatabaseType != "" && u.recallRepo != nil {
		runbooks, err := u.recallRepo.ListRunbooks(ctx, tenantID, opts.DatabaseType, opts.FaultFingerprint)
		if err != nil {
			return nil, err
		}
		for _, runbook := range runbooks {
			doc := runbookToDoc(runbook)
			if score := keywordScore(doc, terms); score > 0 {
				priority := 1
				if opts.FaultFingerprint != "" {
					priority = 0
				}
				out = append(out, keywordCandidate{ref: runbookCandidateRef(runbook.IncidentID), doc: doc, score: score, priority: priority})
			}
		}
	}
	sort.Slice(out, func(left, right int) bool {
		if out[left].priority != out[right].priority {
			return out[left].priority < out[right].priority
		}
		leftScore := out[left].score
		rightScore := out[right].score
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		if !out[left].doc.UpdatedAt.Equal(out[right].doc.UpdatedAt) {
			return out[left].doc.UpdatedAt.After(out[right].doc.UpdatedAt)
		}
		return out[left].ref < out[right].ref
	})
	return out, nil
}

func runbookCandidateRef(incidentID string) string {
	return "runbook:" + incidentID
}

func runbookToDoc(runbook incidentcontrol.Postmortem) *model.Doc {
	content := strings.Join([]string{
		runbook.Symptom.Summary,
		runbook.Diagnosis.RootCause,
		runbook.FaultFingerprint,
		runbook.TopologyFingerprint,
	}, "\n")
	for _, action := range runbook.EffectiveActions {
		content += "\n" + action.Summary
	}
	return &model.Doc{
		ID:           docID("runbook||" + runbook.TenantID + "||" + runbook.IncidentID),
		SourceType:   "runbook_memory",
		TenantScopes: []string{"tenant:" + runbook.TenantID},
		Title:        "Runbook " + runbook.IncidentID,
		Content:      content,
		Path:         "runbooks/" + runbook.DatabaseType + "/" + runbook.FaultFingerprint,
		Tags:         []string{runbook.DatabaseType, runbook.FaultFingerprint},
		UpdatedAt:    runbook.ConfirmedAt,
	}
}

func keywordTerms(query string) []string {
	return strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func keywordScore(doc *model.Doc, terms []string) int {
	if doc == nil || len(terms) == 0 {
		return 0
	}
	score := 10 * fieldTermHits(doc.Title, terms)
	score += 10 * fieldTermHits(doc.TitleEN, terms)
	score += 5 * fieldTermHits(doc.Path, terms)
	score += 5 * fieldTermHits(strings.Join(doc.Tags, " "), terms)
	score += fieldTermHits(doc.Content, terms)
	return score
}

func fieldTermHits(field string, terms []string) int {
	field = strings.ToLower(field)
	hits := 0
	for _, term := range terms {
		if term == "" {
			continue
		}
		hits += strings.Count(field, term)
	}
	return hits
}

// ----- Repos -----

// CreateRepoInput is the form payload.
type CreateRepoInput struct {
	URL         string
	Branch      string
	Description string
}

// CreateRepo persists a repo registration. Doesn't sync — caller hits
// /v1/knowledge/repos/{id}/sync to pull.
func (u *Usecase) CreateRepo(ctx context.Context, in CreateRepoInput) (*model.Repository, error) {
	url := strings.TrimSpace(in.URL)
	if url == "" {
		return nil, fmt.Errorf("%w: url required", errs.ErrInvalid)
	}
	branch := strings.TrimSpace(in.Branch)
	if branch == "" {
		branch = "main"
	}
	now := time.Now().UTC()
	r := &model.Repository{
		URL:         url,
		Branch:      branch,
		Description: strings.TrimSpace(in.Description),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := u.repo.CreateRepo(ctx, r); err != nil {
		return nil, fmt.Errorf("knowledge: create repo: %w", err)
	}
	return r, nil
}

// GetRepo returns one registered repo, or nil when there is no such row.
// It exists for the delete path: decision 345 needs the URL and the file
// count **before** the row goes, because a chain that records only an
// auto-increment id cannot answer "which repository was that".
func (u *Usecase) GetRepo(ctx context.Context, id uint64) (*model.Repository, error) {
	return u.repo.GetRepo(ctx, id)
}

// ListRepos returns every registered repo.
func (u *Usecase) ListRepos(ctx context.Context) ([]*model.Repository, error) {
	return u.repo.ListRepos(ctx)
}

// DeleteRepo removes the registration + every qdrant point owned by
// it + the on-disk clone. We snapshot the URL BEFORE the row goes so
// the post-delete forensic log records what was nuked (operators have
// complained about silent knowledge loss — without this we can't trace
// which URL the audit-trail repo_id once referred to).
//
// Decision 345 deleted the seed-optout hook that used to hang off this
// function. It existed only for the OPSKEEPER_BUILTIN_VAULT_URL seeding
// path, and ADR-029 removed that path — `main.go` now explicitly says so
// ("the old OPSKEEPER_BUILTIN_VAULT_URL path was removed because it
// registered the vault as a knowledge_repos row and leaked it into
// Repos"). Nothing had registered the hook, so the second reason this
// snapshot existed had quietly stopped existing while its comment
// carried on describing it.
func (u *Usecase) DeleteRepo(ctx context.Context, id uint64) error {
	var (
		deletedURL string
		preRepo    *model.Repository
	)
	if r, gErr := u.repo.GetRepo(ctx, id); gErr == nil && r != nil {
		preRepo = r
		deletedURL = r.URL
	}
	if err := u.vec.DeleteByFilter(ctx, CollectionName, map[string]any{
		"source_type": model.SourceRepo,
		"repo_id":     id,
	}); err != nil {
		u.log.Warn("knowledge: drop repo points failed", slog.Uint64("repo_id", id), slog.Any("err", err))
	}
	if err := u.repo.DeleteRepo(ctx, id); err != nil {
		return err
	}
	dir := u.repoDir(id)
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		u.log.Warn("knowledge: clean repo dir", slog.String("dir", dir), slog.Any("err", err))
	}
	if preRepo != nil {
		u.log.Warn("knowledge: repo deleted",
			slog.Uint64("repo_id", id),
			slog.String("url", deletedURL),
			slog.Int("file_count", preRepo.FileCount))
	}
	return nil
}

// Sync clones (first time) or pulls (subsequent) the repo's branch,
// walks the tree for indexable files, embeds them, and replaces the
// qdrant point set for repo_id=id. Synchronous.
func (u *Usecase) Sync(ctx context.Context, id uint64) (*model.Repository, error) {
	if u.embed == nil {
		return nil, fmt.Errorf("%w: embedder not configured (set OPSKEEPER_EMBEDDING_API_KEY)", errs.ErrNotWiredYet)
	}
	repo, err := u.repo.GetRepo(ctx, id)
	if err != nil {
		return nil, err
	}
	// Repo docs are globally visible. Repo registrations are shared
	// infrastructure — ListRepos/DeleteRepo/Sync are unscoped, so every
	// caller sees and can re-sync every repo. Scoping the *docs* to
	// whoever happened to click sync made repo knowledge appear and
	// vanish depending on the last syncer (A syncs → only A retrieves;
	// B re-syncs → docs flip to B and A loses them). Same rationale as
	// the vault: platform/团队-shared content is global.
	repoScopes := []string{globalTenantScope}
	dir := u.repoDir(id)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, fmt.Errorf("knowledge: mkdir parent: %w", err)
	}

	// The built-in vault ships embedded in the binary, so its "sync" is
	// a local materialize — no git, no network. This is the whole point
	// of [BuiltinVaultURL]: an air-gapped or mainland-China host (where
	// github.com:443 times out) still gets a populated knowledge base.
	// Downstream (scan → chunk → embed → qdrant) is identical to the git
	// path; we only swap out where the raw .md files come from.
	if IsBuiltinVaultURL(repo.URL) {
		if err := u.materializeBuiltinVault(dir); err != nil {
			return u.recordSyncFailure(ctx, repo, fmt.Errorf("materialize built-in vault: %w", err))
		}
	} else {
		// Auth strategy: we never splice a token into the URL — that
		// pattern leaks the token via git argv (visible to anyone who
		// can ps / docker top / read /proc/<pid>/cmdline) and via stderr
		// being captured into last_sync_error. Instead, for github URLs,
		// we look up the operator-configured PAT in system_settings
		// (`git.github_token`) and feed it to git via GIT_ASKPASS. The
		// token only enters the spawned git's environment, never its
		// argv. The askpass script doesn't contain the token either —
		// it `printenv`s a separate env var. Non-github URLs run with no
		// extra env at all.
		gitEnv, askpassCleanup, askpassErr := u.buildGitAuthEnv(ctx, repo.URL)
		if askpassErr != nil {
			return u.recordSyncFailure(ctx, repo, fmt.Errorf("git askpass setup: %w", askpassErr))
		}
		defer askpassCleanup()

		// Storage model: "atomic temp-clone + rename swap".
		//
		// Old model wrote directly into `dir` and tried to recover from
		// partial state in place. Every failure mode produced a different
		// kind of corruption: "destination path already exists" after a
		// half-clone, "ambiguous argument 'origin/<branch>'" after a
		// half-fetch left no remote-tracking ref, missing files after a
		// crash during reset. We kept patching individual cases; the
		// underlying issue is that the published-state directory and the
		// in-progress-write directory were the same path.
		//
		// New model has two paths, both atomic:
		//
		//   Fast (healthy existing .git): fetch + reset --hard FETCH_HEAD
		//     in place. If anything fails we fall through to the slow
		//     path — the worst case is "wasted bandwidth", never corruption.
		//
		//   Slow / repair: clone --depth=1 into a sibling tmp dir, then on
		//     success rm -rf <dir> and os.Rename(tmp, dir). os.Rename is
		//     atomic within the same filesystem (and our parent is one
		//     dir, so it's always the same FS); a crash mid-sync leaves
		//     either the OLD dir (rename hadn't run yet) or the NEW dir
		//     (rename completed) — never a hybrid. The user-visible state
		//     never goes through "destination already exists" because the
		//     clone target is a fresh random path.
		// Sweep stale .tmp-clone-* siblings before either path runs. They
		// only exist when a previous sync was killed between the clone-
		// succeeded and rename-completed instants — extremely rare, but
		// without this they'd accumulate one per failed run and eat disk.
		purgeStaleCloneTmps(dir)
		if !u.syncFastPath(ctx, dir, gitEnv, repo.Branch) {
			if out, err := u.syncAtomicReplace(ctx, dir, gitEnv, repo.URL, repo.Branch); err != nil {
				// Store the RAW git output (locale-neutral English from git
				// itself) — NOT a pre-localized string. The SPA classifies +
				// localizes it for display (see gitErrorHint in
				// KnowledgeRepos.tsx) so the message follows the UI locale.
				// Storing a Chinese annotation here made English-mode show
				// Chinese (the stored string is fixed at sync time).
				return u.recordSyncFailure(ctx, repo, fmt.Errorf("git clone failed: %v\n%s", err, strings.TrimSpace(out)))
			}
		}
	}

	files, err := scanRepoFiles(dir)
	if err != nil {
		return u.recordSyncFailure(ctx, repo, fmt.Errorf("scan files: %w", err))
	}

	// Expand each scanned file into 1+ chunks of ≤chunkChars runes each.
	// Small docs become a single chunk (identical to the pre-chunking
	// behaviour); large docs (RFCs, long kernel admin guides) become N
	// chunks so semantic search can hit content past the first ~2500
	// chars instead of being stuck on the lead. Each chunk becomes its
	// own qdrant point with its own embedding; payload dedup ('parent_url'
	// + 'chunk_index') lets listings collapse to one entry per file.
	type chunkRef struct {
		file       *scannedFile
		chunkIndex int
		chunkTotal int
		body       string
	}
	now := time.Now().UTC()
	chunks := make([]chunkRef, 0, len(files))
	for i := range files {
		parts := splitForChunks(files[i].Content)
		for j, p := range parts {
			// Chunk 0 prepends the title so the embedding picks up the
			// "what is this doc" signal — same as the pre-chunking
			// behaviour for short docs. Chunks beyond 0 carry only
			// their slice (the title would dominate the vector
			// otherwise).
			var body string
			if j == 0 {
				body = files[i].Title + "\n\n" + p
			} else {
				body = p
			}
			chunks = append(chunks, chunkRef{
				file:       &files[i],
				chunkIndex: j,
				chunkTotal: len(parts),
				body:       body,
			})
		}
	}

	// Embed EVERY chunk before dropping the previously published set.
	//
	// The old order was delete-prior → embed → upsert, so one transient
	// embedder failure (provider hiccup, rate limit, oversized-batch
	// rejection) deleted the whole repo corpus and never wrote the
	// replacement — the "RAG 又没了" report where search returned
	// nothing although the vectors had been fine seconds earlier. With
	// all vectors computed up front, an embed failure aborts before
	// anything is dropped and the previous set stays searchable. The
	// residual window is a qdrant failure mid-upsert (local, rare, and
	// repaired by the next sync).
	//
	// Embed in batches of 32 — keeps each request well under the
	// embedding provider's per-request input cap (Zhipu = 3072 tokens
	// per single input; we cap each input to chunkChars=2500 runes
	// before truncateForEmbedding clips further if needed).
	const batch = 32
	vectors := make([][]float32, 0, len(chunks))
	for i := 0; i < len(chunks); i += batch {
		end := i + batch
		if end > len(chunks) {
			end = len(chunks)
		}
		texts := make([]string, 0, end-i)
		for _, c := range chunks[i:end] {
			texts = append(texts, truncateForEmbedding(c.body))
		}
		batchVectors, err := u.embed.Embed(ctx, texts)
		if err != nil {
			return u.recordSyncFailure(ctx, repo, fmt.Errorf("embed batch %d: %w", i, err))
		}
		vectors = append(vectors, batchVectors...)
	}

	// Vectors in hand — now the published set can be replaced safely.
	if err := u.vec.DeleteByFilter(ctx, CollectionName, map[string]any{
		"source_type": model.SourceRepo,
		"repo_id":     id,
	}); err != nil {
		return u.recordSyncFailure(ctx, repo, fmt.Errorf("drop prior: %w", err))
	}

	for i := 0; i < len(chunks); i += batch {
		end := i + batch
		if end > len(chunks) {
			end = len(chunks)
		}
		points := make([]qdrantx.Point, 0, end-i)
		for j := i; j < end; j++ {
			c := chunks[j]
			// Path: derive from URL directory so the SPA folder-tree
			// view groups docs by their repo subdirectory (concepts/,
			// reference/external/dns/, etc.). Repo docs never set Path
			// explicitly — without this derivation the folder tree was
			// silently empty for the entire repo corpus.
			folder := filepath.Dir(c.file.URL)
			if folder == "." || folder == "/" {
				folder = ""
			}
			pt := repoChunkPoint(repo.ID, c.file.URL, c.chunkIndex, c.chunkTotal, vectors[j], model.Doc{
				SourceType:   model.SourceRepo,
				TenantScopes: repoScopes,
				RepoID:       ptrU64(repo.ID),
				URL:          c.file.URL,
				Title:        c.file.Title,
				Content:      c.file.Content,
				Path:         folder,
				CreatedAt:    now,
				UpdatedAt:    now,
			}, c.body)
			points = append(points, pt)
		}
		if err := u.vec.Upsert(ctx, CollectionName, points); err != nil {
			return u.recordSyncFailure(ctx, repo, fmt.Errorf("upsert batch %d: %w", i, err))
		}
	}
	// file_count tracks distinct files (the operator-facing "how many
	// docs are in this repo"), not the chunk fanout count.
	indexed := len(files)
	if err := u.repo.UpdateRepoSync(ctx, id, indexed, ""); err != nil {
		return nil, fmt.Errorf("knowledge: update sync state: %w", err)
	}
	return u.repo.GetRepo(ctx, id)
}

// SyncBuiltinVault refreshes the platform vault in qdrant under
// source_type="vault". The vault is NOT a knowledge_repos row — it's
// platform-shipped content — so this path never touches the repos table and
// the vault never shows up in the 代码仓库 / Repos list. The 内置知识库 sync
// button (and the boot seed) call straight here.
//
// Source of truth is the go:embed snapshot (see builtin_vault.go): the vault
// ships in the binary, gets materialized to disk, then scanned → chunked →
// embedded → upserted. There is deliberately no runtime clone — the upstream
// repo the old cloud path pointed at was a never-filled placeholder, so every
// "cloud" attempt burned retry timeouts and then fell back to this same
// snapshot anyway; only the UI message differed.
//
// Idempotent: re-syncs (boot, button) replace the prior set cleanly — no repo
// row to leak, no duplicate docs. Returns (fileCount, source, err) with
// source fixed to "embedded"; the field stays in the API shape for the audit
// log and SPA.
func (u *Usecase) SyncBuiltinVault(ctx context.Context) (int, string, error) {
	if u.embed == nil {
		return 0, "", fmt.Errorf("%w: embedder not configured", errs.ErrNotWiredYet)
	}
	dir := filepath.Join(u.cloneDir, "builtin-vault")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return 0, "", fmt.Errorf("knowledge: mkdir parent: %w", err)
	}

	const source = "embedded"
	if err := u.materializeBuiltinVault(dir); err != nil {
		return 0, "", fmt.Errorf("knowledge: materialize built-in vault: %w", err)
	}
	files, err := scanRepoFiles(dir)
	if err != nil {
		return 0, "", fmt.Errorf("knowledge: scan vault files: %w", err)
	}
	now := time.Now().UTC()
	type chunkRef struct {
		file               *scannedFile
		chunkIndex, chunkN int
		body               string
	}
	chunks := make([]chunkRef, 0, len(files))
	for i := range files {
		parts := splitForChunks(files[i].Content)
		for j, p := range parts {
			body := p
			if j == 0 {
				body = files[i].Title + "\n\n" + p
			}
			chunks = append(chunks, chunkRef{file: &files[i], chunkIndex: j, chunkN: len(parts), body: body})
		}
	}
	// Embed everything before dropping the prior vault set (same rationale as
	// the repo path): a transient embedder failure must not leave the
	// knowledge base empty.
	const batch = 32
	vectors := make([][]float32, 0, len(chunks))
	for i := 0; i < len(chunks); i += batch {
		end := i + batch
		if end > len(chunks) {
			end = len(chunks)
		}
		texts := make([]string, 0, end-i)
		for _, c := range chunks[i:end] {
			texts = append(texts, truncateForEmbedding(c.body))
		}
		batchVectors, err := u.embed.Embed(ctx, texts)
		if err != nil {
			return 0, "", fmt.Errorf("knowledge: embed vault batch %d: %w", i, err)
		}
		vectors = append(vectors, batchVectors...)
	}

	if err := u.vec.DeleteByFilter(ctx, CollectionName, map[string]any{
		"source_type": model.SourceVault,
	}); err != nil {
		return 0, "", fmt.Errorf("knowledge: drop prior vault: %w", err)
	}

	for i := 0; i < len(chunks); i += batch {
		end := i + batch
		if end > len(chunks) {
			end = len(chunks)
		}
		points := make([]qdrantx.Point, 0, end-i)
		for j := i; j < end; j++ {
			c := chunks[j]
			folder := filepath.Dir(c.file.URL)
			if folder == "." || folder == "/" {
				folder = ""
			}
			points = append(points, vaultChunkPoint(c.file.URL, c.chunkIndex, c.chunkN, vectors[j], model.Doc{
				SourceType: model.SourceVault,
				URL:        c.file.URL,
				Title:      c.file.Title,
				Content:    c.file.Content,
				Path:       folder,
				CreatedAt:  now,
				UpdatedAt:  now,
			}, c.body))
		}
		if err := u.vec.Upsert(ctx, CollectionName, points); err != nil {
			return 0, "", fmt.Errorf("knowledge: upsert vault batch %d: %w", i, err)
		}
	}
	u.log.Info("knowledge: built-in vault synced",
		slog.String("source", source), slog.Int("file_count", len(files)))
	return len(files), source, nil
}

// HasVaultDocs reports whether any source_type=vault points already exist.
// Lets boot skip a redundant re-embed when the vault is already indexed.
func (u *Usecase) HasVaultDocs(ctx context.Context) bool {
	res, err := u.vec.Scroll(ctx, CollectionName, qdrantx.ScrollOpts{
		MustMatch: map[string]any{"source_type": model.SourceVault},
		Limit:     1,
	})
	return err == nil && res != nil && len(res.Points) > 0
}

// MigrateBuiltinVaultTenantScope marks every legacy built-in-vault point as
// globally visible. Pre-scope versions did not write tenant_scopes; the
// signed-tenant search filter would safely hide them. Updating payload only
// avoids a full re-embed and is idempotent.
func (u *Usecase) MigrateBuiltinVaultTenantScope(ctx context.Context) (bool, error) {
	migrator, ok := u.vec.(payloadMigrator)
	if !ok {
		return false, nil
	}

	offset := (*uint64)(nil)
	for {
		result, err := u.vec.Scroll(ctx, CollectionName, qdrantx.ScrollOpts{
			MustMatch: map[string]any{"source_type": model.SourceVault},
			Limit:     256,
			Offset:    offset,
		})
		if err != nil {
			return false, fmt.Errorf("knowledge: scroll vault for tenant scope migration: %w", err)
		}
		for _, point := range result.Points {
			if !payloadHasGlobalScope(point.Payload) {
				if err := migrator.SetPayloadByFilter(ctx, CollectionName,
					map[string]any{"tenant_scopes": []string{globalTenantScope}},
					map[string]any{"source_type": model.SourceVault},
				); err != nil {
					return true, fmt.Errorf("knowledge: migrate vault tenant scopes: %w", err)
				}
				return true, nil
			}
		}
		if result.NextOffset == nil {
			return false, nil
		}
		offset = result.NextOffset
	}
}

func payloadHasGlobalScope(payload map[string]any) bool {
	rawScopes, ok := payload["tenant_scopes"].([]any)
	if !ok {
		return false
	}
	for _, rawScope := range rawScopes {
		if scope, ok := rawScope.(string); ok && scope == globalTenantScope {
			return true
		}
	}
	return false
}

// PurgeBuiltinVaultRepo deletes any legacy knowledge_repos row for the
// embedded vault (url == builtin://vault) plus its source_type=repo points.
// Pre-refactor installs seeded the vault AS a repo row; after the move to
// source_type=vault that row would linger in the Repos list. This migration
// runs at boot. It calls the store directly (not DeleteRepo) so the
// vault_seed_optout delete-hook does NOT fire — purging the legacy row is a
// migration, not an operator opting out. Returns true if a row was purged.
func (u *Usecase) PurgeBuiltinVaultRepo(ctx context.Context) (bool, error) {
	repos, err := u.repo.ListRepos(ctx)
	if err != nil {
		return false, err
	}
	purged := false
	for _, r := range repos {
		if !IsBuiltinVaultURL(r.URL) {
			continue
		}
		if dErr := u.vec.DeleteByFilter(ctx, CollectionName, map[string]any{
			"source_type": model.SourceRepo,
			"repo_id":     r.ID,
		}); dErr != nil {
			u.log.Warn("knowledge: purge legacy vault points", slog.Uint64("repo_id", r.ID), slog.Any("err", dErr))
		}
		if dErr := u.repo.DeleteRepo(ctx, r.ID); dErr != nil {
			return purged, fmt.Errorf("knowledge: delete legacy vault repo %d: %w", r.ID, dErr)
		}
		_ = os.RemoveAll(u.repoDir(r.ID))
		u.log.Info("knowledge: purged legacy built-in vault repo row", slog.Uint64("repo_id", r.ID), slog.String("url", r.URL))
		purged = true
	}
	return purged, nil
}

// ----- internal helpers -----

// embeddingMaxChars caps each input fed to the embedding provider.
// Picked empirically:
//   - Zhipu's embedding-3 enforces 3072 tokens / single input. For
//     CJK content the BPE tokenizer can output ≥1 token per char
//     (sometimes 1.2 for less-common glyphs), so a "1.5 char/token"
//     assumption breaks on dense Chinese pages. Empirically 2500
//     chars covers ~95% of our seed corpus without the model 1210
//     rejecting; the long-tail few docs that still exceed get clipped.
//   - OpenAI's text-embedding-3-* allows 8191 tokens / input, so this
//     cap is conservative there but harmless (still indexes the lead
//     of every doc).
//
// Truncation for embedding intentionally clips suffix (not summarise).
// The semantic vector still captures the document's lead, which is
// where titles + summaries live in our knowledge corpus; for surfacing
// the full body the SPA renders from the qdrant-stored `content`
// payload which is independent of the embedding input.
const embeddingMaxChars = 2500

// truncateForEmbedding returns s clipped to embeddingMaxChars characters.
// Counts runes (not bytes) so multi-byte CJK characters are not
// over-counted.
func truncateForEmbedding(s string) string {
	count := 0
	for i := range s {
		if count >= embeddingMaxChars {
			return s[:i]
		}
		count++
	}
	return s
}

func (u *Usecase) upsertDoc(ctx context.Context, d *model.Doc) error {
	ctx, endSpan := otelgenai.StartRAG(ctx, "upsert", CollectionName)
	vecs, err := u.embed.Embed(ctx, []string{truncateForEmbedding(d.Title + "\n\n" + d.Content)})
	if err != nil {
		err = fmt.Errorf("knowledge: embed: %w", err)
		endSpan(err)
		return err
	}
	if len(vecs) != 1 {
		err = fmt.Errorf("knowledge: embedder returned %d vectors", len(vecs))
		endSpan(err)
		return err
	}
	pt := qdrantx.Point{
		ID:     d.ID,
		Vector: vecs[0],
		Payload: map[string]any{
			"source_type":   d.SourceType,
			"tenant_scopes": d.TenantScopes,
			"title":         d.Title,
			"title_en":      d.TitleEN,
			"content":       d.Content,
			"url":           d.URL,
			"path":          d.Path,
			// chunk_index/chunk_total were added when repo Sync
			// started splitting big files; manual docs are always
			// single-chunk but ListDocs / ListPaths now require the
			// field to dedupe (MustMatch chunk_index=0). Setting them
			// here keeps the manual-doc path consistent with
			// repoChunkPoint — without this every manual doc gets
			// silently filtered out of the SPA's knowledge listing
			// (reported as "RAG 又没了" — vectors fine, UI blank).
			"chunk_index":   0,
			"chunk_total":   1,
			"path_prefixes": pathPrefixes(d.Path),
			"tags":          d.Tags, // qdrant accepts JSON array verbatim
			"created_at":    d.CreatedAt.Format(time.RFC3339),
			"updated_at":    d.UpdatedAt.Format(time.RFC3339),
			"id_alias":      d.ID, // helper field (legacy; not indexed)
		},
	}
	if d.RepoID != nil {
		pt.Payload["repo_id"] = *d.RepoID
	}
	if err := u.vec.Upsert(ctx, CollectionName, []qdrantx.Point{pt}); err != nil {
		err = fmt.Errorf("knowledge: upsert: %w", err)
		endSpan(err)
		return err
	}
	endSpan(nil, attribute.Int("opskeeper.genai.rag.point_count", 1))
	return nil
}

// recordSyncFailure logs the error and records it WITHOUT clobbering the
// repo's published state. A failure typically happens before the point-set
// swap, so the previously indexed docs are still live — file_count and
// last_synced_at must keep describing the last successful sync. Writing
// 0 + CURRENT_TIMESTAMP here made a failed clone read "文件 0 · 刚刚同步过"
// while the old docs were still searchable.
func (u *Usecase) recordSyncFailure(ctx context.Context, repo *model.Repository, syncErr error) (*model.Repository, error) {
	u.log.Warn("knowledge: sync failed", slog.Uint64("repo_id", repo.ID), slog.Any("err", syncErr))
	_ = u.repo.MarkRepoSyncFailed(ctx, repo.ID, syncErr.Error())
	return nil, syncErr
}

func (u *Usecase) repoDir(id uint64) string {
	return filepath.Join(u.cloneDir, fmt.Sprintf("%d", id))
}

// runGit invokes `git` with the given args. extraEnv (non-nil only on
// authenticated clones/fetches) carries the GIT_ASKPASS path + the
// token env var the askpass script reads — see buildGitAuthEnv. We
// always append GIT_TERMINAL_PROMPT=0 so a missing askpass / wrong
// token fails fast instead of hanging waiting for stdin.
func runGit(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	// CRITICAL (mainland↔github stall fix): on ctx timeout CommandContext
	// kills `git`, but git's child `ssh`/`git-remote-https` inherits the
	// output pipe and keeps it open on a stalled network read — so
	// CombinedOutput() would block on EOF long past the 60s deadline (we
	// observed clone requests hanging >280s). WaitDelay force-closes the
	// inherited pipes shortly after the kill so runGit actually returns,
	// surfacing a "signal: killed" error that isTransientGitErr retries.
	cmd.WaitDelay = 10 * time.Second
	if dir != "" {
		cmd.Dir = dir
	}
	env := append([]string{}, os.Environ()...)
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	env = append(env, extraEnv...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runGitWithRetry wraps runGit with up to 3 attempts + exponential
// backoff (5s, 15s). Retries only on network-shaped failures:
// SSL_read / unexpected eof / signal: killed / connection refused /
// timed out — these are the symptom strings emitted by libcurl/openssl
// when the test-env-to-github.com path flaps (observed repeatedly in
// May 2026). Auth failures, "repository not found", bad branch names
// — none of those retry. The function returns the LAST attempt's
// (out, err); callers' annotateGitError still gives the right hint.
// syncFastPath tries fetch + reset --hard FETCH_HEAD in the existing
// working tree. Returns true on success; on ANY failure (missing .git,
// bad ref, network blip, working tree corruption) returns false and
// the caller falls through to syncAtomicReplace which rebuilds from
// scratch into a tmp dir. We never let a fast-path failure surface to
// the operator — the slow path covers it transparently.
func (u *Usecase) syncFastPath(ctx context.Context, dir string, gitEnv []string, branch string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	if _, err := runGitWithRetry(ctx, dir, gitEnv, "fetch", "--depth=1", "origin", branch); err != nil {
		return false
	}
	// FETCH_HEAD is what git fetch always writes; using it sidesteps the
	// missing refs/remotes/origin/<branch> ref that shallow clones in
	// some git versions don't maintain.
	if _, err := runGit(ctx, dir, nil, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return false
	}
	// Drop anything untracked / ignored that lingered from a prior
	// failed sync; otherwise scanRepoFiles indexes stale content.
	_, _ = runGit(ctx, dir, nil, "clean", "-fdx")
	return true
}

// syncAtomicReplace performs the full repair path: clone into a sibling
// tmp dir, then on success rm -rf <dir> and os.Rename(tmp, dir). The
// rename is atomic within the same filesystem (and the parent dir is
// always the same FS), so the visible state always points at either a
// complete old snapshot or a complete new snapshot — never a half
// state. The retry loop covers transient network errors; each retry
// gets a fresh tmp dir so "destination not empty" is physically
// impossible.
func (u *Usecase) syncAtomicReplace(ctx context.Context, dir string, gitEnv []string, repoURL, branch string) (string, error) {
	delays := []time.Duration{0, 5 * time.Second, 15 * time.Second}
	var lastOut string
	var lastErr error
	for _, delay := range delays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return lastOut, ctx.Err()
			case <-time.After(delay):
			}
		}
		tmp, err := newCloneTmpDir(dir)
		if err != nil {
			return "", fmt.Errorf("make tmp clone dir: %w", err)
		}
		// Clone into the fresh tmp. On failure clean tmp and try again
		// (or fall out of the loop) without ever touching `dir`.
		// http.lowSpeedLimit/Time: abort an HTTPS clone that drops below
		// 1 KB/s for 20s (the libcurl analogue of the ssh ServerAlive
		// teardown) so a stalled transfer becomes a retryable error rather
		// than a hang. Harmless on the ssh path.
		out, cloneErr := runGit(ctx, "", gitEnv,
			"-c", "http.lowSpeedLimit=1024", "-c", "http.lowSpeedTime=20",
			"clone", "--depth=1", "--branch", branch, repoURL, tmp)
		if cloneErr != nil {
			_ = os.RemoveAll(tmp)
			lastOut, lastErr = out, cloneErr
			// Classify on output AND the error string: a WaitDelay/ctx kill
			// surfaces "signal: killed" / "context deadline exceeded" in the
			// error, not always in the captured output.
			if !isTransientGitErr(out + " " + cloneErr.Error()) {
				return out, cloneErr
			}
			continue
		}
		// Clone succeeded — swap in atomically. If the rename target
		// already exists we have to nuke it first (os.Rename on POSIX
		// won't overwrite a non-empty dir). If RemoveAll fails for
		// some reason we still keep the tmp around so the next sync
		// can attempt the swap again.
		if err := os.RemoveAll(dir); err != nil {
			return out, fmt.Errorf("remove stale published dir: %w", err)
		}
		if err := os.Rename(tmp, dir); err != nil {
			// Best-effort cleanup of the orphaned tmp on rename failure.
			_ = os.RemoveAll(tmp)
			return out, fmt.Errorf("atomic rename %s → %s: %w", tmp, dir, err)
		}
		return out, nil
	}
	return lastOut, lastErr
}

// purgeStaleCloneTmps deletes any leftover .tmp-clone-<base>-* siblings
// of dir. A crash between clone-success and the rename swap could leave
// one behind; without periodic cleanup they'd pile up across syncs.
// Best-effort — errors are silently ignored (the sweep is purely
// cosmetic; the active sync's tmp dir lives in a different name).
func purgeStaleCloneTmps(dir string) {
	parent := filepath.Dir(dir)
	prefix := ".tmp-clone-" + filepath.Base(dir) + "-"
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			_ = os.RemoveAll(filepath.Join(parent, e.Name()))
		}
	}
}

// newCloneTmpDir creates a fresh sibling of `dir` to clone into. Sibling
// (not /tmp) is intentional: os.Rename is atomic only within a single
// filesystem, and the parent dir of `dir` is always the same FS we want
// the final published copy to live on. Prefix `.tmp-clone-` makes the
// in-progress state self-describing in any operator `ls` they run.
func newCloneTmpDir(dir string) (string, error) {
	parent := filepath.Dir(dir)
	base := filepath.Base(dir)
	// os.MkdirTemp picks a fresh suffix; the leading dot keeps the tmp
	// out of casual `ls` (matches the convention git itself uses for
	// in-flight refs / pack files).
	return os.MkdirTemp(parent, ".tmp-clone-"+base+"-")
}

func runGitWithRetry(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	delays := []time.Duration{0, 5 * time.Second, 15 * time.Second}
	var lastOut string
	var lastErr error
	for attempt, delay := range delays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return lastOut, ctx.Err()
			case <-time.After(delay):
			}
		}
		out, err := runGit(ctx, dir, extraEnv, args...)
		if err == nil {
			return out, nil
		}
		lastOut, lastErr = out, err
		if !isTransientGitErr(out) {
			// Non-network failure — don't waste retries on auth /
			// not-found / branch-missing. Return immediately.
			return out, err
		}
		_ = attempt
	}
	return lastOut, lastErr
}

// isTransientGitErr matches the symptom strings emitted when git's
// underlying libcurl / openssl path flakes. Conservative — only
// matches the patterns we have actually observed; everything else
// (auth, 404, bad-ref) is fatal and not retried.
func isTransientGitErr(combinedOutput string) bool {
	s := strings.ToLower(combinedOutput)
	patterns := []string{
		"ssl_read",
		"unexpected eof",
		"early eof",
		"signal: killed",
		"connection refused",
		"connection timed out",
		"connection reset",
		"could not resolve host",
		"name or service not known",
		"rpc failed",
		"the remote end hung up",
		// ctx-timeout / WaitDelay kill + ssh keepalive teardown signatures
		// (mainland↔github stall): retry rather than fail the whole sync.
		"context deadline exceeded",
		"deadline exceeded",
		"waitdelay",
		"timed out",
		"i/o timeout",
		"operation timed out",
		"broken pipe",
		"timeout, server", // ssh ServerAlive teardown
		"client_loop",     // ssh "client_loop: send disconnect: ..."
	}
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// ----- repo file scan -----

// indexableExts is the default allow-list for repo ingest. Knowledge
// content is prose-shaped — markdown / reStructuredText / plain text —
// so we deliberately exclude .yaml / .yml / .toml / .json. Those are
// configuration / data formats that pollute RAG with non-prose noise
// when a code repo gets registered as a knowledge source (e.g. a
// repo's _data/, .github/, .gitee/ trees full of CI config). When a
// future "code repo" mode lands (HLD TBD), it'll widen the allow-list
// behind an explicit kind=code flag.
var indexableExts = map[string]bool{
	".md":  true,
	".txt": true,
	".rst": true,
}

// skipDirNames are directories that scanRepoFiles drops without
// descent. Two categories:
//
//  1. Tooling / build artifacts that have no prose content:
//     .git, vendor, node_modules, dist, build, .build, target,
//     __pycache__
//  2. Static-site / blog scaffolding that pollutes RAG with
//     templates / data / posts irrelevant to operational knowledge:
//     _data, _posts, _drafts, _layouts, _includes, _sass, _site
//
// Any directory whose name starts with "." is also skipped (covers
// .github, .gitee, .vscode, .idea, .husky, .circleci, .devcontainer
// without enumeration). This is the discovery from the v0.7.49 gitee
// self-test where _posts/ alone contributed 134/141 noise files.
var skipDirNames = map[string]bool{
	".git":         true,
	"vendor":       true,
	"node_modules": true,
	"dist":         true,
	"build":        true,
	".build":       true,
	"target":       true,
	"__pycache__":  true,
	"_data":        true,
	"_posts":       true,
	"_drafts":      true,
	"_layouts":     true,
	"_includes":    true,
	"_sass":        true,
	"_site":        true,
	"_assets":      true,
}

const (
	// maxFileBytes caps how large a single file can be before scanRepoFiles
	// drops it silently. 2 MB was picked to cover the long-tail of canonical
	// references (RFC 8446 = 330 KB, RFC 9110 = 492 KB, large kernel admin
	// guides) while still bounding pathological commits (full novels in
	// docs/, generated SQL dumps misfiled as .md, etc.).
	maxFileBytes = 2 * 1024 * 1024
	maxFiles     = 2000

	// chunkChars / chunkOverlap drive how a single file gets split into
	// overlapping pieces before embedding. A document longer than chunkChars
	// runes becomes N chunks, each producing its own qdrant point with a
	// dedicated embedding — so semantic search can hit content in the middle
	// of a 500 KB RFC, not just the lead. Overlap preserves context across
	// chunk boundaries so a sentence isn't bisected at the cut.
	chunkChars   = 2500
	chunkOverlap = 250
	// maxChunksPerFile prevents one pathologically large file from
	// monopolising the embed budget. With chunkChars=2500 and overlap=250,
	// stride is 2250 chars/chunk → 256 chunks ≈ 560 KB of content; the
	// largest sane reference (RFC 9110 = 492 KB) fits inside.
	maxChunksPerFile = 256
)

type scannedFile struct {
	URL     string
	Title   string
	Content string
}

// extractDocTitle pulls a human-readable title out of a repo-sourced
// document. Earlier versions stored the relative file path as the
// title, which surfaced as ugly raw paths like
// `reference/external/observability/bcccce42-…md` in the knowledge
// list. Resolution order matches what authors expect:
//
//  1. YAML front-matter `title:` (Hugo / Jekyll / our own playbooks)
//  2. First markdown `# H1`, skipping front-matter
//  3. Filename minus extension and minus an optional 8-hex-digit prefix
//     (bulk-imports use `<hash>-<slug>.md`; the hash is noise to humans)
//
// Always returns a non-empty string — the path fallback at the end
// keeps row uniqueness when none of the heuristics match.
func extractDocTitle(body, relPath string) string {
	s := body
	// (1) YAML front-matter block at the top: `--- ... ---` with a
	// `title:` line inside.
	if strings.HasPrefix(strings.TrimSpace(s), "---") {
		// Skip the opening fence and locate the closing one.
		lines := strings.SplitN(s, "\n", 200)
		if len(lines) > 1 && strings.TrimSpace(lines[0]) == "---" {
			for i := 1; i < len(lines); i++ {
				ln := strings.TrimSpace(lines[i])
				if ln == "---" {
					break
				}
				if strings.HasPrefix(ln, "title:") {
					t := strings.TrimSpace(strings.TrimPrefix(ln, "title:"))
					t = strings.Trim(t, `"'`)
					if t != "" {
						return t
					}
					break
				}
			}
		}
	}
	// (2) First `# H1` line in the body. Skip any front-matter block we
	// already inspected.
	scanFrom := s
	if strings.HasPrefix(strings.TrimSpace(s), "---") {
		if idx := strings.Index(s[3:], "\n---"); idx >= 0 {
			scanFrom = s[3+idx+4:]
		}
	}
	for _, ln := range strings.SplitN(scanFrom, "\n", 200) {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "# ") {
			t := strings.TrimSpace(strings.TrimPrefix(ln, "#"))
			if t != "" {
				return t
			}
		}
	}
	// (3) Filename fallback. Strip extension + an `<8-hex>-` prefix that
	// our bulk import tool prepends to avoid collisions.
	base := filepath.Base(relPath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if len(base) > 9 && base[8] == '-' && isHex8(base[:8]) {
		base = base[9:]
	}
	if base != "" {
		return base
	}
	return relPath
}

// isHex8 reports whether s is exactly 8 lower-hex characters. Used by
// extractDocTitle to detect the `<8-hex>-` filename prefix our bulk
// importer prepends.
func isHex8(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func scanRepoFiles(root string) ([]scannedFile, error) {
	var out []scannedFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			// Drop any dot-directory (.github, .gitee, .vscode,
			// .idea, .husky, .circleci, .devcontainer, ...) and the
			// curated skipDirNames list above. Root "." stays in.
			if name != "." && strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			if skipDirNames[name] {
				return fs.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if !indexableExts[ext] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > maxFileBytes {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, scannedFile{URL: rel, Title: extractDocTitle(string(body), rel), Content: string(body)})
		if len(out) >= maxFiles {
			return fs.SkipAll
		}
		return nil
	})
	return out, err
}

// ----- payload <-> Doc -----

func payloadToDoc(id uint64, p map[string]any) *model.Doc {
	d := &model.Doc{ID: id}
	if v, ok := p["source_type"].(string); ok {
		d.SourceType = v
	}
	if raw, ok := p["tenant_scopes"].([]any); ok {
		scopes := make([]string, 0, len(raw))
		for _, item := range raw {
			if scope, ok := item.(string); ok && scope != "" {
				scopes = append(scopes, scope)
			}
		}
		if len(scopes) > 0 {
			d.TenantScopes = scopes
		}
	}
	if v, ok := p["title"].(string); ok {
		d.Title = v
	}
	if v, ok := p["title_en"].(string); ok {
		d.TitleEN = v
	}
	if v, ok := p["content"].(string); ok {
		d.Content = v
	}
	if v, ok := p["url"].(string); ok {
		d.URL = v
	}
	if v, ok := p["repo_id"].(float64); ok {
		x := uint64(v)
		d.RepoID = &x
	}
	if v, ok := p["path"].(string); ok {
		d.Path = v
	}
	if raw, ok := p["tags"].([]any); ok {
		tags := make([]string, 0, len(raw))
		for _, item := range raw {
			if s, ok := item.(string); ok && s != "" {
				tags = append(tags, s)
			}
		}
		if len(tags) > 0 {
			d.Tags = tags
		}
	}
	if s, ok := p["created_at"].(string); ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			d.CreatedAt = t
		}
	}
	if s, ok := p["updated_at"].(string); ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			d.UpdatedAt = t
		}
	}
	return d
}

func ptrU64(v uint64) *uint64 { return &v }

// ID coining: stable across re-imports so a re-sync overwrites the
// old qdrant point instead of creating a new one. md5(scope||url) →
// take 8 bytes → uint64. Probability of collision at ~10^9 docs is
// still ~10^-3 — acceptable for our scale.
func manualDocID(tenantID, title string) uint64 {
	return docID("manual||" + tenantID + "||" + title)
}
func repoDocID(repoID uint64, url string) uint64 {
	return docID(fmt.Sprintf("repo||%d||%s", repoID, url))
}

// repoChunkDocID returns the qdrant point ID for one chunk of a repo doc.
// Chunk 0 keeps the original `repoDocID(repoID, url)` so existing point IDs
// (and any operator-saved deep links) survive the move to chunking; higher
// chunk indices get a derived key. Within a single repo+url the IDs are
// guaranteed distinct because the chunk suffix is non-empty for n>0.
func repoChunkDocID(repoID uint64, url string, chunkIndex int) uint64 {
	if chunkIndex == 0 {
		return repoDocID(repoID, url)
	}
	return docID(fmt.Sprintf("repo||%d||%s||chunk-%d", repoID, url, chunkIndex))
}

// vaultChunkDocID is the repo-free analogue for embedded-vault chunks —
// keyed only on url+chunk (the vault has no repo_id). Stable across syncs
// so a re-sync overwrites in place.
func vaultChunkDocID(url string, chunkIndex int) uint64 {
	if chunkIndex == 0 {
		return docID("vault||" + url)
	}
	return docID(fmt.Sprintf("vault||%s||chunk-%d", url, chunkIndex))
}

// vaultChunkDocID / uploadChunkDocID share the repo-free shape: keyed on a
// scope tag + url + chunk so a re-sync / re-upload overwrites in place.
func uploadChunkDocID(scope, url string, chunkIndex int) uint64 {
	if chunkIndex == 0 {
		return docID("upload||" + scope + "||" + url)
	}
	return docID(fmt.Sprintf("upload||%s||%s||chunk-%d", scope, url, chunkIndex))
}

// uploadChunkPoint builds a qdrant point for one chunk of an org-uploaded
// doc (ADR-028): source_type=upload, no repo_id, id_alias = head chunk id.
func uploadChunkPoint(url string, chunkIndex, chunkTotal int, vec []float32, d model.Doc, chunkContent string) qdrantx.Point {
	pt := vaultChunkPoint(url, chunkIndex, chunkTotal, vec, d, chunkContent)
	scope := uploadScope(d)
	pt.ID = uploadChunkDocID(scope, url, chunkIndex)
	pt.Payload["source_type"] = model.SourceUpload
	pt.Payload["tenant_scopes"] = d.TenantScopes
	pt.Payload["id_alias"] = uploadChunkDocID(scope, url, 0)
	return pt
}

// vaultChunkPoint builds a qdrant point for one chunk of an embedded-vault
// doc: source_type=vault, no repo_id, id_alias = the head chunk's id so
// ListDocs collapses chunks to one row (same scheme as repoChunkPoint).
func vaultChunkPoint(url string, chunkIndex, chunkTotal int, vec []float32, d model.Doc, chunkContent string) qdrantx.Point {
	id := vaultChunkDocID(url, chunkIndex)
	parentID := vaultChunkDocID(url, 0)
	contentForPayload := chunkContent
	if chunkIndex == 0 {
		contentForPayload = d.Content
	}
	return qdrantx.Point{
		ID:     id,
		Vector: vec,
		Payload: map[string]any{
			"source_type":   model.SourceVault,
			"tenant_scopes": []string{globalTenantScope},
			"title":         d.Title,
			"title_en":      d.TitleEN,
			"content":       contentForPayload,
			"url":           d.URL,
			"parent_url":    d.URL,
			"chunk_index":   chunkIndex,
			"chunk_total":   chunkTotal,
			"path":          d.Path,
			"path_prefixes": pathPrefixes(d.Path),
			"tags":          d.Tags,
			"created_at":    d.CreatedAt.Format(time.RFC3339),
			"updated_at":    d.UpdatedAt.Format(time.RFC3339),
			"id_alias":      parentID,
		},
	}
}

func docID(key string) uint64 {
	sum := md5.Sum([]byte(key))
	return binary.BigEndian.Uint64(sum[:8])
}

// splitForChunks splits a body into chunkChars-sized overlapping pieces.
// Returns one chunk for short docs, multiple for long ones. Operates on
// runes (not bytes) so CJK is handled correctly. Hard-caps at
// maxChunksPerFile so a single misfiled novel can't blow up the embed
// budget.
func splitForChunks(content string) []string {
	runes := []rune(content)
	n := len(runes)
	if n <= chunkChars {
		return []string{content}
	}
	stride := chunkChars - chunkOverlap
	chunks := make([]string, 0, (n/stride)+1)
	for start := 0; start < n; start += stride {
		end := start + chunkChars
		if end > n {
			end = n
		}
		chunks = append(chunks, string(runes[start:end]))
		if end == n {
			break
		}
		if len(chunks) >= maxChunksPerFile {
			break
		}
	}
	return chunks
}

// repoDocPoint builds the qdrantx.Point from a doc + its embedding.
// Single-chunk shortcut for callers that aren't chunking — keeps the
// existing manual-doc upsert path unchanged.
func repoDocPoint(repoID uint64, url string, vec []float32, d model.Doc) qdrantx.Point {
	return repoChunkPoint(repoID, url, 0, 1, vec, d, d.Content)
}

// repoChunkPoint builds the qdrantx.Point for one chunk of a repo doc.
// chunkIndex/chunkTotal land in the payload so the SPA / RAG layer can
// dedupe by parent (only chunk 0 surfaces in listings; all chunks compete
// in search). chunkContent is the slice of content this chunk embedded;
// chunk 0 stores the full body too so the SPA's doc-detail render shows
// the complete document.
func repoChunkPoint(repoID uint64, url string, chunkIndex, chunkTotal int, vec []float32, d model.Doc, chunkContent string) qdrantx.Point {
	id := repoChunkDocID(repoID, url, chunkIndex)
	// All chunks of one doc share the head chunk's id as their id_alias —
	// that's the "logical doc id" ListDocs dedupes on. Keying it to each
	// chunk's own id (the old bug) meant chunks never collapsed, so a
	// 3-chunk file showed up as 3 rows and inflated the doc count
	// (38 files rendered as 82 entries). Manual single-chunk docs are
	// unaffected (their alias is their own id, which is also the head).
	parentID := repoChunkDocID(repoID, url, 0)
	d.ID = id
	contentForPayload := chunkContent
	if chunkIndex == 0 {
		// Chunk 0 carries the full body so GET /knowledge/docs/<id> on
		// the parent ID returns the complete document (existing
		// behaviour).
		contentForPayload = d.Content
	}
	pt := qdrantx.Point{
		ID:     id,
		Vector: vec,
		Payload: map[string]any{
			"source_type":   d.SourceType,
			"tenant_scopes": d.TenantScopes,
			"title":         d.Title,
			"title_en":      d.TitleEN,
			"content":       contentForPayload,
			"url":           d.URL,
			"parent_url":    d.URL,
			"chunk_index":   chunkIndex,
			"chunk_total":   chunkTotal,
			"repo_id":       repoID,
			"path":          d.Path,
			"path_prefixes": pathPrefixes(d.Path),
			"tags":          d.Tags,
			"created_at":    d.CreatedAt.Format(time.RFC3339),
			"updated_at":    d.UpdatedAt.Format(time.RFC3339),
			"id_alias":      parentID,
		},
	}
	return pt
}

// buildGitAuthEnv resolves the right credential path for repoURL and
// returns extra env to feed runGit + a cleanup func.
//
// Dispatch by URL scheme:
//   - ssh-style (git@host:... or ssh://...) → look up an SSH identity
//     whose Hosts match the URL host; on hit, materialise the key to
//     a 0600 temp file and produce a GIT_SSH_COMMAND env line that
//     pins -i + IdentitiesOnly so the key picked is the only one
//     tried (avoids accidental fallback to the container's ~/.ssh).
//   - anything else (https / http) → no auth env; git tries anonymous.
//     Right for public repos like builtin://vault. Private HTTPS repos
//     will fail until P3 wires the credential.helper-based
//     per-host token table.
//
// The cleanup func deletes any temp files (keyfile + known_hosts for
// SSH) and is always safe to call, even on the no-auth path.
func (u *Usecase) buildGitAuthEnv(ctx context.Context, repoURL string) ([]string, func(), error) {
	noop := func() {}

	if !isSSHURL(repoURL) {
		// HTTPS / http → anonymous. P3 will introduce credential.helper.
		return nil, noop, nil
	}

	host := extractSSHHost(repoURL)
	identity, err := u.pickSSHIdentityForHost(ctx, host)
	if err != nil {
		return nil, noop, fmt.Errorf("ssh identity lookup: %w", err)
	}
	if identity == nil {
		// No identity matched → let git try whatever's in the container's
		// default ssh setup. Almost always fails for non-public hosts;
		// annotateGitError points the operator at "add an SSH identity
		// for host=X".
		return nil, noop, nil
	}
	env, cleanup, err := buildSSHEnv(identity)
	if err != nil {
		return nil, noop, err
	}
	// Best-effort usage timestamp; never fails the clone.
	_ = u.repo.TouchSSHIdentityUsage(ctx, identity.ID)
	return env, cleanup, nil
}

// buildSSHEnv writes the identity's private key + known_hosts to 0600
// temp files and assembles a GIT_SSH_COMMAND env line that points git
// at them. Returns the env + a cleanup func that removes both files.
func buildSSHEnv(identity *model.SSHIdentity) ([]string, func(), error) {
	noop := func() {}
	keyFile, err := os.CreateTemp("", "opskeeper-sshkey-*")
	if err != nil {
		return nil, noop, fmt.Errorf("write ssh key tempfile: %w", err)
	}
	if _, err := keyFile.WriteString(identity.PrivateKey); err != nil {
		_ = keyFile.Close()
		_ = os.Remove(keyFile.Name())
		return nil, noop, fmt.Errorf("write ssh key: %w", err)
	}
	// Trailing newline matters for some ssh client versions. Write only
	// if the key body doesn't already end with one.
	if !strings.HasSuffix(identity.PrivateKey, "\n") {
		_, _ = keyFile.WriteString("\n")
	}
	if err := keyFile.Close(); err != nil {
		_ = os.Remove(keyFile.Name())
		return nil, noop, fmt.Errorf("close ssh key: %w", err)
	}
	if err := os.Chmod(keyFile.Name(), 0o600); err != nil {
		_ = os.Remove(keyFile.Name())
		return nil, noop, fmt.Errorf("chmod ssh key: %w", err)
	}

	khFile, err := os.CreateTemp("", "opskeeper-known_hosts-*")
	if err != nil {
		_ = os.Remove(keyFile.Name())
		return nil, noop, fmt.Errorf("write known_hosts tempfile: %w", err)
	}
	if _, err := khFile.WriteString(identity.KnownHosts); err != nil {
		_ = khFile.Close()
		_ = os.Remove(khFile.Name())
		_ = os.Remove(keyFile.Name())
		return nil, noop, fmt.Errorf("write known_hosts: %w", err)
	}
	if err := khFile.Close(); err != nil {
		_ = os.Remove(khFile.Name())
		_ = os.Remove(keyFile.Name())
		return nil, noop, fmt.Errorf("close known_hosts: %w", err)
	}

	cleanup := func() {
		_ = os.Remove(keyFile.Name())
		_ = os.Remove(khFile.Name())
	}

	// GIT_SSH_COMMAND is git's standard way to swap the ssh client
	// invocation. We give it:
	//   -i <keyfile> force this private key
	//   -o IdentitiesOnly=yes refuse to fall back to any other key
	//                          (ssh-agent / ~/.ssh/id_*) — crucial so
	//                          a misconfigured container ssh doesn't
	//                          smear the auth across identities
	//   -o UserKnownHostsFile pin to the identity's own known_hosts
	//   -o StrictHostKeyChecking=accept-new
	//                          TOFU: first connection auto-pins host
	//                          key; subsequent mismatches fail
	//   -o BatchMode=yes never prompt for input (we're headless)
	//   -o ConnectTimeout=20 bound the initial TCP/handshake
	//   -o ServerAliveInterval=10 -o ServerAliveCountMax=3
	//                          mainland↔github stall fix: if the connection
	//                          goes silent mid-clone (no data for ~30s), ssh
	//                          tears it down and exits non-zero → git exits →
	//                          runGit returns a transient error to retry,
	//                          instead of the read hanging indefinitely.
	cmd := fmt.Sprintf(
		"ssh -i %s -o IdentitiesOnly=yes -o UserKnownHostsFile=%s -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=10 -o ServerAliveCountMax=3",
		keyFile.Name(), khFile.Name(),
	)
	return []string{"GIT_SSH_COMMAND=" + cmd}, cleanup, nil
}
