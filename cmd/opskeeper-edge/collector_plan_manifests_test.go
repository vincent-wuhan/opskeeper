package main

// collector 方案草案的准入闸门（edge 侧，对照部署 profile 渲染出的真实 policy）。
//
// 方案 §9.1 把三份草案过 sdk.Decode → sdk.Validate → Admit 的结果记录成任务
// 期间一次性跑过的验证，并注明未留测试（§10 的 S-5 建议固化成
// collector-manifests_test.go）。本文件是那道闸门的固化件；住在
// cmd/opskeeper-edge 是因为闸门要对照的 policy 由 core/edge/pluginhost.
// PolicyFrom(profile) 渲染，而这台宿主就是 edge——闸门住在它要装机的地方。
//
// 与方案成文时相比有一处口径差：方案表格里 saas 档对 L3 采集器「过」被标成
// 待裁决的 D2。本任务按今天的 profile 断言（saas 上限就是 L3），并把这一点
// 写进 gate-run-report.md 的遗留项——要不要在采集面单独压低 ceiling，是
// profile 的决定，不是这道闸门的。

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/pluginhost"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/reporoot"
	aiopsdatasource "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/datasource"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// planChangeName 是草案所属的 OpenSpec change 目录名。
const planChangeName = "plugin-architecture-recovery-plan"

// collectorDraft 是一份 collector 草案：文件名、磁盘原文、解码结果。原文保留
// 下来是因为负例必须改在 YAML 上——未知字段这类最有价值的拒绝只发生在解码
// 期，解码后的结构体上造不出来。
type collectorDraft struct {
	file     string
	raw      []byte
	manifest domain.PluginManifest
}

// collectorPlanDir 定位 collector 草案目录。按相对后缀搜索而不是写死
// openspec/changes/<change>/，change 归档进 archive/ 后这里继续工作；草案目录
// 被删除（方案落地为真实包）时这里失败并要求把闸门搬到包目录上，而不是静默
// 变绿。
func collectorPlanDir(t *testing.T) string {
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
		if rel := filepath.ToSlash(rel); strings.HasSuffix(rel, planChangeName+"/collector-manifests") {
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
		t.Fatalf("openspec/ 下找不到 %s/collector-manifests。草案目录被移动或删除了："+
			"若方案已落地为真实包，请把这道闸门改对着 plugins/<pkg>/pig-ops.yaml 跑", planChangeName)
	default:
		t.Fatalf("openspec/ 下有 %d 个候选目录（%v）；草案目录必须唯一", len(hits), hits)
	}
	return ""
}

// loadCollectorDrafts 解码三份草案。files 是期望清单：多一份、少一份都失败。
func loadCollectorDrafts(t *testing.T, files ...string) []collectorDraft {
	t.Helper()
	dir := collectorPlanDir(t)
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

	out := make([]collectorDraft, 0, len(files))
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
		out = append(out, collectorDraft{file: name, raw: raw, manifest: m})
	}
	return out
}

// edited 把草案原文里的 old 换成 new。old 不在原文里时直接失败：一次没有发生
// 的改写会让负例去断言一份正例。
func edited(t *testing.T, d collectorDraft, old, replacement string) []byte {
	t.Helper()
	if !strings.Contains(string(d.raw), old) {
		t.Fatalf("%s: 负例改写点 %q 不在原文里；草案改版后请同步改这条负例", d.file, old)
	}
	return []byte(strings.Replace(string(d.raw), old, replacement, 1))
}

// edgePolicyByName 用 profile 名渲染这台节点宿主的 policy——与
// pluginhost.EdgePolicyFor 同一条路径，负例与正例都必须对着它跑。
func edgePolicyByName(t *testing.T, name pluginmanifest.ProfileName) kernel.Policy {
	t.Helper()
	pol, err := pluginhost.EdgePolicyFor(name)
	if err != nil {
		t.Fatalf("渲染 %s: %v", name, err)
	}
	return pol
}

// admitCollector 在给定 policy 下跑包级准入，返回 ""（放行）或拒绝文本。
func admitCollector(m domain.PluginManifest, pol kernel.Policy) string {
	host := kernel.NewHost(kernel.NewRegistry(), pol, nil)
	if err := host.Admit(pluginmanifest.Plugin{Manifest: m}); err != nil {
		return err.Error()
	}
	return ""
}

func mustAdmitCollector(t *testing.T, name string, m domain.PluginManifest, pol kernel.Policy) {
	t.Helper()
	if got := admitCollector(m, pol); got != "" {
		t.Fatalf("%s: 期望在 %s 档下放行，实际被拒：%s",
			name, pol.Target, got)
	}
}

// mustRefuseCollector 断言拒绝且理由点名 wantSub。
func mustRefuseCollector(t *testing.T, name string, m domain.PluginManifest, pol kernel.Policy, wantSub string) {
	t.Helper()
	got := admitCollector(m, pol)
	if got == "" {
		t.Fatalf("%s: 期望被拒（拒绝点 %q），实际被放行", name, wantSub)
	}
	if !strings.Contains(got, wantSub) {
		t.Fatalf("%s: 拒绝理由 %q 不含 %q", name, got, wantSub)
	}
}

// TestCollectorDraftManifestsAdmitUnderBothProfiles 是方案 §9.1 的主表：三份
// 草案在 finance（L2/pod）与 saas（L3/ns）两档 edge policy 下全部通过。
//
// 这也是「采集器不需要动 profile / policy」这句话的证据：scope 全落在两个
// profile 都授予的读集合里，与 4.4 需要 D1/D2 扩 policy 形成对照。
func TestCollectorDraftManifestsAdmitUnderBothProfiles(t *testing.T) {
	drafts := loadCollectorDrafts(t,
		"builtin-collectors.yaml", "filelog.yaml", "otelcol-kubeletstats.yaml")
	finance := edgePolicyByName(t, pluginmanifest.ProfileFinance)
	saas := edgePolicyByName(t, pluginmanifest.ProfileSaaS)

	for _, d := range drafts {
		mustAdmitCollector(t, d.file, d.manifest, finance)
		mustAdmitCollector(t, d.file, d.manifest, saas)

		// 采集器一律 L1 + [read] + 只在节点：低报闸门挡不住（等级是评审
		// 判断），高报由下面的负例拦，所以这三项钉数量。
		if d.manifest.Spec.SafetyLevel != domain.SafetyL1 {
			t.Errorf("%s: safety_level = %s, want L1（一个要写什么的采集器不是采集器）",
				d.file, d.manifest.Spec.SafetyLevel)
		}
		caps := d.manifest.Spec.Capabilities
		if len(caps) != 1 || caps[0] != domain.ClassRead {
			t.Errorf("%s: capabilities = %v, want 恰好 [read]", d.file, caps)
		}
		targets := d.manifest.Spec.Targets
		if len(targets) != 1 || !targets.Has(domain.TargetEdge) {
			t.Errorf("%s: targets = %v, want [edge]", d.file, targets)
		}
	}
}

// TestCollectorDraftNegativeControls 固化方案 §9.1 的负例表。每个对照回答
// 「闸门真的在比什么」的一个分支。
func TestCollectorDraftNegativeControls(t *testing.T) {
	drafts := loadCollectorDrafts(t,
		"builtin-collectors.yaml", "filelog.yaml", "otelcol-kubeletstats.yaml")
	byFile := map[string]collectorDraft{}
	for _, d := range drafts {
		byFile[d.file] = d
	}
	finance := edgePolicyByName(t, pluginmanifest.ProfileFinance)
	saas := edgePolicyByName(t, pluginmanifest.ProfileSaaS)

	t.Run("spec.collector 资源声明位今天不存在", func(t *testing.T) {
		d := byFile["filelog.yaml"]
		raw := edited(t, d, "  targets: [edge]",
			"  targets: [edge]\n  collector:\n    process: subprocess")
		_, err := sdk.Decode(raw)
		if err == nil {
			t.Fatal("带 spec.collector 块的草案被解码放行了；KnownFields 应当 fail-closed")
		}
		if !strings.Contains(err.Error(), "field collector not found in type domain.PluginSpec") {
			t.Fatalf("解码拒绝理由 %q 不含未知字段说明", err.Error())
		}
		// 这个负例同时是 S-5 的接口契约：schema 变更落地那天，这里会红，
		// 提醒把资源声明的 admission 检查（ask ≤ cap）一起接上。
	})

	t.Run("k8s.exec 在金融档被拒、在 SaaS 档放行", func(t *testing.T) {
		d := byFile["filelog.yaml"]
		m, err := sdk.Decode(edited(t, d, "    - host.read", "    - k8s.exec"))
		if err != nil {
			t.Fatalf("解码 k8s.exec 变体: %v", err)
		}
		mustRefuseCollector(t, d.file, m, finance, "not granted: k8s.exec")
		mustAdmitCollector(t, d.file, m, saas)
	})

	t.Run("L1 挂 blast radius 在解码期被拒", func(t *testing.T) {
		d := byFile["filelog.yaml"]
		raw := edited(t, d,
			"  approval:\n    required: false",
			"  approval:\n    required: false\n    max_blast_radius: pod")
		_, err := sdk.Decode(raw)
		if err == nil {
			t.Fatal("L1 + max_blast_radius 的草案被放行了；只读包不得携带影响面")
		}
		if !strings.Contains(err.Error(), "L1 is read-only and cannot carry a blast radius") {
			t.Fatalf("解码拒绝理由 %q 不含只读说明", err.Error())
		}
	})

	t.Run("L3 在金融档被拒、在 SaaS 档放行", func(t *testing.T) {
		d := byFile["filelog.yaml"]
		m, err := sdk.Decode(edited(t, d, "  safety_level: L1", "  safety_level: L3"))
		if err != nil {
			t.Fatalf("解码 L3 变体: %v", err)
		}
		mustRefuseCollector(t, d.file, m, finance, "plugin is L3, target policy allows at most L2")
		// SaaS 档上限就是 L3，所以这里放行——方案 §9.1 把它记为 D2 待裁决。
		// 断言「放行」而不是回避它：这是 profile 的现实，改它要改 profile。
		mustAdmitCollector(t, d.file, m, saas)
	})

	t.Run("曾经的 L1 单 scope policy 拒掉全部随包", func(t *testing.T) {
		// 复刻 core/edge/pluginhost/host.go 注释里记录的那个被真实节点运行
		// 证伪的 policy：L1、只授予 host.read。一个装不下仓库自己随包的
		// policy 不是保守，是坏掉——这个对照防止它以「收紧」的名义回来。
		old := kernel.Policy{
			Target:         domain.TargetEdge,
			MaxSafetyLevel: domain.SafetyL1,
			MaxBlastRadius: domain.RadiusPod,
			GrantedScopes:  domain.Scopes{domain.ScopeHostRead},
		}
		for _, d := range drafts {
			mustRefuseCollector(t, d.file, d.manifest, old, "not granted:")
		}
	})

	t.Run("manager 宿主不会误装 edge 包", func(t *testing.T) {
		for _, d := range drafts {
			mustRefuseCollector(t, d.file, d.manifest, aiopsdatasource.ManagerPolicy(),
				`targets [edge], host runs "manager"`)
		}
	})
}
