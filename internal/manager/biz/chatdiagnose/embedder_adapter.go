// chatdiagnose/embedder_adapter.go — bridge chatdiagnose.Embedder 到
// internal/pkg/embedding.Embedder。
//
// 接口差异：
//   chatdiagnose.Embedder : Embed(ctx, string) ([]float64, error)
//   pkg/embedding.Embedder: Embed(ctx, []string) ([][]float32, error) + Dim() int
//
// Adapter 把 (string) 包成单元素 []string，float32 → float64 转换，再透传 Dim()。
// 复用而非重写：OpenAI / 本地 BGE / retry / batch 都在 internal/pkg/embedding 实现，
// 这里只做单点 bridge，未来 pkg/embedding 改接口只需改本文件。

package chatdiagnose

import (
	"context"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/embedding"
)

// EmbedderAdapter bridges chatdiagnose.Embedder ↔ internal/pkg/embedding.Embedder.
type EmbedderAdapter struct {
	inner embedding.Embedder
	dim   int
}

// NewEmbedderAdapter 构造 adapter。inner 不能为 nil（生产 wire）。
func NewEmbedderAdapter(inner embedding.Embedder) *EmbedderAdapter {
	return &EmbedderAdapter{inner: inner, dim: inner.Dim()}
}

// Dim 返回向量维度（来自 pkg/embedding）。
// Qdrant EnsureCollection 会按此值设置 collection dim。
func (a *EmbedderAdapter) Dim() int { return a.dim }

// Embed 单文本 embedding。内部转 []string 调 pkg/embedding，再 float32→float64。
func (a *EmbedderAdapter) Embed(ctx context.Context, text string) ([]float64, error) {
	vecs, err := a.inner.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	v := vecs[0]
	out := make([]float64, len(v))
	for i, f := range v {
		out[i] = float64(f)
	}
	return out, nil
}
