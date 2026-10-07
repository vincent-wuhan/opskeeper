package pg

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// 这一文件是上一版 pg.go 里那个 `skeleton: "true"` 标记背后的空缺的实体。
//
// 上一版的 Inject 做的是：把一条记录写进内存 map，返回一个 InjectID。
// 这一版做的每一件事都必须能从数据库外面看见——
// pg_locks 里有它、pg_stat_activity 里有它、或者表的物理尺寸变了。
// 一个诊断 agent 要能查得到并且查得对，评分才有意义。

// ensureTable 拿到一张可以放心弄坏的表。
//
// case 里写着 `table: orders`，而 `orders` 未必存在（语料的 prerequisites
// 写的是 "pg.bench dataset loaded"，那个数据集在本仓库里没有）。所以：
// 表在就直接用；表不在就建一张，**并在撤销时只删自己建的那一张**。
// 永远不 truncate、不 drop、不改写一张不是自己建的表。
func (i *Injector) ensureTable(ctx context.Context, conn *pgx.Conn, name, injectID string) (created bool, err error) {
	if !validIdent(name) {
		return false, fmt.Errorf("table %q is not a plain identifier; refusing to interpolate it into SQL", name)
	}
	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		                 WHERE c.relname = $1 AND n.nspname = current_schema())`, name).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	// 每一行都带一个标记，Cleanup 认它而不是认表名——
	// 万一有人在我们建表之后建了同名表，我们仍然只删自己那张。
	if _, err := conn.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (
			id    bigserial PRIMARY KEY,
			pad   text NOT NULL DEFAULT repeat('x', 256),
			note  text
		)`, name)); err != nil {
		return false, fmt.Errorf("create %s: %w", name, err)
	}
	// COMMENT ON TABLE 不接受参数——它是 DDL 家族里少数不接受绑定变量的成员。
	// injectID 是本进程生成的（时间戳 + 序号），不是语料里的字符串，所以直接
	// 拼进去是安全的；表名走的是上面那道 validIdent。
	// 从这里往后的每一步失败都必须仍然报 created=true。
	// 表已经建出来了，而调用方靠这个标志决定要不要在撤销时删掉它——
	// 报 false 就等于把一张自己建的表留在目标上。
	if _, err := conn.Exec(ctx, fmt.Sprintf(
		`COMMENT ON TABLE %s IS 'created by opskeeper harness fault %s'`, name, injectID)); err != nil {
		return true, fmt.Errorf("comment %s: %w", name, err)
	}
	return true, nil
}

// ensureRows makes sure the table has at least `need` rows and returns the ids
// this injection added (empty when the table already had enough).
//
// 它存在是因为一件很具体的事：一张**上一次注入失败后被留下的空表**。
// 那一版 ensureTable 只在"自己建表"的时候播种，于是第二次注入看到表已存在、
// 就不再播种，锁链去锁一张空表——FOR UPDATE 匹配零行，不取任何锁，
// 后面那次"应该等不到"的请求于是立刻成功，而链根本没搭起来。
// 顺带说明为什么这条不能靠"每次都用一张新表"绕过去：case 写的是
// `table: orders`，注入到一张叫 orders_probe 的表上，诊断结论就答非所问。
// 第一返回值是**这次要用的行的 id**（已有的 + 新播的，按 id 升序），
// 调用方必须按它去锁——见 injectLockChain 里那个关于自增序列的坑。
// 第二返回值只含有本次新播种的那些行，撤销时只删它们。
func (i *Injector) ensureRows(ctx context.Context, conn *pgx.Conn, table string, need int) (ids, added []int64, err error) {
	rows, err := conn.Query(ctx, fmt.Sprintf(`SELECT id FROM %s ORDER BY id LIMIT $1`, table), need)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", table, err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(ids) >= need {
		return ids, nil, nil
	}
	for n := len(ids); n < need; n++ {
		var id int64
		if err := conn.QueryRow(ctx,
			fmt.Sprintf(`INSERT INTO %s (note) VALUES ('opskeeper fault row') RETURNING id`, table)).Scan(&id); err != nil {
			return nil, added, fmt.Errorf("seed %s: %w", table, err)
		}
		ids = append(ids, id)
		added = append(added, id)
	}
	return ids, added, nil
}

// deleteRows removes exactly the rows this injection added.
func (i *Injector) deleteRows(table string, ids []int64) func(context.Context) error {
	return func(ctx context.Context) error {
		if len(ids) == 0 {
			return nil
		}
		conn, err := i.connect(ctx)
		if err != nil {
			return err
		}
		defer release(conn)
		_, err = conn.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = ANY($1)`, table), ids)
		return err
	}
}

// validIdent keeps a case-supplied table name from becoming SQL.
// 语料是 YAML，而 YAML 是可以手写的。
func validIdent(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// dropOwnTable removes a table only if it carries this injector's marker.
func (i *Injector) dropOwnTable(ctx context.Context, name, injectID string) func(context.Context) error {
	return func(ctx context.Context) error {
		conn, err := i.connect(ctx)
		if err != nil {
			return err
		}
		defer conn.Close(context.Background())
		var comment string
		if err := conn.QueryRow(ctx,
			`SELECT COALESCE(obj_description(c.oid), '') FROM pg_class c
			 JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE c.relname = $1 AND n.nspname = current_schema()`, name).Scan(&comment); err != nil {
			return err
		}
		if !strings.Contains(comment, injectID) {
			// 表已经不是我们的了（或者从来没有是）。删掉它就是删别人的东西。
			return fmt.Errorf("refusing to drop %s: it does not carry this injection's marker", name)
		}
		if _, err := conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)); err != nil {
			return fmt.Errorf("drop %s: %w", name, err)
		}
		return nil
	}
}

// connect opens one dedicated connection. 见 New 上方关于"为什么不要池子"的说明。
func (i *Injector) connect(ctx context.Context) (*pgx.Conn, error) {
	return pgx.Connect(ctx, i.dsn)
}

// release closes a connection obtained from connect.
func release(c *pgx.Conn) {
	if c == nil {
		return
	}
	_ = c.Close(context.Background())
}

// ---------------------------------------------------------------- lock chain

// injectLockChain stages a wait chain: session k holds row k and then asks for
// row k+1, which session k+1 already holds. Every session after the first is
// parked in pg_locks waiting on a real lock, and the first is the root cause
// an agent has to find.
//
// It is a wait chain, not a deadlock: PG would detect and abort a true
// deadlock within deadlock_timeout, and a fault that repairs itself before
// anyone looks at it is not a fault.
func (i *Injector) injectLockChain(ctx context.Context, spec injector.InjectSpec, l *live) error {
	sessions := injector.IntParam(spec.Params, "sessions", 5)
	depth := injector.IntParam(spec.Params, "chain_depth", sessions-1)
	if depth > sessions {
		depth = sessions
	}
	if depth < 2 {
		return fmt.Errorf("inject_lock_chain: chain_depth=%d cannot form a chain; need at least 2", depth)
	}
	table := injector.StringParam(spec.Params, "table", "orders")
	conn, err := i.connect(ctx)
	if err != nil {
		return err
	}
	created, err := i.ensureTable(ctx, conn, table, l.id)
	release(conn)
	if err != nil {
		return err
	}
	l.table = table
	l.createdBy = created
	rowConn, err := i.connect(ctx)
	if err != nil {
		return err
	}
	rowIDs, added, err := i.ensureRows(ctx, rowConn, table, depth)
	release(rowConn)
	if err != nil {
		return err
	}
	if created {
		// 逆序：先删自己建的表，删表本身就把那些行带走了。
		l.rollback = append(l.rollback, i.dropOwnTable(ctx, table, l.id))
	} else if len(added) > 0 {
		l.rollback = append(l.rollback, i.deleteRows(table, added))
	}

	// 两趟，一条连接一环。
	//
	// 第一次接线时这两件事写在同一个循环里：link 0 攥住第 1 行，紧接着就去要
	// 第 2 行——而 link 1 此刻还没上线，那一行是空的。链根本没搭起来，
	// 而命令返回了一个 InjectID 和一句"注入成功"。
	//
	// 链的定义就是"全部先攥住，再依次去要"，所以必须是两趟；
	// 而且必须**同一条连接**：先 BEGIN、锁住自己那一行、再去要下一行，
	// 那次等待才是这个 backend 在 pg_locks 里的等待。
	links := make([]*pgx.Conn, 0, depth)
	for k := 0; k < depth; k++ {
		c, err := i.openLink(ctx, l, fmt.Sprintf("chain-%d", k))
		if err != nil {
			return err
		}
		links = append(links, c)
	}
	//
	// 锁的是 ensureRows 交回来的**真实 id**，不是 1、2、3。
	// 一张被反复注入过的表，它的自增序列早就跑过第 16 行了——按 k+1 去锁
	// 匹配零行，一把锁都没取到，而这条语句是**成功返回**的：
	// SELECT ... FOR UPDATE 匹配不到行不报错。
	// 于是链"搭好了"，pg_locks 里空空如也，而注入报告成功。
	// 判据只能是 RowsAffected，拿不到行就当场失败，绝不往下走。
	for k, c := range links {
		tag, err := c.Exec(ctx, fmt.Sprintf(`SELECT id FROM %s WHERE id = $1 FOR UPDATE`, table), rowIDs[k])
		if err != nil {
			return fmt.Errorf("chain link %d could not take row %d: %w", k, rowIDs[k], err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("chain link %d matched %d row(s) for id %d, want exactly 1",
				k, tag.RowsAffected(), rowIDs[k])
		}
	}
	// 第三趟：除最后一环外，每条去要下一行——并且**真的卡在那儿**。
	//
	// 这里踩过一个坑，值得写下来：给等待语句一个几百毫秒的 context、
	// 然后把"超时"当成"链搭起来了"的证据，是错的。语句一被取消，
	// backend 立刻从 pg_locks 里消失，注入在故障还存在的那一刻自己修好了它，
	// 而 Inject 早已返回成功。**一次自己会好的故障不是故障。**
	//
	// 所以等待语句跑在这次注入自己的 context 上（l.ctx），一直等到 Cleanup
	// 把连接关掉为止；Inject 只负责确认"确实有人在等"之后才返回。
	for k := 0; k < depth-1; k++ {
		k := k
		go func() {
			// 这一行要么拿到锁（链没搭起来，属于下面的检测要抓的），
			// 要么一直等到 l.cancel。返回值不用管：真正的判据在 pg_locks。
			_, _ = links[k].Exec(l.ctx,
				fmt.Sprintf(`SELECT id FROM %s WHERE id = $1 FOR UPDATE`, table), rowIDs[k+1])
		}()
	}
	if err := i.waitForWaiters(ctx, depth-1, 3*time.Second); err != nil {
		return err
	}
	return nil
}

// waitForWaiters waits until at least `want` backends are blocked on a lock.
//
// 这是"注入成功"的唯一判据。返回一个 InjectID 不算：上一版就是那样，
// 而链根本没搭起来。
func (i *Injector) waitForWaiters(ctx context.Context, want int, timeout time.Duration) error {
	observer, err := i.connect(ctx)
	if err != nil {
		return err
	}
	defer release(observer)
	deadline := time.Now().Add(timeout)
	var last int
	for {
		// 判据是 pg_stat_activity 里的 wait_event_type='Lock'，不是 pg_locks。
		// 后者要靠 locktype 猜：PG 16 上等一把行锁的 backend 报的是
		// 'transactionid'（它要等别人的事务结束），不是 'tuple'——
		// 写死 tuple 的一次结果是永远等不到，而那正好是"看起来在等、
		// 其实什么也没观察到"这个错误。
		if err := observer.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND application_name LIKE 'chain-%'`).Scan(&last); err != nil {
			return fmt.Errorf("observe locks: %w", err)
		}
		if last >= want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("lock chain did not form: %d backend(s) waiting, want %d", last, want)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// openLink opens one connection of the chain, in a transaction, and registers
// its teardown.
func (i *Injector) openLink(ctx context.Context, l *live, appName string) (*pgx.Conn, error) {
	conn, err := i.connect(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, `SELECT set_config('application_name', $1, false)`, appName); err != nil {
		release(conn)
		return nil, err
	}
	if _, err := conn.Exec(ctx, `BEGIN`); err != nil {
		release(conn)
		return nil, err
	}
	var once sync.Once
	l.rollback = append(l.rollback, func(context.Context) error {
		once.Do(func() {
			// 关连接就是解锁。ROLLBACK 只是把话说完整。
			bg := context.Background()
			_, _ = conn.Exec(bg, `ROLLBACK`)
			release(conn)
		})
		return nil
	})
	return conn, nil
}

// holdOn runs begin in its own connection and keeps it open until undone.
func (i *Injector) holdOn(ctx context.Context, l *live, appName string, body func(*pgx.Conn) error) (func(), error) {
	conn, err := i.connect(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, `SELECT set_config('application_name', $1, false)`, appName); err != nil {
		release(conn)
		return nil, err
	}
	if _, err := conn.Exec(ctx, `BEGIN`); err != nil {
		release(conn)
		return nil, err
	}
	if err := body(conn); err != nil {
		_, _ = conn.Exec(context.Background(), `ROLLBACK`)
		release(conn)
		return nil, err
	}
	var once sync.Once
	undo := func() {
		once.Do(func() {
			// 关连接就是解锁。ROLLBACK 只是把话说完整。
			bg := context.Background()
			_, _ = conn.Exec(bg, `ROLLBACK`)
			release(conn)
		})
	}
	l.rollback = append(l.rollback, func(context.Context) error { undo(); return nil })
	return undo, nil
}

// ------------------------------------------------------------ held transaction

// holdTransaction stages a long-running transaction: BEGIN, a write, and then
// nothing. It is the shape pg_stat_activity's state='idle in transaction'
// reports, and it is what stops vacuum from reclaiming anything underneath it.
func (i *Injector) holdTransaction(ctx context.Context, spec injector.InjectSpec, l *live, typ string) error {
	tables := injector.StringListParam(spec.Params, "tables", []string{injector.StringParam(spec.Params, "table", "orders")})
	isolation := injector.StringParam(spec.Params, "isolation", "read committed")
	if !validIsolation(isolation) {
		return fmt.Errorf("%s: isolation %q is not one of the four PostgreSQL levels", typ, isolation)
	}
	// 校验那一步允许下划线写法（语料里两种都有），而 SQL 只认空格。
	// 原样拼过去的后果是 SET TRANSACTION ISOLATION LEVEL read_committed
	// 报语法错误——一个只在真库上才暴露的错，而它落在一个 case 会直接失败。
	isolationSQL := strings.ReplaceAll(isolation, "_", " ")
	locks := injector.IntParam(spec.Params, "lock_count", len(tables))
	if locks < 1 {
		return fmt.Errorf("%s: lock_count=%d", typ, locks)
	}
	conn, err := i.connect(ctx)
	if err != nil {
		return err
	}
	created, err := i.ensureTable(ctx, conn, tables[0], l.id)
	release(conn)
	if err != nil {
		return err
	}
	l.table = tables[0]
	l.createdBy = created
	if created {
		l.rollback = append(l.rollback, i.dropOwnTable(ctx, tables[0], l.id))
	}
	_, err = i.holdOn(ctx, l, typ, func(held *pgx.Conn) error {
		if _, err := held.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL `+isolationSQL); err != nil {
			return fmt.Errorf("set isolation: %w", err)
		}
		// 一次真实的写：没有它这就是个只读事务，vacuum 照常回收。
		if _, err := held.Exec(ctx, fmt.Sprintf(
			`UPDATE %s SET note = $1 WHERE id <= $2`, tables[0]), typ, locks); err != nil {
			return fmt.Errorf("write in held txn: %w", err)
		}
		return nil
	})
	return err
}

func validIsolation(level string) bool {
	switch strings.ToLower(strings.ReplaceAll(level, "_", " ")) {
	case "read committed", "read uncommitted", "repeatable read", "serializable":
		return true
	}
	return false
}

// ---------------------------------------------------------------- slow queries

// runSlowQueries parks N backends in pg_sleep, which is exactly what
// pg_stat_activity shows as an active long-running query. The query text is
// taken from the case when it gives one, so the diagnosis the agent has to
// make names the query the operator actually wrote.
func (i *Injector) runSlowQueries(ctx context.Context, spec injector.InjectSpec, l *live) error {
	concurrent := injector.IntParam(spec.Params, "concurrent", 5)
	if concurrent < 1 {
		return fmt.Errorf("run_slow_queries: concurrent=%d", concurrent)
	}
	meanMS := injector.IntParam(spec.Params, "mean_duration_ms", 2000)
	if meanMS < 1 {
		return fmt.Errorf("run_slow_queries: mean_duration_ms=%d", meanMS)
	}
	query := injector.StringParam(spec.Params, "query", "SELECT pg_sleep($1)")
	l.table = "n/a (pg_sleep)"

	for n := 0; n < concurrent; n++ {
		seconds := float64(meanMS) / 1000
		_, err := i.holdOn(ctx, l, fmt.Sprintf("slow-query-%d", n), func(conn *pgx.Conn) error {
			// 这个语句要跑完 mean_duration_ms 才会结束。它在自己的 goroutine 里跑，
			// 连接由 holdOn 持有到撤销为止。
			//
			// **不能用同一条连接去确认它忙了**：pgx 在一条连接上是串行的，
			// 观察用的 SELECT 1 会排在 pg_sleep 后面，然后一起等——
			// 上一版就是这么写的，于是每一次注入都卡满 2 秒然后报
			// "backend never became busy"，而 backend 明明正在跑。
			go func() {
				_, _ = conn.Exec(context.Background(), query, seconds)
			}()
			return i.waitBackendRunning(ctx, fmt.Sprintf("slow-query-%d", n), 3*time.Second)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// waitBackendRunning waits until a backend with this application_name is
// actually running a query, seen from **another** connection.
//
// 同一条连接问不了自己：pgx 在一条连接上串行，观察语句会排在慢查询后面。
// 而"Inject 返回了但 pg_stat_activity 里什么都没有"是一次假的注入成功，
// 所以这里等不到就必须失败。
func (i *Injector) waitBackendRunning(ctx context.Context, appName string, timeout time.Duration) error {
	observer, err := i.connect(ctx)
	if err != nil {
		return err
	}
	defer release(observer)
	deadline := time.Now().Add(timeout)
	for {
		var n int
		if err := observer.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE application_name = $1 AND state = 'active'`, appName).Scan(&n); err != nil {
			return fmt.Errorf("observe %s: %w", appName, err)
		}
		if n > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("backend %q never became active within %s; the fault is not observable",
				appName, timeout)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- table bloat

// injectTableBloat grows a table's dead-tuple count by updating every row
// enough times that autovacuum cannot keep up. `vacuum_disabled` turns
// autovacuum off for the duration, which is what makes the bloat observable
// in pg_stat_user_tables rather than being cleaned up before anyone looks.
// 两处播种量。写成常量而不是散落的字面量，是因为它们要出现在断言里。
const (
	bloatSeedRows      = 20
	autovacuumSeedRows = 50
)

func (i *Injector) injectTableBloat(ctx context.Context, spec injector.InjectSpec, l *live) error {
	table := injector.StringParam(spec.Params, "table", "order_events")
	updates := injector.IntParam(spec.Params, "update_count", 1000)
	if updates < 1 {
		return fmt.Errorf("inject_table_bloat: update_count=%d", updates)
	}
	conn, err := i.connect(ctx)
	if err != nil {
		return err
	}
	created, err := i.ensureTable(ctx, conn, table, l.id)
	if err != nil {
		release(conn)
		return err
	}
	// 表里得有东西可膨胀。一张空表上跑一万遍 UPDATE 影响零行，
	// 于是"表膨胀"退化成一个没有任何可观测后果的开关——
	// 而这正是"注入报告成功、数据库其实没坏"最难被发现的那种失败。
	_, added, err := i.ensureRows(ctx, conn, table, bloatSeedRows)
	release(conn)
	if err != nil {
		return err
	}
	l.table = table
	l.createdBy = created
	if created {
		l.rollback = append(l.rollback, i.dropOwnTable(ctx, table, l.id))
	} else if len(added) > 0 {
		l.rollback = append(l.rollback, i.deleteRows(table, added))
	}
	if injector.BoolParam(spec.Params, "vacuum_disabled", false) {
		if err := i.setAutovacuum(ctx, l, table, false); err != nil {
			return err
		}
	}
	//
	// 这里**不用 holdOn**，是一个被真库测试逼出来的结论。
	// holdOn 开的是一个不提交的事务：那些 UPDATE 的新版本行只存在于
	// 自己的事务里，别的 backend 看不见，pg_stat_user_tables.n_dead_tup
	// 读出来是 0，pg_relation_size 也还是原样——膨胀没有落到磁盘上，
	// 而注入报告成功。
	//
	// 膨胀的定义就是"改过的行留下了、新的版本没被回收"，所以它必须提交。
	// 提交之后死元组才真的在那儿，等着 autovacuum（或者不来的 autovacuum）
	// 来处理。
	_, err = i.holdOn(ctx, l, "table-bloat", func(held *pgx.Conn) error {
		for n := 0; n < updates; n++ {
			if _, err := held.Exec(ctx, fmt.Sprintf(
				`UPDATE %s SET pad = pad || 'x'`, table)); err != nil {
				return fmt.Errorf("bloat pass %d: %w", n, err)
			}
		}
		// 一条连接一次提交，而不是每句一次：后者会把膨胀变成一个
		// 几千次往返的写放大，而两者造出来的死元组是一样的。
		if _, err := held.Exec(ctx, `COMMIT`); err != nil {
			return fmt.Errorf("commit bloat: %w", err)
		}
		// 死元组的计数住在**做出这些更新的那个 backend** 的统计缓存里，
		// 由统计收集器按周期刷进视图。pg_stat_force_next_flush() 让它
		// 立刻上报——而且必须由这条连接自己调用：
		// 换一条连接去问，问到的是那条连接自己的空统计。
		// 不刷这一下，注入返回一个谁也查不到证据的故障。
		if _, err := held.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
			return fmt.Errorf("flush stats after bloat: %w", err)
		}
		return nil
	})
	return err
}

// runAutovacuum stages the vacuum-stuck case: a long-lived transaction that
// keeps an old xmin alive, plus autovacuum switched off so the table's dead
// tuples stay visible. It is a separate injection type because the case file
// lists it as one, and because "vacuum 卡住" is two faults, not one.
func (i *Injector) runAutovacuum(ctx context.Context, spec injector.InjectSpec, l *live) error {
	table := injector.StringParam(spec.Params, "table", "orders")
	conn, err := i.connect(ctx)
	if err != nil {
		return err
	}
	created, err := i.ensureTable(ctx, conn, table, l.id)
	if err != nil {
		release(conn)
		return err
	}
	// 同 injectTableBloat：没有行就没有死元组，"vacuum 卡住"也就无从观测。
	// 播种量取 50：足够让 n_dead_tup 与 pg_stat_user_tables 讲一个诊断 agent
	// 能查证的故事，又不至于让一次注入跑成一次压力测试。
	_, added, err := i.ensureRows(ctx, conn, table, autovacuumSeedRows)
	release(conn)
	if err != nil {
		return err
	}
	l.table = table
	l.createdBy = created
	if created {
		l.rollback = append(l.rollback, i.dropOwnTable(ctx, table, l.id))
	} else if len(added) > 0 {
		l.rollback = append(l.rollback, i.deleteRows(table, added))
	}
	if err := i.setAutovacuum(ctx, l, table, false); err != nil {
		return err
	}
	// 再压一批死元组，否则"卡住"只是一个设置项，没有可观测的后果。
	_, err = i.holdOn(ctx, l, "autovacuum-blocker", func(held *pgx.Conn) error {
		_, err := held.Exec(ctx, fmt.Sprintf(`UPDATE %s SET pad = pad || 'y'`, table))
		return err
	})
	return err
}

// setAutovacuum toggles autovacuum on a table and registers the restore.
func (i *Injector) setAutovacuum(ctx context.Context, l *live, table string, enabled bool) error {
	conn, err := i.connect(ctx)
	if err != nil {
		return err
	}
	defer release(conn)
	if _, err := conn.Exec(ctx, fmt.Sprintf(
		`ALTER TABLE %s SET (autovacuum_enabled = %t)`, table, enabled)); err != nil {
		return fmt.Errorf("set autovacuum on %s: %w", table, err)
	}
	// 只登记"打开"，不登记原值：这是一次注入，它有权假设自己面对的是默认设置，
	// 而把一个语料没声明的设置还原回去是另一种猜测。
	l.rollback = append(l.rollback, func(ctx context.Context) error {
		c, err := i.connect(ctx)
		if err != nil {
			return err
		}
		defer release(c)
		_, err = c.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s SET (autovacuum_enabled = true)`, table))
		return err
	})
	return nil
}
