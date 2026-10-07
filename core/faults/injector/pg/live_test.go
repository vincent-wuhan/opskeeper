package pg

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// 这一组测试要一个真的 PostgreSQL。它不是 mock，也不是 sqlmock：
// 一条锁链是一组持有行锁的会话，mock 出来的"锁链"什么也证明不了。
//
// 门槛只有一个环境变量。**设了就连不上必须红**——一个跳过的集成测试
// 证明不了任何事，而一个连不上却变绿的集成测试比没有更坏。
// 没设则整组跳过，并在输出里说清差什么。
func liveInjector(t *testing.T) *Injector {
	t.Helper()
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		t.Skipf("%s not set; this test injects into a real PostgreSQL and will not pretend to", DSNEnv)
	}
	i := New(WithDSN(dsn))
	if err := i.CheckAvailable(context.Background()); err != nil {
		t.Fatalf("%s is set but the database is not usable: %v", DSNEnv, err)
	}
	return i
}

func queryInt(t *testing.T, dsn, sql string) int {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	var n int
	if err := conn.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

// dropProbeTable 清掉一张测试专用的探针表。
func dropProbeTable(t *testing.T, dsn, name string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	if !validIdent(name) {
		t.Fatalf("probe table %q is not a plain identifier", name)
	}
	if _, err := conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("drop probe table %s: %v", name, err)
	}
}

// 一条锁链必须真的让 backend 停在等待里，而且撤销之后必须一个都不剩。
//
// 只断言"返回了 InjectID"是不够的——上一版就是那样：ID 有了，故障没有。
func TestALockChainActuallyParksBackendsAndCleanupReleasesThem(t *testing.T) {
	i := liveInjector(t)
	dsn := os.Getenv(DSNEnv)

	// 这张探针表归这组测试所有，所以测试自己负责把它收拾干净再开始。
	// 上一版留下的残表没有 marker，注入器按设计**拒绝**删别人的表
	// （dropOwnTable 会报错而不是硬删），于是从那一版之后这条断言就永远红。
	// 一个依赖"这台机器的库是处女地"的断言不是断言。
	dropProbeTable(t, dsn, "opskeeper_chain_probe")

	before := queryInt(t, dsn, `SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND query LIKE 'SELECT id FROM opskeeper_chain%'`)

	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "pg.inject_lock_chain",
		Duration: 60 * time.Second,
		Params:   map[string]any{"table": "opskeeper_chain_probe", "sessions": 4, "chain_depth": 3},
	})
	if err != nil {
		t.Fatalf("inject lock chain: %v", err)
	}
	if res.ResourceID != "opskeeper_chain_probe" {
		t.Fatalf("ResourceID = %q, want the table the case named", res.ResourceID)
	}
	if _, ok := res.Metadata["expires"]; !ok {
		t.Errorf("metadata = %v, want an expiry: InjectSpec.Duration must be read", res.Metadata)
	}

	// 至少有一个 backend 真的卡在等待里。
	waiters := queryInt(t, dsn, `
		SELECT count(*) FROM pg_stat_activity
		WHERE wait_event_type = 'Lock' AND application_name LIKE 'chain-%'`)
	if waiters == 0 {
		t.Errorf("no backend is waiting on a lock; the chain was reported but not staged")
	}

	if err := i.Cleanup(context.Background(), res.InjectID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	// 撤销之后：等待者归零，表被删掉（因为是注入器建的）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		waiters = queryInt(t, dsn, `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND application_name LIKE 'chain-%'`)
		if waiters == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backend(s) still waiting after cleanup", waiters)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := queryInt(t, dsn, `SELECT count(*) FROM pg_class WHERE relname = 'opskeeper_chain_probe'`); n != 0 {
		t.Errorf("the table the injector created is still there; cleanup left damage behind")
	}
	_ = before
}

// 一个长事务必须真的停在 idle in transaction，并且撤销后消失。
func TestAHeldTransactionIsVisibleAndThenGone(t *testing.T) {
	i := liveInjector(t)
	dsn := os.Getenv(DSNEnv)

	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "pg.begin_txn_hold",
		Duration: 60 * time.Second,
		Params:   map[string]any{"table": "opskeeper_txn_probe", "lock_count": 2},
	})
	if err != nil {
		t.Fatalf("inject held txn: %v", err)
	}
	n := queryInt(t, dsn, `SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction' AND application_name = 'begin_txn_hold'`)
	if n == 0 {
		t.Errorf("no backend is idle in transaction; the hold was reported but not staged")
	}
	if err := i.Cleanup(context.Background(), res.InjectID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n := queryInt(t, dsn, `SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction' AND application_name = 'begin_txn_hold'`); n != 0 {
		t.Errorf("%d backend(s) still idle in transaction after cleanup", n)
	}
}

// 慢查询必须真的让 backend 忙起来。
func TestSlowQueriesOccupyBackends(t *testing.T) {
	i := liveInjector(t)
	dsn := os.Getenv(DSNEnv)
	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "pg.run_slow_queries",
		Duration: 30 * time.Second,
		Params:   map[string]any{"concurrent": 2, "mean_duration_ms": 3000},
	})
	if err != nil {
		t.Fatalf("inject slow queries: %v", err)
	}
	n := queryInt(t, dsn, `SELECT count(*) FROM pg_stat_activity WHERE application_name LIKE 'slow-query-%'`)
	if n == 0 {
		t.Errorf("no backend is running a slow query; the injection was reported but not staged")
	}
	if err := i.Cleanup(context.Background(), res.InjectID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// Duration 到点必须自己撤销。这是 InjectSpec.Duration 第一次真的被读。
func TestAnInjectionExpiresOnItsOwn(t *testing.T) {
	i := liveInjector(t)
	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "pg.begin_txn_hold",
		Duration: 700 * time.Millisecond,
		Params:   map[string]any{"table": "opskeeper_expiry_probe", "lock_count": 1},
	})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(i.Live()) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the injection outlived its duration; live = %v", i.Live())
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 到期自毁是**唯一一条不经过显式 Cleanup 的撤销路径**，
	// 所以它必须单独证明自己真的动了数据库——
	// `len(i.Live()) == 0` 只是账本归零，一个只清账本的实现照样能通过它。
	// 判据是那条 backend 消失、那张表被删掉。
	dsn := os.Getenv(DSNEnv)
	deadline = time.Now().Add(5 * time.Second)
	for {
		held := queryInt(t, dsn, `
			SELECT count(*) FROM pg_stat_activity
			WHERE application_name = 'begin_txn_hold'`)
		tables := queryInt(t, dsn, `SELECT count(*) FROM pg_class WHERE relname = 'opskeeper_expiry_probe'`)
		if held == 0 && tables == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after expiry: %d backend(s) still held, %d table(s) still there; "+
				"expiry cleared the bookkeeping but not the database",
				held, tables)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 自动撤销之后，手工再撤一次必须报"没有这一条"，而不是再撤一遍。
	if err := i.Cleanup(context.Background(), res.InjectID); !errors.Is(err, injector.ErrInjectionNotFound) {
		t.Fatalf("second cleanup = %v, want ErrInjectionNotFound", err)
	}
}

// 复制延迟在这个环境里造不出来，必须**大声**说不能，而不是退化成"可用但没做"。
func TestReplicaLagRefusesWithAReason(t *testing.T) {
	i := New(WithDSN(os.Getenv(DSNEnv)))
	_, err := i.Inject(context.Background(), injector.InjectSpec{Type: "pg.inject_replica_lag"})
	if !errors.Is(err, injector.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if len(err.Error()) < 30 {
		t.Errorf("error = %q, want it to say why a replica is needed", err)
	}
}

// 表名来自 YAML，所以它必须被当作标识符校验，而不是拼进 SQL。
func TestATableNameThatIsNotAnIdentifierIsRefused(t *testing.T) {
	i := liveInjector(t)
	_, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:   "pg.begin_txn_hold",
		Params: map[string]any{"table": "orders; DROP TABLE users"},
	})
	if err == nil {
		t.Fatal("a table name carrying SQL was accepted")
	}
}

// 没配 DSN 时必须拒绝，并且说清差什么。
func TestNoDSNMeansUnavailableWithTheVariableNamed(t *testing.T) {
	t.Setenv(DSNEnv, "")
	i := New()
	err := i.CheckAvailable(context.Background())
	if !errors.Is(err, injector.ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if !contains(err.Error(), DSNEnv) {
		t.Errorf("error = %q, want it to name %s", err, DSNEnv)
	}
}

// 一个配了但连不上的 DSN，错误里不能带密码。
func TestAnUnreachableDSNDoesNotLeakThePassword(t *testing.T) {
	const secret = "hunter2-should-never-appear"
	i := New(WithDSN("postgres://opskeeper:" + secret + "@127.0.0.1:1/nope?connect_timeout=1"))
	err := i.CheckAvailable(context.Background())
	if err == nil {
		t.Skip("something is listening on 127.0.0.1:1; the leak cannot be shown here")
	}
	if contains(err.Error(), secret) {
		t.Fatalf("error = %q, want the password redacted", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

var _ = strconv.Itoa

// 表膨胀必须真的在磁盘上留下死元组，而且是一个**诊断 agent 查得到**的数字。
//
// 判据是 pg_stat_user_tables.n_dead_tup，不是"注入返回了 ID"。
// 上一版的膨胀注入在一张空表上跑 UPDATE，影响零行，
// 而语句成功返回——于是没有死元组、没有可观测后果，注入却报告成功。
func TestTableBloatLeavesDeadTuplesAndCleanupTakesThemAway(t *testing.T) {
	i := liveInjector(t)
	dsn := os.Getenv(DSNEnv)
	const table = "opskeeper_bloat_probe"
	dropProbeTable(t, dsn, table)

	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "pg.inject_table_bloat",
		Duration: 60 * time.Second,
		Params:   map[string]any{"table": table, "update_count": 200, "vacuum_disabled": true},
	})
	if err != nil {
		t.Fatalf("inject table bloat: %v", err)
	}
	// 死元组的计数住在做出这些更新的那个 backend 的统计缓存里，
	// 注入器在提交之后自己 flush 过一次，所以这里查得到。
	// 仍然给一个短窗口：flush 与视图更新之间没有强制的先后保证，
	// 而"查一次拿到 0"与"根本没有死元组"是两件事。
	dead := 0
	for range 25 {
		dead = queryInt(t, dsn, `
			SELECT n_dead_tup FROM pg_stat_user_tables WHERE relname = '`+table+`'`)
		if dead > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if dead == 0 {
		t.Errorf("n_dead_tup is 0 after a bloat injection; there is nothing for a diagnosis to find")
	}
	if err := i.Cleanup(context.Background(), res.InjectID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n := queryInt(t, dsn, `SELECT count(*) FROM pg_class WHERE relname = '`+table+`'`); n != 0 {
		t.Errorf("cleanup left the bloat probe table behind")
	}
}

// vacuum 卡住是**两个**故障：autovacuum 被关掉，加上一个长事务压着 xmin。
// 少任何一个都只是一个设置项，而设置项不是故障。
func TestAutovacuumIsOffAndAnOldXminIsHeld(t *testing.T) {
	i := liveInjector(t)
	dsn := os.Getenv(DSNEnv)
	const table = "opskeeper_autovac_probe"
	dropProbeTable(t, dsn, table)

	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "pg.run_autovacuum",
		Duration: 60 * time.Second,
		Params:   map[string]any{"table": table},
	})
	if err != nil {
		t.Fatalf("inject autovacuum: %v", err)
	}
	var enabled string
	if err := pgxRow(t, dsn,
		`SELECT c.reloptions::text FROM pg_class c WHERE c.relname = $1`, table, &enabled); err != nil {
		t.Fatalf("read reloptions: %v", err)
	}
	if !strings.Contains(enabled, "autovacuum_enabled=false") {
		t.Errorf("reloptions = %q, want autovacuum_enabled=false; without it this is a setting, not a fault", enabled)
	}
	held := queryInt(t, dsn,
		`SELECT count(*) FROM pg_stat_activity WHERE application_name = 'autovacuum-blocker' AND state = 'idle in transaction'`)
	if held == 0 {
		t.Errorf("no transaction is holding the old xmin; autovacuum is off but nothing is actually blocked")
	}
	if err := i.Cleanup(context.Background(), res.InjectID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n := queryInt(t, dsn, `SELECT count(*) FROM pg_class WHERE relname = '`+table+`'`); n != 0 {
		t.Errorf("cleanup left the autovacuum probe table behind")
	}
}

// pgxRow 查一个单值，错误直接算测试失败。
func pgxRow(t *testing.T, dsn, sql, arg string, dst *string) error {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	return conn.QueryRow(context.Background(), sql, arg).Scan(dst)
}

// 一次失败的注入必须**从账本上消失**。
//
// 上一版只撤销、不销账：失败的注入仍然占着 Live() 里的一个名字，
// 而 Cleanup(id) 会因为"还在账本里"而返回一个成功的撤销——
// 于是一个从没成功过的东西，看上去像是被正常撤销过了。
// 更糟的是"当前有几条故障在生效"这个读数会只增不减，
// 而它是判断一次评测跑得干不干净的依据。
//
// 这条只在"参数错误发生在 begin 之后"时才触发：类型检查与不可用检查
// 都在 begin 之前，它们天然不会留下账目。所以判据要挑一个 post-begin 的失败。
func TestARefusedInjectionLeavesNoLedgerEntry(t *testing.T) {
	i := liveInjector(t)
	// chain_depth=1 造不出链，而它是在 begin 之后才被检查的。
	_, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "pg.inject_lock_chain",
		Duration: 30 * time.Second,
		Params:   map[string]any{"table": "opskeeper_chain_probe", "chain_depth": 1},
	})
	if err == nil {
		t.Fatal("Inject accepted chain_depth=1; want a refusal")
	}
	if n := len(i.Live()); n != 0 {
		t.Errorf("%d injection(s) still recorded after a refusal: %v", n, i.Live())
	}
}
