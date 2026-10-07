// Package redis 是 Redis 中间件 Adapter。
//
// 本文件负责生命周期与入口：客户端、审批闸门、Diagnose 路由与工具注册。
// 写操作在 ops.go，SCAN 类采样在 scan.go。
package redis

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/secretbox"
)

const (
	defaultPoolSize = 10
	defaultTimeout  = 30 * time.Second

	// defaultScanLimit caps how many keys a big-key or hot-key sample walks.
	//
	// Redis has no index of key sizes, so the only way to find the largest
	// key is to visit keys and ask. On an instance with ten million keys
	// that is ten million round trips, and a diagnostic that saturates the
	// server it was called to rescue is not a diagnostic. The cap is
	// reported in the result rather than hidden, because a sample that
	// says "the biggest key I saw" and a scan that says "the biggest key"
	// are different claims.
	defaultScanLimit = 20000

	maxScanLimit = 200000
)

// Adapter 是 Redis Adapter 实现。
type Adapter struct {
	mu        sync.RWMutex
	conn      adapter.ConnectionSpec
	connected bool
	client    redis.UniversalClient
	// cluster records whether the client is a cluster client. It is kept
	// separately from the client's type because a failover is only a
	// meaningful request against a cluster, and asking the interface
	// whether it is a cluster is not possible without a type assertion at
	// every call site.
	cluster bool
}

// New 创建 Redis Adapter 实例（不连接）。
func New() *Adapter {
	return &Adapter{}
}

// NewWithClient wraps an already-built client.
//
// The client is owned by the caller: Close does not close it. This exists so
// a test can drive the real command path against miniredis — a fake that
// stubbed out this adapter's own methods would not execute a single Redis
// command, and the commands are the part that can destroy data.
func NewWithClient(client redis.UniversalClient, cluster bool) *Adapter {
	return &Adapter{client: client, connected: client != nil, cluster: cluster}
}

// Type 返回资源类型。
func (a *Adapter) Type() adapter.ResourceType {
	return adapter.TypeRedis
}

// Connect 建立客户端并验证可达。
//
// The scheme decides single vs cluster. It is read from the DSN rather than
// guessed from the host count, because "host1,host2" is also a legitimate
// way to write a single-instance URL's hostname list in some tools, and a
// client that guessed wrong would either fail every command or silently
// treat one node as the whole cluster.
func (a *Adapter) Connect(ctx context.Context, conn adapter.ConnectionSpec) error {
	if conn.PoolSize == 0 {
		conn.PoolSize = defaultPoolSize
	}
	if conn.Timeout == 0 {
		conn.Timeout = defaultTimeout
	}
	dsn, err := secretbox.Decrypt(conn.DSN)
	if err != nil {
		return fmt.Errorf("redis: decrypt DSN: %w", err)
	}
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return errors.New("redis: DSN is required; a client with no target cannot be probed")
	}

	client, cluster, err := buildClient(dsn, conn)
	if err != nil {
		return err
	}
	pingCtx, cancel := context.WithTimeout(ctx, conn.Timeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return fmt.Errorf("redis: ping: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	old := a.client
	a.client = client
	a.cluster = cluster
	a.conn = conn
	a.connected = true
	if old != nil && old != client {
		_ = old.Close()
	}
	return nil
}

// clusterScheme is the DSN scheme that selects a cluster client. The
// standard redis:// scheme stays single-instance, so an existing DSN in a
// config table keeps meaning what it meant.
const (
	clusterScheme    = "redis+cluster"
	clusterTLSScheme = "rediss+cluster"
)

func buildClient(dsn string, conn adapter.ConnectionSpec) (redis.UniversalClient, bool, error) {
	if strings.HasPrefix(dsn, clusterScheme+"://") || strings.HasPrefix(dsn, clusterTLSScheme+"://") {
		tls := strings.HasPrefix(dsn, clusterTLSScheme+"://")
		rest := strings.TrimPrefix(strings.TrimPrefix(dsn, clusterTLSScheme+"://"), clusterScheme+"://")
		// Split the authority off any path/query so a URL written as
		// redis+cluster://h1,h2/0 parses as hosts + db.
		authority := rest
		tail := ""
		if i := strings.IndexAny(rest, "/?"); i >= 0 {
			authority, tail = rest[:i], rest[i:]
		}
		if authority == "" {
			return nil, false, errors.New("redis: cluster DSN has no hosts")
		}
		addrs := strings.Split(authority, ",")
		for i := range addrs {
			addrs[i] = strings.TrimSpace(addrs[i])
			if addrs[i] == "" {
				return nil, false, fmt.Errorf("redis: cluster DSN has an empty host in %q", authority)
			}
		}
		opts := &redis.ClusterOptions{
			Addrs:        addrs,
			PoolSize:     conn.PoolSize,
			DialTimeout:  conn.Timeout,
			ReadTimeout:  conn.Timeout,
			WriteTimeout: conn.Timeout,
		}
		if tls {
			opts.TLSConfig = tlsConfigFor(conn)
		}
		if db := dbIndexFromTail(tail); db > 0 {
			// A cluster has one logical database. Accepting a non-zero
			// index silently would send every command to db 0 while the
			// operator believed otherwise.
			return nil, false, fmt.Errorf("redis: cluster DSN names database %d, but a cluster has only db 0", db)
		}
		return redis.NewClusterClient(opts), true, nil
	}

	opts, err := redis.ParseURL(dsn)
	if err != nil {
		// A bare "host:port" is a common shape in config tables and
		// ParseURL rejects it. Retrying with the scheme prepended is a
		// convenience, not a guess: the fallback only applies when the
		// string has no scheme at all.
		if strings.Contains(dsn, "://") {
			return nil, false, fmt.Errorf("redis: parse DSN: %w", err)
		}
		opts, err = redis.ParseURL("redis://" + dsn)
		if err != nil {
			return nil, false, fmt.Errorf("redis: parse DSN: %w", err)
		}
	}
	opts.PoolSize = conn.PoolSize
	opts.DialTimeout = conn.Timeout
	opts.ReadTimeout = conn.Timeout
	opts.WriteTimeout = conn.Timeout
	return redis.NewClient(opts), false, nil
}

func dbIndexFromTail(tail string) int {
	q := tail
	if i := strings.Index(q, "?"); i >= 0 {
		q = q[:i]
	}
	q = strings.TrimPrefix(q, "/")
	if q == "" {
		return 0
	}
	n := 0
	for _, r := range q {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// Close 关闭客户端。
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connected = false
	client := a.client
	a.client = nil
	if client == nil {
		return nil
	}
	if err := client.Close(); err != nil {
		return fmt.Errorf("redis: close: %w", err)
	}
	return nil
}

func (a *Adapter) handle() (redis.UniversalClient, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected || a.client == nil {
		return nil, adapter.ErrNotConnected
	}
	return a.client, nil
}

func (a *Adapter) isCluster() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cluster
}

// Health 健康检查：PING，测量延迟。
func (a *Adapter) Health(ctx context.Context) (*adapter.HealthStatus, error) {
	client, err := a.handle()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	if err := client.Ping(ctx).Err(); err != nil {
		return &adapter.HealthStatus{
			Status:    "down",
			Message:   "redis: ping failed: " + err.Error(),
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
		CheckedAt: time.Now(),
	}, nil
}

// ── Diagnose ───────────────────────────────────────────────────────────

const (
	catServerInfo    = "server_info"
	catKeyspace      = "keyspace"
	catBigKeys       = "big_keys"
	catHotKeys       = "hot_keys"
	catSlowLog       = "slow_log"
	catClients       = "clients"
	catBlocked       = "blocked_clients"
	catMemory        = "memory"
	catFragmentation = "fragmentation"
	catCluster       = "cluster"
	catConfig        = "config"
)

type diagnoseFunc func(ctx context.Context, a *Adapter, q adapter.DiagnoseQuery) ([]map[string]any, string, error)

// diagnoseRoutes maps a category to the function that answers it and the
// suggestion that goes with a non-empty answer.
var diagnoseRoutes = map[string]struct {
	run        diagnoseFunc
	summary    string
	suggestion string
}{
	catServerInfo: {run: diagnoseServerInfo, summary: "server identity and runtime counters"},
	catKeyspace:   {run: diagnoseKeyspace, summary: "per-database key counts"},
	catBigKeys: {run: diagnoseBigKeys, summary: "largest keys by sampled memory usage",
		suggestion: "redis.scan_and_delete is destructive; confirm the key is not a live cache entry before proposing removal"},
	catHotKeys: {run: diagnoseHotKeys, summary: "hottest keys by sampled LFU access frequency",
		suggestion: "OBJECT FREQ is only tracked under an LFU maxmemory-policy; a non-LFU instance reports the policy instead of a ranking"},
	catSlowLog: {run: diagnoseSlowLog, summary: "recent slow commands",
		suggestion: "redis.client_kill needs the client address the slow-log entry reports"},
	catClients: {run: diagnoseClients, summary: "connected clients"},
	catBlocked: {run: diagnoseBlocked, summary: "clients blocked on a slow command"},
	catMemory:  {run: diagnoseMemory, summary: "memory usage and allocator counters"},
	catFragmentation: {run: diagnoseFragmentation, summary: "memory fragmentation ratio",
		suggestion: "redis.memory_purge returns allocator-freed pages to the OS; it does not delete keys"},
	catCluster: {run: diagnoseCluster, summary: "cluster topology and slot coverage"},
	catConfig:  {run: diagnoseConfig, summary: "configuration parameters"},
}

func diagnoseCategoryNames() []string {
	names := make([]string, 0, len(diagnoseRoutes))
	for name := range diagnoseRoutes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Diagnose 通用诊断入口。
func (a *Adapter) Diagnose(ctx context.Context, q adapter.DiagnoseQuery) (*adapter.DiagnoseResult, error) {
	if _, err := a.handle(); err != nil {
		return nil, err
	}
	route, ok := diagnoseRoutes[q.Category]
	if !ok {
		return nil, fmt.Errorf("redis: unknown diagnose category %q (known: %s)",
			q.Category, strings.Join(diagnoseCategoryNames(), ", "))
	}
	start := time.Now()
	findings, note, err := route.run(ctx, a, q)
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(start).Milliseconds()
	suggestions := []string{}
	if route.suggestion != "" && len(findings) > 0 {
		suggestions = append(suggestions, route.suggestion)
	}
	summary := route.summary
	if note != "" {
		summary += ": " + note
	}
	return &adapter.DiagnoseResult{
		Category:    q.Category,
		Findings:    findings,
		Summary:     summary,
		Suggestions: suggestions,
		ElapsedMs:   elapsed,
	}, nil
}

// Collect 采集指标。
//
// Redis INFO is already an aggregate: there is no history in it, so Samples
// stays empty and the snapshot is reported in Metrics. A server that
// returned the same reading as a time series would let a dashboard draw a
// flat line and call it a trend.
func (a *Adapter) Collect(ctx context.Context, q adapter.CollectQuery) (*adapter.CollectResult, error) {
	client, err := a.handle()
	if err != nil {
		return nil, err
	}
	metrics := map[string]interface{}{}
	metadata := map[string]string{
		"source":    "redis INFO snapshot",
		"sampling":  "snapshot; not a time series",
		"requested": strings.Join(q.Metrics, ","),
	}
	raw, err := client.Info(ctx).Result()
	if err != nil {
		metadata["error"] = err.Error()
	} else {
		metrics["info"] = parseInfoSections(raw)
	}
	if n, err := client.DBSize(ctx).Result(); err == nil {
		metrics["db_size"] = n
	}
	return &adapter.CollectResult{
		Metrics:  metrics,
		Samples:  []map[string]interface{}{},
		Metadata: metadata,
	}, nil
}

// ── Execute ────────────────────────────────────────────────────────────

// Execute 受限执行（写操作需审批）。
//
// The approval check runs before the connection is resolved: a write that
// arrives unapproved is refused whether or not a server is reachable,
// because "this would have deleted my database if it had been pointed at
// the right host" is not a defence.
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
		Metadata:  map[string]string{"approved_by": op.ApprovedBy},
	}, nil
}

func (a *Adapter) dispatch(ctx context.Context, op string, p params) (int, string, bool, error) {
	switch op {
	case "memory_purge":
		return a.memoryPurge(ctx, p)
	case "client_kill":
		return a.clientKill(ctx, p)
	case "failover":
		return a.failover(ctx, p)
	case "config_set":
		return a.configSet(ctx, p)
	case "flushdb":
		return a.flushDB(ctx, p)
	case "scan_and_delete":
		return a.scanAndDelete(ctx, p)
	default:
		return 0, "", false, fmt.Errorf("%w: redis.%s", ErrUnknownOperation, op)
	}
}

// OpRiskLevel 返回 op 的风险等级。
func (a *Adapter) OpRiskLevel(op string) adapter.RiskLevel {
	switch op {
	case "memory_purge":
		// Returns freed pages to the allocator. It removes no key, which is
		// why it is the loop's safe, auto-approved remedy.
		return adapter.RiskL2SoftWrite
	case "config_set":
		return adapter.RiskL3HardWrite
	case "client_kill", "failover":
		return adapter.RiskL3HardWrite
	case "scan_and_delete":
		// Deletes real data, one key at a time, with no undo. It sits
		// below flushdb only because the floor is required and the batch
		// is capped, so one call cannot empty a keyspace.
		return adapter.RiskL3HardWrite
	case "flushdb":
		return adapter.RiskL4Destructive
	default:
		return adapter.RiskL1Diagnostic
	}
}

// ── RegisterTools ──────────────────────────────────────────────────────

// RegisterTools registers the Redis tool set into reg.
func RegisterTools(reg *registry.Registry, a *Adapter) error {
	tools := []registry.Tool{
		// L0 只读
		makeTool("redis.connect", adapter.RiskL0ReadOnly, "建立 Redis 连接（DSN 经 secretbox 解密）", nil, connectOp(a)),
		makeTool("redis.info", adapter.RiskL0ReadOnly, "server info（分段解析）", nil, readOp(a, runInfo)),
		makeTool("redis.dbsize", adapter.RiskL0ReadOnly, "当前 db 的 key 数量", nil, readOp(a, runDBSize)),
		makeTool("redis.cluster_info", adapter.RiskL0ReadOnly, "cluster 拓扑信息", nil, readOp(a, runClusterInfo)),
		makeTool("redis.config_get", adapter.RiskL0ReadOnly, "读取配置项",
			map[string]string{"parameter": "string!"}, readOp(a, runConfigGet)),
		// L1 诊断
		makeTool("redis.big_keys", adapter.RiskL1Diagnostic, "TOP N 大 key（SCAN 采样 + MEMORY USAGE）",
			map[string]string{"limit": "int", "scan_limit": "int"}, readOp(a, runBigKeys)),
		makeTool("redis.hot_keys", adapter.RiskL1Diagnostic, "TOP N 热 key（SCAN 采样 + OBJECT FREQ，需 LFU 淘汰策略）",
			map[string]string{"limit": "int", "scan_limit": "int"}, readOp(a, runHotKeys)),
		makeTool("redis.slow_log", adapter.RiskL1Diagnostic, "Redis 慢日志",
			map[string]string{"limit": "int"}, readOp(a, runSlowLog)),
		makeTool("redis.key_space", adapter.RiskL1Diagnostic, "各 db 的 key 分布", nil, readOp(a, runKeyspace)),
		makeTool("redis.memory_usage", adapter.RiskL1Diagnostic, "整体内存占用与分配器统计", nil, readOp(a, runMemory)),
		makeTool("redis.fragmentation_ratio", adapter.RiskL1Diagnostic, "内存碎片率", nil, readOp(a, runFragmentation)),
		makeTool("redis.client_list", adapter.RiskL1Diagnostic, "客户端连接列表", nil, readOp(a, runClients)),
		makeTool("redis.blocked_clients", adapter.RiskL1Diagnostic, "阻塞客户端", nil, readOp(a, runBlocked)),
		// L2/L3 写操作（需审批）
		makeTool("redis.memory_purge", adapter.RiskL2SoftWrite,
			"MEMORY PURGE：把分配器已释放的页归还 OS（不删除任何 key）", nil, writeOp(a, "memory_purge")),
		makeTool("redis.client_kill", adapter.RiskL3HardWrite, "终止一个客户端连接",
			map[string]string{"addr": "string!"}, writeOp(a, "client_kill")),
		makeTool("redis.failover", adapter.RiskL3HardWrite, "cluster 手动 failover（仅 cluster 模式）",
			map[string]string{"force": "bool"}, writeOp(a, "failover")),
		makeTool("redis.config_set", adapter.RiskL3HardWrite, "修改配置（需审批）",
			map[string]string{"parameter": "string!", "value": "string!"}, writeOp(a, "config_set")),
		// L4 破坏性
		makeTool("redis.scan_and_delete", adapter.RiskL3HardWrite,
			"按大小下限扫描并删除超大 key（min_bytes 必填，单次上限 100 个，支持 dry_run）",
			map[string]string{"min_bytes": "int!", "pattern": "string", "limit": "int", "scan_limit": "int", "dry_run": "bool"},
			writeOp(a, "scan_and_delete")),
		makeTool("redis.flushdb", adapter.RiskL4Destructive,
			"清空当前 db（删除全部 key，需审批 + 显式确认）",
			map[string]string{"confirm": "string!"}, writeOp(a, "flushdb")),
	}
	return reg.RegisterTools(adapter.TypeRedis, tools)
}

// handler is the body behind a registered tool.
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

func readOp(a *Adapter, run func(ctx context.Context, a *Adapter, args map[string]interface{}) ([]map[string]any, string, error)) handler {
	return func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		if _, err := a.handle(); err != nil {
			return nil, err
		}
		if args == nil {
			args = map[string]interface{}{}
		}
		rows, note, err := run(ctx, a, args)
		if err != nil {
			return nil, err
		}
		out := map[string]interface{}{"rows": rows, "count": len(rows)}
		if note != "" {
			out["note"] = note
		}
		return out, nil
	}
}

// writeOp routes through the approval gate.
//
// approved_by comes from the caller rather than being synthesised here: the
// broker knows who approved, and a handler that invented its own approver
// would turn the gate into a formality that still logs a name against a
// production change.
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
		return map[string]interface{}{
			"connected":  true,
			"status":     h.Status,
			"latency_ms": h.LatencyMs,
			"cluster":    a.isCluster(),
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

// Compile-time interface checks.
var (
	_ adapter.Adapter     = (*Adapter)(nil)
	_ adapter.OpRiskLevel = (*Adapter)(nil)
)
