// Package pg is the PostgreSQL fault injector.
//
// Decision 297 moved this tree out of core/harness, whose go.mod states
// "reaches no database" as an invariant. A lock chain is a set of sessions
// holding row locks; a slow query is a pg_sleep on a live connection; a held
// transaction is a BEGIN that never commits. None of those is a description
// of a fault — they are the fault, staged against a running database.
//
// What this file does NOT do is worth stating first, because the previous
// version of it did the opposite: it did not touch a database at all, it
// claimed `IsAvailable() == true`, and it returned an InjectID that meant
// nothing. A fault injector that reports success without staging anything
// is worse than no injector, because every score computed on top of it is
// evidence about nothing.
//
// Four rules the implementation below holds to:
//
//  1. A fault is observable from outside. Every one of them changes
//     pg_locks, pg_stat_activity or the table's on-disk state — something a
//     diagnosing agent can query and be right about.
//  2. It is reversible. Every Inject registers rollback steps, and
//     Cleanup runs them in reverse. A fault that cannot be undone is not a
//     fault injection, it is damage.
//  3. It expires on its own. InjectSpec.Duration has been a field since the
//     beginning and, until this revision, nothing read it. A fault that
//     outlives the case that asked for it corrupts every case after it.
//  4. It is scoped. It creates its own table when the case names one that
//     does not exist, and drops exactly that table on cleanup. It never
//     truncates, drops or rewrites a table it did not create.
package pg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// DSNEnv is where the injector looks for a connection string when the
// caller supplies none.
const DSNEnv = "OPSKEEPER_HARNESS_PG_DSN"

// connectTimeout bounds CheckAvailable. A fault injector that hangs is a
// fault injector that stalls the evaluation it was supposed to start.
const connectTimeout = 5 * time.Second

// supportedTypes 是这个注入器认识的全部注入类型。
//
// 它原先是 Inject 里一个 switch 的 case 列表。清单从"死代码"变成"数据"之后，
// 调用方能列出它，也能被测试逐条覆盖——上一版那份清单没有任何东西能读它。
var supportedTypes = map[string]bool{
	"pg.inject_lock_chain":  true,
	"pg.begin_txn_hold":     true,
	"pg.inject_replica_lag": true,
	"pg.run_slow_queries":   true,
	"pg.inject_table_bloat": true,
	"pg.hold_old_txn":       true,
	"pg.run_autovacuum":     true,
}

// replicaOnlyTypes 列出的是这个注入器**没有**实现的类型，以及为什么。
//
// 复制延迟不是一条能在单个节点上造出来的故障：它要的是一个真实的流复制
// 副本和一条被拖住的 WAL 发送进程。假装造得出来，只会让 pg/replication-lag
// 这个 case 在一个没有副本的机器上"通过"。
var replicaOnlyTypes = map[string]string{
	"pg.inject_replica_lag": "复制延迟需要一个真实的流复制副本与被拖住的 WAL 发送进程；" +
		"单节点上造不出来。造一个假的会让 pg/replication-lag 这个 case 在没有副本的机器上通过。",
}

// SupportedTypes 返回这个注入器认识的全部注入类型（已排序）。
//
// 它此前是一个 switch 的 case 列表——一份从不执行、也无法被查询的清单：
// 调用方想知道"这个 case 到底要注入什么"只能去读源码。
func SupportedTypes() []string {
	out := make([]string, 0, len(supportedTypes))
	for typ := range supportedTypes {
		out = append(out, typ)
	}
	sort.Strings(out)
	return out
}

// live 是一次正在进行的注入。
type live struct {
	id      string
	typ     string
	started time.Time
	// rollback 按注册顺序记录撤销步骤，Cleanup 逆序执行。
	rollback []func(context.Context) error
	// timer 到点自动撤销。**它就是 InjectSpec.Duration 第一次被读的地方。**
	timer *time.Timer
	// done 在撤销全部完成后关闭，让并发 Cleanup 等同一份结果而不是各做一次。
	done chan struct{}
	err  error
	// table 是这次注入动过的表（可能由它自己创建）。
	table     string
	createdBy bool
	// duration 是 spec.Duration 的副本：它是这次注入的寿命，
	// 也是 result 里 expires 的来源。
	duration time.Duration
	// ctx 是这次注入自己的生命周期。**它不能是调用方那个 ctx**：
	// Inject 返回之后调用方会把那个 ctx 取消掉，而一个正在等锁的语句
	// 一被取消，等待就消失了——注入会在"故障还在"的那一刻自己修好。
	ctx    context.Context
	cancel context.CancelFunc
}

// Injector is the PostgreSQL fault injector.
type Injector struct {
	dsn string
	// dsnSet 区分"没给"和"显式给了空"。
	dsnSet bool
	mu     sync.Mutex
	seq    int
	live   map[string]*live
}

// Option configures an Injector.
type Option func(*Injector)

// WithDSN supplies a PostgreSQL connection string.
//
// WithDSN("") 是**显式的"没有连接"**，它与"没调用这个 Option"必须能被分开：
// 单元测试要断言的正是"一个没有连接的注入器会拒绝"，而这台机器上
// 恰好配着 OPSKEEPER_HARNESS_PG_DSN 时，那条断言必须仍然指向它自己。
func WithDSN(dsn string) Option {
	return func(i *Injector) { i.dsn = dsn; i.dsnSet = true }
}

// **这个注入器不接受一个连接池，而且是刻意的。**
// 一次"持有事务"或"锁链"注入的整个含义就是：这条连接不能被别人拿去用，
// 直到它被撤销。把它放进一个池子，就等于给一条被钉住的故障一个被别人复用的
// 出口——下一次 Acquire 拿到它，下一次调用在同一个 backend 上执行，
// 而 pg_stat_activity 里看到的是别人的名字。
// 所以每一条被持有的连接都是专属的，也因此在撤销时被专属地关掉。

// New builds a PG injector. With no DSN it constructs fine
// and reports itself unavailable — that is the difference between this
// revision and the last one, where "constructed" and "claimed to work" were
// the same statement.
func New(opts ...Option) *Injector {
	i := &Injector{live: map[string]*live{}}
	for _, opt := range opts {
		opt(i)
	}
	if !i.dsnSet && i.dsn == "" {
		i.dsn = os.Getenv(DSNEnv)
	}
	return i
}

// Type returns the type prefix.
func (i *Injector) Type() string { return "pg." }

// CheckAvailable reports whether this injector can actually stage a fault.
//
// The error says what is missing rather than a bare false, because a caller
// that gets "unavailable" and a caller that gets "false" take completely
// different next steps: the first reads the message, the second goes looking
// in the injector.
func (i *Injector) CheckAvailable(ctx context.Context) error {
	if i.dsn == "" {
		return fmt.Errorf("%w: 没有配置 PostgreSQL 连接（设 %s，或用 WithDSN / WithPool 传进来）",
			injector.ErrUnavailable, DSNEnv)
	}
	dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := pgx.Connect(dialCtx, i.dsn)
	if err != nil {
		return fmt.Errorf("%w: 连不上 %s: %w", injector.ErrUnavailable, redactDSN(i.dsn), err)
	}
	defer conn.Close(context.Background())
	if err := conn.Ping(dialCtx); err != nil {
		return fmt.Errorf("%w: %s 连上了但 ping 失败: %w", injector.ErrUnavailable, redactDSN(i.dsn), err)
	}
	return nil
}

// redactDSN keeps a password out of an error message. These strings get
// logged, and a harness that leaks a DSN into CI output is a harness that
// leaks it into every archived run.
func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	slash := strings.Index(dsn, "://")
	if at < 0 || slash < 0 || at < slash {
		return dsn
	}
	return dsn[:slash+3] + "***" + dsn[at:]
}

// Inject stages one fault.
func (i *Injector) Inject(ctx context.Context, spec injector.InjectSpec) (*injector.InjectResult, error) {
	if !supportedTypes[spec.Type] {
		return nil, fmt.Errorf("%w: %s", injector.ErrUnsupportedType, spec.Type)
	}
	// 认得这个类型、但这个类型需要单节点造不出来的环境，是一个必须说出口的
	// 拒绝，不能退化成"可用但没做"。
	if reason, replicaOnly := replicaOnlyTypes[spec.Type]; replicaOnly {
		return nil, fmt.Errorf("%w: %s 需要一个 %s", injector.ErrUnavailable, spec.Type, reason)
	}
	// 不可用就一步都不走。这一条不是防御性编程：Inject 之后会真的改一个数据库，
	// 不在这里拦住，一次"注入成功"就会一路走到 judge 那里变成一个假的回归结论。
	//
	// 它排在类型检查**之后**：认不出的类型和跑不了的注入器是两回事，
	// 接线之后前者会一直是真的错误，而把它报成"不可用"等于让调用方
	// 去查环境。
	if err := i.CheckAvailable(ctx); err != nil {
		return nil, err
	}

	l := i.begin(spec)
	defer func() {
		if l != nil && l.err != nil {
			// 出错时立刻撤销已经做的那几步，而不是把半截故障留在数据库里。
			_ = i.runRollback(context.Background(), l)
			// **并且把这一条从账本里划掉。**
			//
			// 只撤销不销账的后果是：一次失败的注入仍然占着 `Live()` 里的一个
			// 名字，而 `Cleanup(id)` 会因为"还在账本里"而返回一个成功的撤销——
			// 于是一次从没成功过的东西，看上去像是被正常撤销过了。
			// 它同时会让"当前有几条故障在生效"这个读数永远只增不减，
			// 而那个读数是判断一次评测跑得干不干净的依据。
			i.forget(l.id)
		}
	}()

	var err error
	switch spec.Type {
	case "pg.inject_lock_chain":
		err = i.injectLockChain(ctx, spec, l)
	case "pg.begin_txn_hold":
		err = i.holdTransaction(ctx, spec, l, "begin_txn_hold")
	case "pg.hold_old_txn":
		err = i.holdTransaction(ctx, spec, l, "hold_old_txn")
	case "pg.run_slow_queries":
		err = i.runSlowQueries(ctx, spec, l)
	case "pg.inject_table_bloat":
		err = i.injectTableBloat(ctx, spec, l)
	case "pg.run_autovacuum":
		err = i.runAutovacuum(ctx, spec, l)
	default:
		err = fmt.Errorf("%w: %s", injector.ErrUnsupportedType, spec.Type)
	}
	if err != nil {
		l.err = err
		return nil, err
	}
	l.err = nil
	i.startExpiry(l, spec.Duration)
	return i.result(l), nil
}

func (i *Injector) begin(spec injector.InjectSpec) *live {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.seq++
	id := spec.InjectID
	if id == "" {
		id = fmt.Sprintf("pg-inj-%d-%d", time.Now().UTC().UnixNano(), i.seq)
	}
	lctx, lcancel := context.WithCancel(context.Background())
	l := &live{
		id:       id,
		typ:      spec.Type,
		started:  time.Now().UTC(),
		done:     make(chan struct{}),
		duration: spec.Duration,
		ctx:      lctx,
		cancel:   lcancel,
	}
	i.live[id] = l
	return l
}

// startExpiry is where InjectSpec.Duration is finally read.
func (i *Injector) startExpiry(l *live, d time.Duration) {
	if d <= 0 {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	l.timer = time.AfterFunc(d, func() {
		_ = i.Cleanup(context.Background(), l.id)
	})
}

func (i *Injector) result(l *live) *injector.InjectResult {
	meta := map[string]string{
		"backend": "pgx",
		"table":   l.table,
	}
	// expires 说的是"最晚到这里"，不是"一定到这里"：Cleanup 可以更早。
	if l.duration > 0 {
		meta["expires"] = l.started.Add(l.duration).Format(time.RFC3339)
	}
	if l.createdBy {
		meta["table_created_by_injector"] = "true"
	}
	return &injector.InjectResult{
		InjectID:   l.id,
		Type:       l.typ,
		ResourceID: l.table,
		StartedAt:  l.started,
		Metadata:   meta,
	}
}

// Cleanup undoes one injection. It is idempotent: a second call on the same
// ID returns ErrInjectionNotFound rather than undoing anything twice.
func (i *Injector) Cleanup(ctx context.Context, injectID string) error {
	if injectID == "" {
		return injector.ErrInjectionNotFound
	}
	i.mu.Lock()
	l, ok := i.live[injectID]
	if ok {
		delete(i.live, injectID)
	}
	i.mu.Unlock()
	if !ok {
		return injector.ErrInjectionNotFound
	}
	if l.timer != nil {
		l.timer.Stop()
	}
	// 先取消派生出来的语句，再撤销连接：等锁的语句必须先停，
	// 否则关连接这一步要等它自己超时。
	if l.cancel != nil {
		l.cancel()
	}
	err := i.runRollback(ctx, l)
	close(l.done)
	l.err = err
	return err
}

// runRollback runs the undo steps in reverse order.
//
// 逆序不是习惯：先建表再关 autovacuum，就得先开 autovacuum 再删表。
// 顺序错了会把一个本该消失的副作用留在目标上。
func (i *Injector) runRollback(ctx context.Context, l *live) error {
	var errs []error
	for n := len(l.rollback) - 1; n >= 0; n-- {
		if err := l.rollback[n](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	l.rollback = nil
	return errors.Join(errs...)
}

// forget 划掉一条注入，不撤销它。
//
// 它只回答"账本上还有没有这一条"，不碰目标系统——
// 撤销由 runRollback 负责，两件事分开，所以调用点不会在"忘了撤销"
// 和"忘了销账"之间只做一半。
func (i *Injector) forget(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.live, id)
}

// Live returns the IDs of the injections currently staged.
func (i *Injector) Live() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]string, 0, len(i.live))
	for id := range i.live {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

var _ injector.Injector = (*Injector)(nil)
