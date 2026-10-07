// Package redis is the Redis fault injector.
//
// Decision 297 turned pg from a skeleton into a real implementation and left
// this one untouched. It is the second one now, and the same four rules apply
// — the ones that matter are not "connect to Redis" but:
//
//  1. A fault is observable from outside. A big key is a number you can read
//     back with MEMORY USAGE; a hot key is a call-count delta in
//     INFO commandstats; a memory burst is a jump in INFO memory's
//     used_memory; slow commands are a client's own measured latency.
//     None of them is "Inject returned an ID".
//  2. It is reversible, and **it only ever touches keys it made**. Every key
//     this injector writes carries the injector's own id in its name, so
//     cleanup deletes by ownership rather than by remembering what it wrote.
//     That is stronger than pg's marker-on-comment approach, and it is the
//     shape the next injectors should copy: a name that cannot collide is
//     better than a name you have to verify.
//  3. It expires on its own. InjectSpec.Duration is read here too.
//  4. It is scoped. It never touches a key that was not created by this
//     injection, and it never runs a command that would disable the server.
package redis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// 两条环境变量，形状与 pg 的 DSNEnv 对齐。
//
// 地址与口令分开，而不是一个 URL：opskeeper 自己的配置就是这两个字段
// （goredis.Options{Addr, Password}），合成一个 URL 只会让装配根再多做一次
// 翻译，而那一次翻译没有任何信息量。
const (
	AddrEnv     = "OPSKEEPER_HARNESS_REDIS_ADDR"
	PasswordEnv = "OPSKEEPER_HARNESS_REDIS_PASSWORD"
)

// connectTimeout bounds CheckAvailable.
const connectTimeout = 5 * time.Second

// keyPrefix 是这个注入器写的每一个 key 的前缀。
//
// 它是**归属**而不是命名空间美学：前缀后面跟着的是 injectID，
// 而 injectID 是本进程生成的。所以「我只删我建的 key」不需要任何一次
// 额外的核对——key 的名字本身就是凭证。
const keyPrefix = "opskeeper:fault:"

// clientNamePrefix 让 CLIENT LIST 能认出哪些连接是注入器的。
//
// 这是"故障从外面看得见"的一部分：一条热 key 的故障应该有若干条具名连接
// 在打同一个 key，而不是一个匿名的连接池。
const clientNamePrefix = "opskeeper-fault-"

// supportedTypes 是这个注入器认识的全部注入类型。
var supportedTypes = map[string]bool{
	"redis.inject_big_key":       true,
	"redis.inject_hot_key":       true,
	"redis.inject_memory_burst":  true,
	"redis.inject_slow_commands": true,
}

// SupportedTypes 返回这个注入器认识的全部注入类型（已排序）。
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
	timer    *time.Timer
	duration time.Duration
	// keys 是这次注入写下的全部 key 名。撤销时按名字删，
	// 不依赖"我记得写过什么"。
	keys []string
	ctx  context.Context
	// err 让 Inject 的 defer 知道要不要立刻撤销半截故障。
	err error
	// cancel 停掉派生出来的后台循环（热 key 的那些 goroutine）。
	cancel context.CancelFunc
}

// Injector is the Redis fault injector.
type Injector struct {
	addr     string
	password string
	// addrSet 区分"没给"和"显式给了空"。
	addrSet bool
	mu      sync.Mutex
	seq     int
	live    map[string]*live
}

// Option configures an Injector.
type Option func(*Injector)

// WithAddr supplies a Redis address.
func WithAddr(addr string) Option {
	return func(i *Injector) { i.addr = addr; i.addrSet = true }
}

// WithPassword supplies a Redis password.
func WithPassword(pw string) Option {
	return func(i *Injector) { i.password = pw }
}

// New builds a Redis injector. With no address it constructs fine and reports
// itself unavailable — "constructed" and "claimed to work" are not the same
// statement.
func New(opts ...Option) *Injector {
	i := &Injector{live: map[string]*live{}}
	for _, opt := range opts {
		opt(i)
	}
	if !i.addrSet {
		i.addr = os.Getenv(AddrEnv)
	}
	if i.password == "" {
		i.password = os.Getenv(PasswordEnv)
	}
	return i
}

// Type returns the type prefix.
func (i *Injector) Type() string { return "redis." }

// CheckAvailable reports whether this injector can actually stage a fault.
func (i *Injector) CheckAvailable(ctx context.Context) error {
	if i.addr == "" {
		return fmt.Errorf("%w: 没有配置 Redis 地址（设 %s，或用 WithAddr 传进来）",
			injector.ErrUnavailable, AddrEnv)
	}
	dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := i.connect(dialCtx, "probe")
	if err != nil {
		return fmt.Errorf("%w: 连不上 %s: %w", injector.ErrUnavailable, i.addr, err)
	}
	defer conn.Close()
	if err := conn.Ping(dialCtx).Err(); err != nil {
		return fmt.Errorf("%w: %s 连上了但 PING 失败: %w", injector.ErrUnavailable, i.addr, err)
	}
	return nil
}

// connect opens one dedicated, named connection.
//
// **不共用连接池**，理由与 pg 那一份相同但更强一点：热 key 这个故障的
// 全部内容就是"很多条连接同时在打一个 key"，把它放进一个池子，
// 池子会在下一次 Acquire 时把那条被钉住的连接借给别人，于是
// CLIENT LIST 里看到的名字与实际在施压的连接对不上。
// 每条被持有的连接都是专属的，也因此在撤销时被专属地关掉。
func (i *Injector) connect(ctx context.Context, name string) (*goredis.Client, error) {
	return goredis.NewClient(&goredis.Options{
		Addr:     i.addr,
		Password: i.password,
		// 单连接：注入器要的是"我知道我在用哪一条"，不是一个池子。
		PoolSize:     1,
		MinIdleConns: 0,
		ClientName:   clientNamePrefix + name,
	}), nil
}

// Inject stages one fault.
func (i *Injector) Inject(ctx context.Context, spec injector.InjectSpec) (*injector.InjectResult, error) {
	if !supportedTypes[spec.Type] {
		return nil, fmt.Errorf("%w: %s", injector.ErrUnsupportedType, spec.Type)
	}
	if err := i.CheckAvailable(ctx); err != nil {
		return nil, err
	}

	l := i.begin(spec)
	defer func() {
		if l != nil && l.err != nil {
			_ = i.runRollback(context.Background(), l)
			// 并且把这一条从账本里划掉——理由见 pg 注入器里同一段注释：
			// 一次失败的注入留在 Live() 里，Cleanup(id) 就会把它当成
			// "已生效并被撤销"，而它其实从没成功过。
			i.forget(l.id)
		}
	}()

	var err error
	switch spec.Type {
	case "redis.inject_big_key":
		err = i.injectBigKey(ctx, spec, l)
	case "redis.inject_hot_key":
		err = i.injectHotKey(ctx, spec, l)
	case "redis.inject_memory_burst":
		err = i.injectMemoryBurst(ctx, spec, l)
	case "redis.inject_slow_commands":
		err = i.injectSlowCommands(ctx, spec, l)
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
		id = fmt.Sprintf("redis-inj-%d-%d", time.Now().UTC().UnixNano(), i.seq)
	}
	lctx, lcancel := context.WithCancel(context.Background())
	l := &live{
		id:       id,
		typ:      spec.Type,
		started:  time.Now().UTC(),
		duration: spec.Duration,
		ctx:      lctx,
		cancel:   lcancel,
	}
	i.live[id] = l
	return l
}

// startExpiry is where InjectSpec.Duration is read.
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
		"backend": "go-redis",
		"addr":    i.addr,
	}
	// 这次注入动过的 key 名单。
	//
	// 它进 result 是因为两方都要用：人被告知"我在哪几条 key 上放了故障"
	// （撤销之后还能从 Redis 自己的 TTL 与 commandstats 里对账），
	// 而"撤销之后这些 key 真的不在了"这个断言需要一个可枚举的对象。
	//
	// 上一版这里是 `keysOf` 这个只被测试调用的方法——那正是死代码闸门
	// 要抓的东西：一个只挂在测试上的公开方法，说的不是生产需要什么，
	// 而是说测试需要什么。
	if len(l.keys) > 0 {
		meta["keys"] = strings.Join(l.keys, ",")
		meta["key_count"] = fmt.Sprintf("%d", len(l.keys))
	}
	if l.duration > 0 {
		meta["expires"] = l.started.Add(l.duration).Format(time.RFC3339)
	}
	return &injector.InjectResult{
		InjectID:   l.id,
		Type:       l.typ,
		ResourceID: l.id,
		StartedAt:  l.started,
		Metadata:   meta,
	}
}

// Cleanup undoes one injection. It is idempotent.
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
	// 先停后台循环，再删 key：循环还在跑的时候删掉它，
	// 撤销完它会再写回去。
	if l.cancel != nil {
		l.cancel()
	}
	return i.runRollback(ctx, l)
}

// runRollback runs the undo steps in reverse order.
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

// keyFor 造一个属于这次注入的 key 名。
func (l *live) keyFor(suffix string) string {
	return keyPrefix + l.id + ":" + suffix
}

var _ injector.Injector = (*Injector)(nil)
