package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// resolveInsideRoot 把 case 里的 path 收窄到 root 之内。
//
// 它返回的是**解析过的绝对路径**，并且拒绝任何落在 root 之外的写法——
// 包括 `../`、符号链接指向外面，以及根本就是另一个绝对路径。
// 这一条是整个包的核心闸门：case 是可以手写的 YAML，
// 而一个能被 case 指到任意目录的"磁盘写满"注入器就是一个能把
// 节点写瘫的工具。
func (i *Injector) resolveInsideRoot(p string) (string, bool, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false, fmt.Errorf("resolve %q: %w", p, err)
	}
	abs, err = resolveDeepest(abs)
	if err != nil {
		return "", false, err
	}
	root := i.root
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false, fmt.Errorf("path %q is outside the fault root %s; "+
			"a case may narrow where the fault lands, never widen it. "+
			"Set %s to a directory that contains it", p, i.root, RootEnv)
	}
	if rel == "." {
		return "", false, fmt.Errorf("path %q is the fault root itself; the injector writes into a "+
			"subdirectory named after the injection so cleanup is a single remove", p)
	}
	// **不**在这里建目录。statfs 需要它存在，但建它的那一步必须由调用方
	// 登记撤销：这一版是在这里建的，而后面还有 target_percent / max_bytes
	// 几道检查，任何一道拒绝都会把一个空目录留在目标上——
	// 一次被拒绝的注入在文件系统上留下了痕迹，而"拒绝"本该意味着什么都没发生。
	_, statErr := os.Stat(abs)
	return abs, errors.Is(statErr, os.ErrNotExist), nil
}

// resolveDeepest 解析一条路径里**已经存在**的那一段。
//
// EvalSymlinks 只能解析存在的路径，而 case 里的 path 通常指向一个
// 还不存在的子目录。直接对整条路径调它会失败，于是拿到的是一条
// 未解析的路径——在 macOS 上那意味着 /var/... 与 /private/var/...
// 被当成两个地方，于是"path 在 root 之内"这条判断会把每一个合法的
// case 都拒掉。
//
// 所以这里从最深往上找第一个存在的祖先，解析它，再把剩下的部分接回去。
// 剩下的部分不需要解析：它们还不存在，也就没有符号链接。
func resolveDeepest(abs string) (string, error) {
	remainder := []string{}
	current := abs
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for n := len(remainder) - 1; n >= 0; n-- {
				resolved = filepath.Join(resolved, remainder[n])
			}
			return resolved, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			// 一路走到根都解析不了；交出未解析的版本，
			// 让下面那道"不得超出 root"的比较去拒绝它。
			return abs, nil
		}
		remainder = append(remainder, filepath.Base(current))
		current = parent
	}
}

// ---------------------------------------------------------------- fill disk

// fillDisk writes real blocks until the filesystem crosses a target.
//
// 判据是 statfs 的可用字节前后差值——一个**从外面量得到**的数：
// `df` 读到多少、写入发起了多少块、文件多大，都不是同一件事。
//
// 三道闸门按顺序生效，任何一道命中就当场停：
//
//	地板：还剩的字节不得低于 min_free（默认 2048MB）。
//	      写之前查一次，每写一块再查一次——所以一个"别的进程也在写这块盘"
//	      的竞争只会让注入提前收手，不会让它冲过地板。
//	上限：这一次最多写 max_bytes。地板管"还剩多少"，上限管"这次动多少"。
//	目录：全部写在 <root>/opskeeper-fault-<injectID>/ 下面。
func (i *Injector) fillDisk(ctx context.Context, spec injector.InjectSpec, l *live) error {
	// case 里的 `path` 只能收窄范围，不能扩大。
	//
	// 语料写的是 `path: /tmp/test-fill`，而让它能指到 root 之外的任何地方
	// 就等于把"只往一个被指定目录里写"这道闸门拆了——那正是这个包
	// 存在的理由。所以 `path` 是**相对于 root 的一次收窄**：
	// 落在 root 之外的写法不是被忽略，而是被明确拒绝并说出该配什么。
	target := i.root
	if p := injector.StringParam(spec.Params, "path", ""); p != "" {
		resolved, missing, err := i.resolveInsideRoot(p)
		if err != nil {
			return fmt.Errorf("fill_disk: %w", err)
		}
		target = resolved
		if missing {
			// 登记在**任何**会失败的检查之前：目录是我们建的，
			// 我们就欠它一次删除，哪怕这次注入最后没写成。
			l.rollback = append(l.rollback, func(context.Context) error {
				if err := os.RemoveAll(target); err != nil {
					return fmt.Errorf("remove %s: %w", target, err)
				}
				return nil
			})
		}
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return fmt.Errorf("fill_disk: create %s: %w", target, err)
	}
	minFree := int64(injector.IntParam(spec.Params, "min_free_mb", defaultMinFreeMB)) << 20
	if minFree < 0 {
		return fmt.Errorf("fill_disk: min_free_mb=%d", minFree>>20)
	}
	maxBytes := int64(injector.IntParam(spec.Params, "max_bytes_mb", int(defaultMaxBytes>>20))) << 20
	if maxBytes < chunkBytes {
		return fmt.Errorf("fill_disk: max_bytes_mb=%d is below one chunk (%d bytes)",
			maxBytes>>20, chunkBytes)
	}
	// target_bytes 与 target_percent 二选一。给了绝对量就直接写那个量；
	// 给百分比就算出「要写到剩多少为止」，而那个数会被上面的地板再削一次。
	var want int64
	if mb := injector.IntParam(spec.Params, "target_bytes_mb", 0); mb > 0 {
		want = int64(mb) << 20
	} else if pct := injector.IntParam(spec.Params, "target_percent", 0); pct > 0 {
		if pct >= 100 {
			return fmt.Errorf("fill_disk: target_percent=%d leaves nothing for anything else", pct)
		}
		free, pctTotal, err := diskFree(target)
		if err != nil {
			return err
		}
		want = pctTotal*int64(pct)/100 - free
	} else {
		return fmt.Errorf("fill_disk: neither target_bytes_mb nor target_percent given; " +
			"a fill with no target would stop at the floor, which is not a fault anyone asked for")
	}
	if want > maxBytes {
		return fmt.Errorf("fill_disk: want to write %d bytes but max_bytes_mb caps it at %d; "+
			"raise the cap deliberately rather than discovering it as a truncated fault", want, maxBytes)
	}

	before, total, err := diskFree(target)
	if err != nil {
		return err
	}
	if before <= minFree {
		return fmt.Errorf("fill_disk: %s already has only %d bytes free, at or below the %d byte floor; "+
			"filling it now would take down whatever is already on it",
			target, before, minFree)
	}
	if want > before-minFree {
		want = before - minFree
	}
	if want <= 0 {
		// 请求的目标已经被满足了（磁盘本来就更满，或者说本来就更空）。
		//
		// 这一条必须报错而不是"目标已达成"式地返回成功：这次注入
		// 一个字节都没写，而**注入之后仍然存在的故障不是这次注入的成果**。
		// 报成功会让一个 case 的判分建立在一个本来就有的状态上。
		return fmt.Errorf("fill_disk: %s is already past the requested target "+
			"(%d bytes free of %d, want at most %d%%); this injection would stage nothing, "+
			"and a fault that was already there is not this injection's work", target, before, total,
			injector.IntParam(spec.Params, "target_percent", 0))
	}

	dir := filepath.Join(target, "opskeeper-fault-"+l.id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	l.dir = dir
	// 撤销就是删掉这一个目录：它带 injectID，所以不可能是别人的东西。
	l.rollback = append(l.rollback, func(context.Context) error {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
		return nil
	})

	block := make([]byte, chunkBytes)
	// 填非零字节：全零的块在很多文件系统与稀疏感知的工具眼里是"没写"。
	for i := range block {
		block[i] = 'x'
	}
	var written int64
	for written < want {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// 每写一块之前重查一次地板。这就是"不会被冲过去"的全部机制。
		free, _, err := diskFree(target)
		if err != nil {
			return err
		}
		if free <= minFree+chunkBytes {
			// 停在地板之上，不是一次失败：case 要的是"磁盘快满了"，
			// 而这个状态已经达到了。把它当成错误会让一次成功的注入报红。
			break
		}
		chunk := int64(chunkBytes)
		if remaining := want - written; remaining < chunk {
			chunk = remaining
		}
		path := filepath.Join(dir, fmt.Sprintf("fill-%04d", len(l.paths)))
		f, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
		n, werr := f.Write(block[:chunk])
		closeErr := f.Close()
		if werr != nil {
			return fmt.Errorf("write %s: %w", path, werr)
		}
		if closeErr != nil {
			return fmt.Errorf("close %s: %w", path, closeErr)
		}
		l.paths = append(l.paths, path)
		written += int64(n)
	}

	after, _, err := diskFree(target)
	if err != nil {
		return err
	}
	if err := fillDiskObservable(target, before, after, written); err != nil {
		return err
	}
	return nil
}

// fillDiskObservable decides whether a fill actually moved the needle, and
// says which of the two failures it is when it did not.
//
// 这两个判据量的都是**整卷**的剩余字节数，而写盘的是我们、读盘的也是我们，
// 被观测的却不是我们的那个量：同一卷上任何别的进程（另一个测试包、
// 一次 go build、一条编译缓存写入）都会往这个数里加减。于是
// 「我们写了 4 MiB 而剩余空间没掉」有两种解释，而它们必须被分开：
//
//  1. 写入没生效（实现坏了）——要报红；
//  2. 别人在这段时间里写了至少同样多（这台机器正忙）——要报忙。
//
// 分流用的是 `written`：write 一个字节都没返回时没有别的解释，报红；
// 而写入全部成功返回、剩余空间却没有相应下降时，剩下的解释只有两个——
// 别人抵消了它，或者（写不到一块时）块对齐让这次写真的不占空间。
// 第二个解释我们分不出来，所以那种情形也报忙：一个分不出来的事实报成红，
// 会让人先去查一段正确的代码。cpu_stress 的利用率判据有
// 同一个问题，也已经有 ErrMachineBusy 在管这件事（见 verifyCPU），
// 所以这里沿用同一个出口，而不是让调用方去猜错误字符串。
//
// 拆成独立函数是为了让这条分类能被单测——它在集成测试里只在机器恰好忙的
// 那一瞬间才走到，而"只在偶发时走到"的分支正是最需要被固定下来的那种。
func fillDiskObservable(target string, before, after, written int64) error {
	drop := before - after
	if drop <= 0 {
		if written > 0 {
			return fmt.Errorf("%w: fill_disk: wrote %d byte(s) to %s but free space went %d -> %d; "+
				"our writes landed and something else on this volume wrote at least as much in the same window, "+
				"so the remaining-byte count cannot be attributed to this injection",
				ErrMachineBusy, written, target, before, after)
		}
		return fmt.Errorf("fill_disk: free space did not move (%d -> %d) after writing %d byte(s); "+
			"the fault is not observable", before, after, written)
	}
	// 判据是"确实掉了一块以上"，不是"掉到了 target_percent"。
	// 后者会因为文件系统的块对齐、稀疏回填与别的进程的并发而在一个
	// 永远到不了的数字上红——而那不是实现错了，是判据写错了。
	if drop < chunkBytes {
		if written >= chunkBytes {
			return fmt.Errorf("%w: fill_disk: wrote %d byte(s) but free space dropped by only %d; "+
				"concurrent writers on this volume are masking the change, so the drop cannot be "+
				"attributed to this injection",
				ErrMachineBusy, written, drop)
		}
		return fmt.Errorf("fill_disk: free space dropped by only %d byte(s) after writing %d; "+
			"a fault nobody can see in `df` is not a fault", drop, written)
	}
	return nil
}

// ---------------------------------------------------------------- cpu stress

// cpuStress burns CPU on this process until the injection is undone.
//
// 判据是 Getrusage 报的 CPU 时间增量——**内核量的**。
//
// 为什么在进程内而不是 stress-ng 子进程：子进程版本需要多一个二进制依赖，
// 而"kill 掉子进程并祈祷它真的死了"是一个比"关掉一个 ctx"弱得多的
// 撤销保证。代价是这次注入和注入器共享一个进程：注入器自己会变热，
// 而故障会随进程一起消失——这与 pg / redis 的故障是同一个性质
// （它们也活在注入器的连接上），所以它不是这个包独有的取舍。
//
// goroutine 数默认是 GOMAXPROCS，且**不许超过它**：在一个只有两核的节点上
// 烧满 32 个 goroutine 不会让 CPU 故障更真，只会让注入器自己被调度出去，
// 于是它连"什么时候停"都答不出来。
func (i *Injector) cpuStress(ctx context.Context, spec injector.InjectSpec, l *live) error {
	// case 写的是 `cores` 与 `target_load`；`workers` 是同一件事的另一个名字。
	// 两个都认，是因为语料与命令行是两类调用方，强迫其中一类改写参数名
	// 只会得到一个"两份参数名都得维护"的分支。
	workers := injector.IntParam(spec.Params, "workers", 0)
	if workers == 0 {
		workers = injector.IntParam(spec.Params, "cores", runtime.GOMAXPROCS(0))
	}
	// target_load 是 case 声明的目标利用率。默认 80：一个容里几乎永远拿不到
	// 100%，而把判据定在 98 会让这条注入在一台正常负载的机器上假红。
	targetLoad := injector.IntParam(spec.Params, "target_load", 80)
	if workers < 1 {
		return fmt.Errorf("cpu_stress: workers=%d", workers)
	}
	if workers > runtime.GOMAXPROCS(0) {
		return fmt.Errorf("cpu_stress: workers=%d exceeds GOMAXPROCS=%d; "+
			"more goroutines than cores does not make the fault more real, "+
			"it only makes the injector unable to answer when to stop",
			workers, runtime.GOMAXPROCS(0))
	}
	l.workers = workers

	// 一个不参与计算的底数：让编译器没法把循环优化掉。
	// 一个被优化成空循环的"CPU 负载"是最安静的一种假注入。
	var sink uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for n := 0; n < workers; n++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			x := seed | 1
			for {
				select {
				case <-stop:
					return
				default:
				}
				// 一串不会被识别成闭式形式的混合运算。
				for k := 0; k < 4096; k++ {
					x ^= x << 13
					x ^= x >> 7
					x ^= x << 17
					x += uint64(k)
				}
			}
		}(uint64(n)*2654435761 + 1)
	}
	l.rollback = append(l.rollback, func(context.Context) error {
		close(stop)
		wg.Wait()
		sink = 0
		_ = sink
		return nil
	})

	// 采样窗口：故障必须**被量到**才算注入成功，所以这里必须等一段时间。
	//
	// 1 秒是**下限**而不是上限——CPU 时间是累积量，给它越短的时间
	// 越可能在调度还没铺开的时候就去读。原实现只等 1 秒就下判决，
	// 于是与自己的注释相反：在一台**瞬时**繁忙的机器上（另一个测试
	// 进程在并行跑、一个 CI 作业刚起步），worker 分到的核比平时少，
	// 1 秒的读数就落在验收线以下，注入器报"负载比 case 期望的弱"。
	//
	// 那句话在**持续**饱和的机器上是对的，而且必须留着：如果这台机器
	// 本来就被别的负载占着，那么"CPU 打满"这个故障本来就不可能
	// 归因到我们，多等也等不出来——那种拒绝是诚实的。
	//
	// 但**瞬时**繁忙等得出来。所以这里采样到验收线满足为止，
	// 上限是一个真上限：超过它还在等，只是在等一个不会到来的数。
	const (
		sampleTick    = 250 * time.Millisecond
		sampleFloor   = time.Second
		sampleCeiling = 10 * time.Second
	)
	bar := float64(targetLoad) - 25
	start := time.Now()
	startCPU, err := cpuSeconds()
	if err != nil {
		return err
	}
	var burned, elapsed float64
	util := 0.0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sampleTick):
		}
		endCPU, err := cpuSeconds()
		if err != nil {
			return err
		}
		elapsed = time.Since(start).Seconds()
		burned = endCPU - startCPU
		util = burned / elapsed / float64(workers) * 100
		if elapsed >= sampleFloor.Seconds() && util >= bar {
			break
		}
		if time.Since(start) >= sampleCeiling {
			break
		}
	}
	// 利用率按 worker 归一：一个 worker 在十核机器上跑满仍然是 100% 的
	// 单 worker 饱和度，用总核数去归一会把一台空闲的大机器读成 10%。
	//
	// 25 个百分点是容差，理由写在 targetLoad 上面。
	//
	// 判据在**这里**而不只是在 0.5 那一行，是因为 case 声明了它要什么：
	// 一个 `target_load: 98` 的 case 如果只拿到 30%，那不是"负载起来了"，
	// 而是一个比 case 期望温和得多的故障——而一个温和的故障会让诊断
	// agent 在一个不真实的场景里被判为通过。
	// 量到的那个数进 result：人能看到它与 target_load 差多少。
	//
	// 判据是「这 1 秒里至少烧掉了半个 CPU 秒」的下限版本：CPU 时间是
	// 累积量，无论窗口多长，半个 CPU 秒都是"负载确实跑过"的门槛，
	// 而它在负载完全没起来的机器上必然达不到。
	if burned < 0.5 {
		return fmt.Errorf("cpu_stress: %d worker(s) burned only %.2f CPU second(s) in %s; "+
			"the load is not observable", workers, burned, time.Duration(elapsed*float64(time.Second)).Round(time.Millisecond))
	}
	if util < bar {
		// 采样上限是 %s 还够不着验收线：这不是"再等一会儿"的问题，
		// 是这台机器此刻被别人占着，CPU 打满归因不到我们身上。
		return fmt.Errorf("%w: cpu_stress: %d worker(s) reached %.0f%% utilisation over %s "+
			"(sampled up to %s), want at least %d%% (tolerance 25 points) — "+
			"this machine is busy with something else and no amount of waiting will "+
			"make this fault attributable",
			ErrMachineBusy, workers, util,
			time.Duration(elapsed*float64(time.Second)).Round(time.Millisecond),
			sampleCeiling, targetLoad)
	}
	l.utilization = int(util + 0.5)
	return nil
}
