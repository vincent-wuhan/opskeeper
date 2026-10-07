// Package gitartifact — link_runtime_to_commit.go
//
// Runtime-Source-Bridge 合并设计（zero-manual-ops-loop Day 4 任务 4.4，
// design §D11 借鉴 v1 `linkRuntimeToCommit`）。
//
// 背景：v1 release-8.20 在 auto-diagnose.ts 里挂了 9 条
// DIAGNOSIS_SKILL_MAP，把 3 个 resourceType × 多个 skill 全部指向
// `opskeeper_code_link_investigation`。该 skill 调 linkRuntimeToCommit
// 拿到 runtime 证据 + commit SHA。
//
// v2 路径 A 集成：postmortem 渲染阶段调用本文件暴露的
// LinkRuntimeToCommit，把"运行时符号 → git commit"反查结果塞进
// postmortem Markdown 的 "Source commits" 章节。
//
// 与 gitartifact 既有 Linker 体系的关系：
//   - 4 个 Linker（pg_query / redis_cmd / k8s_image / http_route）
//     已经在 linker.go 落地，存储 4 类运行时符号到 commit 的反向索引。
//   - 本文件提供"批量多类型统一入口"：调用方传一组 selector，
//     内部按 SymbolType 路由到对应 Linker，合并结果。
//   - 不复制 v1 6 个新 git 工具（v2 改用既有 4 个 Linker + go-git
//     由 opskeeper cmd 自行处理），符合"借鉴哲学不复制代码"约束。
//
// 公共 API：
//   - LinkRuntimeToCommitInput / LinkRuntimeToCommitResult / ResolvedCommit
//   - LinkRuntimeToCommit(ctx, registry, input) — 统一入口
//   - 4 个 selector 构造器：NewK8sSelector / NewPGSelector /
//     NewRedisSelector / NewHTTPSelector（带 runtime-key 字符串）
package gitartifact

import (
	"context"
	"fmt"
	"time"
)

// RuntimeSelector is a tagged-union of the 4 supported runtime
// selector types. Use the typed constructors (NewK8sSelector, etc.)
// to build one — direct field assignment is discouraged because the
// runtime key derivation is non-trivial.
//
// Exactly one of K8s / PG / Redis / HTTP is populated; the others
// MUST be nil. The SymbolType field acts as a discriminator and is
// set by the constructors.
type RuntimeSelector struct {
	Type  SymbolType
	K8s   *K8sImage
	PG    *PGQuery
	Redis *RedisCmd
	HTTP  *HTTPRoute
}

// NewK8sSelector builds a K8sImage-backed RuntimeSelector.
func NewK8sSelector(image, tag string) RuntimeSelector {
	return RuntimeSelector{
		Type: SymbolTypeK8sImage,
		K8s:  &K8sImage{Image: image, Tag: tag},
	}
}

// NewPGSelector builds a PGQuery-backed RuntimeSelector.
func NewPGSelector(query, database string) RuntimeSelector {
	return RuntimeSelector{
		Type: SymbolTypePGQuery,
		PG:   &PGQuery{Query: query, Database: database},
	}
}

// NewRedisSelector builds a RedisCmd-backed RuntimeSelector.
func NewRedisSelector(cmd, key string) RuntimeSelector {
	return RuntimeSelector{
		Type:  SymbolTypeRedisCmd,
		Redis: &RedisCmd{Cmd: cmd, Key: key},
	}
}

// NewHTTPSelector builds an HTTPRoute-backed RuntimeSelector.
func NewHTTPSelector(method, path, handler string) RuntimeSelector {
	return RuntimeSelector{
		Type: SymbolTypeHTTPRoute,
		HTTP: &HTTPRoute{Method: method, Path: path, Handler: handler},
	}
}

// RuntimeKey returns the string form used by postmortem Markdown
// (e.g. "k8s_image:registry.example.com/order-svc:v1.2.3"). It
// also serves as a stable identifier for the ResolvedCommit row.
func (s RuntimeSelector) RuntimeKey() string {
	switch s.Type {
	case SymbolTypeK8sImage:
		if s.K8s == nil {
			return ""
		}
		if s.K8s.Tag != "" {
			return string(SymbolTypeK8sImage) + ":" + s.K8s.Image + ":" + s.K8s.Tag
		}
		return string(SymbolTypeK8sImage) + ":" + s.K8s.Image
	case SymbolTypePGQuery:
		if s.PG == nil {
			return ""
		}
		// Use the normalised form so the same query in different
		// casings collapses to the same runtime key.
		return string(SymbolTypePGQuery) + ":" + normalizePGQuery(s.PG.Query)
	case SymbolTypeRedisCmd:
		if s.Redis == nil {
			return ""
		}
		return string(SymbolTypeRedisCmd) + ":" + s.Redis.Cmd + ":" + s.Redis.Key
	case SymbolTypeHTTPRoute:
		if s.HTTP == nil {
			return ""
		}
		return string(SymbolTypeHTTPRoute) + ":" + s.HTTP.Method + " " + s.HTTP.Path
	default:
		return ""
	}
}

// Validate reports whether the selector is well-formed (Type set +
// the corresponding pointer non-nil).
func (s RuntimeSelector) Validate() error {
	switch s.Type {
	case SymbolTypeK8sImage:
		if s.K8s == nil || s.K8s.Image == "" {
			return fmt.Errorf("gitartifact: k8s selector requires non-empty image")
		}
	case SymbolTypePGQuery:
		if s.PG == nil || s.PG.Query == "" {
			return fmt.Errorf("gitartifact: pg selector requires non-empty query")
		}
	case SymbolTypeRedisCmd:
		if s.Redis == nil || s.Redis.Cmd == "" {
			return fmt.Errorf("gitartifact: redis selector requires non-empty cmd")
		}
	case SymbolTypeHTTPRoute:
		if s.HTTP == nil || s.HTTP.Method == "" || s.HTTP.Path == "" {
			return fmt.Errorf("gitartifact: http selector requires method + path")
		}
	default:
		return fmt.Errorf("gitartifact: unknown selector type %q", s.Type)
	}
	return nil
}

// LinkRuntimeToCommitInput is the input bundle.
type LinkRuntimeToCommitInput struct {
	// TenantID enforces multi-tenant isolation. Required.
	TenantID uint64

	// Selectors is the batch of runtime symbols to attribute. Order
	// is preserved in the output (ResolvedCommits and
	// UnmatchedRuntime mirror the input order).
	Selectors []RuntimeSelector
}

// ResolvedCommit is the postmortem-friendly attribution row. It
// carries the four fields the postmortem Markdown renders
// (commit_sha / file_path / blame_author / first_introduced_at)
// plus a few rendering aids (runtime_key, confidence, needs_human).
//
// Field naming uses Go's idiomatic mixed-case so the
// renderer's JSON marshal / struct copy is one-line.
type ResolvedCommit struct {
	// RuntimeKey is the selector's string form (see RuntimeSelector.RuntimeKey).
	// Used as a stable identifier in the postmortem's source-commits table.
	RuntimeKey string

	// CommitSHA is the resolved commit hash. Empty when the linker
	// returned (nil, nil); the row is then in UnmatchedRuntime
	// instead.
	CommitSHA string

	// FilePath is the file in the resolved commit.
	FilePath string

	// LineStart / LineEnd locate the symbol within FilePath.
	LineStart int
	LineEnd   int

	// BlameAuthor is the commit author (or whoever last touched the
	// file:line per `git blame`). v1's linkRuntimeToCommit returns
	// the commit author; v2 mirrors that.
	BlameAuthor string

	// FirstIntroducedAt is the wall-clock time of the commit. v2
	// does NOT yet have a real `git blame`; the commit timestamp
	// is the closest approximation. Zero value when the linker
	// didn't return a timestamp.
	FirstIntroducedAt time.Time

	// Confidence is the linker-reported score in [0, 1].
	Confidence float64

	// NeedsHumanConfirm is true when Confidence < ConfidenceThreshold.
	NeedsHumanConfirm bool

	// Repo is the source repository URL (linker.Meta).
	Repo string
}

// LinkRuntimeToCommitResult is the output bundle.
type LinkRuntimeToCommitResult struct {
	// TenantID echoes the input (for audit / log lines).
	TenantID uint64

	// ResolvedCommits lists the selectors that hit a linker.
	// Order matches the input order (filtered).
	ResolvedCommits []ResolvedCommit

	// UnmatchedRuntime lists the selectors that did NOT hit a
	// linker (either the linker was not registered, or the linker
	// returned (nil, nil) for no match). Order matches the input.
	UnmatchedRuntime []RuntimeSelector
}

// LinkRuntimeToCommit is the runtime→commit attribution batch
// entry point. It iterates the input selectors, routes each to the
// registered Linker by SymbolType, and merges results.
//
// Behaviour:
//   - Selectors that fail Validate() are skipped (counted as
//     UnmatchedRuntime, with a synthetic type="invalid" marker so
//     the postmortem renderer can surface the reason).
//   - Selectors whose SymbolType has no registered Linker go to
//     UnmatchedRuntime.
//   - Selectors that hit a linker but receive (nil, nil) from it
//     also go to UnmatchedRuntime.
//   - On a linker-level error the selector is skipped and counted
//     as UnmatchedRuntime (callers that need strict semantics can
//     inspect ResolvedCommits for the absence).
//
// The function is goroutine-safe (Linker implementations hold their
// own locks). It does NOT spawn goroutines; callers batch.
//
// Day 4 deviation from design §E.2: the design listed HostSelector /
// AppSelector as additional types. v2's linker.go only has the
// 4 existing types (pg_query / redis_cmd / k8s_image / http_route).
// HostSelector is intentionally NOT added in this PR — Day 6 will
// evaluate whether the host recovery case warrants a new linker
// (open question in the v1→v2 mapping).
func LinkRuntimeToCommit(
	ctx context.Context,
	registry *LinkerRegistry,
	in LinkRuntimeToCommitInput,
) LinkRuntimeToCommitResult {
	out := LinkRuntimeToCommitResult{TenantID: in.TenantID}
	if registry == nil {
		// Without a registry every selector is unmatched. Returning
		// the full unmatched slice is more informative than an
		// error — the postmortem path treats a nil registry as
		// "code source unavailable" and renders a placeholder.
		out.UnmatchedRuntime = append(out.UnmatchedRuntime, in.Selectors...)
		return out
	}
	for _, sel := range in.Selectors {
		if err := sel.Validate(); err != nil {
			out.UnmatchedRuntime = append(out.UnmatchedRuntime, sel)
			continue
		}
		linker, ok := registry.Get(sel.Type)
		if !ok {
			out.UnmatchedRuntime = append(out.UnmatchedRuntime, sel)
			continue
		}
		var input any
		switch sel.Type {
		case SymbolTypeK8sImage:
			input = *sel.K8s
		case SymbolTypePGQuery:
			input = *sel.PG
		case SymbolTypeRedisCmd:
			input = *sel.Redis
		case SymbolTypeHTTPRoute:
			input = *sel.HTTP
		}
		lr, err := linker.Link(ctx, input)
		if err != nil || lr == nil {
			// Treat errors and (nil, nil) as unmatched (consistent
			// with the linker convention).
			out.UnmatchedRuntime = append(out.UnmatchedRuntime, sel)
			continue
		}
		out.ResolvedCommits = append(out.ResolvedCommits, ResolvedCommit{
			RuntimeKey:        sel.RuntimeKey(),
			CommitSHA:         lr.Commit,
			FilePath:          lr.FilePath,
			LineStart:         lr.LineStart,
			LineEnd:           lr.LineEnd,
			BlameAuthor:       lr.Author,
			Confidence:        lr.Confidence,
			NeedsHumanConfirm: lr.NeedsHumanConfirm(),
			Repo:              lr.Repo,
		})
	}
	return out
}
