// Package gitsink provides loop.GitArtifactSink over whatever store owns
// the postmortem documents.
//
// The previous version of this file imported core/manager/biz/report and its
// doc comment explained why that was fine:
//
//   - loop 包不能直接 import report 包（形成 loop → report → loop 的 cycle）
//   - 通过把 adapter 放在独立子包 loop/gitsink，包图为 loop/gitsink →
//     report → loop，无环 ✅
//
// The package graph was acyclic. The **domain** graph was not: domaincheck
// resolves biz/loop/gitsink to the `loop` domain, so the cycle stood and the
// ✅ was describing a different graph than the one the boundary rule is
// about. Same shape as decision 114 — an interface declared on the consuming
// side whose signature still named the producing side.
//
// The import was never needed. All this adapter does is build a minimal
// PostmortemDoc and hand it to something that can save one, so it now says
// that (Sink) instead of saying who that something is. The production
// implementation, *report.GitArtifactSink, satisfies Sink structurally, and
// cmd/opskeeper/main.go still passes it — unchanged, because structural
// satisfaction needs no import on either side.
//
// Behaviour, including the empty-input soft failures and the nil-sink panic,
// is unchanged; see decision 115.
//
// Known limitation (carried over, still open):
//   - the body currently comes only from the postmortem worker's own
//     self-rendered Markdown. When LLM-driven rendering lands, a
//     ContentSource callback can be added on the adapter so the Sources
//     field reflects the real origin without breaking the interface.
package gitsink

import (
	"context"
	"log/slog"
	"time"

	loop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

// Sink persists a rendered postmortem document and returns its commit SHA.
//
// It is the same shape as report.PostmortemSink and is satisfied by
// *report.GitArtifactSink without either side importing the other. Declared
// here because the adapter's job is to call "something that can save a doc",
// and naming who that is turns a one-way call into a two-way dependency
// (decision 115).
type Sink interface {
	Save(ctx context.Context, doc *loop.PostmortemDoc) (commitSHA string, err error)
}

// Adapter 适配 Sink → loop.GitArtifactSink。
type Adapter struct {
	sink Sink
	log  *slog.Logger
	now  func() time.Time
}

// NewAdapter 构造。sink 不得为 nil；log 为 nil 时回退 slog.Default()。
//
// The nil check is on the interface, so it catches an unwired adapter and
// not a nil pointer stored inside it — a limitation it has always had, kept
// as-is so no caller changes shape.
func NewAdapter(sink Sink, log *slog.Logger) *Adapter {
	if sink == nil {
		panic("gitsink: NewAdapter: sink is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Adapter{
		sink: sink,
		log:  log.With(slog.String("comp", "loop.git_artifact_sink_adapter")),
		now:  func() time.Time { return time.Now().UTC() },
	}
}

// Compile-time interface satisfaction check.
var _ loop.GitArtifactSink = (*Adapter)(nil)

// CommitMarkdown 实现 loop.GitArtifactSink 接口。
//
// 行为：
//   - incidentID 为空 → slog warn + 返回 ("", nil)（与 NoopGitArtifactSink 语义一致）
//   - body 为空 → slog warn + 返回 ("", nil)（validate 会失败，视为软失败避免阻塞 postmortem）
//   - 构造最小 PostmortemDoc 调 sink.Save；sink 错误透传
func (a *Adapter) CommitMarkdown(ctx context.Context, incidentID, body string) (string, error) {
	if incidentID == "" {
		a.log.Warn("git_artifact_sink_adapter: empty incidentID, skipping commit (non-fatal)")
		return "", nil
	}
	if body == "" {
		a.log.Warn("git_artifact_sink_adapter: empty body, skipping commit (non-fatal)",
			slog.String("incident_id", incidentID))
		return "", nil
	}

	doc := &loop.PostmortemDoc{
		SchemaVersion: loop.ContractSchemaV1,
		IncidentID:    incidentID,
		Markdown:      body,
		GeneratedAt:   a.now(),
		Sources:       []string{"loop.postmortem"},
	}
	sha, err := a.sink.Save(ctx, doc)
	if err != nil {
		a.log.Warn("git_artifact_sink_adapter: sink.Save failed (non-fatal)",
			slog.String("incident_id", incidentID), slog.Any("err", err))
		return "", err
	}
	return sha, nil
}
