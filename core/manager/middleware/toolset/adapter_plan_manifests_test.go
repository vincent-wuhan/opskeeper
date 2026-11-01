package toolset

// adapter 方案草案的准入闸门（对照 adapter 宿主真实的 policy）。
//
// 方案 §4.4 / §5.4 / §6.4 把三份草案的 sdk.Decode → sdk.Validate → Admit 记录成
// 任务期间一次性跑过的验证，并注明未留测试（§10 的 S-5）。本文件是那道闸门
// 的固化件：住在 toolset 是因为要对照的 policy（adapterPolicy，基于
// defaultAdapterScopes）就在这个包里，且未导出——把测试放在别处就只能复述
// policy，而复述可以和真本各说各话。
//
// 与方案成文时相比，两件事变了，测试按今天的现实断言并把差异写进注释：
//
//   - D1 / D2 已裁决并落地：defaultAdapterScopes() 在八个一方 scope 之外加了
//     topology.read 与 cloud.read。于是 cmdb / alicloud 两份草案从方案里的
//     「琥珀：被拒」变成「绿：放行」，而方案留的 Contract 回归保护（当前
//     policy 断言拒绝并点名缺失 scope）也随之翻面——这里保留同一组断言的
//     「翻面后」版本：授予集收回到裁决前的八项时，拒绝必须逐字复现。
//   - 闸门经 kernel.Host.Admit 表达：target 检查与 policy → Admission 的换算
//     都由宿主完成，测试不抄第二份。

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
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// planChangeName 是草案所属的 OpenSpec change 目录名。
const planChangeName = "plugin-architecture-recovery-plan"

// adapterDraft 是一份 adapter 草案：文件名与解码结果。
type adapterDraft struct {
	file     string
	manifest domain.PluginManifest
}

// adapterPlanDir 定位 adapter 草案目录。按相对后缀搜索而不是写死
// openspec/changes/<change>/，change 归档进 archive/ 后这里继续工作；草案目录
// 被删除（方案落地为真实包）时这里失败并要求把闸门搬到包目录上，而不是静默
// 变绿。
func adapterPlanDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, ok := reporoot.Find(wd, 10)
	if !ok {
		t.Fatalf("%s 上方找不到仓库根目录", wd)
	}
	base := filepath.Join(root, "openspec")

	var hits []string
	err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(base, p)
		if relErr != nil {
			return relErr
		}
		if rel := filepath.ToSlash(rel); strings.HasSuffix(rel, planChangeName+"/adapter-manifests") {
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
		t.Fatalf("openspec/ 下找不到 %s/adapter-manifests。草案目录被移动或删除了："+
			"若方案已落地为真实包，请把这道闸门改对着 plugins/<pkg>/pig-ops.yaml 跑", planChangeName)
	default:
		t.Fatalf("openspec/ 下有 %d 个候选目录（%v）；草案目录必须唯一", len(hits), hits)
	}
	return ""
}

// loadAdapterDrafts 解码三份草案。files 是期望清单：多一份、少一份都失败——
// 一份没有被点名的草案就是一份没有闸门的草案。
func loadAdapterDrafts(t *testing.T, files ...string) []adapterDraft {
	t.Helper()
	dir := adapterPlanDir(t)
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
		t.Fatalf("%s 里的草案是 %v，期望 %v；新增或删除草案时必须同步这份清单", dir, got, want)
	}

	out := make([]adapterDraft, 0, len(files))
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
		out = append(out, adapterDraft{file: name, manifest: m})
	}
	return out
}

// admitAdapter 在给定 policy 下跑包级准入，返回 ""（放行）或拒绝文本。
func admitAdapter(m domain.PluginManifest, pol kernel.Policy) string {
	host := kernel.NewHost(kernel.NewRegistry(), pol, nil)
	if err := host.Admit(pluginmanifest.Plugin{Manifest: m}); err != nil {
		return err.Error()
	}
	return ""
}

func mustAdmitAdapter(t *testing.T, name string, m domain.PluginManifest, pol kernel.Policy) {
	t.Helper()
	if got := admitAdapter(m, pol); got != "" {
		t.Fatalf("%s: 期望放行，实际被拒：%s", name, got)
	}
}

// mustRefuseAdapter 断言拒绝且理由点名 wantSub。点名是为了证明闸门在比什么：
// 一份因为别的字段被拒的 manifest 也会让裸的「期望被拒」通过。
func mustRefuseAdapter(t *testing.T, name string, m domain.PluginManifest, pol kernel.Policy, wantSub string) {
	t.Helper()
	got := admitAdapter(m, pol)
	if got == "" {
		t.Fatalf("%s: 期望被拒（拒绝点 %q），实际被放行", name, wantSub)
	}
	if !strings.Contains(got, wantSub) {
		t.Fatalf("%s: 拒绝理由 %q 不含 %q", name, got, wantSub)
	}
}

// TestTheAdapterDefaultGrantSetCarriesTheD1D2Decisions 先钉住闸门对照的授予集
// 本身。D1（topology.read）与 D2（cloud.read）是两条已裁决的显式决策，各对应
// 一份草案；授予集回退到裁决前的八项时，下一条测试的拒绝必须逐字复现——
// 这两条测试合起来才是「决策落地了，且落地的是这两项」的证据。
func TestTheAdapterDefaultGrantSetCarriesTheD1D2Decisions(t *testing.T) {
	granted := defaultAdapterScopes()
	for _, s := range []domain.Scope{
		domain.ScopeTopologyRO, // D1：外部 CMDB 的资产/拓扑读
		domain.ScopeCloudRead,  // D2：云厂商 OpenAPI 读，独立于 host.read
	} {
		if !granted.Has(s) {
			t.Errorf("defaultAdapterScopes() 缺 %s；D1/D2 的裁决是「同意授予」，"+
				"删掉它等于悄悄撤销一条已裁决的决策", s)
		}
	}
	// 决策只加了两项读：cloud.write 属于写操作切片，明确不在默认授予集里。
	if granted.Has("cloud.write") {
		t.Errorf("defaultAdapterScopes() 不应包含 cloud.write；云侧写在写操作切片评审，" +
			"不随默认授予集进来")
	}
}

// TestAdapterDraftManifestsAdmitUnderTheAdapterHostPolicy 是 §2.2 / §4.4 /
// §5.4 / §6.4 的主闸门：三份草案在 adapter 宿主真实的 policy 下全部通过。
//
// 方案成文时 cmdb / alicloud 是琥珀（授予集缺一项），D1 / D2 裁决并落地后
// 转绿。这条测试的存在让「转绿」成为被断言的状态，而不是一句注记。
func TestAdapterDraftManifestsAdmitUnderTheAdapterHostPolicy(t *testing.T) {
	drafts := loadAdapterDrafts(t, "alicloud.yaml", "cmdb.yaml", "karmada.yaml")
	pol := adapterPolicy()

	for _, d := range drafts {
		mustAdmitAdapter(t, d.file, d.manifest, pol)

		// 采集/适配面一律只读：L1 + [read] + 只在控制面。低报闸门挡不住
		// （等级是评审判断），所以这几项必须钉数量。
		if d.manifest.Spec.SafetyLevel != domain.SafetyL1 {
			t.Errorf("%s: safety_level = %s, want L1（本批三份全部只读）",
				d.file, d.manifest.Spec.SafetyLevel)
		}
		caps := d.manifest.Spec.Capabilities
		if len(caps) != 1 || caps[0] != domain.ClassRead {
			t.Errorf("%s: capabilities = %v, want 恰好 [read]", d.file, caps)
		}
		targets := d.manifest.Spec.Targets
		if len(targets) != 1 || !targets.Has(domain.TargetManager) {
			t.Errorf("%s: targets = %v, want [manager]", d.file, targets)
		}
		if d.manifest.Spec.Install.Strategy != domain.InstallRolling {
			t.Errorf("%s: strategy = %q, want rolling（只读包换版只是换工具面）",
				d.file, d.manifest.Spec.Install.Strategy)
		}
	}
}

// TestAdapterDraftsRefuseUnderThePreDecisionGrantSet 是方案 §5.4 / §6.4 留下的
// Contract 行的翻面版本：把授予集收回到 D1 / D2 裁决前的八项一方 scope，
// 拒绝必须逐字复现方案里记录的那两条。
//
// 它证明两件事：一是拒绝确实出自 Satisfies 的授予检查（点名缺失的 scope），
// 而不是碰巧被别的字段拦下；二是 D1 / D2 落地的只有「授予」这一件事——
// 撤掉授予，方案里的琥珀立刻回来。
func TestAdapterDraftsRefuseUnderThePreDecisionGrantSet(t *testing.T) {
	preDecisions := AdapterPolicy(domain.Scopes{
		domain.ScopeDBRead, domain.ScopeDBWrite,
		domain.ScopeK8sRead, domain.ScopeK8sExec,
		domain.ScopeMQRead, domain.ScopeMQWrite,
		domain.ScopeHostRead, domain.ScopeHostWrite,
	})
	byFile := map[string]domain.PluginManifest{}
	for _, d := range loadAdapterDrafts(t, "alicloud.yaml", "cmdb.yaml", "karmada.yaml") {
		byFile[d.file] = d.manifest
	}

	t.Run("karmada 一开始就是绿的", func(t *testing.T) {
		mustAdmitAdapter(t, "karmada.yaml", byFile["karmada.yaml"], preDecisions)
	})
	t.Run("cmdb 在 D1 裁决前被拒并点名 topology.read", func(t *testing.T) {
		mustRefuseAdapter(t, "cmdb.yaml", byFile["cmdb.yaml"], preDecisions, "not granted: topology.read")
	})
	t.Run("alicloud 在 D2 裁决前被拒并点名 cloud.read", func(t *testing.T) {
		mustRefuseAdapter(t, "alicloud.yaml", byFile["alicloud.yaml"], preDecisions, "not granted: cloud.read")
	})
}
