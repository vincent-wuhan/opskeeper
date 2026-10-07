package redis

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// 这一组测试要一个真的 Redis。它不是 mock，也不是 miniredis：
// 一个 big key 的判据是 MEMORY USAGE，一个热 key 的判据是
// `INFO commandstats` 里的 calls 增量，一个内存突刺的判据是
// `INFO memory` 的 used_memory 差值——这三样 mock 一个都答不出来，
// 而一个答不出来的判据不是判据。
//
// 门槛只有一个环境变量。**设了就连不上必须红。**
// 没设则整组跳过，并在输出里说清差什么。
func liveInjector(t *testing.T) *Injector {
	t.Helper()
	addr := os.Getenv(AddrEnv)
	if addr == "" {
		t.Skipf("%s not set; this test injects into a real Redis and will not pretend to", AddrEnv)
	}
	i := New(WithAddr(addr), WithPassword(os.Getenv(PasswordEnv)))
	if err := i.CheckAvailable(context.Background()); err != nil {
		t.Fatalf("%s is set but the database is not usable: %v", AddrEnv, err)
	}
	return i
}

// probe 是一条与被测注入无关的旁观连接：它用来读那些"从外面看得见"的量。
func probe(t *testing.T) *goredis.Client {
	t.Helper()
	c := goredis.NewClient(&goredis.Options{
		Addr:       os.Getenv(AddrEnv),
		Password:   os.Getenv(PasswordEnv),
		ClientName: "opskeeper-test-observer",
	})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// keysOf 从注入结果里读出这次注入动过的 key 名单。
func keysOf(t *testing.T, res *injector.InjectResult) []string {
	t.Helper()
	raw, ok := res.Metadata["keys"]
	if !ok || raw == "" {
		t.Fatalf("result metadata %v does not name the keys it wrote; "+
			"without them there is nothing to assert cleanup against", res.Metadata)
	}
	return strings.Split(raw, ",")
}

// keyBytes 读一条 key 占多少字节；key 不在就是 0。
//
// 它用 EXISTS 而不是 `GET ... Int()`：后者在 key 不存在时返回 `redis: nil`，
// 而"key 不在了"恰恰是这些断言要的成功结果——用 GET 去表达"它没了"，
// 会在最该绿的那一刻红掉。
func keyBytes(t *testing.T, c *goredis.Client, key string) int {
	t.Helper()
	n, err := c.Exists(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("EXISTS %s: %v", key, err)
	}
	return int(n)
}

// 一个 big key 必须真的占着那块内存，而且撤销之后一个字节都不剩。
//
// 判据是 MEMORY USAGE 而不是 STRLEN：前者报这条 key 真正占了多少，
// 后者只报值有多长。而"搬一次它要花多少"才是 big key 这个故障的全部含义。
func TestABigKeyOccupiesMemoryAndCleanupTakesItAway(t *testing.T) {
	i := liveInjector(t)
	obs := probe(t)

	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "redis.inject_big_key",
		Duration: 60 * time.Second,
		Params:   map[string]any{"value_size_mb": 4},
	})
	if err != nil {
		t.Fatalf("inject big key: %v", err)
	}
	if _, ok := res.Metadata["expires"]; !ok {
		t.Errorf("metadata = %v, want an expiry: InjectSpec.Duration must be read", res.Metadata)
	}
	keys := keysOf(t, res)
	usage, err := obs.MemoryUsage(context.Background(), keys[0]).Result()
	if err != nil {
		t.Fatalf("MEMORY USAGE %s: %v", keys[0], err)
	}
	if usage < 2*1024*1024 {
		t.Errorf("MEMORY USAGE = %d bytes after writing 4 MB; the fault is not observable", usage)
	}

	if err := i.Cleanup(context.Background(), res.InjectID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n := keyBytes(t, obs, keys[0]); n != 0 {
		t.Errorf("key %s still holds %d bytes after cleanup; the injector left damage behind", keys[0], n)
	}
}

// 热 key 的判据是持续的读，不是"这条 key 存在"。
//
// 一条 key 存在不算热，被反复打才算。所以这里量的是
// `INFO commandstats` 里 cmdstat_get 的 calls 增量——
// 一个不需要看注入器内部状态的数。
func TestAHotKeyActuallyGetsRead(t *testing.T) {
	i := liveInjector(t)
	obs := probe(t)

	before, err := getCalls(context.Background(), obs)
	if err != nil {
		t.Fatalf("read commandstats: %v", err)
	}
	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "redis.inject_hot_key",
		Duration: 60 * time.Second,
		Params:   map[string]any{"clients": 6, "key_prefix": "orders"},
	})
	if err != nil {
		t.Fatalf("inject hot key: %v", err)
	}
	after, err := getCalls(context.Background(), obs)
	if err != nil {
		t.Fatalf("read commandstats: %v", err)
	}
	if after-before < 6 {
		t.Errorf("cmdstat_get advanced by %d call(s) during injection, want at least 6; "+
			"the connections are not actually reading anything", after-before)
	}
	// 连接必须具名：CLIENT LIST 是"故障从外面看得见"的另一半。
	clients, err := obs.ClientList(context.Background()).Result()
	if err != nil {
		t.Fatalf("CLIENT LIST: %v", err)
	}
	// go-redis 的 ClientList 返回的是一行一个客户端的原始文本，
	// 所以这里读的是行内的 name= 字段，而不是结构体。
	named := 0
	for _, line := range strings.Split(clients, "\n") {
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "name=") && strings.Contains(field, clientNamePrefix) {
				named++
				break
			}
		}
	}
	if named < 6 {
		t.Errorf("CLIENT LIST shows %d connection(s) named %s*, want at least 6",
			named, clientNamePrefix)
	}

	// keys 必须在 Cleanup 之前取：撤销之后这一条注入已经不在账本里，
	// keysOf 诚实地返回空，而一条空切片被 [0] 一下就是一个 panic。
	keys := keysOf(t, res)
	if err := i.Cleanup(context.Background(), res.InjectID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	// 撤销之后读数必须**停住**——这才是"热"结束了。
	time.Sleep(200 * time.Millisecond)
	stopped, err := getCalls(context.Background(), obs)
	if err != nil {
		t.Fatalf("read commandstats: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	later, err := getCalls(context.Background(), obs)
	if err != nil {
		t.Fatalf("read commandstats: %v", err)
	}
	if later-stopped > 2 {
		t.Errorf("cmdstat_get advanced by %d call(s) after cleanup; the hammering is still running", later-stopped)
	}
	if n := keyBytes(t, obs, keys[0]); n != 0 {
		t.Errorf("the hot key still exists after cleanup")
	}
}

// 内存突刺的判据是 INFO memory 的 used_memory 前后差值。
func TestAMemoryBurstMovesUsedMemoryAndCleanupGivesItBack(t *testing.T) {
	i := liveInjector(t)
	obs := probe(t)

	before, err := usedMemory(context.Background(), obs)
	if err != nil {
		t.Fatalf("read used_memory: %v", err)
	}
	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "redis.inject_memory_burst",
		Duration: 60 * time.Second,
		Params:   map[string]any{"key_count": 400, "value_size_kb": 4},
	})
	if err != nil {
		t.Fatalf("inject memory burst: %v", err)
	}
	after, err := usedMemory(context.Background(), obs)
	if err != nil {
		t.Fatalf("read used_memory: %v", err)
	}
	if after <= before {
		t.Errorf("used_memory did not move (%d -> %d); the fault is not observable", before, after)
	}

	if err := i.Cleanup(context.Background(), res.InjectID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	// 内存不一定立刻还给操作系统（Redis 按 jemalloc 的节奏回收），
	// 所以判据是"我们自己写的那些 key 都不在了"，
	// 那才是这次注入欠下的债。
	keys := keysOf(t, res)
	remain := 0
	for _, k := range keys {
		if keyBytes(t, obs, k) != 0 {
			remain++
		}
	}
	if remain != 0 {
		t.Errorf("%d of %d injected keys survived cleanup", remain, len(keys))
	}
}

// 慢命令的判据是**旁观者的时钟**。
//
// 这是四种故障里唯一一种影响所有人的，所以它也最不能靠自我报告：
// 说"我已经暂停了"没有任何意义，要看一条没被碰过的连接的 PING
// 是不是真的被拖住了。
func TestSlowCommandsAreObservableToAnOutsideClient(t *testing.T) {
	i := liveInjector(t)
	obs := probe(t)

	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "redis.inject_slow_commands",
		Duration: 60 * time.Second,
		Params:   map[string]any{"pause_ms": 300},
	})
	if err != nil {
		t.Fatalf("inject slow commands: %v", err)
	}
	// 注入器自己已经量过一次旁观者的时钟了；这里再独立量一次，
	// 免得"Inject 返回了"这件事本身就成了唯一的证据。
	start := time.Now()
	if err := obs.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("observer ping: %v", err)
	}
	// 判据是暂停时长的一半，不是整个时长：一条命令只能经历暂停窗口里
	// 它到达之后的那一段，所以"≥ 300ms"对一条碰巧在暂停刚开始时到达的
	// PING 永远不成立（实测它会给出 294ms）。而没被暂停的 PING 是亚毫秒级的，
	// 所以 150ms 这道线与"完全没被挡住"之间有一道极宽的沟。
	if took := time.Since(start); took < 150*time.Millisecond {
		t.Errorf("an outside PING took %v while commands were paused for 300ms; "+
			"the fault is not observable", took.Round(time.Millisecond))
	}

	if err := i.Cleanup(context.Background(), res.InjectID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	// 撤销之后必须真的恢复了——没有 UNPAUSE 的话，一次到期的 PAUSE
	// 会让这台 Redis 一直慢下去。
	deadline := time.Now().Add(5 * time.Second)
	for {
		start := time.Now()
		if err := obs.Ping(context.Background()).Err(); err == nil && time.Since(start) < 100*time.Millisecond {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("commands are still paused 5s after cleanup; the injection was not undone")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 到期自毁是唯一一条不经过显式 Cleanup 的撤销路径，
// 所以它必须单独证明自己真的动了 Redis——账本归零不算证据。
func TestAnInjectionExpiresOnItsOwn(t *testing.T) {
	i := liveInjector(t)
	obs := probe(t)

	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "redis.inject_big_key",
		Duration: 700 * time.Millisecond,
		Params:   map[string]any{"value_size_mb": 1},
	})
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	keys := keysOf(t, res)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if keyBytes(t, obs, keys[0]) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("key %s still exists after the injection expired; "+
				"expiry cleared the bookkeeping but not the database", keys[0])
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := i.Cleanup(context.Background(), res.InjectID); err == nil {
		t.Errorf("second cleanup returned nil, want ErrInjectionNotFound")
	}
}

// 撤销必须只删这次注入自己的 key。
//
// 这条断言守的是本次设计里最贵的一个决定：key 的名字里带着 injectID，
// 所以「这是不是我的 key」在命名那一刻就回答完了，撤销不需要任何核对。
// 如果有人把 key 名改成 case 点名的那个 key，这条会红。
func TestCleanupNeverTouchesAKeyItDidNotCreate(t *testing.T) {
	i := liveInjector(t)
	obs := probe(t)

	// 一条与注入器同前缀、但 injectID 不同的 key：它属于"别人"。
	other := keyPrefix + "someone-else" + ":bigkey"
	if err := obs.Set(context.Background(), other, "not mine", 0).Err(); err != nil {
		t.Fatalf("seed foreign key: %v", err)
	}
	t.Cleanup(func() { _ = obs.Del(context.Background(), other).Err() })

	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "redis.inject_big_key",
		Duration: 60 * time.Second,
		Params:   map[string]any{"value_size_mb": 1},
	})
	if err != nil {
		t.Fatalf("inject big key: %v", err)
	}
	if err := i.Cleanup(context.Background(), res.InjectID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if v, err := obs.Get(context.Background(), other).Result(); err != nil || v != "not mine" {
		t.Errorf("a key this injection did not create was %s (err=%v); "+
			"cleanup must only ever delete what it wrote", v, err)
	}
}

// 一次失败的注入必须**从账本上消失**。
//
// 只撤销、不销账的后果：失败的注入仍然占着 Live() 里的一个名字，
// Cleanup(id) 会因为"还在账本里"而返回一个成功的撤销——
// 于是一个从没成功过的东西，看上去像是被正常撤销过了。
//
// 判据要挑一个 post-begin 的失败：类型检查与不可用检查都在 begin 之前，
// 它们天然不会留下账目。
func TestARefusedInjectionLeavesNoLedgerEntry(t *testing.T) {
	i := liveInjector(t)
	// value_size_mb=0 在 CheckAvailable 之后、任何一次 SET 之前被拒。
	_, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:     "redis.inject_big_key",
		Duration: 30 * time.Second,
		Params:   map[string]any{"value_size_mb": 0},
	})
	if err == nil {
		t.Fatal("Inject accepted value_size_mb=0; want a refusal")
	}
	if n := len(i.Live()); n != 0 {
		t.Errorf("%d injection(s) still recorded after a refusal: %v", n, i.Live())
	}
}
