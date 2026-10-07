package higress

import (
	"errors"
	"net/http"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
)

// The gateway's chain verification surface (决策 328).
//
// 决策 324 给这个进程单独立了一条链，理由成立：它持有 OPSKEEPER_JWT_SECRET，
// 能写控制面的链就等于能伪造控制面的审计。**但那条链从来没有被验证过**——
// 全仓 `VerifyChain` 的生产调用方只有一个，是控制面自己的读端
// （core/domains/server/audit）。而立链的那段注释写着「Two chains, two keys,
// two verifiers」：有两条链、两把钥匙，**只有一个验证者**。
//
// **一条从来不被验证的链是装饰品**：它给每一行算一次 HMAC，什么也没换回来。
// 一个人只要能改数据库，就能把改过的那一行的摘要重算成自洽的样子——只要
// 没有人去走一遍。因此这一刀给网关补上它缺的那一端：能回答「我这条链还完整吗，
// 从第几行开始不完整」。
//
// 语义逐字沿用控制面那一端（core/domains/server/audit/http.go 的 chain）：
//
//   - **永远 200**。运维问「这份记录被改过吗」，答案可以是「是」，为它返回 500
//     会让只看状态码的客户端把两种结果混为一谈。
//   - 链没开就说没开，并说清为什么，而不是让「没配置」看起来像「检查通过」。
//   - 坏了就报出坏在哪一行（BrokenAtSeq），因为「你的审计不可信」和「你的审计
//     从第 41231 行起不可信」对处置动作的要求是不同的。

// chainResp is the wire shape, and it is deliberately the same one the
// control plane answers with. Two processes reporting on two chains should
// not need two clients.
type chainResp struct {
	Enabled bool `json:"enabled"`
	// Intact is a pointer so "not verified" and "verified false" are
	// different JSON. A bool default would make a chain that could not be
	// walked render as a chain that is broken, which is the one thing this
	// endpoint must never do.
	Intact *bool `json:"intact,omitempty"`
	// HeadSeq and AnchorSeq say how much of the chain is present. An
	// AnchorSeq above 1 means a retention sweep removed the prefix, and
	// "verified" covers only what is left — a materially weaker claim than
	// the same word on an untouched chain.
	HeadSeq   uint64 `json:"head_seq"`
	AnchorSeq uint64 `json:"anchor_seq"`
	// BrokenAtSeq is where verification first failed.
	BrokenAtSeq uint64 `json:"broken_at_seq,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// handleChain reports whether this gateway's own audit trail is
// tamper-evident and intact.
func (s *Server) handleChain(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Chain == nil {
		// The route only installs when a chain is wired, so this cannot
		// normally happen — and if it somehow does, the honest answer is
		// "there is nothing to verify", not a 500 that reads like a
		// broken chain.
		writeJSON(w, http.StatusOK, chainResp{Reason: "this process has no audit chain wired"})
		return
	}
	state, err := s.cfg.Chain.ChainState(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, chainResp{Reason: "chain state unavailable: " + err.Error()})
		return
	}
	out := chainResp{Enabled: state.Enabled, HeadSeq: state.HeadSeq, AnchorSeq: state.AnchorSeq}
	if !state.Enabled {
		out.Reason = "audit chain disabled: OPSKEEPER_HIGRESS_AUDIT_HMAC_KEY is not set, so rows carry no digest"
		writeJSON(w, http.StatusOK, out)
		return
	}
	switch verr := s.cfg.Chain.VerifyChain(r.Context()); {
	case verr == nil:
		ok := true
		out.Intact = &ok
	case errors.Is(verr, auditport.ErrChainDisabled):
		out.Reason = verr.Error()
	default:
		bad := false
		out.Intact = &bad
		var broken *auditport.ErrChainBroken
		if errors.As(verr, &broken) {
			out.BrokenAtSeq = broken.Seq
			out.Reason = broken.Reason
		} else {
			out.Reason = verr.Error()
		}
	}
	writeJSON(w, http.StatusOK, out)
}
