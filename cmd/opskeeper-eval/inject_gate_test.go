package main

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
	hostinjector "github.com/vincent-wuhan/opskeeper/core/faults/injector/host"
	k8sinjector "github.com/vincent-wuhan/opskeeper/core/faults/injector/k8s"
	kafkainjector "github.com/vincent-wuhan/opskeeper/core/faults/injector/mq/kafka"
	rabbitmqinjector "github.com/vincent-wuhan/opskeeper/core/faults/injector/mq/rabbitmq"
	pginjector "github.com/vincent-wuhan/opskeeper/core/faults/injector/pg"
	redisinjector "github.com/vincent-wuhan/opskeeper/core/faults/injector/redis"
)

// 这一组测试守的是一句话：**环境撑不住注入时，它不许说"注入成功"。**
//
// 决策 297 把 pg 与 redis 从骨架变成了真实现，所以下面这条守的不再是
// "六个都还没接线"，而是"任何一个在环境不支持时都必须拒绝"——
// 拒绝的理由还必须说清差什么。
//
// 上一版六个注入器的 IsAvailable() 全部 `return true`，Inject 把一条记录写进
// 内存 map 并打上 skeleton 标记，六个测试里还有三个在断言这个假动作
// （"expected skeleton marker"、"generates unique ids"）。一个不碰任何真实
// 系统的注入器，自称可用并返回 InjectID，是这条链上能造出的最贵的假象：
// 它一路走到 judge，变成一个假的回归结论。
//
// 结构性防线是接口本身：CheckAvailable 返回 error 而不是 bool——
// "不可用"这句话终于有了地方可说。

// allRegistered 是生产代码装进注册表的那六个。
func allRegistered() []injector.Injector {
	return newInjectorRegistry().Injectors()
}

func TestEveryRegisteredInjectorIsPresent(t *testing.T) {
	if got := len(allRegistered()); got != 6 {
		t.Fatalf("registered %d injectors, want 6", got)
	}
}

// 每一个注入器都必须自报不可用，并且说清差什么。
//
// 注意这条断言的前提：**没有 DSN**。pg 已经不是骨架了，它会真的连库、
// 真的改数据；而这一组测试要的是一个"环境不支持注入"的状态。
// 所以下面先把这个状态钉死——不钉的话，开发者本机配了
// OPSKEEPER_HARNESS_PG_DSN 之后，这条测试会拿他的真库当靶子。
func TestNoInjectorClaimsItCanInject(t *testing.T) {
	pinNoBackend(t)
	for _, impl := range allRegistered() {
		err := impl.CheckAvailable(context.Background())
		if err == nil {
			t.Errorf("%s: CheckAvailable returned nil, but nothing here is available", impl.Type())
			continue
		}
		if !errors.Is(err, injector.ErrUnavailable) {
			t.Errorf("%s: error = %v, want it to wrap ErrUnavailable", impl.Type(), err)
		}
		// 已接线的注入器缺的不是"实现"，是连接，所以它必须点名那个
		// 环境变量；还没接线的必须说自己是骨架。
		//
		// 这张表必须显式列出而不是靠"impl.Type() != ..." 反推：
		// 反推的那一版在接上第二个注入器时会静默地把要求从
		// "点名环境变量" 退回成 "说自己是骨架"——而一个已经接好线的
		// 注入器说自己是骨架，跟没接线一样误导人。
		if envVar, wired := wiredInjectors[impl.Type()]; wired {
			if !strings.Contains(err.Error(), envVar) {
				t.Errorf("%s: error = %q, want it to name %s", impl.Type(), err, envVar)
			}
			continue
		}
		if !strings.Contains(err.Error(), "skeleton") {
			t.Errorf("%s: error = %q, want it to say it is a skeleton", impl.Type(), err)
		}
	}
}

// wiredInjectors 是已经接到真实系统上的注入器，以及它们缺的那条环境变量。
//
// 它是**数据**而不是散在测试里的 if：每接上一个注入器，
// 在这里加一行，它"必须点名自己缺什么"这条要求就自动跟着它走。
var wiredInjectors = map[string]string{
	"pg.":       pginjector.DSNEnv,
	"redis.":    redisinjector.AddrEnv,
	"host.":     hostinjector.RootEnv,
	"kafka.":    kafkainjector.BrokersEnv,
	"rabbitmq.": rabbitmqinjector.URLEnv,
	"k8s.":      k8sinjector.KubeconfigEnv,
}

// wiredInjectorEnvs 是 wiredInjectors 里那些环境变量的并集。
//
// 下面两条测试要走真实的注入路径，所以必须把**所有**已接线注入器
// 一起钉住。少钉一个的后果不是测试红，而是测试往开发者的真 Redis 里
// 写进几个 MB 的数据。
func wiredInjectorEnvs() []string {
	out := make([]string, 0, len(wiredInjectors))
	for _, env := range wiredInjectors {
		out = append(out, env)
	}
	sort.Strings(out)
	return out
}

// pinNoBackend 把所有已接线的注入器钉在"没有连接"这个状态上。
//
// t.Setenv 设成空串而不是 Unsetenv：New() 读的是 os.Getenv，空串与"没设"
// 在它眼里是同一件事，于是这一组测试在任何开发机上跑出来的结论都一样。
func pinNoBackend(t *testing.T) {
	t.Helper()
	for _, env := range wiredInjectorEnvs() {
		t.Setenv(env, "")
	}
}

// 不可用就必须一步都不走：既不返回结果，也不留下可被 Cleanup 认领的 ID。
func TestNoInjectorProducesAResultWhileUnavailable(t *testing.T) {
	pinNoBackend(t)
	ctx := context.Background()
	// 用注册表里**同一批实例**去比：另建一批再比较身份，六个指针两两不等，
	// 断言会红在一个与被测行为无关的地方——这种失败会让人怀疑被测代码而不是怀疑测试。
	reg := newInjectorRegistry()
	for _, impl := range reg.Injectors() {
		types := supportedTypesOf(t, impl.Type())
		if len(types) == 0 {
			t.Errorf("%s: no supported types listed; the loop below would pass on an empty list", impl.Type())
		}
		for _, typ := range types {
			got, action, ok := reg.Route(typ)
			if !ok || got != impl {
				t.Errorf("%s: type %q did not route back to its own injector", impl.Type(), typ)
				continue
			}
			res, err := impl.Inject(ctx, injector.InjectSpec{Type: typ})
			if !errors.Is(err, injector.ErrUnavailable) {
				t.Errorf("%s %s: error = %v, want ErrUnavailable", impl.Type(), typ, err)
			}
			if res != nil {
				t.Errorf("%s %s: returned a result %+v while refusing", impl.Type(), typ, res)
			}
			if action == "" {
				t.Errorf("%s %s: Route returned an empty action", impl.Type(), typ)
			}
		}
		// 拒绝之后 Cleanup 什么也清不掉：它从来没注入过。
		if err := impl.Cleanup(ctx, ""); !errors.Is(err, injector.ErrInjectionNotFound) {
			t.Errorf("%s: Cleanup(\"\") = %v, want ErrInjectionNotFound", impl.Type(), err)
		}
	}
}

// 一个认不出的类型报"不可用"是错的——那是接线问题，与环境无关。
//
// 六个都要试。只试注册表里的第一个（host）的话，把 pg 的两个检查调换顺序
// 这条断言照样是绿的，而它守的正是"顺序"这件事。
func TestAnUnknownTypeIsReportedAsUnsupportedNotUnavailable(t *testing.T) {
	pinNoBackend(t)
	ctx := context.Background()
	for _, impl := range allRegistered() {
		_, err := impl.Inject(ctx, injector.InjectSpec{Type: impl.Type() + "does_not_exist"})
		if !errors.Is(err, injector.ErrUnsupportedType) {
			t.Errorf("%s: error = %v, want ErrUnsupportedType", impl.Type(), err)
		}
		if errors.Is(err, injector.ErrUnavailable) {
			t.Errorf("%s: an unknown type was reported as unavailable; the caller would go debug the environment", impl.Type())
		}
	}
}

// inject 子命令必须以非零退出，并且不许在输出里出现 "skeleton: true" 之外的成功迹象。
// 上一版它打印一行 "inject: case=... confirm_prod=false" 就返回 0。
func TestInjectFailsLoudlyOnAShippedCase(t *testing.T) {
	// 这一条会真的走一遍注入路径，所以必须先把 pg 钉成没有连接：
	// 否则一个配了 DSN 的开发机上，`go test ./cmd/...` 会把锁链
	// 打进他自己的开发库，然后把表留在那儿。
	pinNoBackend(t)
	err := cmdInject(context.Background(), []string{"--case", "pg/lock-waits", "--cases-dir", shippedCasesDir})
	if err == nil {
		t.Fatal("cmdInject returned nil on a shipped case, want a non-zero exit")
	}
	if !strings.Contains(err.Error(), "not executed") {
		t.Fatalf("error = %q, want it to say the steps were not executed", err)
	}
}

// --dry-run 是这条命令现在唯一能真正完成的事：它读真实的 case 文件、
// 走真实的注册表路由、打印真实的注入类型，不假装注入发生过。
func TestInjectDryRunListsRealStepsAndSucceeds(t *testing.T) {
	pinNoBackend(t)
	if err := cmdInject(context.Background(),
		[]string{"--case", "pg/lock-waits", "--cases-dir", shippedCasesDir, "--dry-run"}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
}

// prod 必须显式确认，这与注入器是否接线无关。
func TestProdRequiresConfirmation(t *testing.T) {
	err := cmdInject(context.Background(),
		[]string{"--case", "pg/lock-waits", "--cases-dir", shippedCasesDir, "--env", "prod"})
	if err == nil || !strings.Contains(err.Error(), "--confirm-prod") {
		t.Fatalf("error = %v, want prod to be refused without --confirm-prod", err)
	}
}

// 一个不存在的 case 必须报错，而不是当成"没有要注入的东西"然后成功。
func TestAnUnknownCaseIsAnError(t *testing.T) {
	err := cmdInject(context.Background(),
		[]string{"--case", "pg/does-not-exist", "--cases-dir", shippedCasesDir, "--dry-run"})
	if err == nil {
		t.Fatal("an unknown case returned nil, want an error")
	}
}

// supportedTypesOf 从每个注入器包读它自己列出的类型。
// 读不到就是失败：一条"每个类型都被拒绝"的断言在类型集为空时会通过。
func supportedTypesOf(t *testing.T, prefix string) []string {
	t.Helper()
	for impl := range typeIndex() {
		if impl == prefix {
			return typeIndex()[prefix]
		}
	}
	t.Fatalf("no injector with prefix %q", prefix)
	return nil
}

func typeIndex() map[string][]string {
	return map[string][]string{
		"pg.":       pginjectorTypes,
		"redis.":    redisinjectorTypes,
		"host.":     hostinjectorTypes,
		"k8s.":      k8sinjectorTypes,
		"rabbitmq.": rabbitmqinjectorTypes,
		"kafka.":    kafkainjectorTypes,
	}
}

var (
	pginjectorTypes       = mustTypes(pginjector.SupportedTypes)
	redisinjectorTypes    = mustTypes(redisinjector.SupportedTypes)
	hostinjectorTypes     = mustTypes(hostinjector.SupportedTypes)
	k8sinjectorTypes      = mustTypes(k8sinjector.SupportedTypes)
	rabbitmqinjectorTypes = mustTypes(rabbitmqinjector.SupportedTypes)
	kafkainjectorTypes    = mustTypes(kafkainjector.SupportedTypes)
)

func mustTypes(f func() []string) []string { return f() }

// stubInjector 只记录自己被清理过没有。
type stubInjector struct {
	prefix  string
	cleaned *[]string
}

func (s *stubInjector) Type() string                         { return s.prefix }
func (s *stubInjector) CheckAvailable(context.Context) error { return nil }
func (s *stubInjector) Cleanup(_ context.Context, id string) error {
	*s.cleaned = append(*s.cleaned, id)
	return nil
}
func (s *stubInjector) Inject(_ context.Context, spec injector.InjectSpec) (*injector.InjectResult, error) {
	return &injector.InjectResult{InjectID: spec.InjectID, Type: spec.Type}, nil
}

// 已经落到目标上的故障必须被撤销，而且**逆序**撤销。
//
// 上一版 cmdInject 注入完就返回，进程一退出连接就关、锁就松开，
// 故障在诊断开始之前就自己好了。命令行的另一半是"把它按住再收掉"，
// 收的顺序也得对：后注入的那一步往往依赖先注入的那一步。
func TestHoldFaultsCleansUpInReverseOrder(t *testing.T) {
	var cleaned []string
	impl := &stubInjector{prefix: "stub.", cleaned: &cleaned}
	staged := []stagedFault{
		{impl: impl, res: &injector.InjectResult{InjectID: "a"}, until: time.Minute},
		{impl: impl, res: &injector.InjectResult{InjectID: "b"}, until: time.Minute},
		{impl: impl, res: &injector.InjectResult{InjectID: "c"}, until: time.Minute},
	}
	holdFaults(context.Background(), staged, 0, nil)
	want := []string{"c", "b", "a"}
	if len(cleaned) != len(want) {
		t.Fatalf("cleaned %v, want %v", cleaned, want)
	}
	for i := range want {
		if cleaned[i] != want[i] {
			t.Fatalf("cleaned %v, want %v (reverse order: a later step may depend on an earlier one)",
				cleaned, want)
		}
	}
}

// 按住的时间到了就收；中途 ctx 被取消也要收——
// 一个被 Ctrl-C 打断的注入如果把故障留在真库上，那比不注入更难收拾。
func TestHoldFaultsReturnsEarlyOnCancelAndStillCleansUp(t *testing.T) {
	var cleaned []string
	impl := &stubInjector{prefix: "stub.", cleaned: &cleaned}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	holdFaults(ctx, []stagedFault{{impl: impl, res: &injector.InjectResult{InjectID: "only"}}}, time.Hour, nil)
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("holdFaults waited %v after cancel; it should return as soon as the context is done", elapsed)
	}
	if len(cleaned) != 1 || cleaned[0] != "only" {
		t.Fatalf("cleaned %v, want [only] — a cancelled hold must still undo the fault", cleaned)
	}
}
