// Package gitsink 提供 loop.GitArtifactSink 的 narrow adapter，
// 包装 internal/manager/biz/report.PostmortemSink（默认 *report.GitArtifactSink）。
//
// 设计动机：
//   - loop 包不能直接 import report 包（形成 loop → report → loop 的 cycle；
//     report.postmortem_sink.go 实现 loop.PostmortemSink 接口）
//   - 通过把 adapter 放在独立子包 loop/gitsink，包图为：
//     loop/gitsink → report → loop
//     无环 ✅
//
// 行为：
//   - 构造最小 PostmortemDoc（schema_version=v1, IncidentID, Markdown, GeneratedAt,
//     Sources=["loop.postmortem"]）满足 ValidatePostmortemDoc 不变量
//   - 失败：slog warn + 返回 ("", err)（postmortem worker 会把 commit failure 视为非致命）
//
// 已知 limitation：
//   - body 来源目前仅 postmortem worker 自渲染 Markdown；Day 5+ 接 LLM-driven rendering 时
//     可在 adapter 上加 ContentSource 回调让 Sources 字段精确反映来源（不破坏接口）
package gitsink

import (
	"context"
	"log/slog"
	"time"

	loop "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
	managerbizreport "github.com/vincent-wuhan/opskeeper/internal/manager/biz/report"
)

// Adapter 适配 report.PostmortemSink → loop.GitArtifactSink。
type Adapter struct {
	sink managerbizreport.PostmortemSink
	log  *slog.Logger
	now  func() time.Time
}

// NewAdapter 构造。sink 不得为 nil；log 为 nil 时回退 slog.Default()。
func NewAdapter(sink managerbizreport.PostmortemSink, log *slog.Logger) *Adapter {
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
