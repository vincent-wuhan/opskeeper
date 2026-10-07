// Package host is the host fault injector.
//
// This is the **most dangerous** of the six injectors, and its design is shaped
// by that fact more than by anything else. `host.fill_disk` writes real blocks
// to a real filesystem; done carelessly it wedges the very machine the agent
// runs on, and there is no rollback for a wedged machine. So this package has
// three guards that the other four injectors do not need, and all three are
// non-negotiable:
//
//  1. **It can only touch one directory, and only if it was told which.**
//     RootEnv names it. With no root the injector reports itself unavailable —
//     it does not fall back to the temp directory, because "the temp
//     directory" on a node agent is usually the node's root filesystem.
//  2. **It refuses the filesystem root even when told to use it.** A
//     misconfigured root must not be able to say "yes" to filling `/`.
//  3. **It never crosses a free-space floor.** The floor is checked before the
//     first byte and again before every chunk, so a slow race (another process
//     filling the same disk) makes the injection stop, not overshoot.
//
// The four rules the pg and redis injectors hold to still apply: a fault is
// observable from outside, it is reversible, it expires on its own, and it is
// scoped. The scope rule here is stronger than "only what I created" — every
// byte this package writes lives in a directory named after the injection's
// own id, so cleanup is one RemoveAll of a directory nobody else can be using.
package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// RootEnv 指向这个注入器**唯一允许写入**的目录。
//
// 没有它就是不可用。它不读一个"合理的默认值"：在节点 agent 上，
// 任何形式的默认目录都极可能就是节点的根文件系统。
const RootEnv = "OPSKEEPER_HARNESS_HOST_ROOT"

const (
	// defaultMinFreeMB 是默认的可用空间地板。
	//
	// 2048MB 不是随便取的：一个还在运行的 PostgreSQL / Redis 在磁盘被写满时
	// 的行为是丢数据或崩溃，而那正是这次注入之后我们要诊断的东西——
	// 注入本身不应该顺手把被测服务也带走。
	defaultMinFreeMB = 2048
	// defaultMaxBytes 是这一次注入允许写的上限。
	// 地板是"还剩多少"，上限是"这次最多动多少"；两道闸门缺一不可。
	defaultMaxBytes int64 = 4 << 30
	// chunkBytes 是一次写多少。
	//
	// 1MB 是为了两件事：每写一块就重新查一次地板（所以地板不会被跨过去），
	// 以及单次系统调用不会太长（一个 30 秒不可中断的 write 会让
	// "按 Ctrl-C 提前撤销"变成一句空话）。
	chunkBytes = 1 << 20
)

// supportedTypes 是这个注入器认识的全部注入类型。
var supportedTypes = map[string]bool{
	"host.cpu_stress": true,
	"host.fill_disk":  true,
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
	// dir 是这次注入自己的目录（fill_disk 用），或空串。
	dir string
	// paths 是这次注入动过的路径，进 result 供人对账。
	paths []string
	ctx   context.Context
	// cancel 停掉派生出来的 CPU 负载。
	cancel context.CancelFunc
	// workers 是 cpu_stress 起的 goroutine 数，进 result 供人核对。
	workers int
	// utilization 是量到的单 worker 饱和度百分比（0-100），
	// 进 result 因为 case 声明了 target_load，而人与 case 都该看到差多少。
	utilization int
	err         error
}

// Injector is the host fault injector.
type Injector struct {
	root    string
	rootSet bool
	mu      sync.Mutex
	seq     int
	live    map[string]*live
}

// Option configures an Injector.
type Option func(*Injector)

// WithRoot names the only directory this injector may write in.
func WithRoot(dir string) Option {
	return func(i *Injector) { i.root = dir; i.rootSet = true }
}

// New builds a host injector.
func New(opts ...Option) *Injector {
	i := &Injector{live: map[string]*live{}}
	for _, opt := range opts {
		opt(i)
	}
	if !i.rootSet {
		i.root = os.Getenv(RootEnv)
	}
	return i
}

// ErrMachineBusy says the machine is too busy to host this fault *right
// now*, as opposed to the injector being unable to do its job.
//
// 这两件事必须能被 caller 分开，因为它们要的后续动作正好相反：
// 前者是在一台繁忙的机器上重试或者换节点，后者是注入器坏了要去修。
//
// 一个两者都报"注入失败"的返回，会让排障的人先去查代码——而代码是对的，
// 那台机器上此刻根本造不出这个故障。它包着 ErrUnavailable 而不是自成一个
// 类型：不可用是它的上位判断，这个错误只是把"为什么此刻不可用"说得更细。
var ErrMachineBusy = fmt.Errorf("%w: 这台机器此刻太忙，这个故障造不出来", injector.ErrUnavailable)

// Type returns the type prefix.
func (i *Injector) Type() string { return "host." }

// CheckAvailable reports whether this injector can actually stage a fault.
//
// 它检查的是**真的会出现什么症状**，不是"这个包能不能编译"：
// 目录在不在、能不能写、以及它是不是一个我们会拒绝的路径。
// 一个报"可用"然后在 Inject 里才发现写不进去的注入器，
// 比一个诚实的 ErrUnavailable 差得多。
func (i *Injector) CheckAvailable(ctx context.Context) error {
	if i.root == "" {
		return fmt.Errorf("%w: 没有配置 %s（这个注入器只往一个被明确指定的目录里写；"+
			"不设它就是不可用——在节点 agent 上，任何默认目录都可能就是根文件系统）",
			injector.ErrUnavailable, RootEnv)
	}
	if err := i.checkRoot(); err != nil {
		return fmt.Errorf("%w: %s=%s: %w", injector.ErrUnavailable, RootEnv, i.root, err)
	}
	return nil
}

// checkRoot refuses the paths that must never be filled.
func (i *Injector) checkRoot() error {
	abs, err := filepath.Abs(i.root)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	// root 也要解析符号链接。
	//
	// macOS 上 /var 是指向 /private/var 的符号链接，所以一个未经解析的
	// /var/folders/... 与一个解析过的 /private/var/folders/... 在
	// filepath.Rel 眼里是"一个在另一个之外"——于是每一个**合法的** case
	// path 都会被那条"path 不得超出 root"的闸门拒掉，而那条闸门
	// 恰恰是这个包最重要的一道。两边必须先归到同一个命名空间。
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	i.root = abs
	if abs == "/" {
		// 这条不是防御性编程，是**唯一**一道拦住"有人把 root 配成 /"的闸门。
		// 而它必须在这里而不是在 Inject 里：Inject 的失败会被当成
		// "环境不支持"，而这一次失败的原因是一个配错了的变量。
		return fmt.Errorf("refusing to use the filesystem root; a fault injector that can fill / is a fault injector that can wedge the machine")
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}
	probe, err := os.CreateTemp(abs, ".opskeeper-probe-")
	if err != nil {
		return fmt.Errorf("not writable: %w", err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
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
	case "host.fill_disk":
		err = i.fillDisk(ctx, spec, l)
	case "host.cpu_stress":
		err = i.cpuStress(ctx, spec, l)
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
		id = fmt.Sprintf("host-inj-%d-%d", time.Now().UTC().UnixNano(), i.seq)
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
		"backend": "host-local",
		"root":    i.root,
	}
	if l.dir != "" {
		meta["dir"] = l.dir
	}
	if l.workers > 0 {
		meta["workers"] = fmt.Sprintf("%d", l.workers)
		meta["utilization"] = fmt.Sprintf("%d", l.utilization)
	}
	if len(l.paths) > 0 {
		meta["files"] = fmt.Sprintf("%d", len(l.paths))
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

// diskFree reports the free bytes on the filesystem holding path.
//
// 它走 statfs 而不是 `df` 的文本输出：`df` 的列宽、locale 与单位
// 都是给人看的，而这一轮已经付过一次"解析一个格式会漂移的文本输出"的
// 代价（§4.232 的 cmdstat_get）。同一个错误不该在一个仓库里犯两次。
func diskFree(path string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	// darwin 上 Bsize 是 int32 而 linux 上是 int64；先转成 int64 再乘，
	// 否则在 32 位平台上 Bavail*Bsize 会先溢出。
	bs := int64(st.Bsize)
	return int64(st.Bavail) * bs, int64(st.Blocks) * bs, nil
}

// cpuSeconds returns this process's CPU time (user + system) in seconds.
//
// 判据必须是**内核量的**，不能是自己循环里数的迭代次数：
// 一个自己数自己的循环，在被优化掉、被调度掉或者根本没跑起来的时候，
// 报出来的数都是漂亮的。Getrusage 是内核在每次时钟中断里累加的，
// 它骗不了人。
//
// RUSAGE_SELF 而不是 RUSAGE_CHILDREN：负载是本进程在烧，这是设计选择
// （见 cpuStress 的注释），不是缺陷。
func cpuSeconds() (float64, error) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, fmt.Errorf("getrusage: %w", err)
	}
	toSec := func(tv syscall.Timeval) float64 {
		return float64(tv.Sec) + float64(tv.Usec)/1e6
	}
	// 用户态 + 内核态。只算一个会漏掉系统调用那一半，而
	// 一个写 1MB 的负载在系统态上的时间是相当可见的。
	return toSec(ru.Utime) + toSec(ru.Stime), nil
}

var _ injector.Injector = (*Injector)(nil)
