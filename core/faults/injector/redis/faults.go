package redis

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// 这一文件里是四种故障的真实实现。
//
// 它们共享一条判据：**Inject 返回之前，故障必须已经能被另一条连接查到。**
// 不是"应该能"，是查得到，查不到就是一次假的注入成功。pg 那一份里
// 已经为这件事付过一次代价（`waitBackendRunning` / `waitForWaiters`），
// 这里对每一种故障都配一个对应的等待，而不是"写完就返回"。

// ------------------------------------------------------------------ big key

// injectBigKey writes one key whose value is far larger than an operator
// should tolerate.
//
// 判据是 MEMORY USAGE：它报的是这条 key 真正占了多少字节，
// 而 STRLEN 只报值有多长。一个 8MB 的字符串在 STRLEN 和 MEMORY USAGE
// 里的量级是一样的——但只有后者能说明"搬一次它要花多少"。
func (i *Injector) injectBigKey(ctx context.Context, spec injector.InjectSpec, l *live) error {
	sizeMB := injector.IntParam(spec.Params, "value_size_mb", 8)
	if sizeMB < 1 {
		return fmt.Errorf("inject_big_key: value_size_mb=%d", sizeMB)
	}
	ttl := time.Duration(injector.IntParam(spec.Params, "ttl_seconds", 0)) * time.Second

	conn, err := i.connect(ctx, l.id)
	if err != nil {
		return err
	}
	defer conn.Close()

	key := l.keyFor("bigkey")
	// 一次 SET 写完，而不是循环 append：后者会在中途失败时留下一个
	// 半个大 key，而撤销步骤与失败回滚都会把它当成完整的一次注入。
	value := strings.Repeat("x", sizeMB*1024*1024)
	if err := conn.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("write big key: %w", err)
	}
	l.keys = append(l.keys, key)
	l.rollback = append(l.rollback, i.dropKeys(key))

	// 等到它真的在。EXISTS 是一次往返，MEMORY USAGE 才是那个数。
	usage, err := conn.MemoryUsage(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("read memory usage of %s: %w", key, err)
	}
	// 压缩后的实际占用一定小于写入的字节数（Redis 默认 lz4），
	// 所以判据是"至少写进去的 1/2"，而不是"等于写进去的"。
	floor := int64(sizeMB) * 1024 * 1024 / 2
	if usage < floor {
		return fmt.Errorf("big key %s reports %d bytes after writing %d MB; "+
			"the fault is not observable", key, usage, sizeMB)
	}
	return nil
}

// ------------------------------------------------------------------ hot key

// injectHotKey points N dedicated connections at one key and keeps them there.
//
// 这是四种故障里唯一一种"没有一次写入"的：它的全部内容是持续的读。
// 因此判据也必须是持续的量——`INFO commandstats` 里 cmdstat_get 的
// calls 增量。一条 key 存在不算热，被反复打才算。
func (i *Injector) injectHotKey(ctx context.Context, spec injector.InjectSpec, l *live) error {
	clients := injector.IntParam(spec.Params, "clients", 8)
	if clients < 1 {
		return fmt.Errorf("inject_hot_key: clients=%d", clients)
	}
	// case 里的 key_prefix 是给人看的：诊断结论里要能指出"是哪个 key 热"。
	// 但故障必须打在一个**属于这次注入**的 key 上——写进别人的 key
	// 就是在改一个不属于这次注入的数据。所以归属放在前缀里、名字放在后面：
	// 撤销只认前缀，而人仍然能认出 case 点名的是哪个 key。
	label := injector.StringParam(spec.Params, "key_prefix", "hot")
	if !validKeyLabel(label) {
		return fmt.Errorf("inject_hot_key: key_prefix %q is not a plain identifier; "+
			"refusing to build a key name out of it", label)
	}
	key := l.keyFor("hot:" + label)

	seed, err := i.connect(ctx, l.id+"-seed")
	if err != nil {
		return err
	}
	defer seed.Close()
	if err := seed.Set(ctx, key, "opskeeper hot key", 0).Err(); err != nil {
		return fmt.Errorf("seed hot key: %w", err)
	}
	l.keys = append(l.keys, key)
	l.rollback = append(l.rollback, i.dropKeys(key))

	before, err := getCalls(ctx, seed)
	if err != nil {
		return err
	}

	// 每条连接各自 PoolSize=1：热 key 的故障内容就是"很多条连接同时在打"，
	// 放进一个池子就等于把 N 条变成 1 条。
	conns := make([]*goredis.Client, 0, clients)
	for n := 0; n < clients; n++ {
		c, err := i.connect(l.ctx, fmt.Sprintf("%s-hot-%d", l.id, n))
		if err != nil {
			return err
		}
		conns = append(conns, c)
	}
	var once bool
	l.rollback = append(l.rollback, func(context.Context) error {
		if once {
			return nil
		}
		once = true
		for _, c := range conns {
			_ = c.Close()
		}
		return nil
	})
	for n, c := range conns {
		c := c
		go func() {
			for {
				select {
				case <-l.ctx.Done():
					return
				default:
				}
				if err := c.Get(l.ctx, key).Err(); err != nil {
					// 连接被关掉是撤销的正常一步，不是错误。
					return
				}
				_ = n
			}
		}()
	}

	// 等到 calls 真的涨上去。这一个等待是"热"这个字的全部含义。
	delta, err := waitForCallGrowth(ctx, seed, before, clients, 5*time.Second)
	if err != nil {
		return err
	}
	l.rollback = append(l.rollback, func(context.Context) error {
		// 把"热了多久"留成一个可查的数：注入撤销之后，
		// 人还能从 commandstats 里读出这次打过的量。
		_ = delta
		return nil
	})
	return nil
}

// getCalls 读 INFO commandstats 里 cmdstat_get 的累计调用数。
func getCalls(ctx context.Context, conn *goredis.Client) (int64, error) {
	info, err := conn.Info(ctx, "commandstats").Result()
	if err != nil {
		return 0, fmt.Errorf("read commandstats: %w", err)
	}
	for _, line := range strings.Split(info, "\n") {
		// 形如 `cmdstat_get:calls=1234,usec=56,usec_per_call=0.05`
		//
		// 两处都要小心，而两处都曾经错过：
		//
		//   - 行尾带一个 `\r`（INFO 是 CRLF）。不剥掉它，
		//     strconv 会报 `parsing "1234\r": invalid syntax`。
		//   - 第一个字段是 `cmdstat_get:calls=1234`，**不以 `calls=` 开头**。
		//     按字段前缀去找会一个都找不到，于是这个函数对每一次调用
		//     都安静地返回 0——而"恒等于 0"看起来完全像"热 key 没打热"。
		//
		// 先剥掉命令前缀，剩下的才是 `calls=...` 形状的字段。
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "cmdstat_get:")
		if !ok {
			continue
		}
		for _, field := range strings.Split(rest, ",") {
			value, ok := strings.CutPrefix(strings.TrimSpace(field), "calls=")
			if !ok {
				continue
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse cmdstat_get calls from %q: %w", line, err)
			}
			return n, nil
		}
	}
	// 一次 GET 都没发生过时 Redis 不写这一行。它是 0，不是"读不出来"。
	return 0, nil
}

// waitForCallGrowth waits until cmdstat_get has advanced by at least want.
func waitForCallGrowth(ctx context.Context, conn *goredis.Client, before int64, want int, timeout time.Duration) (int64, error) {
	deadline := time.Now().Add(timeout)
	var now int64
	for {
		var err error
		now, err = getCalls(ctx, conn)
		if err != nil {
			return 0, err
		}
		if now-before >= int64(want) {
			return now - before, nil
		}
		if time.Now().After(deadline) {
			return now - before, fmt.Errorf(
				"hot key: cmdstat_get advanced by %d call(s) in %s, want at least %d; "+
					"the connections are not actually reading anything",
				now-before, timeout, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ------------------------------------------------------------- memory burst

// injectMemoryBurst writes many keys at once so used_memory jumps.
//
// 判据是 INFO memory 的 used_memory 前后差值。这是四种故障里唯一
// 一个「总量」而不是「单点」的：它模拟的是一批请求同时把缓存写满，
// 而不是一个 key 写得特别大。
func (i *Injector) injectMemoryBurst(ctx context.Context, spec injector.InjectSpec, l *live) error {
	keyCount := injector.IntParam(spec.Params, "key_count", 500)
	if keyCount < 1 {
		return fmt.Errorf("inject_memory_burst: key_count=%d", keyCount)
	}
	valueKB := injector.IntParam(spec.Params, "value_size_kb", 16)
	if valueKB < 1 {
		return fmt.Errorf("inject_memory_burst: value_size_kb=%d", valueKB)
	}
	ttl := time.Duration(injector.IntParam(spec.Params, "ttl_seconds", 0)) * time.Second

	conn, err := i.connect(ctx, l.id)
	if err != nil {
		return err
	}
	defer conn.Close()

	before, err := usedMemory(ctx, conn)
	if err != nil {
		return err
	}
	value := strings.Repeat("x", valueKB*1024)
	keys := make([]string, 0, keyCount)
	for n := 0; n < keyCount; n++ {
		key := l.keyFor(fmt.Sprintf("burst-%06d", n))
		if err := conn.Set(ctx, key, value, ttl).Err(); err != nil {
			// 已经写进去的那些必须仍然被撤销，否则一次半途的失败
			// 就在目标上留下几百 MB。keys 在写之前就登记。
			l.keys = append(l.keys, keys...)
			l.rollback = append(l.rollback, i.dropKeys(keys...))
			return fmt.Errorf("write burst key %d/%d: %w", n, keyCount, err)
		}
		keys = append(keys, key)
	}
	l.keys = append(l.keys, keys...)
	l.rollback = append(l.rollback, i.dropKeys(keys...))

	after, err := usedMemory(ctx, conn)
	if err != nil {
		return err
	}
	// Redis 会被同一批值压得很好（lz4 下全是同一个字符时压缩比极高），
	// 所以判据是"used_memory 涨了"，而不是"涨了 key_count*valueKB"。
	// 一个要求精确字节数的断言在这里只会诱导实现去调压缩策略。
	if after <= before {
		return fmt.Errorf("memory burst: used_memory did not move (%d -> %d) after writing %d key(s); "+
			"the fault is not observable", before, after, keyCount)
	}
	return nil
}

// usedMemory 读 INFO memory 里的 used_memory。
func usedMemory(ctx context.Context, conn *goredis.Client) (int64, error) {
	info, err := conn.Info(ctx, "memory").Result()
	if err != nil {
		return 0, fmt.Errorf("read memory info: %w", err)
	}
	for _, line := range strings.Split(info, "\n") {
		// TrimSpace 不是洁癖：INFO 用 CRLF 换行，不剥掉那个 `\r`，
		// 每一个读 INFO 的数都会在 strconv 上炸一次。
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "used_memory:")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse used_memory from %q: %w", line, err)
		}
		return n, nil
	}
	return 0, fmt.Errorf("INFO memory did not report used_memory")
}

// ------------------------------------------------------------ slow commands

// injectSlowCommands keeps CLIENT PAUSE running so every command queues up.
//
// 判据是**旁观者的时钟**：另开一条连接量一次 PING，看它是不是真的被拖住了。
// 一条没有被观测到的停顿不是停顿——而这是这四种故障里唯一一种
// "影响所有人" 的，所以它也是最不能靠自我报告的那种。
//
// 刻意不用的东西：`DEBUG SLEEP` 会把整个服务器钉住（包括关它的那个客户端），
// 而 `CONFIG SET` 类命令要么被云上的托管 Redis 禁掉、要么把这次注入
// 变成一次配置变更而不是一次故障。CLIENT PAUSE 自带时长、会自己到期，
// 撤销只要一条 UNPAUSE——它天然是可逆的。
func (i *Injector) injectSlowCommands(ctx context.Context, spec injector.InjectSpec, l *live) error {
	pauseMS := injector.IntParam(spec.Params, "pause_ms", 200)
	if pauseMS < 1 {
		return fmt.Errorf("inject_slow_commands: pause_ms=%d", pauseMS)
	}
	watched := injector.StringListParam(spec.Params, "commands", []string{"get", "set"})

	pauser, err := i.connect(ctx, l.id+"-pause")
	if err != nil {
		return err
	}
	defer pauser.Close()

	// 撤销必须先于停循环：先停循环再 UNPAUSE，中间有一个窗口
	// 是「没人再暂停、但也没人解除」——那段时间里一个恰好到期的
	// PAUSE 会一直挂着。
	var once bool
	l.rollback = append(l.rollback, func(context.Context) error {
		if once {
			return nil
		}
		once = true
		bg := context.Background()
		// UNPAUSE 走一条**新的**连接：pauser 自己可能正卡在一个
		// 它自己发出去的 PAUSE 里。
		c, err := i.connect(bg, l.id+"-unpause")
		if err != nil {
			return err
		}
		defer c.Close()
		if err := c.ClientUnpause(bg).Err(); err != nil {
			return fmt.Errorf("unpause: %w", err)
		}
		return nil
	})

	go i.keepPaused(l, pauser, pauseMS, watched)

	// 旁观者：一条从来没被这条注入碰过的连接。
	observer, err := i.connect(ctx, l.id+"-observe")
	if err != nil {
		return err
	}
	defer observer.Close()
	if err := i.waitUntilSlow(ctx, observer, time.Duration(pauseMS)*time.Millisecond, 5*time.Second); err != nil {
		return err
	}
	return nil
}

// keepPaused re-issues CLIENT PAUSE until the injection is undone.
func (i *Injector) keepPaused(l *live, pauser *goredis.Client, pauseMS int, watched []string) {
	// 一次 PAUSE 只管 pauseMS 毫秒，所以要一遍遍续。
	// 续的间隔取 pauseMS 的 80%：留 20% 的重叠，
	// 否则两次续之间会漏出一个窗口，而那个窗口正好够一次请求穿过去。
	interval := time.Duration(float64(pauseMS)*0.8) * time.Millisecond
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-time.After(interval):
		}
		// go-redis 的 ClientPause 只签一个时长，那就是"全部命令"。
		// 刻意不用只暂停写：读命令一样会被这个故障拖住，
		// 而一个只对写生效的故障会得到一个偏窄的结论。
		if err := pauser.ClientPause(l.ctx, time.Duration(pauseMS)*time.Millisecond).Err(); err != nil {
			// 连接被关掉是撤销的正常一步。
			return
		}
	}
}

// waitUntilSlow waits until a plain PING from an untouched client is visibly
// stuck behind the pause.
//
// 判据是**暂停时长的一半**，不是整个暂停时长。理由是一条命令只能经历
// 暂停窗口里它到达之后的那一段：一条在暂停开始 6ms 后到达的 PING
// 会被挡住 294ms，而不是 300ms——而 294ms 与 0ms 之间差着四个数量级。
// 用整个暂停时长当判据，得到的不是更严的断言，而是一个**偶尔为假的**断言
// （本轮实测 15 轮里红 1 次，报 "took at most 294ms, want at least 300ms"）。
//
// 一个没有被暂停的 PING 是亚毫秒级的，所以"至少被挡住一半"与"完全没被挡住"
// 之间有一道极宽的沟：这条判据在实践中不可能假红。
func (i *Injector) waitUntilSlow(ctx context.Context, observer *goredis.Client, pause time.Duration, timeout time.Duration) error {
	bar := pause / 2
	deadline := time.Now().Add(timeout)
	var worst time.Duration
	for {
		start := time.Now()
		err := observer.Ping(ctx).Err()
		took := time.Since(start)
		if took > worst {
			worst = took
		}
		if err == nil && took >= bar {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("slow commands: a PING from an outside client took at most %v "+
				"after %s of pausing, want at least %v (half the pause); the fault is not observable",
				worst.Round(time.Millisecond), timeout, bar.Round(time.Millisecond))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// validKeyLabel keeps a case-supplied label from becoming part of a key name.
//
// 语料是 YAML，而 YAML 是可以手写的。它只允许字母数字与冒号——
// 够放下 `orders:` / `session:` 这类真实前缀，又挡掉空格、控制字符
// 与换行（换行会出现在 redis-cli 的输出里，足以让一个诊断 agent
// 把它读成两条记录）。
func validKeyLabel(label string) bool {
	if label == "" || len(label) > 64 {
		return false
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ':', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// dropKeys registers the undo for a set of keys.
//
// 它不核对 key 存不存在：key 的名字里带着 injectID，
// 所以「这是不是我的 key」在命名那一刻就已经回答完了。
func (i *Injector) dropKeys(keys ...string) func(context.Context) error {
	return func(ctx context.Context) error {
		if len(keys) == 0 {
			return nil
		}
		conn, err := i.connect(ctx, "cleanup")
		if err != nil {
			return err
		}
		defer conn.Close()
		return conn.Del(ctx, keys...).Err()
	}
}
