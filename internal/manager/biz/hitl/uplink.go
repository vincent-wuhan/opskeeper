package hitl

// uplink.go — proposal 创建后自动上行 paperclip（BENY-907）。
//
// 闭环最后一环：opskeeper 产生高危提案 → 本 uplink 把 change-proposal v1
// 契约块 POST 到 paperclip 桥接 issue → walker 事件摄入 → 门 A 审批 →
// 决议回传 /decision（fail-closed）→ 执行。
//
// 设计约束：
//   - 异步 fire-and-forget：上行失败只记日志，不影响提案创建主流程
//     （paperclip 不可达时审批退回本地 admin JWT 通道）
//   - 契约字段与 PaperPlugins/walker/contracts/change-proposal-v1.md 对齐：
//     proposal_ref = payload.RequestID；upstream_ref = proposal UUID（决议寻址键）；
//     proposal_hash = CanonicalPayloadHash（opskeeper 是哈希生产方）

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const uplinkTimeout = 10 * time.Second

// PaperclipUplink 把 HITL proposal 上行为 paperclip 契约 issue。
type PaperclipUplink struct {
	baseURL string
	token   string
	company string
	client  *http.Client
	log     interface{ Warn(msg string, args ...any) }
	nowFunc func() time.Time
}

// NewPaperclipUplinkFromEnv 从 OPSKEEPER_PAPERCLIP_URL/TOKEN/COMPANY_ID 构造；
// 任一缺失返回 nil（上行禁用，行为与无 uplink 完全一致）。
func NewPaperclipUplinkFromEnv(log interface{ Warn(msg string, args ...any) }) *PaperclipUplink {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("OPSKEEPER_PAPERCLIP_URL")), "/")
	token := strings.TrimSpace(os.Getenv("OPSKEEPER_PAPERCLIP_TOKEN"))
	company := strings.TrimSpace(os.Getenv("OPSKEEPER_PAPERCLIP_COMPANY_ID"))
	if baseURL == "" || token == "" || company == "" {
		return nil
	}
	return &PaperclipUplink{
		baseURL: baseURL,
		token:   token,
		company: company,
		client:  &http.Client{Timeout: uplinkTimeout},
		log:     log,
		nowFunc: func() time.Time { return time.Now().UTC() },
	}
}

// BuildProposalContract 构造 change-proposal v1 契约块（纯函数，测试用）。
func BuildProposalContract(proposalID string, payload AgentTeamsPayload, payloadHash string, now time.Time) string {
	var b strings.Builder
	b.WriteString("# ==== change-proposal v1 ====\n")
	fmt.Fprintf(&b, "schema: change-proposal/1\n")
	fmt.Fprintf(&b, "proposed_at: %s\n", now.Format(time.RFC3339))
	fmt.Fprintf(&b, "proposer: opskeeper:repairer\n")
	fmt.Fprintf(&b, "proposal_ref: %s\n", payload.RequestID)
	fmt.Fprintf(&b, "upstream_ref: %s\n", proposalID)
	fmt.Fprintf(&b, "incident_ref: %s\n", payload.IncidentID)
	fmt.Fprintf(&b, "target_ref: %s\n", payload.Resource)
	fmt.Fprintf(&b, "action: { verb: %s, object: %s, params: {}, blast_radius: %s }\n",
		payload.Action, payload.Resource, payload.BlastRadius)
	fmt.Fprintf(&b, "safety: { reversible: true, rollback_ref: runbook:%s#rollback, dry_run_available: true }\n", payload.Action)
	fmt.Fprintf(&b, "approval_request: { approver_role: oncall, expires_at: %s }\n", payload.ExpiresAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "proposal_hash: %s\n", payloadHash)
	fmt.Fprintf(&b, "evidence_refs: []\n")
	fmt.Fprintf(&b, "flags: [high-risk]\n")
	b.WriteString("# ==== end change-proposal ====")
	return b.String()
}

// NotifyProposalCreated 异步上行（fire-and-forget）。
func (u *PaperclipUplink) NotifyProposalCreated(proposalID string, payload AgentTeamsPayload, payloadHash string) {
	if u == nil {
		return
	}
	body := map[string]string{
		"title":       fmt.Sprintf("[change-proposal] %s %s", payload.RequestID, payload.Action),
		"description": BuildProposalContract(proposalID, payload, payloadHash, u.nowFunc()),
	}
	go func() {
		var buf bytes.Buffer
		// 简单手写 JSON 编码（title/description 均需转义，字段固定两个）
		fmt.Fprintf(&buf, `{"title":%q,"description":%q}`, body["title"], body["description"])
		url := fmt.Sprintf("%s/api/companies/%s/issues", u.baseURL, u.company)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, &buf)
		if err != nil {
			u.log.Warn("paperclip uplink: build request failed", "err", err.Error())
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+u.token)
		resp, err := u.client.Do(req)
		if err != nil {
			u.log.Warn("paperclip uplink: post failed", "proposal", payload.RequestID, "err", err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			u.log.Warn("paperclip uplink: non-2xx", "proposal", payload.RequestID, "status", resp.StatusCode)
			return
		}
	}()
}
