// Command higress-console is the real Higress AI Gateway consumer-resolver
// service. It replaces bin/mock_higress.py with a persistent,
// admin-driven implementation.
//
// Sub-commands:
//
//	serve      run the HTTP server (default if no sub-command given)
//	bootstrap  register consumers from --jwt-dir=<dir>/*.jwt
//	version    print version
//
// Environment variables:
//
//	OPSKEEPER_HIGRESS_ADDR    listen address (default ":18001")
//	OPSKEEPER_HIGRESS_DB      SQLite database path (default ./data/higress.db)
//	OPSKEEPER_JWT_SECRET         HS256 secret (must match opskeeper's)
//	OPSKEEPER_HIGRESS_USER    admin user (default "admin")
//	OPSKEEPER_HIGRESS_PASS    admin password (default "higress-admin")
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"

	managermiddleware "github.com/vincent-wuhan/opskeeper/core/domains/server/middleware"
	"github.com/vincent-wuhan/opskeeper/core/manager/higress"
)

const version = "1.0.0-dev"

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("higress-console %s\n", version)
		return
	}
	sub := "serve"
	if len(os.Args) >= 2 && !strings.HasPrefix(os.Args[1], "-") {
		sub = os.Args[1]
	}

	switch sub {
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "bootstrap":
		os.Exit(runBootstrap(os.Args[2:]))
	case "version":
		fmt.Printf("higress-console %s\n", version)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown sub-command %q\n", sub)
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `usage: higress-console <serve|bootstrap|version> [flags]

sub-commands:
  serve                  run HTTP server (default)
  bootstrap              register consumers from --jwt-dir=<dir>
  version                print version`)
}

// ---------- serve ----------

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envDefault("OPSKEEPER_HIGRESS_ADDR", ":18001"), "listen address")
	dbPath := fs.String("db", envDefault("OPSKEEPER_HIGRESS_DB", "./data/higress.db"), "SQLite database path")
	_ = fs.Parse(args)

	secret, err := readJWTSecret()
	if err != nil {
		fmt.Fprintf(os.Stderr, "higress-console: %v\n", err)
		return 1
	}
	adminUser := envDefault("OPSKEEPER_HIGRESS_USER", "admin")
	adminPass := envDefault("OPSKEEPER_HIGRESS_PASS", "higress-admin")

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "higress-console: mkdir: %v\n", err)
		return 1
	}
	store, err := higress.NewStore(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "higress-console: store: %v\n", err)
		return 1
	}
	defer store.Close()

	// The gateway's own audit trail. Mounted here rather than inside the
	// higress package because installing it is a decision about this
	// *process* — routes/routeaudit's entry-point table judges that
	// decision per binary, and a package cannot make it for every deployment
	// that links it (decision 321, 324).
	auditSink, err := buildAuditSink(store.DB(), slog.Default())
	if err != nil {
		fmt.Fprintf(os.Stderr, "higress-console: audit sink: %v\n", err)
		return 1
	}

	srv, err := higress.NewServer(higress.Config{
		Addr:      *addr,
		Store:     store,
		JWTSecret: secret,
		// 决策 328：把这条链交给它的路由，让运维能问「我这条链还完整吗」。
		// 立链是 324 做的，验证端是这一刀补的——**在那之前这条链从来没有被
		// 走过一遍**，而下面那段注释还写着「two verifiers」。
		Chain:         auditSink,
		AdminUser:     adminUser,
		AdminPassword: adminPass,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "higress-console: server: %v\n", err)
		return 1
	}

	httpSrv := &http.Server{
		Addr: *addr,
		// AuditMiddleware installs the per-request slot the handlers in
		// package higress write into. Without this line every SetAuditEvent
		// below it is a documented no-op, and no test in this repository can
		// see that, because a no-op and a working write look identical from
		// inside the handler.
		Handler:           managermiddleware.AuditMiddleware(auditSink)(srv.Routes()),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// 决策 330：网关这条链此前**没有任何保留期**——每天增长而从不被清理，
	// 一个长期运行的网关会把它自己的 SQLite 磁盘写满，而一个写满的审计库在
	// 运维眼里和「审计没开」是一回事。
	//
	// 默认关闭，与控制面同语义（0 = 不清理），**这不是疏忽而是一个要被看见的
	// 选择**：默认开启等于替运维决定多少天的审计历史可以不要，而审计历史恰恰
	// 是别人不会愿意替他决定的东西。要开就明写。
	retentionCtx, stopRetention := context.WithCancel(context.Background())
	defer stopRetention()
	retentionDays := higressAuditRetentionDays()
	if retentionDays > 0 {
		fmt.Printf("[higress-console] audit retention: %d days\n", retentionDays)
		go func() { _ = auditSink.RunRetention(retentionCtx, retentionDays) }()
	} else {
		fmt.Println("[higress-console] audit retention: off (set OPSKEEPER_HIGRESS_AUDIT_RETENTION_DAYS to bound the chain)")
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("[higress-console] listening on %s  db=%s  version=%s\n", *addr, *dbPath, version)
		errCh <- httpSrv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "higress-console: %v\n", err)
			return 1
		}
	case <-stop:
		fmt.Println("[higress-console] shutting down…")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}
	return 0
}

// ---------- bootstrap ----------

func runBootstrap(args []string) int {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	jwtDir := fs.String("jwt-dir", envDefault("OPSKEEPER_JWT_DIR", "/tmp/jwt"), "directory containing <role>.jwt files")
	dbPath := fs.String("db", envDefault("OPSKEEPER_HIGRESS_DB", "./data/higress.db"), "SQLite database path")
	apiURL := fs.String("api", envDefault("HIGRESS_CONSOLE_URL", "http://127.0.0.1:18001"), "higress console URL for admin login")
	adminUser := fs.String("user", envDefault("OPSKEEPER_HIGRESS_USER", "admin"), "admin username")
	adminPass := fs.String("pass", envDefault("OPSKEEPER_HIGRESS_PASS", "higress-admin"), "admin password")
	insecure := fs.Bool("insecure", false, "skip HMAC pre-verify (still sends to admin)")
	_ = fs.Parse(args)

	secret, err := readJWTSecret()
	if err != nil {
		fmt.Fprintf(os.Stderr, "higress-console: %v\n", err)
		return 1
	}

	entries, err := os.ReadDir(*jwtDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "higress-console: read jwt-dir: %v\n", err)
		return 1
	}

	store, err := higress.NewStore(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "higress-console: store: %v\n", err)
		return 1
	}
	defer store.Close()

	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jwt") {
			continue
		}
		role := strings.TrimSuffix(e.Name(), ".jwt")
		token, err := os.ReadFile(filepath.Join(*jwtDir, e.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: read failed: %v\n", role, err)
			continue
		}
		tokenStr := strings.TrimSpace(string(token))

		// HMAC verify up front so we don't register garbage.
		if !*insecure {
			if _, ok := verifyHMAC(tokenStr, secret); !ok {
				fmt.Fprintf(os.Stderr, "  %s: JWT signature invalid against OPSKEEPER_JWT_SECRET — skipped\n", role)
				continue
			}
		}

		claims, _ := decodeClaims(tokenStr)
		worker, roleClaim, tenant := "", role, ""
		if svc, ok := claims["agentteams_service"].(map[string]any); ok {
			if w, ok := svc["worker"].(string); ok {
				worker = w
			}
			if r, ok := svc["role"].(string); ok {
				roleClaim = r
			}
			if t, ok := svc["tenant_id"].(string); ok {
				tenant = t
			}
		}
		name := "opskeeper-" + roleClaim
		c := higress.Consumer{
			Name:         name,
			JWTRequired:  true,
			WorkerClaim:  worker,
			RoleClaim:    roleClaim,
			TenantClaim:  tenant,
			MetadataJSON: fmt.Sprintf(`{"source":"bootstrap","file":%q}`, e.Name()),
		}
		if err := store.Upsert(context.Background(), c, tokenStr); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: upsert failed: %v\n", role, err)
			continue
		}
		count++
		fmt.Printf("  registered %s → apikey_hash=%s worker=%s tenant=%s\n",
			name, higress.Fingerprint(tokenStr)[:16], worker, tenant)
	}
	fmt.Printf("[higress-console] bootstrap: %d consumers registered (db=%s)\n", count, *dbPath)

	// Also notify the running server via admin API so its in-process store
	// matches the on-disk one (the server reads from disk, but a health
	// check confirms roundtrip).
	if err := pingServer(*apiURL, *adminUser, *adminPass); err != nil {
		fmt.Fprintf(os.Stderr, "[higress-console] server ping failed: %v (the bootstrap is in the DB; restart the server to see live metrics)\n", err)
	}
	return 0
}

// pingServer does a /session/login + /admin/consumers roundtrip.
func pingServer(apiURL, user, pass string) error {
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, user, pass)
	resp, err := http.Post(apiURL+"/session/login", "application/json", strings.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("login HTTP %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "" || c.Value == "" {
			continue
		}
		req, _ := http.NewRequest("GET", apiURL+"/admin/consumers", nil)
		req.AddCookie(c)
		r2, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		r2.Body.Close()
		if r2.StatusCode/100 != 2 {
			return fmt.Errorf("admin list HTTP %d", r2.StatusCode)
		}
		return nil
	}
	return errors.New("no session cookie")
}

// ---------- helpers ----------

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func readJWTSecret() ([]byte, error) {
	secret := os.Getenv("OPSKEEPER_JWT_SECRET")
	if secret == "" {
		return nil, errors.New("OPSKEEPER_JWT_SECRET is required")
	}
	if len(secret) < 16 {
		return nil, fmt.Errorf("OPSKEEPER_JWT_SECRET must be at least 16 chars (got %d)", len(secret))
	}
	return []byte(secret), nil
}

// verifyHMAC + decodeClaims are local helpers — they exercise the same
// algorithm path the running server uses internally.
func verifyHMAC(token string, secret []byte) (jwt.MapClaims, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	h := hmacSum(parts[0]+"."+parts[1], secret)
	if h != parts[2] {
		fmt.Fprintf(os.Stderr, "[debug] hmac mismatch\n  got:  %s\n  want: %s\n  secret_len=%d\n", h, parts[2], len(secret))
		return nil, false
	}
	claims, err := decodeClaims(token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[debug] decodeClaims: %v\n", err)
		return nil, false
	}
	return claims, true
}

func hmacSum(input string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(input))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func decodeClaims(token string) (jwt.MapClaims, error) {
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		return []byte(os.Getenv("OPSKEEPER_JWT_SECRET")), nil
	})
	if err != nil {
		return nil, err
	}
	if !parsed.Valid {
		return nil, errors.New("invalid token")
	}
	c, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("claims not map")
	}
	return c, nil
}

// dumpJSON is used in --verbose mode (not wired yet).
func dumpJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// higressAuditRetentionDays reads the gateway's retention window.
//
// It returns 0 for "unset" and for "set to 0", which both mean "do not
// sweep" — the same reading the control plane gives OPSKEEPER_AUDIT_RETENTION_DAYS,
// deliberately, so that one mental model covers both processes. A value that
// does not parse is 0 as well, and the startup line says which of the three
// happened, because **一个被拼错的保留期变量静默地变成「不清理」，比它大声地
// 失败更贵**：磁盘会慢慢满，而没有人知道为什么。
func higressAuditRetentionDays() int {
	raw := os.Getenv("OPSKEEPER_HIGRESS_AUDIT_RETENTION_DAYS")
	if raw == "" {
		return 0
	}
	days, err := strconv.Atoi(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "higress-console: OPSKEEPER_HIGRESS_AUDIT_RETENTION_DAYS=%q is not a number; retention is off\n", raw)
		return 0
	}
	return days
}
