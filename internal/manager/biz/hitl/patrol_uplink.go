package hitl

// patrol_uplink.go — 托管平台例行拨测（BENY-929 P1/P2，2026-11-05 架构修正：
// 拨测属执行层职责，从 paperclip walker 迁回 opskeeper）。
//
// 链路：opskeeper 定期拨测托管目标（multica 等）→ 构造 ops-patrol-report v1
// 契约块 → 追加评论到 paperclip 桥接 issue（首次自动创建）→ walker 事件
// 摄入 → 熔断线/驾驶舱/自动立案。执行层产信号，决策面只消费——四通道
// 通道①（进度上行）的标准用法。
//
// 配置：
//   OPSKEEPER_PATROL_TARGETS="multica=https://opc.yueming.xin,foo=https://foo.example"
//   OPSKEEPER_PATROL_INTERVAL=5m（默认）
//   OPSKEEPER_PATROL_CERT_WARN_DAYS=14（默认）
// 复用 OPSKEEPER_PAPERCLIP_URL/TOKEN/COMPANY_ID 三件套（与 proposal 上行共用）。

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// PatrolTarget 一个被托管拨测目标。
type PatrolTarget struct {
	Name    string
	BaseURL string
}

// ParsePatrolTargets 解析 "name=url,name=url"；空返回 nil（拨测禁用）。
func ParsePatrolTargets(raw string) []PatrolTarget {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []PatrolTarget
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(kv) != 2 || kv[0] == "" || kv[1] == "" {
			continue
		}
		out = append(out, PatrolTarget{Name: kv[0], BaseURL: strings.TrimRight(kv[1], "/")})
	}
	return out
}

// patrolCheck 单项检查结果（对应契约 check 字段）。
type patrolCheck struct {
	Name      string  `json:"name"`
	Result    string  `json:"result"`
	LatencyMs *int64  `json:"latency_ms"`
	Detail    *string `json:"detail"`
	Layer     string  `json:"layer"`
}

// PatrolOnce 对全部目标拨测一次，返回契约 YAML 块（ops-patrol-report v1）。
// client 为 nil 时用默认（10s 超时）；now 注入便于测试。
func PatrolOnce(targets []PatrolTarget, certWarnDays int, client *http.Client, now time.Time) string {
	if certWarnDays <= 0 {
		certWarnDays = 14
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	var b strings.Builder
	b.WriteString("# ==== ops-patrol-report v1 ====\n")
	b.WriteString("schema: ops-patrol-report/1\n")
	fmt.Fprintf(&b, "collected_at: %s\n", now.UTC().Format(time.RFC3339))
	b.WriteString("collector: opskeeper:managed-patrol\n")
	b.WriteString("scope: probe\n")
	b.WriteString("targets:\n")
	for _, t := range targets {
		fmt.Fprintf(&b, "  - target_ref: tgt:managed/%s\n", t.Name)
		b.WriteString("    checks:\n")
		for _, path := range []string{"/healthz", "/health"} {
			c := probeHTTP(t.BaseURL+path, client)
			fmt.Fprintf(&b, "      - { name: %s, result: %s, latency_ms: %s, detail: %s, layer: live }\n",
				c.Name, c.Result, yamlNum(c.LatencyMs), yamlStr(c.Detail))
		}
		if strings.HasPrefix(t.BaseURL, "https://") {
			c := probeCert(strings.TrimPrefix(t.BaseURL, "https://"), certWarnDays)
			fmt.Fprintf(&b, "      - { name: %s, result: %s, latency_ms: null, detail: %s, layer: live }\n",
				c.Name, c.Result, yamlStr(c.Detail))
		}
		b.WriteString("    self_check: pass\n")
	}
	b.WriteString("flags: []\n")
	b.WriteString("# ==== end ops-patrol-report ====")
	return b.String()
}

func probeHTTP(url string, client *http.Client) patrolCheck {
	name := "http:" + strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	if i := strings.Index(name, "/"); i > 0 {
		name = "http:" + name[i:]
	}
	t0 := time.Now()
	resp, err := client.Get(url)
	lat := time.Since(t0).Milliseconds()
	if err != nil {
		d := err.Error()
		return patrolCheck{Name: name, Result: "fail", LatencyMs: &lat, Detail: &d, Layer: "live"}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return patrolCheck{Name: name, Result: "pass", LatencyMs: &lat, Layer: "live"}
	}
	d := fmt.Sprintf("HTTP %d", resp.StatusCode)
	return patrolCheck{Name: name, Result: "fail", LatencyMs: &lat, Detail: &d, Layer: "live"}
}

func probeCert(host string, warnDays int) patrolCheck {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(&d, "tcp", host+":443", &tls.Config{ServerName: host})
	if err != nil {
		d := "tls probe failed: " + err.Error()
		return patrolCheck{Name: "cert-expiry", Result: "degraded", Detail: &d, Layer: "live"}
	}
	defer conn.Close()
	validTo := conn.ConnectionState().PeerCertificates[0].NotAfter
	days := int(time.Until(validTo).Hours() / 24)
	switch {
	case days < 0:
		d := fmt.Sprintf("证书已过期 %d 天 (%s)", -days, host)
		return patrolCheck{Name: "cert-expiry", Result: "fail", Detail: &d, Layer: "live"}
	case days <= warnDays:
		d := fmt.Sprintf("证书仅剩 %d 天 (%s)", days, host)
		return patrolCheck{Name: "cert-expiry", Result: "degraded", Detail: &d, Layer: "live"}
	default:
		d := fmt.Sprintf("证书剩 %d 天 (%s)", days, host)
		return patrolCheck{Name: "cert-expiry", Result: "pass", Detail: &d, Layer: "live"}
	}
}

// yamlNum/yamlStr：契约 YAML 内联值的最小转义（detail 去引号危险字符）。
func yamlNum(v *int64) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprintf("%d", *v)
}

func yamlStr(v *string) string {
	if v == nil {
		return "null"
	}
	s := strings.Map(func(r rune) rune {
		switch r {
		case '{', '}', '"', '\n', '\r', ',':
			return ' '
		}
		return r
	}, *v)
	return fmt.Sprintf("%q", s)
}

// PatrolUplinkLoop 周期拨测并把契约评论追加到 paperclip 桥接 issue。
// 阻塞运行（由 main 以 goroutine 启动）；ctx 取消即退出。
func PatrolUplinkLoop(ctx context.Context, up *PaperclipUplink, targets []PatrolTarget, interval time.Duration, log interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) {
	if up == nil || len(targets) == 0 {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			report := PatrolOnce(targets, 14, nil, time.Now())
			up.PostPatrolComment(ctx, report, log)
			maybePrefileRenewal(up, report, log)
		}
	}
}

// certRenewalPrefile 证书到期自动提单（BENY-930 ①）：
// 巡检发现证书 ≤ prefileDays（默认 30）天时，自动向 paperclip 提交
// change-proposal（verb=cert-renewal，90 天授权窗口建议，auto-eligible），
// 人批一次 = 批一个季度的轮换额度。季度幂等（进程内记录已提季度）。
const (
	certPrefileDays = 30
	certWindowDays  = 90
)

var prefiledQuarters = map[string]bool{}

// BuildRenewalProposal 构造证书轮换周期授权提案契约块（纯函数，测试用）。
func BuildRenewalProposal(host string, daysLeft int, now time.Time) string {
	ref := fmt.Sprintf("prp:cert-renewal:%s:%dQ%d", host, now.Year(), (int(now.Month())-1)/3+1)
	var b strings.Builder
	b.WriteString("# ==== change-proposal v1 ====\n")
	b.WriteString("schema: change-proposal/1\n")
	fmt.Fprintf(&b, "proposed_at: %s\n", now.UTC().Format(time.RFC3339))
	b.WriteString("proposer: opskeeper:cert-patrol\n")
	fmt.Fprintf(&b, "proposal_ref: %s\n", ref)
	fmt.Fprintf(&b, "incident_ref: inc:cert:%s\n", host)
	fmt.Fprintf(&b, "target_ref: cert:%s\n", host)
	fmt.Fprintf(&b, "action: { verb: cert-renewal, object: %s, params: {}, blast_radius: 证书轮换（ACME 续期+部署+重载网关，仅 %s 域名） }\n", host, host)
	fmt.Fprintf(&b, "safety: { reversible: true, rollback_ref: runbook:cert-renewal#rollback, dry_run_available: true }\n")
	fmt.Fprintf(&b, "approval_request: { approver_role: owner, expires_at: %s }\n", now.Add(7*24*time.Hour).UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "proposal_hash: %s\n", CertRenewalHash(ref))
	fmt.Fprintf(&b, "evidence_refs: [patrol:cert-expiry]\n")
	fmt.Fprintf(&b, "flags: [auto-eligible]\n")
	fmt.Fprintf(&b, "detail: 证书剩余 %d 天，申请 %d 天周期轮换授权（窗口内自动执行，不再逐次审批）\n", daysLeft, certWindowDays)
	b.WriteString("# ==== end change-proposal ====")
	return b.String()
}

// CertRenewalHash 周期授权提案指纹（生产方 canonical：verb+object+周期 ref）。
func CertRenewalHash(ref string) string {
	sum := sha256Sum("cert-renewal|" + ref)
	return sum
}

// maybePrefileRenewal 巡检后检查证书结果并按需提单（季度幂等）。
func maybePrefileRenewal(up *PaperclipUplink, contractYAML string, log interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) {
	// 从契约文本提取证书剩余天数（"证书剩 N 天"/"证书仅剩 N 天"）
	for _, m := range certDaysRe.FindAllStringSubmatch(contractYAML, -1) {
		days := atoiSafe(m[1])
		if days > certPrefileDays {
			continue
		}
		host := m[2]
		q := fmt.Sprintf("%s-%dQ%d", host, time.Now().Year(), (int(time.Now().Month())-1)/3+1)
		if prefiledQuarters[q] {
			continue
		}
		prefiledQuarters[q] = true
		log.Info("cert patrol: prefile quarterly renewal proposal", "host", host, "days_left", days)
		up.PostPatrolProposal(context.Background(), BuildRenewalProposal(host, days, time.Now()), log)
	}
}

var certDaysRe = regexpMustCompile(`证书(仅)?剩 (\d+) 天 \(([^)]+)\)`)

// --- 小工具（避免多 import） ---
func regexpMustCompile(re string) *regexp.Regexp { return regexp.MustCompile(re) }

func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func sha256Sum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
