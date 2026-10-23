package kernel

import (
	"context"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// 本文件覆盖 per-kind safety ceiling（S-1）。
//
// 语义由 design.md 裁定，三条不变式逐条对应下面的测试：
//
//	I1 条目生效：某个 kind 有条目时，它的生效 ceiling 就是条目本身，
//	   不与全局 MaxSafetyLevel 取 min。取 min 会让 notifier 的 L3 条目
//	   变成空操作，六个 L3 manifest 仍被拒。
//	I2 无条目回落全局：某个 kind 没有条目时，它的生效 ceiling 是全局
//	   MaxSafetyLevel。没单独说过的一律按全局。
//	I3 写入方向可测：WithKindCeiling 只收窄，放宽入口只放宽；
//	   写反方向 panic，而不是静默忽略——静默忽略会让操作员的改动
//	   看起来生效了，其实没有。
//
// 另一条贯穿全文件的约束：KindCeilings 为空时行为与改动前逐位相同。
// 现有三个 Policy() 构造点（datasource.ManagerPolicy、
// toolset.adapterPolicy、edge 的 Profile.Policy）都走这条路径。

// managerPolicyWithCeiling 造一份与 ManagerPolicy 同形的策略：全局 L2。
// 六个 notifier manifest 都是 L3，全局 L2 是它们今天被拒的那个 ceiling。
func managerPolicyWithCeiling() Policy {
	return Policy{
		Target:         domain.TargetManager,
		MaxSafetyLevel: domain.SafetyL2,
		MaxBlastRadius: domain.RadiusSingleNS,
		GrantedScopes: domain.Scopes{
			domain.ScopeAlertRO,
			domain.ScopeAlertWrite,
			domain.ScopeMetricsRO,
			domain.ScopeTopologyRO,
		},
	}
}

// notifierManifest 造一份 L3 / destructive 的 notifier 形态 manifest，
// 与 notifier-plugin-plan.md §11.1 表里六个 manifest 同形。
func notifierManifest(name string) pluginmanifest.Plugin {
	return pluginmanifest.Plugin{
		Manifest: domain.PluginManifest{
			APIVersion: domain.PluginAPIVersion,
			Kind:       domain.PluginKind,
			Metadata:   domain.PluginMeta{Name: name, Version: "1.0.0"},
			Spec: domain.PluginSpec{
				Targets:        domain.Targets{domain.TargetManager},
				SafetyLevel:    domain.SafetyL3,
				Capabilities:   []domain.ToolClass{domain.ClassDestructive},
				RequiredScopes: domain.Scopes{domain.ScopeAlertRO, domain.ScopeAlertWrite},
			},
		},
	}
}

// ── I2：无条目回落全局 ──────────────────────────────────────────────────────

func TestPolicyWithoutKindCeilingsFallsBackToTheGlobalCeiling(t *testing.T) {
	pol := managerPolicyWithCeiling()

	for _, k := range Kinds() {
		if got := pol.EffectiveSafetyCeiling(k); got != domain.SafetyL2 {
			t.Fatalf("EffectiveSafetyCeiling(%q) = %s; want %s (no entry ⇒ global)", k, got, domain.SafetyL2)
		}
	}
	if got := pol.WidestSafetyCeiling(); got != domain.SafetyL2 {
		t.Fatalf("WidestSafetyCeiling() = %s; want %s", got, domain.SafetyL2)
	}
}

// 空 map 与 nil map 必须同义：调用方不该因为"分配了一个空 map"就
// 拿到不同的语义。
func TestEmptyKindCeilingsMapBehavesLikeNil(t *testing.T) {
	pol := managerPolicyWithCeiling()
	pol.KindCeilings = map[Kind]domain.SafetyLevel{}

	for _, k := range Kinds() {
		if got := pol.EffectiveSafetyCeiling(k); got != domain.SafetyL2 {
			t.Fatalf("EffectiveSafetyCeiling(%q) = %s; want %s", k, got, domain.SafetyL2)
		}
	}
	if got := pol.WidestSafetyCeiling(); got != domain.SafetyL2 {
		t.Fatalf("WidestSafetyCeiling() = %s; want %s", got, domain.SafetyL2)
	}
}

// 既有三个 Policy() 构造点都落在这一条上：KindCeilings 为空 ⇒
// 生效 ceiling 等于全局、准入判定与改动前逐位相同。
func TestEmptyKindCeilingsKeepsAdmissionBitForBit(t *testing.T) {
	pol := managerPolicyWithCeiling()
	h := NewHost(NewRegistry(), pol, nil)

	// L2 包：改动前后都过。
	if err := h.Admit(manifest("l2", domain.TargetManager, domain.SafetyL2, nil)); err != nil {
		t.Fatalf("an L2 package must be admitted by an L2 policy: %v", err)
	}
	// L3 包：改动前后都被拒，错误文本一字不变。
	err := h.Admit(notifierManifest("l3"))
	if err == nil {
		t.Fatal("an L3 package must be refused by an L2 policy")
	}
	if !strings.Contains(err.Error(), "plugin is L3, target policy allows at most L2") {
		t.Fatalf("err = %v; want the unchanged admission text", err)
	}
}

// ── I1：条目生效 ────────────────────────────────────────────────────────────

func TestKindCeilingEntryTakesEffectOverTheGlobal(t *testing.T) {
	// 全局 L2，notifier 条目 L3。条目生效 ⇒ notifier 的生效 ceiling 是 L3，
	// 其余 kind 回落全局 L2。取 min 会让这条变成空操作。
	pol := managerPolicyWithCeiling().WithWiderKindCeiling(KindNotifier, domain.SafetyL3)

	if got := pol.EffectiveSafetyCeiling(KindNotifier); got != domain.SafetyL3 {
		t.Fatalf("EffectiveSafetyCeiling(notifier) = %s; want %s (the entry takes effect)", got, domain.SafetyL3)
	}
	for _, k := range Kinds() {
		if k == KindNotifier {
			continue
		}
		if got := pol.EffectiveSafetyCeiling(k); got != domain.SafetyL2 {
			t.Fatalf("EffectiveSafetyCeiling(%q) = %s; want %s (no entry ⇒ global)", k, got, domain.SafetyL2)
		}
	}
	// 宿主准入用最宽 ceiling：全局 L2 + notifier L3 ⇒ 最宽是 L3。
	if got := pol.WidestSafetyCeiling(); got != domain.SafetyL3 {
		t.Fatalf("WidestSafetyCeiling() = %s; want %s", got, domain.SafetyL3)
	}
}

func TestWidestSafetyCeilingKeepsTheGlobalWhenEntriesAreNarrower(t *testing.T) {
	// 全局 L3，datasource 条目 L1。最宽 ceiling 仍然是全局的 L3——
	// 收窄条目不能把别的 kind 一起收窄。
	pol := managerPolicyWithCeiling().WithWiderKindCeiling(KindNotifier, domain.SafetyL3)
	pol.MaxSafetyLevel = domain.SafetyL3
	pol = pol.WithKindCeiling(KindDatasource, domain.SafetyL1)

	if got := pol.WidestSafetyCeiling(); got != domain.SafetyL3 {
		t.Fatalf("WidestSafetyCeiling() = %s; want %s", got, domain.SafetyL3)
	}
	if got := pol.EffectiveSafetyCeiling(KindDatasource); got != domain.SafetyL1 {
		t.Fatalf("EffectiveSafetyCeiling(datasource) = %s; want %s", got, domain.SafetyL1)
	}
}

// ── I3：写入方向 ────────────────────────────────────────────────────────────

func TestWithKindCeilingRefusesWidening(t *testing.T) {
	pol := managerPolicyWithCeiling()

	// 全局 L2 时写 L3 是"写宽了"，收窄入口必须拒绝。
	assertPanics(t, "WithKindCeiling must refuse a widening write", func() {
		pol.WithKindCeiling(KindNotifier, domain.SafetyL3)
	})

	// 收窄是允许的：全局 L3 ⇒ notifier 收到 L1。
	narrowed := managerPolicyWithCeiling()
	narrowed.MaxSafetyLevel = domain.SafetyL3
	narrowed = narrowed.WithKindCeiling(KindNotifier, domain.SafetyL1)
	if got := narrowed.EffectiveSafetyCeiling(KindNotifier); got != domain.SafetyL1 {
		t.Fatalf("EffectiveSafetyCeiling(notifier) = %s; want %s", got, domain.SafetyL1)
	}
	// 收窄之后想写回 L3 仍然是"写宽了"。
	assertPanics(t, "WithKindCeiling must refuse widening an existing entry", func() {
		narrowed.WithKindCeiling(KindNotifier, domain.SafetyL3)
	})
}

func TestWithWiderKindCeilingRefusesNarrowing(t *testing.T) {
	pol := managerPolicyWithCeiling().WithWiderKindCeiling(KindNotifier, domain.SafetyL3)

	// 已经有 L3 条目，写 L1 是"写窄了"，放宽入口必须拒绝。
	assertPanics(t, "WithWiderKindCeiling must refuse a narrowing write", func() {
		pol.WithWiderKindCeiling(KindNotifier, domain.SafetyL1)
	})

	// 放宽是允许的：L3 条目 → L3 是平写，不是放宽。
	same := pol.WithWiderKindCeiling(KindNotifier, domain.SafetyL3)
	if got := same.EffectiveSafetyCeiling(KindNotifier); got != domain.SafetyL3 {
		t.Fatalf("EffectiveSafetyCeiling(notifier) = %s; want %s", got, domain.SafetyL3)
	}
}

func TestKindCeilingRejectsUnknownKindAndLevel(t *testing.T) {
	pol := managerPolicyWithCeiling()

	assertPanics(t, "an unknown kind must be refused", func() {
		pol.WithKindCeiling(Kind("teleporter"), domain.SafetyL1)
	})
	assertPanics(t, "an unknown safety level must be refused", func() {
		pol.WithKindCeiling(KindNotifier, domain.SafetyLevel("L9"))
	})
	assertPanics(t, "an unknown kind must be refused by the widening entry point", func() {
		pol.WithWiderKindCeiling(Kind("teleporter"), domain.SafetyL1)
	})
	assertPanics(t, "an unknown safety level must be refused by the widening entry point", func() {
		pol.WithWiderKindCeiling(KindNotifier, domain.SafetyLevel("L9"))
	})
}

// NewHost 必须校验每个条目：未经由入口点构造的 Policy（直接写结构体
// 字面量）可能带一个入口点会拒绝的条目，而一个无法执行自己策略的
// 宿主不是保守的宿主，是坏的宿主。
func TestNewHostRefusesAnUnenforceableKindCeiling(t *testing.T) {
	assertPanics(t, "a host must refuse an unknown kind ceiling", func() {
		NewHost(NewRegistry(), Policy{
			Target:         domain.TargetManager,
			MaxSafetyLevel: domain.SafetyL2,
			KindCeilings:   map[Kind]domain.SafetyLevel{Kind("teleporter"): domain.SafetyL1},
		}, nil)
	})
	assertPanics(t, "a host must refuse an unknown safety level ceiling", func() {
		NewHost(NewRegistry(), Policy{
			Target:         domain.TargetManager,
			MaxSafetyLevel: domain.SafetyL2,
			KindCeilings:   map[Kind]domain.SafetyLevel{KindNotifier: domain.SafetyLevel("L9")},
		}, nil)
	})
}

// 写入方拿到的 Policy 是新的一份：调用方持有的 Policy 不会被写穿。
func TestWithKindCeilingDoesNotWriteThroughTheReceiver(t *testing.T) {
	base := managerPolicyWithCeiling()
	base.KindCeilings = map[Kind]domain.SafetyLevel{KindDatasource: domain.SafetyL1}

	narrowed := base.WithKindCeiling(KindNotifier, domain.SafetyL2)
	if _, ok := base.KindCeilings[KindNotifier]; ok {
		t.Fatal("WithKindCeiling must not write through the receiver's map")
	}
	if _, ok := narrowed.KindCeilings[KindNotifier]; !ok {
		t.Fatal("the returned policy must carry the new ceiling")
	}
	if base.KindCeilings[KindDatasource] != domain.SafetyL1 {
		t.Fatal("the receiver's existing ceiling must survive the derived policy")
	}
}

// ── §11.1 准入对比 ──────────────────────────────────────────────────────────

// §11.1 把同一份 manifest 放在两种 ceiling 下各跑一次：L2 全局下拒，
// L3 全局下过。这条测试把那张表固化成代码。
func TestSectionElevenOneComparesL2RejectionAgainstL3Acceptance(t *testing.T) {
	m := notifierManifest("opskeeper-notifier-dingtalk-markdown")

	l2 := NewHost(NewRegistry(), managerPolicyWithCeiling(), nil)
	err := l2.Admit(m)
	if err == nil {
		t.Fatal("an L3 manifest must be refused under an L2 ceiling")
	}
	if !strings.Contains(err.Error(), "plugin is L3, target policy allows at most L2") {
		t.Fatalf("err = %v; want the L2 rejection text", err)
	}

	l3pol := managerPolicyWithCeiling()
	l3pol.MaxSafetyLevel = domain.SafetyL3
	l3 := NewHost(NewRegistry(), l3pol, nil)
	if err := l3.Admit(m); err != nil {
		t.Fatalf("an L3 manifest must be admitted under an L3 ceiling: %v", err)
	}
}

// per-kind ceiling 不是装饰：全局 L2 + notifier 条目 L3 之下，
// L3 的 notifier 包过、L3 的 tool 包在 Activate 时被拒。
// 宿主准入用最宽 ceiling，所以包级准入对两者都放行——真正的闸门
// 在 scoped registrar 上，那里才知道条目落在哪个 kind。
func TestPerKindCeilingLetsNotifierThroughAndRefusesOtherKinds(t *testing.T) {
	pol := managerPolicyWithCeiling().WithWiderKindCeiling(KindNotifier, domain.SafetyL3)
	h := NewHost(NewRegistry(), pol, nil)

	// 包级准入：最宽 ceiling 是 L3，L3 包过。
	if err := h.Admit(notifierManifest("opskeeper-notifier-slack")); err != nil {
		t.Fatalf("admit: %v", err)
	}

	reg := h.Registry()
	notifierEntry := Entry{Kind: KindNotifier, Name: "slack", Impl: "impl"}
	if err := h.Activate(context.Background(), notifierManifest("opskeeper-notifier-slack"),
		&contrib{name: "opskeeper-notifier-slack", failAt: -1, entries: []Entry{notifierEntry}}); err != nil {
		t.Fatalf("an L3 notifier package must be activated under a notifier ceiling of L3: %v", err)
	}
	if _, ok := reg.Lookup(KindNotifier, "slack"); !ok {
		t.Fatal("the notifier entry must be registered")
	}

	// 同一个 L3 包想把条目落在 tool 上：tool 没有条目 ⇒ 回落全局 L2 ⇒ 拒绝。
	toolReg := NewRegistry()
	toolHost := NewHost(toolReg, pol, nil)
	toolEntry := Entry{Kind: KindTool, Name: "query_promql", Impl: "impl"}
	err := toolHost.Activate(context.Background(), notifierManifest("opskeeper-notifier-sneaky"),
		&contrib{name: "opskeeper-notifier-sneaky", failAt: -1, entries: []Entry{toolEntry}})
	if err == nil {
		t.Fatal("an L3 package must not register into a kind whose ceiling is the L2 global")
	}
	if !strings.Contains(err.Error(), "allows at most L2") {
		t.Fatalf("err = %v; want the ceiling text", err)
	}
	if toolReg.Len() != 0 {
		t.Fatalf("registry has %d entries after a refused activation; want 0", toolReg.Len())
	}
}

// 拒绝走既有整包回滚：先注册成功的 notifier 条目也必须被撤掉。
func TestKindCeilingRefusalRollsBackTheWholePackage(t *testing.T) {
	pol := managerPolicyWithCeiling().WithWiderKindCeiling(KindNotifier, domain.SafetyL3)
	reg := NewRegistry()
	h := NewHost(reg, pol, nil)

	entries := []Entry{
		{Kind: KindNotifier, Name: "slack", Impl: "impl"},
		{Kind: KindTool, Name: "query_promql", Impl: "impl"},
	}
	err := h.Activate(context.Background(), notifierManifest("opskeeper-notifier-mixed"),
		&contrib{name: "opskeeper-notifier-mixed", failAt: -1, entries: entries})
	if err == nil {
		t.Fatal("activation must fail when one entry exceeds its kind ceiling")
	}
	if reg.Len() != 0 {
		t.Fatalf("registry has %d entries; want 0 (the whole package is rolled back)", reg.Len())
	}
	if h.IsLive("opskeeper-notifier-mixed") {
		t.Fatal("a refused package must not be live")
	}
}

// KindCeilings 为空 ⇒ Activate 的行为与改动前逐位相同：
// ceiling 判在 kind 上没有条目，回落全局，而全局已经在包级准入里
// 判过一次，所以这道闸在改动前后的判定结果一致。
func TestActivateWithoutKindCeilingsIsUnchanged(t *testing.T) {
	reg := NewRegistry()
	h := NewHost(reg, managerPolicyWithCeiling(), nil)

	// L2 包登记 datasource 条目：过。
	if err := h.Activate(context.Background(), manifest("obs", domain.TargetManager, domain.SafetyL2, nil),
		&contrib{name: "obs", failAt: -1, entries: []Entry{dsEntry("prom")}}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("registry has %d entries; want 1", reg.Len())
	}

	// L3 包：包级准入就拒，一道条目都不写。
	if err := h.Activate(context.Background(), manifest("big", domain.TargetManager, domain.SafetyL3, nil),
		&contrib{name: "big", failAt: -1, entries: []Entry{dsEntry("victoria")}}); err == nil {
		t.Fatal("an L3 package must be refused by an L2 policy")
	}
	if reg.Len() != 1 {
		t.Fatalf("registry has %d entries; want 1 (the refused package wrote nothing)", reg.Len())
	}
}

// assertPanics 断言 fn panic，并检查 panic 文本提到了用途——
// 一个只说"panic"而不说方向的实现，运维看不出自己写反了。
func assertPanics(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("%s: expected a panic", what)
		}
		msg, ok := r.(string)
		if !ok {
			err, isErr := r.(error)
			if !isErr {
				t.Fatalf("%s: panic value = %v; want a message", what, r)
			}
			msg = err.Error()
		}
		if !strings.Contains(msg, "kernel:") {
			t.Fatalf("%s: panic = %q; want it to name the kernel and the direction", what, msg)
		}
	}()
	fn()
}
