// chatdiagnose/noop_kb_deps.go — 占位 PatternRepo / GitArtifactLinker / Embedder。
//
// 真实实现由 Day 10+ chatruntime-side 集成包提供（spec §conversational-diagnosis
// §"KB 优先启用时机（30+ postmortem 后）" Q-δ）。当前阶段 KBLookupImpl 已 wire
// 但内部三个依赖用 noop，让 service 跑得通而不出 panic。

package chatdiagnose

import (
	"context"
	"errors"
	"sync"

	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

// NoopPatternRepo — PatternRepo 占位实现。
type NoopPatternRepo struct {
	mu sync.Mutex
}

func (*NoopPatternRepo) FindSimilar(_ context.Context, _ string, _ []float64, _ int) ([]chatdiagnosemodel.IncidentPattern, error) {
	return nil, nil
}

func (*NoopPatternRepo) IncHitCount(_ context.Context, _ int64) error {
	return nil
}

func (n *NoopPatternRepo) Save(_ context.Context, p *chatdiagnosemodel.IncidentPattern) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return errors.New("chatdiagnose: NoopPatternRepo.Save not implemented (Day 10+)")
}

// NoopGitArtifactLinker — GitArtifactLinker 占位实现。
type NoopGitArtifactLinker struct{}

func (NoopGitArtifactLinker) LookupByResource(_ context.Context, _, _, _ string) ([]GitArtifactHit, error) {
	return nil, nil
}

// NoopEmbedder — Embedder 占位实现。
type NoopEmbedder struct{}

func (NoopEmbedder) Embed(_ context.Context, _ string) ([]float64, error) {
	// 返回 0 维向量 — Jaccard fallback 仍可工作（spec §"computeSimilarity
	// Jaccard placeholder"），不会让 KBLookupImpl panic。
	return nil, nil
}
