package hitl

// cert_executor.go — 证书轮换执行器（BENY-930 ②）。
//
// 治理模型：周期授权（季度 approved 决议，90 天窗口）内自动执行，
// 窗口外/无授权 fail-closed。执行动作白名单（只允许证书域操作）：
//   1. certbot renew --cert-name <host> --deploy-hook <reload>
//   2. caddy reload（经 deploy-hook 触发，不单独暴露）
//
// 授权校验：执行前向 paperclip 查该 host 的周期提案状态——
// approved 且 now < window.to 才放行。查询走桥接 issue 评论里的
// change-decision 块（proposal_ref 精确匹配）。
//
// 配置：
//   OPSKEEPER_CERT_EXEC=1                启用（默认关）
//   OPSKEEPER_CERTBOT_BIN=certbot        可执行路径
//   OPSKEEPER_CADDY_RELOAD_CMD=...       deploy-hook 命令（如 systemctl reload caddy）

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CertExecutor 证书轮换执行器。现实形态（80 口不可达，HTTP-01 不可行）：
// 签发由人在阿里云控制台完成（免费 DV，DNS 自动验证），证书下载后放到
// OPSKEEPER_CERT_INBOX 目录；本执行器在授权窗口内执行"部署"：
// 复制 crt/key 到 TLS 终结容器（agentteams-https）的挂载目录并重启该容器。
// 窗口校验不变（fail-closed）。
type CertExecutor struct {
	up        *PaperclipUplink
	certbot   string
	reloadCmd string
	run       func(ctx context.Context, name string, args ...string) (string, error)
	log       interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
	}
}

// NewCertExecutor 构造；run 为命令执行函数（生产 exec.CommandContext，测试注入）。
func NewCertExecutor(up *PaperclipUplink, log interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) *CertExecutor {
	e := &CertExecutor{up: up, log: log}
	e.certbot = envOr("OPSKEEPER_CERTBOT_BIN", "certbot")
	e.reloadCmd = envOr("OPSKEEPER_CADDY_RELOAD_CMD", "caddy reload")
	e.run = func(ctx context.Context, name string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	return e
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// AuthorizedWindow 描述一次周期授权的窗口。
type AuthorizedWindow struct {
	ProposalRef string
	Approved    bool
	From, To    time.Time
}

// CheckAuthorization 查 paperclip 决议，判定 host 本季度是否处于授权窗口。
// fail-closed：查询失败/无决议/决议未批准/过期 一律不放行。
func (e *CertExecutor) CheckAuthorization(ctx context.Context, host string, now time.Time) (AuthorizedWindow, error) {
	if e.up == nil {
		return AuthorizedWindow{}, fmt.Errorf("cert-exec: uplink 未配置")
	}
	ref := fmt.Sprintf("prp:cert-renewal:%s:%dQ%d", host, now.Year(), (int(now.Month())-1)/3+1)
	dec, err := e.up.FetchDecision(ctx, ref)
	if err != nil {
		return AuthorizedWindow{ProposalRef: ref}, fmt.Errorf("cert-exec: 查询决议失败: %w", err)
	}
	if dec.Decision != "approved" {
		return AuthorizedWindow{ProposalRef: ref}, fmt.Errorf("cert-exec: 决议为 %s（未批准），拒绝执行", dec.Decision)
	}
	if !now.Before(dec.WindowTo) {
		return AuthorizedWindow{ProposalRef: ref}, fmt.Errorf("cert-exec: 授权窗口已于 %s 过期", dec.WindowTo.Format(time.RFC3339))
	}
	return AuthorizedWindow{ProposalRef: ref, Approved: true, From: dec.WindowFrom, To: dec.WindowTo}, nil
}

// Renew 执行一次证书轮换（须先 CheckAuthorization 通过）。
// 白名单：只调 certbot renew 指定 cert-name + deploy-hook 触发网关重载。
func (e *CertExecutor) Renew(ctx context.Context, host string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := e.run(cctx, e.certbot, "renew", "--cert-name", host, "--deploy-hook", e.reloadCmd)
	if err != nil {
		e.log.Warn("cert-exec: renew failed", "host", host, "err", err.Error(), "out", truncateStr(out, 300))
		return out, fmt.Errorf("cert-exec: renew: %w", err)
	}
	e.log.Info("cert-exec: renewed", "host", host)
	return out, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// DeployCert 部署型轮换：从 inbox 目录取 {host}.crt/{host}.key 复制到
// TLS 终结容器的证书挂载目录，然后重启该容器完成生效。白名单：仅 cp + docker restart。
func (e *CertExecutor) DeployCert(ctx context.Context, host string) (string, error) {
	inbox := envOr("OPSKEEPER_CERT_INBOX", "/opt/opskeeper/certs")
	dest := envOr("OPSKEEPER_CERT_DEST", "/opt/opskeeper/agentteams-https-persistent")
	tlsContainer := envOr("OPSKEEPER_CERT_TLS_CONTAINER", "agentteams-https")
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// 1. 预检：证书文件存在且可解析（防部署半成品）
	src := fmt.Sprintf("%s/%s", inbox, host)
	if _, err := e.run(cctx, "openssl", "x509", "-in", src+".crt", "-noout", "-enddate"); err != nil {
		return "", fmt.Errorf("cert-exec: inbox 证书缺失或无效 (%s.crt): %w", src, err)
	}
	// 2. 落位（原子性靠 cp 到同目录 .new 再 mv？容器挂载目录简单 cp 即可，重启失败可回滚文件）
	if out, err := e.run(cctx, "cp", src+".crt", dest+"/"+host+".crt"); err != nil {
		return out, fmt.Errorf("cert-exec: copy crt: %w", err)
	}
	if out, err := e.run(cctx, "cp", src+".key", dest+"/"+host+".key"); err != nil {
		return out, fmt.Errorf("cert-exec: copy key: %w", err)
	}
	// 3. 重启 TLS 终结容器
	if out, err := e.run(cctx, "docker", "restart", tlsContainer); err != nil {
		return out, fmt.Errorf("cert-exec: restart %s: %w", tlsContainer, err)
	}
	e.log.Info("cert-exec: deployed", "host", host)
	return "deployed", nil
}
