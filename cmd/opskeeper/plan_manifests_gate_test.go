package main

// 方案草案的准入闸门（manager 侧：datasource / provider / notifier）。
//
// 4.3–4.7 五份方案各自把「草案 manifest 过 sdk.Decode → sdk.Validate →
// sdk.Admit」记录成本任务期间一次性跑过的验证，未在仓内留下测试——各方案
// 原文都注明了这一点，并建议固化。本文件是 manager 侧三族的固化件；adapter
// 与 collector 分别住在它们所对照的 policy 旁边（core/manager/middleware/
// toolset 与 cmd/opskeeper-edge），那里能拿到各自真实的 policy 构造点。
//
// 放在装配层而不是 core/floor/pluginmanifest，是因为闸门要对照的 policy 有
// 的住在 core/manager（ManagerPolicy），而 floor 不能反向 import manager。
// 这与 prom_backend_wiring_test.go 里 TestThePackageManifestAdmitsUnder-
// ManagerPolicy 的选址是同一条理由：断言「在真实 policy 下准入」的那份测试
// 必须住在能合法 import 该 policy 的地方。
//
// 三段闸门这里用 kernel.Host.Admit 表达：它就是 sdk.Admit 加上 target 检查
// 后的那一道（见 core/floor/kernel/host.go 的 Admit），policy 到 Admission
// 的换算也由它完成——测试自己抄一份换算，等于让闸门和它的复述可以各说各话。

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/reporoot"
	aiopsdatasource "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/datasource"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// planChangeName 是草案所属的 OpenSpec change 目录名。
const planChangeName = "plugin-architecture-recovery-plan"

// planDraft 是一份草案：文件名、磁盘原文、解码结果。原文要保留下来，因为
// 负例必须改在 YAML 上而不是解码后的结构体上——词表外取值与未知字段这两类
// 最有价值的拒绝只发生在解码期，结构体上造不出来。
type planDraft struct {
	file     string
	raw      []byte
	manifest domain.PluginManifest
}

// planManifestsDir 定位某扩展点的草案目录。
//
// 按「change 名 / 扩展点-manifests」这个相对后缀在 openspec/ 下搜索，而不是
// 写死 openspec/changes/<change>/：change 归档时会整体挪进 openspec/changes/
// archive/，一条写死的路径在那天变成「找不到目录」，而按后缀搜索的版本继续
// 工作。草案被删除——意味着方案已落地为真实包——时这条测试应当失败并要求
// 把闸门搬到包目录上，而不是静默变绿。
func planManifestsDir(t *testing.T, extension string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, ok := reporoot.Find(dir, 10)
	if !ok {
		t.Fatalf("%s 上方找不到仓库根目录", dir)
	}
	base := filepath.Join(root, "openspec")
	suffix := "/" + planChangeName + "/" + extension + "-manifests"

	var hits []string
	err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(base, p)
		if relErr != nil {
			return relErr
		}
		if suffix := filepath.ToSlash(rel); strings.HasSuffix(suffix, planChangeName+"/"+extension+"-manifests") {
			hits = append(hits, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s: %v", base, err)
	}
	switch len(hits) {
	case 1:
		return hits[0]
	case 0:
		t.Fatalf("openspec/ 下找不到 %s%s。草案目录被移动或删除了：若方案已落地为真实包，"+
			"请把这道闸门改对着 plugins/<pkg>/pig-ops.yaml 跑，而不是把它删掉", planChangeName, suffix)
	default:
		t.Fatalf("openspec/ 下有 %d 个 %s%s（%v）；草案目录必须唯一，否则闸门对着哪份跑是碰运气",
			len(hits), planChangeName, suffix, hits)
	}
	return ""
}

// loadPlanDrafts 解码一个扩展点的全部草案。
//
// files 是期望的清单：多一份、少一份都失败。台账要逐份给出「测试覆盖状态」，
// 于是一份没有被这里点名的草案就是一份没有闸门的草案——静默接受新增文件，
// 等于让下一份草案回到本任务之前的状态。
func loadPlanDrafts(t *testing.T, extension string, files ...string) []planDraft {
	t.Helper()
	dir := planManifestsDir(t, extension)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 %s: %v", dir, err)
	}
	var got []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			got = append(got, e.Name())
		}
	}
	want := append([]string(nil), files...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s 目录里的草案是 %v，期望 %v；新增或删除草案时必须同步这份清单", dir, got, want)
	}

	out := make([]planDraft, 0, len(files))
	for _, name := range files {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("读取 %s: %v", name, err)
		}
		m, err := sdk.Decode(raw)
		if err != nil {
			t.Fatalf("%s: sdk.Decode: %v", name, err)
		}
		if err := sdk.Validate(m); err != nil {
			t.Fatalf("%s: sdk.Validate: %v", name, err)
		}
		out = append(out, planDraft{file: name, raw: raw, manifest: m})
	}
	return out
}

// edited 把草案原文里的 old 换成 new 并返回新原文。old 不在原文里时直接失败：
// 一次没有发生的改写会让负例去断言一份正例，而测试看起来仍然是绿的。
func edited(t *testing.T, d planDraft, old, replacement string) []byte {
	t.Helper()
	return editRaw(t, d.file, d.raw, [2]string{old, replacement})
}

// editRaw 在原文上依次应用若干组替换。有些负例要同时改两个字段（比如把
// L2+[write] 一起降到 L1+[read]），只改一个会先撞上解码期的合法性检查，
// 到不了想断言的那道闸门。
func editRaw(t *testing.T, name string, raw []byte, pairs ...[2]string) []byte {
	t.Helper()
	out := string(raw)
	for _, p := range pairs {
		if !strings.Contains(out, p[0]) {
			t.Fatalf("%s: 负例改写点 %q 不在原文里；草案改版后请同步改这条负例", name, p[0])
		}
		out = strings.Replace(out, p[0], p[1], 1)
	}
	return []byte(out)
}

// decodeDraftError 只跑解码这一段，用于断言解码期的拒绝。
func decodeDraftError(t *testing.T, raw []byte) string {
	t.Helper()
	_, err := sdk.Decode(raw)
	if err == nil {
		t.Fatalf("这份草案被解码放行了；期望的拒绝没有发生")
	}
	return err.Error()
}

// admitDraft 在真实 policy 下跑准入，返回 ""（放行）或拒绝文本。
func admitDraft(m domain.PluginManifest, pol kernel.Policy) string {
	host := kernel.NewHost(kernel.NewRegistry(), pol, nil)
	if err := host.Admit(pluginmanifest.Plugin{Manifest: m}); err != nil {
		return err.Error()
	}
	return ""
}

// mustAdmit 断言放行。
func mustAdmit(t *testing.T, name string, m domain.PluginManifest, pol kernel.Policy) {
	t.Helper()
	if got := admitDraft(m, pol); got != "" {
		t.Fatalf("%s: 期望在当前 policy 下放行，实际被拒：%s", name, got)
	}
}

// mustRefuse 断言拒绝，且拒绝理由里点名 wantSub。断言理由文本是为了让负例
// 证明「闸门在比什么」——一份因为别的字段被拒的 manifest 也会让裸的
// «期望被拒»通过。
func mustRefuse(t *testing.T, name string, m domain.PluginManifest, pol kernel.Policy, wantSub string) {
	t.Helper()
	got := admitDraft(m, pol)
	if got == "" {
		t.Fatalf("%s: 期望被拒（拒绝点 %q），实际被放行", name, wantSub)
	}
	if !strings.Contains(got, wantSub) {
		t.Fatalf("%s: 拒绝理由 %q 不含 %q", name, got, wantSub)
	}
}

// ── 4.3 datasource：三份草案在 ManagerPolicy() 下全过 ───────────────────
//
// 方案 §7 的闸门表把「安全级别 L1 / 只读 / observability.read / 兼容范围
// 0.4.0+0.8.0」写成必过项。这里把它们逐份钉住，并把方案正文里那两句
// 「写了就会被拒」的话变成会失败的断言。

func TestDatasourceDraftManifestsAdmitUnderManagerPolicy(t *testing.T) {
	drafts := loadPlanDrafts(t, "datasource", "elasticsearch.yaml", "opensearch.yaml", "victoriametrics.yaml")
	pol := aiopsdatasource.ManagerPolicy()
	for _, d := range drafts {
		mustAdmit(t, d.file, d.manifest, pol)

		// L1 / 只读：datasource 方案的第一条闸门。写成断言后，一份悄悄抬到
		// L2 的草案会让这条测试红，而不是等评审发现。
		if d.manifest.Spec.SafetyLevel != domain.SafetyL1 {
			t.Errorf("%s: safety_level = %s, want L1（datasource 一律只读）", d.file, d.manifest.Spec.SafetyLevel)
		}
		if got := (pluginmanifest.Plugin{Manifest: d.manifest}).HighestCapability(); got != domain.ClassRead {
			t.Errorf("%s: 最高能力 = %s, want read", d.file, got)
		}
		// scope 最小化：只要 observability.read。这是评审闸门（Satisfies 只判
		// 包含，多要一个在 ManagerPolicy 下照样过），所以要钉住数量。
		scopes := d.manifest.Spec.RequiredScopes
		if len(scopes) != 1 || scopes[0] != domain.ScopeMetricsRO {
			t.Errorf("%s: required_scopes = %v, want 恰好 [observability.read]", d.file, scopes)
		}
		// 兼容范围：第三方包必须写 min_pig_version / min_edge_version。
		if got := d.manifest.Spec.Install.MinPigVersion; got != "0.4.0" {
			t.Errorf("%s: min_pig_version = %q, want 0.4.0", d.file, got)
		}
		if got := d.manifest.Spec.Install.MinEdgeVersion; got != "0.8.0" {
			t.Errorf("%s: min_edge_version = %q, want 0.8.0", d.file, got)
		}
	}
}

// TestDatasourceDraftRefusesAScopeTheHostDoesNotGrant 对应草案正文里的那句
// 注释：声明 `log.read` 会让准入直接拒绝，因为词表里没有这个 scope。
// 那句话此前只活在 YAML 注释里，这里是它的可执行版本。
func TestDatasourceDraftRefusesAScopeTheHostDoesNotGrant(t *testing.T) {
	drafts := loadPlanDrafts(t, "datasource", "elasticsearch.yaml", "opensearch.yaml", "victoriametrics.yaml")
	for _, d := range drafts {
		raw := edited(t, d, "    - observability.read", "    - log.read")
		m, err := sdk.Decode(raw)
		if err != nil {
			t.Fatalf("%s: 解码改写后的草案: %v", d.file, err)
		}
		mustRefuse(t, d.file, m, aiopsdatasource.ManagerPolicy(), "not granted: log.read")
	}
}

// TestTheInstallStrategyVocabularyHasNoCanaryValue 钉住方案 §7 与代码的漂移。
//
// datasource 方案的闸门表写着「灰度由 install.strategy=canary 控制；先 canary
// 再 rolling，最后 replace」。词表里没有这两个值：domain.InstallPolicy 只接受
// ""、"rolling"、"pin"，canary 是 rolling 内部的一个波次（rollout.go），不是
// 一个可声明的策略。草案自己写的 rolling 是对的，写错的是闸门表那句话。
// 这条负例让那句话没法在不知情的情况下变成真的。
func TestTheInstallStrategyVocabularyHasNoCanaryValue(t *testing.T) {
	drafts := loadPlanDrafts(t, "datasource", "elasticsearch.yaml", "opensearch.yaml", "victoriametrics.yaml")
	for _, d := range drafts {
		raw := edited(t, d, "strategy: rolling", "strategy: canary")
		got := decodeDraftError(t, raw)
		if !strings.Contains(got, "spec.install.strategy") {
			t.Fatalf("%s: 拒绝理由 %q 不含 spec.install.strategy", d.file, got)
		}
	}
}

// TestADatasourceKindCeilingIsWhereTheL2RefusalWouldComeFrom 回答方案 §7 的
// 「manifest 写 L2 即拒」到底发生在哪一道。
//
// 答案分两层，两层都被写成了断言：
//
//   - 包级闸门（Host.Admit）用的是 policy 里「最宽」的那个 ceiling
//     （WidestSafetyCeiling）。把 KindDatasource 单独收窄到 L1，包级闸门
//     仍然按全局 L2 放行——因为一份 manifest 只报一个等级，不说自己会把
//     条目落进哪个 kind，包级闸门只能取最宽的那个，否则会拒掉宿主其实
//     收得下的包。所以「写 L2 即拒」在包级只能来自更低的**全局** ceiling。
//   - per-kind 收窄真正生效的地方是 Activate：贡献体开始注册条目时，每个
//     条目都会对着自己 kind 的 ceiling 复查一次。自报 L2 的草案在那里被拒，
//     且整个包被回滚。
//
// 方案成文时的那次一次性验证没有经过宿主，所以这两层在方案里是看不见的。
func TestADatasourceKindCeilingIsWhereTheL2RefusalWouldComeFrom(t *testing.T) {
	drafts := loadPlanDrafts(t, "datasource", "elasticsearch.yaml", "opensearch.yaml", "victoriametrics.yaml")
	narrowKind := aiopsdatasource.ManagerPolicy().WithKindCeiling(kernel.KindDatasource, domain.SafetyL1)
	narrowHost := aiopsdatasource.ManagerPolicy()
	narrowHost.MaxSafetyLevel = domain.SafetyL1

	for _, d := range drafts {
		// 诚实报 L1 的三份：两种收窄下都照常通过。
		mustAdmit(t, d.file, d.manifest, narrowKind)
		mustAdmit(t, d.file, d.manifest, narrowHost)

		raw := edited(t, d, "  safety_level: L1", "  safety_level: L2")
		m, err := sdk.Decode(raw)
		if err != nil {
			t.Fatalf("%s: 解码自报 L2 的变体: %v", d.file, err)
		}
		// 现状 ManagerPolicy（全局 L2）放行：包级闸门看不见等级虚报。
		mustAdmit(t, d.file, m, aiopsdatasource.ManagerPolicy())
		// per-kind 收窄到 L1 仍然放行：包级闸门取的是最宽 ceiling。
		mustAdmit(t, d.file+"（per-kind 收窄后的包级闸门）", m, narrowKind)
		// 全局 ceiling 落到 L1 才会在包级被拒。
		mustRefuse(t, d.file, m, narrowHost, "plugin is L2, target policy allows at most L1")

		// 激活期复查：同一个变体在 KindDatasource 条目上被 per-kind ceiling
		// 拒绝，且注册表里不留任何条目。
		host := kernel.NewHost(kernel.NewRegistry(), narrowKind, nil)
		if err := host.Activate(t.Context(), pluginmanifest.Plugin{Manifest: m},
			draftContribution{name: m.Metadata.Name, kind: kernel.KindDatasource}); err == nil {
			t.Fatalf("%s: 自报 L2 的草案在 KindDatasource 的 L1 ceiling 下应当被激活期拒绝", d.file)
		} else if !strings.Contains(err.Error(), `allows at most L1`) {
			t.Fatalf("%s: 激活期拒绝理由 %q 不含 per-kind ceiling 的说明", d.file, err.Error())
		}
		if host.Registry().Len() != 0 {
			t.Fatalf("%s: 被拒的包在注册表里留下 %d 条条目；拒绝必须整体回滚",
				d.file, host.Registry().Len())
		}
	}
}

// draftContribution 是一个只注册一条指定 kind 条目的贡献体，用来把「per-kind
// ceiling 在激活期生效」这件事接到真实的 Activate 路径上。草案只有 manifest
// 没有实现，所以 impl 是占位值；这道闸门查的是等级，不是实现。
type draftContribution struct {
	name string
	kind kernel.Kind
}

func (c draftContribution) PluginName() string { return c.name }
func (c draftContribution) Register(r kernel.Registrar) error {
	return r.Register(c.kind, "draft", struct{}{})
}

// ── 4.6 provider：三份草案在现状 policy 下零改动准入 ────────────────────
//
// 方案 §10.1 的结论是 provider 是六个扩展点里唯一「在现状 ManagerPolicy()
// 下零改动准入」的一族。这里固化那张表：现状过、per-kind 收窄后拒、
// 四个负对照各自证明闸门挡得住什么、挡不住什么。

func TestProviderDraftManifestsAdmitUnderThePolicyTheyRunUnder(t *testing.T) {
	drafts := loadPlanDrafts(t, "provider", "bedrock.yaml", "builtin-providers.yaml", "ollama.yaml")
	today := aiopsdatasource.ManagerPolicy()
	for _, d := range drafts {
		mustAdmit(t, d.file, d.manifest, today)

		if d.manifest.Spec.SafetyLevel != domain.SafetyL2 {
			t.Errorf("%s: safety_level = %s, want L2（模型调用写费用账本，见方案 §4.2）",
				d.file, d.manifest.Spec.SafetyLevel)
		}
		// scope 最小化的机械面：三份草案一个 scope 都不要。对照 B 证明多要
		// 的那份挡不住，所以数量必须钉死在这里。
		if got := d.manifest.Spec.RequiredScopes; len(got) != 0 {
			t.Errorf("%s: required_scopes = %v, want 空集（凭据由宿主逐调用解析）", d.file, got)
		}
		if d.manifest.Spec.Install.Strategy != domain.InstallPin {
			t.Errorf("%s: strategy = %q, want pin", d.file, d.manifest.Spec.Install.Strategy)
		}
	}

	// L1 / L0 两列（方案 §10.1）。方案写的是「收窄 provider 这一个 kind 的
	// ceiling」，但那两列的一次性验证是在方案自己的 driver 里对着
	// sdk.Admit 直接跑的，没有经过宿主——经过宿主后这两列分成了两层：
	//
	//   - 包级闸门取的是 policy 的最宽 ceiling（WidestSafetyCeiling）。全局
	//     仍是 L2 时，把 KindProvider 单独压到 L1/L0 挡不住任何包级准入，
	//     因为一份 manifest 只报一个等级，不说自己会把条目落进哪个 kind。
	//   - 要在包级拒掉 L2 的 provider，唯一的办法是把**全局** ceiling 压下来。
	//   - per-kind 收窄真正生效的地方是 Activate（与 datasource 那条测试同
	//     一个机制），这里也一并断言，防止有人把「包级放行」读成「收窄无效」。
	for _, ceiling := range []domain.SafetyLevel{domain.SafetyL1, domain.SafetyL0} {
		perKind := today.WithKindCeiling(kernel.KindProvider, ceiling)
		narrowHost := today
		narrowHost.MaxSafetyLevel = ceiling

		for _, d := range drafts {
			// 包级：per-kind 收窄不拒（最宽 ceiling 仍是全局 L2），全局收窄才拒。
			mustAdmit(t, d.file+"（per-kind 收窄后的包级闸门）", d.manifest, perKind)
			mustRefuse(t, d.file, d.manifest, narrowHost,
				"plugin is L2, target policy allows at most "+string(ceiling))

			// 激活期：per-kind 收窄在条目落地时拒绝，并把包整体回滚。
			host := kernel.NewHost(kernel.NewRegistry(), perKind, nil)
			if err := host.Activate(t.Context(), pluginmanifest.Plugin{Manifest: d.manifest},
				draftContribution{name: d.manifest.Metadata.Name, kind: kernel.KindProvider}); err == nil {
				t.Fatalf("%s: L2 草案在 KindProvider 的 %s ceiling 下应当在激活期被拒",
					d.file, ceiling)
			} else if !strings.Contains(err.Error(), "allows at most "+string(ceiling)) {
				t.Fatalf("%s: 激活期拒绝理由 %q 不含 per-kind ceiling 的说明", d.file, err.Error())
			}
			if host.Registry().Len() != 0 {
				t.Fatalf("%s: 被拒的包在注册表里留下 %d 条条目；拒绝必须整体回滚",
					d.file, host.Registry().Len())
			}
		}
	}
}

// providerNarrowGrant 是方案 §4.6 的 ProviderPolicy() 草案：与 ManagerPolicy
// 同形，唯一差别是授予集为空。它今天不在仓内（方案的定位是「provider 宿主
// 包的单测构造点」），所以这里按方案原文把它写出来，并在 S-1 落地
// ProviderPolicy() 时替换为对那个构造函数的调用——见 gate-run-report.md 的
// 遗留项。
func providerNarrowGrant() kernel.Policy {
	return kernel.Policy{
		Target:         domain.TargetManager,
		MaxSafetyLevel: domain.SafetyL2,
		MaxBlastRadius: domain.RadiusSingleNS,
		GrantedScopes:  domain.Scopes{},
	}
}

// TestProviderNegativeControls 把方案 §10.1 的四个负对照固化下来。
//
// 每个对照都回答同一个问题的一个分支：这份 manifest 的声明到底承不承重。
func TestProviderNegativeControls(t *testing.T) {
	drafts := loadPlanDrafts(t, "provider", "bedrock.yaml", "builtin-providers.yaml", "ollama.yaml")
	today := aiopsdatasource.ManagerPolicy()
	l1Host := today
	l1Host.MaxSafetyLevel = domain.SafetyL1

	for _, d := range drafts {
		t.Run(d.file, func(t *testing.T) {
			// 对照 A（低报）：L1 + [read] 在 L1 ceiling 下也过。闸门不能识别
			// 低报——等级是评审判断。把这件事写成断言，是为了让「我们知道自
			// 己挡不住什么」成为台账里的一行，而不是一句口头交代。
			//
			// ceiling 用的是全局 L1（宿主收窄），不是 per-kind 收窄：包级闸门
			// 取最宽 ceiling，per-kind 收窄在这里放不出「L1 包过 L1 闸」这条
			// 信息。
			lowBalled, err := sdk.Decode(editRaw(t, d.file, d.raw,
				[2]string{"  safety_level: L2", "  safety_level: L1"},
				[2]string{"  capabilities: [write]", "  capabilities: [read]"},
			))
			if err != nil {
				t.Fatalf("解码低报变体: %v", err)
			}
			mustAdmit(t, "对照 A 低报", lowBalled, l1Host)

			// 对照 B（scope 膨胀）：现状 ManagerPolicy 放行（它的授予集本来
			// 就有这两项），空授予集的 ProviderPolicy 形状机械拒绝。
			inflated, err := sdk.Decode(edited(t, d, "  required_scopes: []",
				"  required_scopes: [observability.read, db.read]"))
			if err != nil {
				t.Fatalf("解码 scope 膨胀变体: %v", err)
			}
			mustAdmit(t, "对照 B 现状 policy", inflated, today)
			mustRefuse(t, "对照 B 空授予集", inflated, providerNarrowGrant(), "not granted: observability.read, db.read")

			// 对照 C（升级线）：L3 + destructive + single-ns 在现状 L2 上限下
			// 被拒——想改写 vendor 侧状态的包装不进来，除非运维显式抬 ceiling。
			escalated, err := sdk.Decode(edited(t, d, "  safety_level: L2", "  safety_level: L3"))
			if err != nil {
				t.Fatalf("解码升级变体: %v", err)
			}
			escalated.Spec.Capabilities = []domain.ToolClass{domain.ClassDestructive}
			escalated.Spec.Approval.MaxBlastRadius = domain.RadiusSingleNS
			if err := sdk.Validate(escalated); err != nil {
				t.Fatalf("升级变体应当结构合法: %v", err)
			}
			mustRefuse(t, "对照 C 升级线", escalated, today, "plugin is L3, target policy allows at most L2")

			// 对照 D（目标面）：把 edge 加进 targets 后，manager 宿主的包级
			// 闸门照样放行——RunsOn 只问「是否包含本目标」，不问「是否只要
			// 本目标」。挡住它的是宿主侧规则与切片 S-2，不是这里。
			both, err := sdk.Decode(edited(t, d, "  targets: [manager]", "  targets: [manager, edge]"))
			if err != nil {
				t.Fatalf("解码双目标变体: %v", err)
			}
			mustAdmit(t, "对照 D 双目标", both, today)
		})
	}
}

// ── 4.5 notifier：六份草案在 per-kind ceiling 下的两种命运 ───────────────
//
// 方案 §11.1 的表只有两列：全局 L2 下全拒、notifier 这个 kind 抬到 L3 后全
// 过。这两列就是 WithKindCeiling 存在的理由，也是 S-1 的实测依据。

// notifierPlanPolicy 是方案 §4.6 的 NotifierPolicy() 草案，按它自己的注释逐字
// 翻译成今天的内核 API。注意一个与方案原文的差别：方案写的是
// WithKindCeiling(KindNotifier, L3)，而内核把「收窄」与「抬宽」拆成了两个
// 入口（WithKindCeiling 对抬宽 panic）——所以这里是 WithWiderKindCeiling。
// TestNotifierCeilingWideningMustUseTheWideningEntryPoint 把这件事钉死。
func notifierPlanPolicy() kernel.Policy {
	return notifierPlanGrants().WithWiderKindCeiling(kernel.KindNotifier, domain.SafetyL3)
}

// notifierPlanGrants 是 §4.6 草案里去掉 per-kind 抬宽后的那一半：同样的授予
// 集、全局 L2。方案 §11.1 的「L2 ceiling 拒」列就是对着这一形状测的，所以它
// 要单独留在测试里。
func notifierPlanGrants() kernel.Policy {
	return kernel.Policy{
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

func TestNotifierDraftManifestsAdmitOnlyBehindAKindCeiling(t *testing.T) {
	files := []string{
		"builtin-channels.yaml", "dingtalk-markdown.yaml", "feishu-card.yaml",
		"pagerduty.yaml", "slack-blocks.yaml", "webhook-strict.yaml",
	}
	drafts := loadPlanDrafts(t, "notifier", files...)

	for _, d := range drafts {
		// 全局 L2（§4.6 的授予集、不抬 per-kind）：六份全拒。这是 §4.5 候选 A
		// 的实测依据——不抬 per-kind ceiling，这套 manifest 一份都落不了地。
		mustRefuse(t, d.file, d.manifest, notifierPlanGrants(),
			"plugin is L3, target policy allows at most L2")
		// 顺带写下一个更靠前的事实：在**今天的** ManagerPolicy() 下，拒绝来得
		// 更早——授予集里没有 alert.write，L3 还没被比到就先撞上 scope 检查。
		// 方案 §11.1 那张表假设授予集已经是 §4.6 的形状，所以它看不到这一列。
		mustRefuse(t, d.file+"（今日 ManagerPolicy）", d.manifest, aiopsdatasource.ManagerPolicy(),
			"not granted: alert.write")
		// notifier 这个 kind 抬到 L3：六份全过。
		mustAdmit(t, d.file, d.manifest, notifierPlanPolicy())

		// 方案 §11.2 的评审闸门：一律 L3 + destructive、只要 alert.read +
		// alert.write、一律 pin。低报与 scope 膨胀闸门都挡不住（同 provider
		// 的对照 A/B），所以这几项必须钉数量。
		if d.manifest.Spec.SafetyLevel != domain.SafetyL3 {
			t.Errorf("%s: safety_level = %s, want L3（对外副作用不可撤销）", d.file, d.manifest.Spec.SafetyLevel)
		}
		scopes := d.manifest.Spec.RequiredScopes
		if len(scopes) != 2 || !scopes.Has(domain.ScopeAlertRO) || !scopes.Has(domain.ScopeAlertWrite) {
			t.Errorf("%s: required_scopes = %v, want 恰好 [alert.read alert.write]", d.file, scopes)
		}
		if d.manifest.Spec.Install.Strategy != domain.InstallPin {
			t.Errorf("%s: strategy = %q, want pin（通知外发没有 1/10 灰度的语义）",
				d.file, d.manifest.Spec.Install.Strategy)
		}
	}
}

// TestNotifierCeilingWideningMustUseTheWideningEntryPoint 防止方案 §4.6 的
// 代码片段被原样抄进仓里。
//
// 方案成文时内核只有一个 ceiling 入口；S-1 落地时把它拆成了 WithKindCeiling
// （只许收窄，抬宽 panic）与 WithWiderKindCeiling（只许抬宽）。也就是说，
// 方案里那行 `.WithKindCeiling(kernel.KindNotifier, domain.SafetyL3)` 在今天
// 的内核上是构造期 panic，而不是「全局 L2 + notifier 例外」。切片 S-1 落
// NotifierPolicy() 时必须用抬宽入口——这条测试在有人抄错的那天变红。
func TestNotifierCeilingWideningMustUseTheWideningEntryPoint(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("在 L2 全局上限上用 WithKindCeiling 抬宽 notifier 应当 panic；" +
				"抬宽必须走 WithWiderKindCeiling（见内核 host.go 的两个入口）")
		}
	}()
	base := kernel.Policy{
		Target:         domain.TargetManager,
		MaxSafetyLevel: domain.SafetyL2,
		MaxBlastRadius: domain.RadiusSingleNS,
	}
	_ = base.WithKindCeiling(kernel.KindNotifier, domain.SafetyL3)
}
