// Package postgres 是 PostgreSQL 中间件 Adapter。
//
// 本文件负责生命周期与入口：连接池、审批闸门、Diagnose 路由与工具注册。
// SQL 文本与标识符安全在 queries.go，变更类操作在 ops.go。
//
// 关联 spec：openspec/changes/unified-platform-base-selection/specs/middleware-adapter/spec.md
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/secretbox"
)

const (
	defaultPoolSize = 10
	defaultTimeout  = 30 * time.Second
	// defaultRowLimit caps how many rows a diagnostic returns when the caller
	// asks for none. An uncapped pg_stat_activity on a busy server is tens of
	// thousands of rows, and a model that has to read all of them to answer
	// "is anything blocked" produces a worse answer than one that reads the
	// first 200.
	defaultRowLimit = 200
	maxRowLimit     = 5000
)

// Adapter 是 PostgreSQL Adapter 实现。
type Adapter struct {
	mu        sync.RWMutex
	conn      adapter.ConnectionSpec
	connected bool
	db        *sql.DB
}

// New 创建 PG Adapter 实例（不连接）。
func New() *Adapter {
	return &Adapter{}
}

// NewWithDB wraps an already-open pool.
//
// The pool is owned by the caller: Close does not close it. This exists so a
// test — or a host that keeps its own pool for a fleet of tenants — can drive
// the real query, scan and exec paths without a live PostgreSQL and without
// the adapter taking a second connection to the same database.
func NewWithDB(db *sql.DB) *Adapter {
	return &Adapter{db: db, connected: db != nil}
}

// Type 返回资源类型。
func (a *Adapter) Type() adapter.ResourceType {
	return adapter.TypePostgres
}

// Connect 建立连接池并验证可达。
//
// The DSN is decrypted rather than parsed: ConnectionSpec carries it the way
// the rest of the platform stores secrets, and an adapter that quietly
// required plaintext would be a way to read a credential out of the config
// table. An empty DSN is refused here instead of producing a pool that fails
// on first use — a connection that never existed is a deployment fault, and
// reporting it at Connect is where an operator is still looking.
func (a *Adapter) Connect(ctx context.Context, conn adapter.ConnectionSpec) error {
	if conn.PoolSize == 0 {
		conn.PoolSize = defaultPoolSize
	}
	if conn.Timeout == 0 {
		conn.Timeout = defaultTimeout
	}
	dsn, err := secretbox.Decrypt(conn.DSN)
	if err != nil {
		return fmt.Errorf("postgres: decrypt DSN: %w", err)
	}
	if strings.TrimSpace(dsn) == "" {
		return errors.New("postgres: DSN is required; a pool with no target cannot be probed")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("postgres: open: %w", err)
	}
	db.SetMaxOpenConns(conn.PoolSize)
	db.SetMaxIdleConns(conn.PoolSize)
	db.SetConnMaxIdleTime(conn.Timeout)
	db.SetConnMaxLifetime(time.Hour)

	pingCtx, cancel := context.WithTimeout(ctx, conn.Timeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return fmt.Errorf("postgres: ping: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	old := a.db
	a.db = db
	a.conn = conn
	a.connected = true
	if old != nil && old != db {
		_ = old.Close()
	}
	return nil
}

// Close 关闭连接池。
//
// A pool that failed to open is already closed, and Close is called on the
// teardown path whether or not Connect succeeded, so this must tolerate a nil
// pool rather than turning a clean shutdown into a panic.
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connected = false
	db := a.db
	a.db = nil
	if db == nil {
		return nil
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("postgres: close: %w", err)
	}
	return nil
}

// handle returns the live pool, or ErrNotConnected.
//
// The pool is re-read under the lock on every call rather than captured once,
// so a Close that lands between a diagnostic being scheduled and executed
// produces ErrNotConnected instead of a query against a closed handle — which
// database/sql would report as "sql: database is closed", a message that
// names the driver instead of the deployment.
func (a *Adapter) handle() (*sql.DB, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected || a.db == nil {
		return nil, adapter.ErrNotConnected
	}
	return a.db, nil
}

// Health 健康检查：SELECT 1，测量延迟。
func (a *Adapter) Health(ctx context.Context) (*adapter.HealthStatus, error) {
	db, err := a.handle()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	if err := db.PingContext(ctx); err != nil {
		return &adapter.HealthStatus{
			Status:    "down",
			Message:   "postgres: ping failed: " + err.Error(),
			CheckedAt: time.Now(),
		}, nil
	}
	latency := time.Since(start)
	status := "healthy"
	// A pool that answers in half a second is answering; one that needs two
	// is not, and reporting it healthy would push the decision to the next
	// query instead of the one that measures.
	if latency > 2*time.Second {
		status = "degraded"
	}
	return &adapter.HealthStatus{
		Status:    status,
		LatencyMs: latency.Milliseconds(),
		CheckedAt: time.Now(),
	}, nil
}

// rowLimit resolves how many rows a diagnostic may return.
func rowLimit(q adapter.DiagnoseQuery) int {
	n := q.Limit
	if n <= 0 {
		n = defaultRowLimit
	}
	if n > maxRowLimit {
		return maxRowLimit
	}
	return n
}

func intArg(p map[string]interface{}, name string, def int) (int, error) {
	raw, ok := p[name]
	if !ok {
		return def, nil
	}
	v, err := toInt(raw)
	if err != nil {
		return 0, fmt.Errorf("postgres: %s: %w", name, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("postgres: %s must be positive, got %d", name, v)
	}
	return v, nil
}

// diagnoseRoute is one Diagnose category: the query it runs and the two
// reminders it attaches to the result.
//
// The suggestions are not decoration. Diagnose is what an investigator reads
// before it decides, and a category that found blocked sessions but did not
// say that pg.cancel_query is the next step leaves the model to invent one.
type diagnoseRoute struct {
	query      string
	bind       func(q adapter.DiagnoseQuery) ([]any, error)
	summary    string
	suggestion string
	// emptySuggestion is the answer for a category whose empty result is
	// itself a finding. A route with no rows and no message leaves the
	// reader to decide whether it means "nothing wrong" or "nothing to
	// read", and those are opposite conclusions.
	emptySuggestion string
}

// Diagnose 通用诊断入口。
func (a *Adapter) Diagnose(ctx context.Context, q adapter.DiagnoseQuery) (*adapter.DiagnoseResult, error) {
	if _, err := a.handle(); err != nil {
		return nil, err
	}
	route, ok := diagnoseRoutes[q.Category]
	if !ok {
		return nil, fmt.Errorf("postgres: unknown diagnose category %q (known: %s)",
			q.Category, strings.Join(diagnoseCategoryNames(), ", "))
	}
	// bind is nil for the routes whose query takes no parameters. Calling it
	// unconditionally is a nil call on a func value, which panics rather than
	// returning an error — the worst possible failure for a route table whose
	// whole job is to make the next statement obvious.
	var args []any
	if route.bind != nil {
		var err error
		args, err = route.bind(adapter.DiagnoseQuery{Limit: rowLimit(q), Params: q.Params})
		if err != nil {
			return nil, err
		}
	}
	start := time.Now()
	rows, err := a.queryRows(ctx, route.query, args...)
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(start).Milliseconds()

	suggestions := []string{}
	switch {
	case len(rows) > 0 && route.suggestion != "":
		suggestions = append(suggestions, route.suggestion)
	case len(rows) == 0 && route.emptySuggestion != "":
		suggestions = append(suggestions, route.emptySuggestion)
	}
	summary := route.summary
	if n := len(rows); n == 1 {
		summary += " (1 row)"
	} else {
		summary += fmt.Sprintf(" (%d rows)", n)
	}
	return &adapter.DiagnoseResult{
		Category:    q.Category,
		Findings:    rows,
		Summary:     summary,
		Suggestions: suggestions,
		ElapsedMs:   elapsed,
	}, nil
}

// Collect 采集指标。
//
// CollectResult is shaped for a time series, and a PostgreSQL catalog view is
// a snapshot: there is no history here to put in Samples. Reporting the
// current values as Metrics and saying so in Metadata is honest; filling
// Samples with the same snapshot would let a dashboard draw a flat line and
// call it a time series.
func (a *Adapter) Collect(ctx context.Context, q adapter.CollectQuery) (*adapter.CollectResult, error) {
	if _, err := a.handle(); err != nil {
		return nil, err
	}
	metrics := map[string]interface{}{}
	var samplerErr error
	// Version and size answer "is this the server I think it is" and "how
	// much is it carrying" — the two things every other reading is relative
	// to, and both come back in one round trip.
	if rows, err := a.queryRows(ctx, `SELECT version() AS version,
       current_database() AS database,
       current_user AS user,
       pg_postmaster_start_time() AS started_at,
       (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend') AS connections,
       (SELECT coalesce(sum(numbackends), 0) FROM pg_stat_database) AS total_backends`); err == nil {
		if len(rows) == 1 {
			metrics = rows[0]
		}
	} else {
		samplerErr = err
	}
	// The requested metric names are echoed rather than dropped. A caller
	// that asked for connections and got none under that name has to
	// conclude the whole collection failed, when in fact the name is present
	// one level up.
	metadata := map[string]string{
		"source":    "postgres catalog snapshot",
		"sampling":  "snapshot; not a time series",
		"requested": strings.Join(q.Metrics, ","),
	}
	if samplerErr != nil {
		metadata["error"] = samplerErr.Error()
	}
	return &adapter.CollectResult{
		Metrics:  metrics,
		Samples:  []map[string]interface{}{},
		Metadata: metadata,
	}, nil
}

// Execute 受限执行（写操作需审批）。
//
// The approval check is the first thing that runs, before the connection is
// even resolved. A write that arrives unapproved is refused whether or not
// the adapter is connected, because "this platform would have killed my
// session if it had been pointed at the right database" is not a defence.
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
	impacted, message, ok, err := a.dispatch(ctx, op.Operation, p)
	if err != nil {
		return nil, err
	}
	return &adapter.ExecResult{
		Operation: op.Operation,
		Success:   ok,
		Message:   message,
		Impacted:  impacted,
		Metadata: map[string]string{
			"approved_by": op.ApprovedBy,
		},
	}, nil
}

// dispatch routes an approved operation to its implementation.
//
// The verdict is returned rather than assumed. A whole-database VACUUM that
// legitimately affects zero named tables succeeds with Impacted 0, while a
// pg_terminate_backend that returned false did not do what it was asked — so
// Success cannot be derived from Impacted, and each operation has to report
// its own.
func (a *Adapter) dispatch(ctx context.Context, op string, p params) (int, string, bool, error) {
	switch op {
	case "kill_session":
		return a.killSession(ctx, p)
	case "cancel_query":
		return a.cancelQuery(ctx, p)
	case "terminate_long_tx":
		return a.terminateLongTx(ctx, p)
	case "connection_pause":
		return a.connectionPause(ctx, p)
	case "connection_resume":
		return a.connectionResume(ctx, p)
	case "vacuum_analyze":
		return a.vacuumAnalyze(ctx, p)
	case "vacuum_table":
		return a.vacuumTable(ctx, p)
	case "analyze_table":
		return a.analyzeTable(ctx, p)
	default:
		return 0, "", false, fmt.Errorf("%w: pg.%s", ErrUnknownOperation, op)
	}
}

// OpRiskLevel 返回 op 的风险等级。
//
// The mapping is not the same as the tool registration below, and the
// difference is deliberate: as a tool, terminate_long_tx and vacuum_analyze
// sit at L3 because they reach a production database. As a remediation option
// the closed loop grades them itself — vacuum_analyze is the one action it
// marks safe and auto-approve — so a second, coarser ladder here would force
// an operator to approve the one action the platform already decided was
// harmless.
func (a *Adapter) OpRiskLevel(op string) adapter.RiskLevel {
	switch op {
	case "vacuum_analyze", "vacuum_table", "analyze_table":
		return adapter.RiskL2SoftWrite
	case "kill_session", "cancel_query", "terminate_long_tx", "connection_pause":
		return adapter.RiskL3HardWrite
	case "connection_resume":
		return adapter.RiskL2SoftWrite
	default:
		return adapter.RiskL1Diagnostic
	}
}

// RegisterTools registers the PG tool set into reg.
//
// The adapter is a parameter rather than a package global because the tool
// handlers run without one: by the time a model calls pg.vacuum_analyze, the
// call has come through the broker as ctx + args, and the connection it
// belongs to is whatever the caller wired in. A global would make the tool
// set correct in exactly one tenant.
func RegisterTools(reg *registry.Registry, a *Adapter) error {
	tools := []registry.Tool{
		// L0 只读
		makeTool("pg.connect", adapter.RiskL0ReadOnly, "建立 PG 连接（DSN 经 secretbox 解密）", nil, connectOp(a)),
		makeTool("pg.list_databases", adapter.RiskL0ReadOnly, "列出所有数据库（含大小与连接数）", nil, readOp(a, listDatabases)),
		makeTool("pg.list_schemas", adapter.RiskL0ReadOnly, "列出 schema（含表数量）", nil, readOp(a, listSchemas)),
		makeTool("pg.list_tables", adapter.RiskL0ReadOnly, "列出表（含大小估算）", nil, readOp(a, listTables)),
		// L1 诊断
		makeTool("pg.active_sessions", adapter.RiskL1Diagnostic, "列出当前活跃会话", nil, readOp(a, activeSessions)),
		makeTool("pg.long_running_txns", adapter.RiskL1Diagnostic, "列出长事务（阈值可配，默认 30s）",
			map[string]string{"min_age_seconds": "int"}, readOp(a, longRunningTxns)),
		makeTool("pg.top_queries_by_time", adapter.RiskL1Diagnostic, "按总耗时排序 Top N 慢查询",
			map[string]string{"limit": "int"}, readOp(a, topQueriesByTime)),
		makeTool("pg.top_queries_by_calls", adapter.RiskL1Diagnostic, "按调用次数排序 Top N 慢查询",
			map[string]string{"limit": "int"}, readOp(a, topQueriesByCalls)),
		makeTool("pg.lock_waits", adapter.RiskL1Diagnostic, "锁等待链（等待者 → 阻塞者）",
			map[string]string{"limit": "int"}, readOp(a, lockWaits)),
		makeTool("pg.table_bloat", adapter.RiskL1Diagnostic, "表膨胀估算（死元组占比）",
			map[string]string{"limit": "int"}, readOp(a, tableBloat)),
		makeTool("pg.index_usage", adapter.RiskL1Diagnostic, "索引使用率（未使用索引优先）",
			map[string]string{"limit": "int"}, readOp(a, indexUsage)),
		makeTool("pg.vacuum_status", adapter.RiskL1Diagnostic, "正在进行的 vacuum 进度", nil, readOp(a, vacuumStatus)),
		makeTool("pg.replication_status", adapter.RiskL1Diagnostic,
			"流复制状态：每个 standby 的 state / sync_state / 未应用 WAL 字节数与秒数。空结果=本实例没有连着的 standby（可能它自己就是 standby），不等于零延迟；replay_lsn 为 NULL 表示位置未知而非零",
			map[string]string{"limit": "int"}, readOp(a, replicationStatus)),
		makeTool("pg.slow_log", adapter.RiskL1Diagnostic, "慢查询（pg_stat_statements）",
			map[string]string{"min_ms": "int", "limit": "int"}, readOp(a, slowLog)),
		makeTool("pg.explain_query", adapter.RiskL1Diagnostic, "EXPLAIN（不带 ANALYZE，不执行语句）",
			map[string]string{"query": "string"}, readOp(a, explainQuery)),
		// L2/L3 写操作（需审批）
		makeTool("pg.kill_session", adapter.RiskL3HardWrite, "终止一个后端会话", map[string]string{"pid": "int!"}, writeOp(a, "kill_session")),
		makeTool("pg.cancel_query", adapter.RiskL3HardWrite, "取消一个后端正在执行的语句（保留会话）", map[string]string{"pid": "int!"}, writeOp(a, "cancel_query")),
		makeTool("pg.terminate_long_tx", adapter.RiskL3HardWrite, "终止所有超过阈值的长事务",
			map[string]string{"min_age_seconds": "int"}, writeOp(a, "terminate_long_tx")),
		makeTool("pg.connection_pause", adapter.RiskL3HardWrite, "暂停某角色的新连接并驱逐现存会话（CONNECTION LIMIT 0）",
			map[string]string{"role": "string!"}, writeOp(a, "connection_pause")),
		makeTool("pg.connection_resume", adapter.RiskL2SoftWrite, "恢复某角色的连接额度（CONNECTION LIMIT -1）",
			map[string]string{"role": "string!"}, writeOp(a, "connection_resume")),
		makeTool("pg.vacuum_analyze", adapter.RiskL2SoftWrite, "VACUUM (ANALYZE)，闭环建议的安全维护动作",
			map[string]string{"table": "string"}, writeOp(a, "vacuum_analyze")),
		makeTool("pg.vacuum_table", adapter.RiskL3HardWrite, "手动 VACUUM（不 ANALYZE）",
			map[string]string{"table": "string"}, writeOp(a, "vacuum_table")),
		makeTool("pg.analyze_table", adapter.RiskL3HardWrite, "ANALYZE 统计信息",
			map[string]string{"table": "string"}, writeOp(a, "analyze_table")),
	}
	return reg.RegisterTools(adapter.TypePostgres, tools)
}

// makeTool binds a handler to the adapter.
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

// handler is the body behind a registered tool.
type handler func(ctx context.Context, args map[string]interface{}) (interface{}, error)

// readOp builds a handler that runs one diagnostic query and returns rows.
func readOp(a *Adapter, run func(ctx context.Context, a *Adapter, args map[string]interface{}, limit int) ([]map[string]any, error)) handler {
	return func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		if _, err := a.handle(); err != nil {
			return nil, err
		}
		if args == nil {
			args = map[string]interface{}{}
		}
		limit, err := intArg(args, "limit", defaultRowLimit)
		if err != nil {
			return nil, err
		}
		rows, err := run(ctx, a, args, limit)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"rows": rows, "count": len(rows)}, nil
	}
}

// writeOp builds a handler that routes through the approval gate.
//
// approved_by is a tool argument rather than something the handler
// synthesises: the broker is the component that knows who approved, and a
// handler that invented its own approver would make the gate a formality
// while still logging a name against a production change.
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
		}, nil
	}
}

// connectOp is the one tool that creates the pool, so it resolves the adapter
// differently from every other: there is nothing to resolve yet.
func connectOp(a *Adapter) handler {
	return func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		dsn, _ := args["dsn"].(string)
		spec := adapter.ConnectionSpec{DSN: dsn}
		if n, err := intArg(args, "pool_size", 0); err == nil && n > 0 {
			spec.PoolSize = n
		}
		if err := a.Connect(ctx, spec); err != nil {
			return nil, err
		}
		h, err := a.Health(ctx)
		if err != nil {
			return nil, err
		}
		a.mu.RLock()
		pool := a.conn.PoolSize
		a.mu.RUnlock()
		return map[string]interface{}{
			"connected":  true,
			"status":     h.Status,
			"latency_ms": h.LatencyMs,
			"pool_size":  pool,
		}, nil
	}
}

func approver(args map[string]interface{}) string {
	s, _ := args["approved_by"].(string)
	return strings.TrimSpace(s)
}

func reason(args map[string]interface{}) string {
	s, _ := args["reason"].(string)
	return strings.TrimSpace(s)
}

// ── the individual reads ───────────────────────────────────────────────

func listDatabases(ctx context.Context, a *Adapter, args map[string]interface{}, limit int) ([]map[string]any, error) {
	return a.queryRows(ctx, qListDatabases)
}

func listSchemas(ctx context.Context, a *Adapter, args map[string]interface{}, limit int) ([]map[string]any, error) {
	return a.queryRows(ctx, qListSchemas)
}

func listTables(ctx context.Context, a *Adapter, args map[string]interface{}, limit int) ([]map[string]any, error) {
	return a.queryRows(ctx, qListTables)
}

func activeSessions(ctx context.Context, a *Adapter, args map[string]interface{}, limit int) ([]map[string]any, error) {
	rows, err := a.queryRows(ctx, qActiveSessions)
	if err != nil {
		return nil, err
	}
	return capRows(rows, limit), nil
}

func longRunningTxns(ctx context.Context, a *Adapter, args map[string]interface{}, limit int) ([]map[string]any, error) {
	age, err := intArg(args, "min_age_seconds", 30)
	if err != nil {
		return nil, err
	}
	rows, err := a.queryRows(ctx, qLongRunningTxns, age)
	if err != nil {
		return nil, err
	}
	return capRows(rows, limit), nil
}

func topQueriesByTime(ctx context.Context, a *Adapter, args map[string]interface{}, limit int) ([]map[string]any, error) {
	return a.queryRows(ctx, qTopQueriesByTime, limit)
}

func topQueriesByCalls(ctx context.Context, a *Adapter, args map[string]interface{}, limit int) ([]map[string]any, error) {
	return a.queryRows(ctx, qTopQueriesByCalls, limit)
}

func lockWaits(ctx context.Context, a *Adapter, args map[string]interface{}, limit int) ([]map[string]any, error) {
	return a.queryRows(ctx, qLockWaits, limit)
}

func tableBloat(ctx context.Context, a *Adapter, args map[string]interface{}, limit int) ([]map[string]any, error) {
	return a.queryRows(ctx, qTableBloat, limit)
}

func indexUsage(ctx context.Context, a *Adapter, args map[string]interface{}, limit int) ([]map[string]any, error) {
	return a.queryRows(ctx, qIndexUsage, limit)
}

func vacuumStatus(ctx context.Context, a *Adapter, args map[string]interface{}, limit int) ([]map[string]any, error) {
	rows, err := a.queryRows(ctx, qVacuumStatus)
	if err != nil {
		return nil, err
	}
	return capRows(rows, limit), nil
}

// replicationStatus reports every standby connected to this instance.
//
// The read is deliberately honest about the two ways it can come back empty
// or null, because both look like "healthy" and neither is.
//
// An empty result means this instance has no standbys connected — it is a
// standby itself, or a standalone, or every standby is down. Reporting
// "no lag" for any of those is the same number for three different situations,
// so the summary says what was actually observed.
//
// A standby whose replay_lsn is NULL is not at zero lag. It is one whose
// position is not yet known, which for a streaming standby usually means it
// is still catching up. The row keeps the NULL and the query orders those
// first, so a half-caught-up standby is at the top of the list rather than
// hidden by a zero.
func replicationStatus(ctx context.Context, a *Adapter, _ map[string]interface{}, limit int) ([]map[string]any, error) {
	rows, err := a.queryRows(ctx, qReplicationStatus)
	if err != nil {
		return nil, err
	}
	return capRows(rows, limit), nil
}

func slowLog(ctx context.Context, a *Adapter, args map[string]interface{}, limit int) ([]map[string]any, error) {
	minMs, err := intArg(args, "min_ms", 100)
	if err != nil {
		return nil, err
	}
	return a.queryRows(ctx, qSlowLog, float64(minMs), limit)
}

// explainQuery runs EXPLAIN without ANALYZE.
//
// The statement is caller-supplied, which is why this is the one read tool
// that takes SQL text: EXPLAIN is the only way to answer "why did the planner
// choose this" and there is no catalog alternative. It stays safe because
// ANALYZE is not requested — the plan is produced without running the
// statement — and because the length cap keeps a model from using it to
// smuggle in a script.
func explainQuery(ctx context.Context, a *Adapter, args map[string]interface{}, limit int) ([]map[string]any, error) {
	q, _ := args["query"].(string)
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, errors.New("postgres: explain_query requires a query")
	}
	if len(q) > 8000 {
		return nil, errors.New("postgres: explain_query: statement exceeds 8000 characters")
	}
	return a.queryRows(ctx, qExplain+q)
}

// capRows trims a result set that the query itself did not limit.
func capRows(rows []map[string]any, limit int) []map[string]any {
	if limit > 0 && len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

// Compile-time interface checks.
var (
	_ adapter.Adapter     = (*Adapter)(nil)
	_ adapter.OpRiskLevel = (*Adapter)(nil)
)
