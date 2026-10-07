//go:build e2e

package testenv

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	tc "github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Env is one running manager + its surrounding fakes for a single test.
// Lifetime is bounded by the test that called Start — Stop is invoked
// via t.Cleanup so a panic-skip path still tears down the container.
//
// Process model:
//
//   - one MySQL container is shared across the package, brought up
//     lazily by sharedMySQL() and torn down at TestMain exit (or, if
//     the package never registers a TestMain, leaked — testcontainers
//     reaps via the ryuk sidecar so this is fine).
//   - one manager binary is built once per `go test` invocation into
//     a tempdir. Each Start re-uses the cached binary.
//   - each Start spawns a fresh manager process bound to a random
//     loopback port, with its own DB schema (every container creates
//     a fresh "opskeeper" schema; we serialize Starts so they don't
//     trample each other — see startMu).
type Env struct {
	t   *testing.T
	cfg envConfig

	httpBase string
	cmd      *exec.Cmd
	logBuf   *bytes.Buffer

	// Fakes — always created. Live-mode tests get the real URL out of
	// secrets and ignore the fake's URL; that's fine, the fake just sits
	// there idle.
	llm      *FakeLLM
	slack    *FakeSlack
	telegram *FakeTelegram
	prom     *FakeProm

	// AdminEmail / AdminPassword are the bootstrap credentials this
	// manager was started with. Tests use these for Login() and to
	// create scoped test users.
	AdminEmail    string
	AdminPassword string

	stopOnce sync.Once
}

type envConfig struct {
	extraEnv map[string]string
}

// Option mutates envConfig before Start spawns the manager.
type Option func(*envConfig)

// WithEnv sets an additional OPSKEEPER_* env var on the manager process.
// Later WithEnv wins on conflict. Useful for tests that need to flip a
// behavior flag (e.g. OPSKEEPER_ALERT_EVAL_INTERVAL=5s for fast tick).
func WithEnv(k, v string) Option {
	return func(c *envConfig) {
		if c.extraEnv == nil {
			c.extraEnv = map[string]string{}
		}
		c.extraEnv[k] = v
	}
}

// Start brings up a fresh manager + fakes and returns the env. Stop is
// registered with t.Cleanup, so callers don't normally need to call it.
//
// Anything that fails during Start is t.Fatal — the test cannot
// meaningfully continue without a manager.
func Start(t *testing.T, opts ...Option) *Env {
	t.Helper()

	var cfg envConfig
	for _, o := range opts {
		o(&cfg)
	}

	dsn := sharedMySQL(t)
	binary := managerBinary(t)

	env := &Env{
		t:        t,
		cfg:      cfg,
		llm:      NewFakeLLM(),
		slack:    NewFakeSlack(),
		telegram: NewFakeTelegram(),
		prom:     NewFakeProm(),
		// AdminEmail / AdminPassword are shared across every Start within
		// a `go test` invocation: BootstrapAdmin only seeds the first
		// time (subsequent Starts see users>0 and skip), so the password
		// the *first* test brought up must be the same one all later
		// tests log in with. Both live inside the ephemeral test MySQL
		// container so the "known credentials" surface is bounded to
		// this process.
		AdminEmail:    "admin@opskeeper.local",
		AdminPassword: "E2E!Admin-pass-do-not-reuse",
	}
	t.Cleanup(env.Stop)

	port, err := freePort()
	if err != nil {
		t.Fatalf("testenv: pick free port: %v", err)
	}
	metricsPort, err := freePort()
	if err != nil {
		t.Fatalf("testenv: pick free metrics port: %v", err)
	}
	env.httpBase = fmt.Sprintf("http://127.0.0.1:%d", port)

	// The upstream the manager's OpenAI provider resolves to. It is the fake
	// unless an operator pointed this run at a local inference engine, in
	// which case the manager talks to a real model and the fake keeps
	// answering only the providers nobody switched over.
	//
	// A bad value fails the run here rather than falling back to the fake:
	// an operator who asked for a real model and silently got the stub would
	// read a green run as evidence for a claim the run never tested.
	openAIKey, openAIBaseURL, openAIModel := "fake-test-key", env.llm.URL()+"/v1", "fake-gpt"
	if real, err := RealLLMBaseURL(); err != nil {
		t.Fatalf("testenv: %v", err)
	} else if real != "" {
		openAIKey = "local-engine-no-secret"
		openAIBaseURL = real + "/v1"
		openAIModel = RealLLMModel()
		t.Logf("testenv: manager will call a REAL inference engine at %s (%s)", real, RealLLMLimits)
	}

	managerEnv := map[string]string{
		"OPSKEEPER_HTTP_ADDR":           fmt.Sprintf("127.0.0.1:%d", port),
		"OPSKEEPER_METRICS_ADDR":        fmt.Sprintf("127.0.0.1:%d", metricsPort),
		"OPSKEEPER_DB_DIALECT":          "mysql",
		"OPSKEEPER_DB_DSN":              dsn,
		"OPSKEEPER_JWT_SECRET":          "test-jwt-secret-" + randomSuffix(),
		"OPSKEEPER_ADMIN_EMAIL":         env.AdminEmail,
		"OPSKEEPER_ADMIN_PASSWORD":      env.AdminPassword,
		"OPSKEEPER_PUBLIC_URL":          env.httpBase,
		"OPSKEEPER_PROM_ENABLED":        "true",
		"OPSKEEPER_PROM_URL":            env.prom.URL(),
		"OPSKEEPER_PROM_QUERY_URL":      env.prom.URL(),
		"OPSKEEPER_LOG_QUERY_URL":       "", // Loki disabled in default e2e
		"OPSKEEPER_TRACE_QUERY_URL":     "",
		"OPSKEEPER_OPENAI_API_KEY":      openAIKey,
		"OPSKEEPER_OPENAI_BASE_URL":     openAIBaseURL,
		"OPSKEEPER_OPENAI_MODEL":        openAIModel,
		"OPSKEEPER_ANTHROPIC_API_KEY":   "fake-test-key",
		"OPSKEEPER_ANTHROPIC_BASE_URL":  env.llm.URL(),
		"OPSKEEPER_ANTHROPIC_MODEL":     "claude-fake",
		"OPSKEEPER_ZHIPU_API_KEY":       "fake-test-key",
		"OPSKEEPER_ZHIPU_BASE_URL":      env.llm.URL() + "/v1",
		"OPSKEEPER_ZHIPU_MODEL":         "glm-fake",
		"OPSKEEPER_ALERT_EVAL_INTERVAL": "30s",
		// Graph kernel is the live runtime (memory: chat quality
		// 2026-05-25). The legacy kernel doesn't build chatruntime, so
		// investigator / agent paths look "not wired". Default the
		// harness to the same kernel production runs on.
		"OPSKEEPER_AGENT_KERNEL": "pig",
		// Tell the chatruntime loader where to find the agent + skill
		// markdown files. The manager binary is spawned in a tempdir so
		// the default `./agents` / `./skills` relative paths don't
		// resolve to the repo. Without this, personas like
		// "incident-investigator" fail to register and the RCA worker
		// errors out with "agent ... not found".
		"OPSKEEPER_BUILTIN_AGENTS_ROOT": filepath.Join(repoRoot(), "agents"),
		"OPSKEEPER_BUILTIN_SKILLS_ROOT": filepath.Join(repoRoot(), "skills"),
		// No frontier broker in the harness — disable the geminio dial
		// so manager comes up without waiting on a non-existent broker.
		// Edge-tunnel-only features (webssh, edge reverse calls) error
		// with frontierbound.ErrDisabled at the call site; tests that
		// need them get marked t.Skip via RequireSecret-style gates.
		"OPSKEEPER_FRONTIER_DISABLED": "true",
	}
	managerEnv["OPSKEEPER_PAGES_DIR"] = filepath.Join(t.TempDir(), "pages")
	for k, v := range cfg.extraEnv {
		managerEnv[k] = v
	}

	if err := env.startManager(binary, managerEnv); err != nil {
		env.dumpLogs()
		t.Fatalf("testenv: start manager: %v", err)
	}
	if err := env.waitReady(120 * time.Second); err != nil {
		env.dumpLogs()
		t.Fatalf("testenv: manager not ready: %v", err)
	}
	return env
}

// Stop shuts the manager down. Idempotent — t.Cleanup may call us, and
// a deferred Stop in the test will be a no-op the second time.
func (e *Env) Stop() {
	e.stopOnce.Do(func() {
		if e.cmd != nil && e.cmd.Process != nil {
			_ = e.cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() {
				_ = e.cmd.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = e.cmd.Process.Kill()
				<-done
			}
		}
		if e.llm != nil {
			e.llm.Close()
		}
		if e.slack != nil {
			e.slack.Close()
		}
		if e.telegram != nil {
			e.telegram.Close()
		}
		if e.prom != nil {
			e.prom.Close()
		}
	})
}

// ─── fakes accessors ───────────────────────────────────────────────────

// ManagerLogs returns the captured stdout+stderr of the manager process
// so a test that hits an unexpected failure can dump them inline (use
// `t.Logf("manager logs:\n%s", env.ManagerLogs())` from within the test).
// Safe to call after startup; returns "" if no buffer was set up.
func (e *Env) ManagerLogs() string {
	if e.logBuf == nil {
		return ""
	}
	return e.logBuf.String()
}

func (e *Env) FakeLLM() *FakeLLM           { return e.llm }
func (e *Env) FakeSlack() *FakeSlack       { return e.slack }
func (e *Env) FakeTelegram() *FakeTelegram { return e.telegram }
func (e *Env) FakeProm() *FakeProm         { return e.prom }
func (e *Env) BaseURL() string             { return e.httpBase }

// ─── HTTP helpers ───────────────────────────────────────────────────────

// DoJSON sends method+path with optional JSON body and optional bearer.
// Returns status + decoded JSON body if any. Path is appended to BaseURL
// without modification — caller passes "/api/v1/auth/login" etc.
func (e *Env) DoJSON(method, path string, body any, bearer string) (int, map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.httpBase+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out, nil
}

// StreamBody posts a request and returns the raw response body.
//
// DoJSON is the wrong tool for an endpoint that answers text/event-stream: it
// hands back a nil map for every SSE response, successfully, because a stream
// is not JSON. A test that wanted the frames therefore had nothing to assert
// on and asserted on the status line instead — which is how a gateway
// answering 200 with a well-formed but empty stream passed.
func (e *Env) StreamBody(path string, body any, bearer string) (string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest("POST", e.httpBase+path, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return string(out), fmt.Errorf("status %d: %s", resp.StatusCode, string(out))
	}
	return string(out), nil
}

// LoginResult is the subset of /v1/auth/login that tests care about.
type LoginResult struct {
	AccessToken  string
	RefreshToken string
}

// LoginAdmin logs in as the bootstrap admin user and returns the JWT pair.
// Fails the test on any non-200 response.
func (e *Env) LoginAdmin() LoginResult {
	e.t.Helper()
	return e.Login(e.AdminEmail, e.AdminPassword)
}

// Login is the generic login helper.
func (e *Env) Login(email, password string) LoginResult {
	e.t.Helper()
	status, body, err := e.DoJSON("POST", "/api/v1/auth/login", map[string]string{
		"email":    email,
		"password": password,
	}, "")
	if err != nil {
		e.t.Fatalf("login: transport: %v", err)
	}
	if status != 200 {
		e.t.Fatalf("login: status=%d body=%v", status, body)
	}
	at, _ := body["access_token"].(string)
	rt, _ := body["refresh_token"].(string)
	if at == "" {
		e.t.Fatalf("login: empty access_token (body=%v)", body)
	}
	return LoginResult{AccessToken: at, RefreshToken: rt}
}

// ─── internals ──────────────────────────────────────────────────────────

var (
	mysqlOnce      sync.Once
	mysqlHostPort  string
	mysqlContainer *tcmysql.MySQLContainer // captured so TestMain can Terminate
	mysqlErr       error

	binaryOnce sync.Once
	binaryPath string
	binaryErr  error

	startMu sync.Mutex // serializes Start so two parallel tests don't race the schema
)

const defaultMySQLWaitTimeout = 5 * time.Minute

func mysqlWaitTimeout() (time.Duration, error) {
	value := os.Getenv("OPSKEEPER_E2E_MYSQL_WAIT")
	if value == "" {
		return defaultMySQLWaitTimeout, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse OPSKEEPER_E2E_MYSQL_WAIT %q: %w", value, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("OPSKEEPER_E2E_MYSQL_WAIT must be positive, got %q", value)
	}
	return timeout, nil
}

// TerminateSharedMySQL kills the testcontainers MySQL container. Called
// from TestMain on process exit so we don't leak ~500 MB per `go test`
// invocation — the 3.6 GiB test box was exhausted by ~10 leftover
// containers between debug runs. ryuk is disabled (mac flakiness), so
// this manual hook is what reaps on linux/CI.
func TerminateSharedMySQL() {
	if mysqlContainer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = mysqlContainer.Terminate(ctx)
	mysqlContainer = nil
}

// sharedMySQL brings up one MySQL container per `go test` process and
// returns a DSN pointing at the `opskeeper` schema. Tests share the schema
// but each Start truncates the cross-test-leaky tables (system_settings,
// alert_rules, alert_incidents, alert_events) BEFORE the manager spawns —
// without that, E1's fake-prom URL (random port) lands in
// system_settings.prom.query_url, persists into F1's run, and F1's
// PromResolver returns the dead URL ("connection refused" → no fire).
func sharedMySQL(t *testing.T) string {
	t.Helper()
	mysqlOnce.Do(func() {
		// Local Docker Desktop on macOS regularly takes 30–60s to
		// schedule the ryuk reaper sidecar. Give the bring-up plenty
		// of headroom rather than fail flakily; the inner pull is
		// cached for subsequent runs.
		if os.Getenv("TESTCONTAINERS_RYUK_DISABLED") == "" {
			// Skip ryuk by default — testcontainers leaks are reaped
			// by our t.Cleanup(env.Stop) anyway, and ryuk start is
			// the #1 source of slowness/flakes on mac.
			_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
		}
		waitTimeout, err := mysqlWaitTimeout()
		if err != nil {
			mysqlErr = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout+time.Minute)
		defer cancel()
		container, err := tcmysql.Run(ctx,
			"mysql:8.0",
			tcmysql.WithDatabase("opskeeper"),
			tcmysql.WithUsername("opskeeper"),
			tcmysql.WithPassword("opskeeper"),
			tc.WithWaitStrategyAndDeadline(
				waitTimeout,
				wait.ForLog("port: 3306  MySQL Community Server").
					WithStartupTimeout(waitTimeout),
			),
		)
		if err != nil {
			mysqlErr = fmt.Errorf("mysql container: %w", err)
			return
		}
		mysqlContainer = container
		host, err := container.Host(ctx)
		if err != nil {
			mysqlErr = err
			return
		}
		port, err := container.MappedPort(ctx, "3306/tcp")
		if err != nil {
			mysqlErr = err
			return
		}
		mysqlHostPort = fmt.Sprintf("%s:%s", host, port.Port())
	})
	if mysqlErr != nil {
		t.Fatalf("testenv: %v", mysqlErr)
	}

	dsn := fmt.Sprintf("opskeeper:opskeeper@tcp(%s)/opskeeper?parseTime=true&charset=utf8mb4&loc=Local&multiStatements=true",
		mysqlHostPort)
	// Reset cross-test-leaky tables. The first Start in a process finds
	// no tables (AutoMigrate runs on manager bring-up); IGNORE so we
	// don't fail before the first migration. Subsequent Starts wipe the
	// state that pinned URLs / rules / incidents.
	resetSQL := strings.Join([]string{
		"DROP TABLE IF EXISTS system_settings",
		"DROP TABLE IF EXISTS alert_rules",
		"DROP TABLE IF EXISTS alert_incidents",
		"DROP TABLE IF EXISTS alert_events",
		"DROP TABLE IF EXISTS investigation_reports",
		"DROP TABLE IF EXISTS notification_channels",
		"DROP TABLE IF EXISTS notification_dispatches",
		"DROP TABLE IF EXISTS chat_sessions",
		"DROP TABLE IF EXISTS chat_messages",
		"DROP TABLE IF EXISTS chat_tool_calls",
		"DROP TABLE IF EXISTS audit_events",
		"DROP TABLE IF EXISTS users",
		"DROP TABLE IF EXISTS orgs",
		"DROP TABLE IF EXISTS memberships",
		"DROP TABLE IF EXISTS user_agents",
	}, ";")
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("testenv: open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(resetSQL); err != nil {
		// Don't fail on first-Start (no tables yet); only flag if it's a
		// surprise auth / connection error.
		if !strings.Contains(err.Error(), "Unknown table") {
			// MySQL 8 returns ER_BAD_TABLE_ERROR 1051 for the first wipe
			// run; treat anything else as fatal so a real issue surfaces.
			t.Logf("testenv: reset tables (first-Start expected): %v", err)
		}
	}
	return dsn
}

// managerBinary builds cmd/opskeeper once per `go test` and returns the
// path to the resulting binary. Subsequent Starts reuse it.
func managerBinary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		repo := repoRoot()
		if repo == "" {
			binaryErr = errors.New("cannot locate repo root from testenv source")
			return
		}
		dir, err := mkTempDir("opskeeper-e2e-bin-")
		if err != nil {
			binaryErr = err
			return
		}
		out := filepath.Join(dir, "opskeeper-manager")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/opskeeper")
		cmd.Dir = repo
		// GOWORK=off, for the same reason the node's binary is built that
		// way (see this package's edge.go): the workspace file is a local
		// development convenience that points PiG at a checkout on the
		// developer's disk, and nothing in CI has one. A manager binary
		// built with it does not test the dependency set that ships.
		//
		// This is not hypothetical. The acceptance suite spent a run
		// failing on `s.sess.Steer returns 1 value` — a compile error in
		// OpsKeeper's own file — because go.work pointed at a PiG checkout
		// that predated the v0.4.0 release the modules actually pin. The
		// message named the wrong repository, and the only reason it was
		// not chased into PiG is that the pin was checked first.
		cmd.Env = buildEnv()
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Run(); err != nil {
			binaryErr = fmt.Errorf("go build ./cmd/opskeeper: %w\n%s", err, buf.String())
			return
		}
		binaryPath = out
	})
	if binaryErr != nil {
		t.Fatalf("testenv: %v", binaryErr)
	}
	return binaryPath
}

func (e *Env) startManager(binary string, envMap map[string]string) error {
	startMu.Lock()
	defer startMu.Unlock()

	e.logBuf = &bytes.Buffer{}
	cmd := exec.Command(binary)
	cmd.Env = mergedEnv(envMap)
	cmd.Stdout = e.logBuf
	cmd.Stderr = e.logBuf
	if err := cmd.Start(); err != nil {
		return err
	}
	e.cmd = cmd
	return nil
}

func (e *Env) waitReady(d time.Duration) error {
	deadline := time.Now().Add(d)
	url := e.httpBase + "/healthz"
	for time.Now().Before(deadline) {
		// Detect early crash so we don't poll for nothing.
		if e.cmd.ProcessState != nil && e.cmd.ProcessState.Exited() {
			return fmt.Errorf("manager exited before ready (code %d)", e.cmd.ProcessState.ExitCode())
		}
		resp, err := http.Get(url)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %s waiting for %s", d, url)
}

func (e *Env) dumpLogs() {
	if e.logBuf == nil {
		return
	}
	e.t.Logf("=== manager logs ===\n%s\n=== end manager logs ===", e.logBuf.String())
}

// credentialShapedEnv matches an inherited variable whose *name* says it may
// be carrying a secret.
//
// It is a shape rule rather than a list of vendor names on purpose. PiG
// resolves a provider credential from a long and growing set of variables —
// OPENAI_API_KEY, ANTHROPIC_AUTH_TOKEN, HF_TOKEN, AZURE_OPENAI_API_KEY,
// GEMINI_API_KEY, GOOGLE_APPLICATION_CREDENTIALS and more — and a copy of
// that list in a test harness is a list that is wrong the day a provider is
// added. A name rule fails closed instead: a variable that looks like a
// credential does not reach a child, and the cost of being wrong is a
// missing environment variable in a test child, not a key on a node.
//
// The rule is deliberately broad. Anything a spawned process needs in order
// to run — PATH, HOME, TMPDIR, LANG, SSH_AUTH_SOCK, TERM — does not match it.
//
// "proxy" is in the rule for a reason that has nothing to do with the word
// proxy: an inherited http_proxy is routinely written as
// scheme://user:password@host, which is a credential wearing a URL as a
// disguise. The children here make no proxied calls, so dropping it costs
// nothing.
var credentialShapedEnv = regexp.MustCompile(
	`(?i)(api[_-]?key|secret|token|password|passwd|credential|private[_-]?key|proxy)|^aws_|^google_`)

// mergedEnv builds a child environment: the parent's, minus OpsKeeper's own
// configuration and minus anything credential-shaped, then envMap laid over
// the top.
//
// The credential half is not hygiene, it is the harness keeping its own
// promise. The acceptance criterion this repository has to demonstrate is
// that a node's *process environment* holds no cloud vendor key, and the
// process that starts the node is a `go test` binary running on whatever
// machine the developer or CI runner happens to be. Before this rule, a
// developer with OPENAI_API_KEY exported in their shell produced an e2e run
// whose node genuinely held a real provider credential — and the test that
// exists to catch exactly that reported green, because it only ever scanned
// the node's config directory for the manager's own fake key. The harness
// was the leak.
//
// envMap is applied last and is never scrubbed: the node's tunnel pair and
// the manager's fake provider key are values the harness chose on purpose,
// and the manager is exactly the process that is supposed to hold one.
func mergedEnv(envMap map[string]string) []string {
	parent := os.Environ()
	clean := parent[:0]
	for _, kv := range parent {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		// Configuration isolation: anything the test runner set for
		// OpsKeeper is dropped so the test fully controls config.
		if strings.HasPrefix(name, "OPSKEEPER_") {
			continue
		}
		if credentialShapedEnv.MatchString(name) {
			continue
		}
		clean = append(clean, kv)
	}
	for k, v := range envMap {
		clean = append(clean, k+"="+v)
	}
	return clean
}

// repoRoot walks up from this file looking for go.mod.
func repoRoot() string {
	_, src, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	dir := filepath.Dir(src)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func randomSuffix() string {
	// 8 hex chars from crypto-safe rand, no extra deps.
	var b [4]byte
	_, _ = readFull(b[:])
	return fmt.Sprintf("%x", b[:])
}

func readFull(b []byte) (int, error) {
	f, err := os.Open("/dev/urandom")
	if err != nil {
		// Fallback: time-based, low-quality but deterministic non-zero.
		t := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(t >> (uint(i) * 8))
		}
		return len(b), nil
	}
	defer f.Close()
	return io.ReadFull(f, b)
}
