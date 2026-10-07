// Package host 是 Host（节点主机）中间件 Adapter。
//
// The closed loop's host vocabulary is two actions — `host.garbage_collect`
// and `host.restart_service` — and until this package existed neither
// resolved to a tool, so a host incident was prescribed a remedy nothing in
// the build could carry out.
//
// Both are implemented against the host's own interfaces rather than a
// wrapper around a shell: `sysctl -w vm.drop_caches=` for the page cache and
// `systemctl` for units. Nothing here goes through `sh -c`, so a unit name or
// a path that contains a semicolon is an argument to a program rather than a
// second command. That matters more here than anywhere else in this
// platform, because a host adapter runs as root on the machine the platform
// exists to keep alive.
//
// Reachability is expressed as a DSN:
//
//	local://                          this process's own machine
//	ssh://user@host[:port]?key=/path  a machine reached over ssh
//
// The ssh path shells out to the `ssh` binary with BatchMode, which means a
// host that needs an interactive password fails immediately rather than
// hanging a remediation on a prompt nobody is there to answer.
package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/secretbox"
)

const (
	defaultTimeout = 30 * time.Second
	// maxOutputBytes bounds what one command may return. `df` on a host with
	// thousands of mounts will happily produce more than a model can read.
	maxOutputBytes = 1 << 20
)

// Adapter is the Host Adapter.
type Adapter struct {
	mu        sync.RWMutex
	conn      adapter.ConnectionSpec
	connected bool
	runner    runner
	// lookupEnv is os.LookupEnv, injectable so a test can pin the unit
	// allowlist without mutating the process environment.
	lookupEnv func(string) (string, bool)
}

// New creates an unconnected adapter.
func New() *Adapter {
	return &Adapter{lookupEnv: os.LookupEnv}
}

// NewWithRunner wraps an already-built runner. Test and host use.
func NewWithRunner(r runner) *Adapter {
	return &Adapter{runner: r, connected: r != nil, lookupEnv: os.LookupEnv}
}

// Type returns the resource type.
func (a *Adapter) Type() adapter.ResourceType { return adapter.TypeHost }

// Connect parses the DSN and verifies the host answers.
//
// The probe is `uname -s`: every host this adapter supports answers it, it
// needs no privilege, and its failure distinguishes "the host is unreachable"
// from "the host is reachable and the credential is wrong".
func (a *Adapter) Connect(ctx context.Context, conn adapter.ConnectionSpec) error {
	if conn.Timeout == 0 {
		conn.Timeout = defaultTimeout
	}
	dsn, err := secretbox.Decrypt(conn.DSN)
	if err != nil {
		return fmt.Errorf("host: decrypt DSN: %w", err)
	}
	r, err := newRunner(dsn, conn.Timeout)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, conn.Timeout)
	defer cancel()
	if _, err := r.run(probeCtx, []string{"uname", "-s"}); err != nil {
		return fmt.Errorf("host: %s is not reachable: %w", r.describe(), err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conn = conn
	a.runner = r
	a.connected = true
	return nil
}

// Close releases nothing: a runner holds no connection between calls.
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connected = false
	return nil
}

func (a *Adapter) handle() (runner, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected || a.runner == nil {
		return nil, adapter.ErrNotConnected
	}
	return a.runner, nil
}

// Health probes the host.
func (a *Adapter) Health(ctx context.Context) (*adapter.HealthStatus, error) {
	r, err := a.handle()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	out, err := r.run(ctx, []string{"uname", "-a"})
	if err != nil {
		return &adapter.HealthStatus{
			Status:    "down",
			Message:   "host: probe failed: " + err.Error(),
			CheckedAt: time.Now(),
		}, nil
	}
	latency := time.Since(start)
	status := "healthy"
	if latency > 2*time.Second {
		status = "degraded"
	}
	return &adapter.HealthStatus{
		Status:    status,
		LatencyMs: latency.Milliseconds(),
		Message:   r.describe() + ": " + strings.TrimSpace(string(out)),
		CheckedAt: time.Now(),
	}, nil
}

// Diagnose categories.
const (
	catMemory  = "memory"
	catLoad    = "load"
	catDisk    = "disk"
	catService = "service"
)

func diagnoseCategories() []string {
	return []string{catDisk, catLoad, catMemory, catService}
}

// Diagnose routes a category to its read.
func (a *Adapter) Diagnose(ctx context.Context, q adapter.DiagnoseQuery) (*adapter.DiagnoseResult, error) {
	if _, err := a.handle(); err != nil {
		return nil, err
	}
	p := params(q.Params)
	start := time.Now()
	var rows []map[string]any
	var summary string
	var err error
	switch q.Category {
	case catMemory:
		rows, summary, err = a.memInfo(ctx)
	case catLoad:
		rows, summary, err = a.loadAverage(ctx)
	case catDisk:
		rows, summary, err = a.diskUsage(ctx, p)
	case catService:
		rows, summary, err = a.serviceStatus(ctx, p)
	default:
		return nil, fmt.Errorf("host: unknown diagnose category %q (known: %s)", q.Category, strings.Join(diagnoseCategories(), ", "))
	}
	if err != nil {
		return nil, err
	}
	suggestions := []string{}
	if len(rows) > 0 {
		switch q.Category {
		case catMemory:
			suggestions = append(suggestions, "host.garbage_collect returns reclaimable page cache to the free pool without touching any process")
		case catLoad:
			suggestions = append(suggestions, "host.service_status names the unit; host.restart_service restarts one allowlisted unit")
		case catDisk:
			suggestions = append(suggestions, "a full filesystem usually needs the log or image consumer named in the mount's row, not a delete")
		case catService:
			suggestions = append(suggestions, "host.restart_service restarts one allowlisted unit and reports whether it came back")
		}
	}
	return &adapter.DiagnoseResult{
		Category:    q.Category,
		Findings:    rows,
		Summary:     summary,
		Suggestions: suggestions,
		ElapsedMs:   time.Since(start).Milliseconds(),
	}, nil
}

// Collect samples the host's headline figures.
func (a *Adapter) Collect(ctx context.Context, q adapter.CollectQuery) (*adapter.CollectResult, error) {
	if _, err := a.handle(); err != nil {
		return nil, err
	}
	metrics := map[string]interface{}{}
	var samplerErr error
	if rows, _, err := a.memInfo(ctx); err == nil && len(rows) == 1 {
		for k, v := range rows[0] {
			metrics["mem_"+k] = v
		}
	} else if err != nil {
		samplerErr = err
	}
	if rows, _, err := a.loadAverage(ctx); err == nil && len(rows) == 1 {
		for k, v := range rows[0] {
			metrics["load_"+k] = v
		}
	} else if err != nil && samplerErr == nil {
		samplerErr = err
	}
	metadata := map[string]string{
		"source":    "host /proc snapshot",
		"sampling":  "snapshot; not a time series",
		"requested": strings.Join(q.Metrics, ","),
	}
	if d := a.describe(); d != "" {
		metadata["host"] = d
	}
	if samplerErr != nil {
		metadata["error"] = samplerErr.Error()
	}
	return &adapter.CollectResult{Metrics: metrics, Samples: []map[string]interface{}{}, Metadata: metadata}, nil
}

func (a *Adapter) describe() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.runner == nil {
		return ""
	}
	return a.runner.describe()
}

// ErrUnknownOperation is returned for an operation this adapter does not
// implement.
var ErrUnknownOperation = errors.New("host: unknown operation")

// Execute routes an approved write.
//
// The approval check runs before the host is resolved: an unapproved restart
// is refused whether or not a host is configured, because "this would have
// restarted production if the DSN had been set" is not a defence.
func (a *Adapter) Execute(ctx context.Context, op adapter.ExecOp) (*adapter.ExecResult, error) {
	if strings.TrimSpace(op.ApprovedBy) == "" {
		return nil, adapter.ErrApprovalRequired
	}
	if _, err := a.handle(); err != nil {
		return nil, err
	}
	p := params(op.Params)
	if p == nil {
		p = params{}
	}
	var impacted int
	var message string
	var ok bool
	var argv []string
	var err error
	switch op.Operation {
	case "garbage_collect":
		impacted, message, ok, argv, err = a.garbageCollect(ctx, p)
	case "restart_service":
		impacted, message, ok, argv, err = a.restartService(ctx, p)
	case "kill_process":
		impacted, message, ok, argv, err = a.killProcess(ctx, p)
	case "remove_old_logs":
		impacted, message, ok, argv, err = a.removeOldLogs(ctx, p)
	default:
		return nil, fmt.Errorf("%w: host.%s", ErrUnknownOperation, op.Operation)
	}
	if err != nil {
		return nil, err
	}
	return &adapter.ExecResult{
		Operation: op.Operation,
		Success:   ok,
		Message:   message,
		Impacted:  impacted,
		Argv:      argv,
		Metadata:  map[string]string{"approved_by": op.ApprovedBy, "host": a.describe()},
	}, nil
}

// OpRiskLevel grades an operation.
//
// Both writes sit at L2 rather than L3. Neither destroys anything: dropping
// clean page cache returns memory without touching a process, and restarting
// a unit is the operation the unit's own supervisor is designed to survive.
// What makes them dangerous is *which* host and *which* unit, and that is the
// blast radius the approval carries, not a property of the verb.
func (a *Adapter) OpRiskLevel(op string) adapter.RiskLevel {
	switch op {
	case "garbage_collect", "restart_service":
		return adapter.RiskL2SoftWrite
	case "kill_process", "remove_old_logs":
		// Both destroy something the platform cannot put back: a running
		// process, and the bytes of a log file. Neither is recoverable by
		// retrying, so both sit above the operations a unit restart
		// survives, and both carry a human in the loop.
		return adapter.RiskL3HardWrite
	default:
		return adapter.RiskL1Diagnostic
	}
}

// ── tool registration ──────────────────────────────────────────────────

// RegisterTools registers the host tool set.
//
// host.garbage_collect takes no required argument, which is what makes it
// dispatchable from a RemediationOption: the closed loop marks it safe and
// auto-approves it, which means it runs with no human in the loop, and a tool
// that demanded an argument would only ever be refused. host.restart_service
// names a unit and therefore declares one.
func RegisterTools(reg *registry.Registry, a *Adapter) error {
	tools := []registry.Tool{
		makeTool("host.connect", adapter.RiskL0ReadOnly, "连接主机（local:// 或 ssh://，经 secretbox 解密）", nil, connectOp(a)),
		makeTool("host.mem_info", adapter.RiskL0ReadOnly, "内存 / swap 概况（/proc/meminfo）", nil, readOp(a, runMemInfo)),
		makeTool("host.load_average", adapter.RiskL0ReadOnly, "负载与 CPU 核数（/proc/loadavg）", nil, readOp(a, runLoadAverage)),
		makeTool("host.disk_usage", adapter.RiskL0ReadOnly, "文件系统使用率（df -Pk）",
			map[string]string{"path": "string"}, readOp(a, runDiskUsage)),
		makeTool("host.service_status", adapter.RiskL0ReadOnly, "systemd 单元状态（systemctl show）",
			map[string]string{"unit": "string!"}, readOp(a, runServiceStatus)),
		makeTool("host.garbage_collect", adapter.RiskL2SoftWrite,
			"回收主机 page cache / dentries / inodes（sync + vm.drop_caches），不触碰任何进程",
			map[string]string{"level": "int"}, writeOp(a, "garbage_collect")),
		makeTool("host.restart_service", adapter.RiskL2SoftWrite,
			"重启一个在允许列表内的 systemd 单元并确认其恢复",
			map[string]string{"unit": "string!"}, writeOp(a, "restart_service")),
		makeTool("host.top_processes", adapter.RiskL0ReadOnly,
			"按 CPU 占用排序列出进程（ps），只返回达到阈值的那些",
			map[string]string{"limit": "int"}, readOp(a, runTopProcesses)),
		makeTool("host.old_log_files", adapter.RiskL0ReadOnly,
			"列出目录下超过指定天数未改写的日志文件及大小",
			map[string]string{"path": "string", "older_than_days": "int", "limit": "int"}, readOp(a, runOldLogFiles)),
		makeTool("host.kill_process", adapter.RiskL3HardWrite,
			"向一个进程发送 SIGTERM 并读回它的状态（不自动升级到 SIGKILL）",
			map[string]string{"pid": "int!"}, writeOp(a, "kill_process")),
		makeTool("host.remove_old_logs", adapter.RiskL3HardWrite,
			"删除目录下超过指定天数未改写的日志文件；系统目录一律拒绝",
			map[string]string{"path": "string!", "older_than_days": "int", "dry_run": "bool"}, writeOp(a, "remove_old_logs")),
	}
	return reg.RegisterTools(adapter.TypeHost, tools)
}

type handler func(ctx context.Context, args map[string]interface{}) (interface{}, error)

func makeTool(name string, risk adapter.RiskLevel, desc string, schema map[string]string, h handler) registry.Tool {
	if schema == nil {
		schema = map[string]string{}
	}
	return registry.Tool{
		Name:        name,
		Description: desc,
		RiskLevel:   risk,
		ArgsSchema:  schema,
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			return h(ctx, args)
		},
	}
}

type readRun func(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error)

func readOp(a *Adapter, run readRun) handler {
	return func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		if _, err := a.handle(); err != nil {
			return nil, err
		}
		if args == nil {
			args = map[string]interface{}{}
		}
		rows, summary, err := run(ctx, a, args)
		if err != nil {
			return nil, err
		}
		if rows == nil {
			rows = []map[string]any{}
		}
		return map[string]interface{}{"rows": rows, "count": len(rows), "summary": summary}, nil
	}
}

func writeOp(a *Adapter, operation string) handler {
	return func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		res, err := a.Execute(ctx, adapter.ExecOp{
			Operation:  operation,
			Params:     args,
			ApprovedBy: approver(args),
			Reason:     reason(args),
		})
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"operation": res.Operation,
			"success":   res.Success,
			"message":   res.Message,
			"impacted":  res.Impacted,
			"argv":      res.Argv,
		}, nil
	}
}

func connectOp(a *Adapter) handler {
	return func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		dsn, _ := args["dsn"].(string)
		if err := a.Connect(ctx, adapter.ConnectionSpec{DSN: dsn}); err != nil {
			return nil, err
		}
		h, err := a.Health(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"connected": true, "host": a.describe(), "status": h.Status, "message": h.Message}, nil
	}
}

func approver(args map[string]interface{}) string {
	if v, ok := args["approved_by"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func reason(args map[string]interface{}) string {
	if v, ok := args["reason"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// ── runner ─────────────────────────────────────────────────────────────

// runner executes one argv-style command on a host.
//
// It is an interface rather than a function because the two transports
// differ in exactly one way — where the argv is sent — and every operation
// above is written once against this interface rather than twice against the
// transports.
type runner interface {
	run(ctx context.Context, argv []string) ([]byte, error)
	describe() string
}

// localRunner runs commands on this process's machine.
type localRunner struct {
	timeout time.Duration
}

func (l localRunner) describe() string { return "local" }

func (l localRunner) run(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("host: empty command")
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out boundedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ctx.Err() != nil {
		return out.Bytes(), fmt.Errorf("host: %s timed out after %s", argv[0], l.timeout)
	}
	if err != nil {
		// The exit status and whatever the command printed are both part of
		// the answer. "exit status 1" alone sends an operator nowhere;
		// "exit status 1: sysctl: permission denied" sends them to sudo.
		return out.Bytes(), fmt.Errorf("host: %s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(out.String()))
	}
	return out.Bytes(), nil
}

// sshRunner runs commands on a remote host through the ssh binary.
type sshRunner struct {
	user    string
	host    string // host or host:port
	keyPath string
	timeout time.Duration
}

func (s sshRunner) describe() string { return "ssh://" + s.user + "@" + s.host }

// sshArgs builds the argv for one remote command.
//
// It is a function rather than a few lines inside run so that the two
// properties that keep this safe can be tested without an ssh server:
// BatchMode is set (a password prompt becomes an immediate failure instead of
// a timeout nobody can explain), and the remote command follows `--` so that
// a leading dash in it cannot be read as an ssh option.
func (s sshRunner) sshArgs(argv []string) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
	}
	if s.keyPath != "" {
		args = append(args, "-i", s.keyPath)
	}
	args = append(args, s.user+"@"+s.host, "--")
	return append(args, argv...)
}

func (s sshRunner) run(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("host: empty command")
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", s.sshArgs(argv)...)
	var out boundedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ctx.Err() != nil {
		return out.Bytes(), fmt.Errorf("host: ssh to %s timed out after %s", s.host, s.timeout)
	}
	if err != nil {
		return out.Bytes(), fmt.Errorf("host: ssh %s: %w: %s", s.host, err, strings.TrimSpace(out.String()))
	}
	return out.Bytes(), nil
}

// boundedBuffer collects at most maxOutputBytes and reports truncation.
type boundedBuffer struct {
	buf       []byte
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(b.buf) >= maxOutputBytes {
		b.truncated = true
		return len(p), nil
	}
	room := maxOutputBytes - len(b.buf)
	if len(p) > room {
		b.buf = append(b.buf, p[:room]...)
		b.truncated = true
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf }

func (b *boundedBuffer) String() string {
	s := string(b.buf)
	if b.truncated {
		s += "\n[output truncated]"
	}
	return s
}

// unitName is the shape of a systemd unit this adapter will act on.
//
// It is a whitelist in the strict sense: the pattern admits one unit name
// and nothing else, so a value that arrives carrying a space, a flag or a
// path separator is refused rather than handed to systemctl.
//
// A leading '-' is excluded on purpose. `systemctl show -p X --now.service`
// is not an error: systemctl parses the second value as an option, and the
// same is true of `systemctl restart --now.service`. A name that can become a
// flag is not a name.
var unitName = regexp.MustCompile(`^[A-Za-z0-9:_.@][A-Za-z0-9:_.@\-]*\.service$`)

// newRunner builds a runner from a DSN.
func newRunner(dsn string, timeout time.Duration) (runner, error) {
	d := strings.TrimSpace(dsn)
	switch {
	case d == "local://" || d == "local":
		return localRunner{timeout: timeout}, nil
	case strings.HasPrefix(d, "ssh://"):
		rest := strings.TrimPrefix(d, "ssh://")
		authority, query, _ := strings.Cut(rest, "?")
		user := ""
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			user = authority[:at]
			authority = authority[at+1:]
		}
		if user == "" {
			return nil, errors.New("host: ssh DSN must carry a user (ssh://user@host)")
		}
		if authority == "" {
			return nil, errors.New("host: ssh DSN carries no host")
		}
		keyPath := ""
		if query != "" {
			values, err := parseQuery(query)
			if err != nil {
				return nil, err
			}
			keyPath = values["key"]
		}
		return sshRunner{user: user, host: authority, keyPath: keyPath, timeout: timeout}, nil
	case d == "":
		return nil, errors.New("host: DSN is required; use local:// or ssh://user@host")
	default:
		return nil, fmt.Errorf("host: unsupported DSN form %q; expected local:// or ssh://user@host[:port]", d)
	}
}

func parseQuery(q string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(q, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		if k == "" {
			return nil, fmt.Errorf("host: malformed DSN query %q", q)
		}
		out[k] = v
	}
	return out, nil
}

var _ adapter.Adapter = (*Adapter)(nil)
