// runtimemgmt_wiring.go — 五类只读观察者的装配（决策 449 / 方案 A）。
//
// 它在装配根而不在 runtimemgmt 域内，原因与决策 284 把 adapter 移出域内
// 相同：观察者必须持有被观察域的具体类型（*edge.Usecase、
// *pluginhost.Host、chatruntime 的 Runtime），而 runtimemgmt 域一旦
// import 这些包，就产生了 ADR §3 方案 B 已否决的 5 条跨域边。装配根是
// 全仓唯一有权 import 所有域的地方，观察者放这里，域内只见
// server/runtimemgmt.ReadonlyRuntimeSource 这一个端口。
//
// 只读性由两件事共同保证：
//  1. 编译期：每个观察者只实现 Describe()，没有第二个方法可写；
//  2. 测试期（runtimemgmt_wiring_test.go）：调用 Describe 前后对被观察
//     域状态做双读比对，断言零变化。
//
// 拿不到的读数返回空投影或 health=unknown（ADR 硬约束：不为取数在被
// 观察域开洞）；被观察域一个都不改。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	pluginmanifest "github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	agentteamsstate "github.com/vincent-wuhan/opskeeper/core/manager/biz/agentteams/state"
	chatruntime "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/chatruntime"
	bizedge "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
	pluginhost "github.com/vincent-wuhan/opskeeper/core/manager/middleware/pluginhost"
	runtimemgmtmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
	managerserveragentteams "github.com/vincent-wuhan/opskeeper/core/manager/server/agentteams"
	managerserverruntimemgmt "github.com/vincent-wuhan/opskeeper/core/manager/server/runtimemgmt"
	"gorm.io/gorm"
)

// managerBuildVersion 是 chat worker 观察者的版本来源：进程内 worker 无
// manifest，VersionSource 只能是 release_metadata，取 manager 自身 build
// version（与 /v1/version 路由同一变量，见 main.go）。
var managerBuildVersion = "dev"

// healthUnknown 是观察者对“探不到”的诚实回答——ADR 允许空/unknown，
// 不允许猜测。
const healthUnknown = "unknown"

// runtimePresenceSampleLimit 与 alert pipeline / systemhealth 探针的
// edgeListerSampleLimit/edgeSampleLimit 同值：投影一次封顶一千行，
// 防止把一个万边集群的 inventory 请求变成一次全表扫描。
const runtimePresenceSampleLimit = 1000

// ---- 1. Edge Agent（node_edge）----

// EdgeRuntimeSource 投影 biz/edge 的注册边节点。数据面全部走既有只读
// API：ListPresence（presence.go 的 domain.EdgeQuery 投影，刻意不带
// 凭据六列）与 PluginHealth（内存只读）。版本来源
// release_metadata：AgentVersion/PigVersion 是 register_edge/心跳的自报
// 值。digest 首切片为空——spec 允许（空 ≠ 硬编码）。
type EdgeRuntimeSource struct {
	uc        *bizedge.Usecase
	fetchEdge func(ctx context.Context, id uint64) (agentVersion string, lastSeen *time.Time)
}

// NewEdgeRuntimeSource 构造。fetchEdge 为 nil 时仅用 presence 六列。
func NewEdgeRuntimeSource(uc *bizedge.Usecase) *EdgeRuntimeSource {
	return &EdgeRuntimeSource{uc: uc}
}

// Describe 满足 ReadonlyRuntimeSource。
func (s *EdgeRuntimeSource) Describe() []managerserverruntimemgmt.RuntimeObservation {
	if s == nil || s.uc == nil {
		return nil
	}
	presences, err := s.uc.ListPresence(context.Background(), runtimePresenceSampleLimit)
	if err != nil {
		// 读不到就报 unknown，不猜测，也不让 inventory 500。
		return []managerserverruntimemgmt.RuntimeObservation{{
			RuntimeID:    "edge:*",
			Kind:         runtimemgmtmodel.RuntimeKindNodeEdge,
			Health:       healthUnknown,
			HealthReason: "edge presence unavailable: " + err.Error(),
		}}
	}
	out := make([]managerserverruntimemgmt.RuntimeObservation, 0, len(presences))
	for _, p := range presences {
		obs := managerserverruntimemgmt.RuntimeObservation{
			RuntimeID: fmt.Sprintf("edge:%d", p.ID),
			Kind:      runtimemgmtmodel.RuntimeKindNodeEdge,
			Owner:     p.Name,
			Health:    presenceHealth(p.Status),
		}
		if p.LastSeenAt != nil {
			obs.Capabilities = map[string]any{
				"last_seen_at": p.LastSeenAt.UTC().Format(time.RFC3339),
			}
		}
		out = append(out, obs)
	}
	return out
}

func presenceHealth(status string) runtimemgmtmodel.RuntimeHealth {
	switch strings.ToLower(status) {
	case "online":
		return runtimemgmtmodel.RuntimeHealthOnline
	case "offline":
		return runtimemgmtmodel.RuntimeHealthOffline
	default:
		return healthUnknown
	}
}

// ---- 2. Manager Chat Worker（chat_worker）----

// ChatWorkerRuntimeSource 投影管理进程内的聊天工作器。数据面：
// chatruntime.Runtime 的 AgentRegistry().All()（值拷贝）与 ToolCount()。
// sinkRegistry 的活跃会话数不可导出——按 ADR 裁决不为它改 chatruntime，
// 首切片只投影 agent/tool 计数。
type ChatWorkerRuntimeSource struct {
	rt *chatruntime.Runtime
}

// NewChatWorkerRuntimeSource 构造。rt 为 nil（未启用 chat）时 Describe
// 返回 nil：没有工作器就没有可投影的 runtime，inventory 不该替它编一行。
func NewChatWorkerRuntimeSource(rt *chatruntime.Runtime) *ChatWorkerRuntimeSource {
	return &ChatWorkerRuntimeSource{rt: rt}
}

// Describe 满足 ReadonlyRuntimeSource。
func (s *ChatWorkerRuntimeSource) Describe() []managerserverruntimemgmt.RuntimeObservation {
	if s == nil || s.rt == nil {
		return nil
	}
	agents := 0
	if reg := s.rt.AgentRegistry(); reg != nil {
		agents = len(reg.All())
	}
	return []managerserverruntimemgmt.RuntimeObservation{{
		RuntimeID:          "chat-worker:manager",
		Kind:               runtimemgmtmodel.RuntimeKindChatWorker,
		Owner:              "manager",
		DeploymentLocation: "in-process",
		ObservedVersion:    managerBuildVersion,
		// 推导链：进程内 worker 无 manifest，版本取 manager 二进制的
		// build version（ldflags 注入，与 /v1/version 同源）= 发布元数据。
		VersionSource: runtimemgmtmodel.VersionSourceReleaseMetadata,
		Health:        runtimemgmtmodel.RuntimeHealthOnline,
		Capabilities: map[string]any{
			"agent_count": agents,
			"tool_count":  s.rt.ToolCount(),
		},
	}}
}

// ---- 3. AgentTeams Worker（agentteams_worker）----

// AgentTeamsRuntimeSource 投影 AgentTeams 团队工作器。数据面：state
// Manager 的 LookupProject/Get 只读面（文件账本）+ server/agentteams
// PluginRegistry.List（只读列插件）。版本来源 manifest：
// plugins/opskeeper-teamharness/plugin.yaml 的 metadata.version
// （validate-agentteams-protocols.rb 已强制非空）。
type AgentTeamsRuntimeSource struct {
	stateManager *agentteamsstate.Manager
	plugins      *managerserveragentteams.PluginRegistry
	manifestPath string
}

// NewAgentTeamsRuntimeSource 构造。任一依赖为 nil 时对应读数留空，
// 不猜测。
func NewAgentTeamsRuntimeSource(
	stateManager *agentteamsstate.Manager,
	plugins *managerserveragentteams.PluginRegistry,
	manifestPath string,
) *AgentTeamsRuntimeSource {
	return &AgentTeamsRuntimeSource{
		stateManager: stateManager,
		plugins:      plugins,
		manifestPath: manifestPath,
	}
}

// Describe 满足 ReadonlyRuntimeSource。
func (s *AgentTeamsRuntimeSource) Describe() []managerserverruntimemgmt.RuntimeObservation {
	if s == nil {
		return nil
	}
	obs := managerserverruntimemgmt.RuntimeObservation{
		RuntimeID: "agentteams:worker-1",
		Kind:      runtimemgmtmodel.RuntimeKindAgentTeamsWorker,
		Owner:     "agentteams",
		Health:    healthUnknown,
	}
	obs.ObservedVersion, obs.VersionSource = readTeamHarnessManifestVersion(s.manifestPath)
	caps := map[string]any{}
	if s.stateManager != nil {
		// LookupIncident 读单个事故的任务——state.Manager 没有全量列表
		// 方法（AddAudit 等是写路径），观察者只做一次只读存在性探测，
		// 不为此在 state 域加新 API（ADR：禁改被观察域）。
		_, err := s.stateManager.LookupIncident(context.Background(), "i1")
		switch {
		case err == nil:
			caps["ledger_readable"] = true
			caps["sample_incident_present"] = true
			obs.Health = runtimemgmtmodel.RuntimeHealthOnline
		case strings.Contains(err.Error(), "not found"):
			// 账本可解析、锁可取，只是没有这条样例事故：执行面在。
			caps["ledger_readable"] = true
			obs.Health = runtimemgmtmodel.RuntimeHealthOnline
		default:
			obs.HealthReason = "state ledger read failed: " + err.Error()
		}
	}
	if s.plugins != nil {
		if installed, err := s.plugins.List(context.Background()); err == nil {
			caps["installed_plugins"] = len(installed)
		}
	}
	obs.Capabilities = caps
	return []managerserverruntimemgmt.RuntimeObservation{obs}
}

// readTeamHarnessManifestVersion 从 plugin.yaml 读 metadata.version。
// 返回 ("", "") 表示读不到——空合法（“还无法读出”），绝不硬编码
// （spec 拒绝 hardcoded/script_constant 作为权威来源）。
func readTeamHarnessManifestVersion(path string) (string, runtimemgmtmodel.VersionSource) {
	if path == "" {
		return "", ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	version := readYAMLStringField(raw, "version")
	if version == "" {
		return "", ""
	}
	return version, runtimemgmtmodel.VersionSourceManifest
}

// readYAMLStringField 从小 YAML 文档顶层 metadata 段取一个字符串字段。
// 这里只需要 plugin.yaml 的一个值；引完整 YAML 解析器只为一个字段的
// 做法已被 version.go 的部署面板先例接受（同一文件、同一字段）。
func readYAMLStringField(raw []byte, field string) string {
	inMetadata := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		if trimmed == "" || strings.HasPrefix(strings.TrimSpace(trimmed), "#") {
			continue
		}
		if !strings.HasPrefix(trimmed, " ") && !strings.HasPrefix(trimmed, "\t") {
			inMetadata = trimmed == "metadata:"
			continue
		}
		if !inMetadata {
			continue
		}
		kv := strings.SplitN(strings.TrimSpace(trimmed), ":", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) != field {
			continue
		}
		v := strings.Trim(strings.TrimSpace(kv[1]), `"'`)
		if v != "" {
			return v
		}
	}
	return ""
}

// ---- 4. TeamHarness / plugin manager（plugin_manager）----

// PluginManagerRuntimeSource 投影 pluginhost.Host。数据面：
// Host.Inventory()（kernel.Inventory：Plugins/KindCounts/Entries）。
// 版本来源 manifest + artifact digest：pluginmanifest 的
// Metadata.Version 与 TreeDigest（sha256）——五类里唯一原生双具备。
type PluginManagerRuntimeSource struct {
	host       *pluginhost.Host
	pluginsDir string
}

// NewPluginManagerRuntimeSource 构造。
func NewPluginManagerRuntimeSource(host *pluginhost.Host, pluginsDir string) *PluginManagerRuntimeSource {
	return &PluginManagerRuntimeSource{host: host, pluginsDir: pluginsDir}
}

// Describe 满足 ReadonlyRuntimeSource。
func (s *PluginManagerRuntimeSource) Describe() []managerserverruntimemgmt.RuntimeObservation {
	if s == nil || s.host == nil {
		return nil
	}
	inv := s.host.Inventory()
	obs := managerserverruntimemgmt.RuntimeObservation{
		RuntimeID:          "pluginmgr:manager",
		Kind:               runtimemgmtmodel.RuntimeKindPluginManager,
		Owner:              "manager",
		DeploymentLocation: "in-process",
		Health:             runtimemgmtmodel.RuntimeHealthOnline,
		Capabilities: map[string]any{
			"plugins":     inv.Plugins,
			"kind_counts": inv.KindCounts,
			"entry_count": len(inv.Entries),
		},
	}
	// 版本与 digest 读插件树本身（只读磁盘扫描），读不到留空。
	if s.pluginsDir != "" {
		if pkgs, err := pluginmanifest.LoadAll(s.pluginsDir); err == nil && len(pkgs) > 0 {
			pkg := pkgs[0]
			obs.ObservedVersion = pkg.Manifest.Metadata.Version
			obs.VersionSource = runtimemgmtmodel.VersionSourceManifest
			if digest, derr := pluginmanifest.TreeDigest(pkg.Root); derr == nil {
				obs.ArtifactDigest = digest
			}
		}
	}
	return []managerserverruntimemgmt.RuntimeObservation{obs}
}

// ---- 5. preview-pg（pool_preview）----

// PoolPreviewRuntimeSource 投影 repair-preview 执行面。数据面：DSN 是否
// 已配置 + workload fingerprint（配置态，不碰数据库）；pool-fixture
// 管理器不在本进程内，不可达时 health=unknown，不猜测（ADR 裁决）。
type PoolPreviewRuntimeSource struct {
	dsnConfigured       bool
	workloadFingerprint string
	poolFixtureURL      string
}

// NewPoolPreviewRuntimeSource 构造。
func NewPoolPreviewRuntimeSource(dsnConfigured bool, workloadFingerprint, poolFixtureURL string) *PoolPreviewRuntimeSource {
	return &PoolPreviewRuntimeSource{
		dsnConfigured: dsnConfigured, workloadFingerprint: workloadFingerprint, poolFixtureURL: poolFixtureURL,
	}
}

// Describe 满足 ReadonlyRuntimeSource。
func (s *PoolPreviewRuntimeSource) Describe() []managerserverruntimemgmt.RuntimeObservation {
	if s == nil {
		return nil
	}
	obs := managerserverruntimemgmt.RuntimeObservation{
		RuntimeID: "previewpg:pool-fixture",
		Kind:      runtimemgmtmodel.RuntimeKindPoolPreview,
		Owner:     "repair-preview",
		Health:    healthUnknown,
	}
	if !s.dsnConfigured {
		obs.HealthReason = "repair preview DSN not configured"
		return []managerserverruntimemgmt.RuntimeObservation{obs}
	}
	// workload spec fingerprint 是 manifest 的等价物（ADR §4.4 表格）。
	if s.workloadFingerprint != "" {
		obs.ObservedVersion = s.workloadFingerprint
		obs.VersionSource = runtimemgmtmodel.VersionSourceManifest
	}
	obs.Health = runtimemgmtmodel.RuntimeHealthOnline
	if s.poolFixtureURL == "" {
		// DSN 在而 fixture 管理器不可达：能执行 preview，但池状态读不到。
		obs.HealthReason = "pool fixture manager not configured; pool status unavailable"
	}
	return []managerserverruntimemgmt.RuntimeObservation{obs}
}

// agentteamsLedgerPath 是 AgentTeams 顶层 state.json 的位置（与
// cmd/agentteams-state CLI 的 -state 默认值 state.json 同一约定，部署
// 侧把两边指到同一文件）。默认落在 /var/lib/opskeeper/agentteams，
// 可被 OPSKEEPER_AGENTTEAMS_STATE 覆盖——读不到不是错误（账本可能
// 尚未建），观察者会如实报 unknown。观察者只读这一份文件，不在该
// 目录写任何东西（runtimemgmt_wiring_test.go 双读比对断言零变化），
// 挂载属于写它的 agentteams-state CLI / worker 的部署职责。
func agentteamsLedgerPath() string {
	return firstNonEmpty(os.Getenv("OPSKEEPER_AGENTTEAMS_STATE"),
		"/var/lib/opskeeper/agentteams/state.json")
}

// teamHarnessManifestPath 是 agentteams worker 的 manifest 版本来源
// （plugins/opskeeper-teamharness/plugin.yaml 的 metadata.version）。
func teamHarnessManifestPath() string {
	return firstNonEmpty(os.Getenv("OPSKEEPER_TEAMHARNESS_MANIFEST"),
		"plugins/opskeeper-teamharness/plugin.yaml")
}

// buildRuntimeObservationSources 是装配根的入口：把五份观察者按当时
// 可用的依赖构造出来。任一依赖为 nil 时对应观察者仍然构造（Describe
// 内部判 nil），inventory 永远不会因为某个被观察域缺席而 500。
func buildRuntimeObservationSources(
	edgeUC *bizedge.Usecase,
	chatRT *chatruntime.Runtime,
	agentteamsState *agentteamsstate.Manager,
	pluginRegistry *managerserveragentteams.PluginRegistry,
	pluginHost *pluginhost.Host,
	managerPluginsDir string,
	teamHarnessManifest string,
	previewDSNConfigured bool,
	previewFingerprint string,
	poolFixtureURL string,
) []managerserverruntimemgmt.ReadonlyRuntimeSource {
	return []managerserverruntimemgmt.ReadonlyRuntimeSource{
		NewEdgeRuntimeSource(edgeUC),
		NewChatWorkerRuntimeSource(chatRT),
		NewAgentTeamsRuntimeSource(agentteamsState, pluginRegistry, teamHarnessManifest),
		NewPluginManagerRuntimeSource(pluginHost, managerPluginsDir),
		NewPoolPreviewRuntimeSource(previewDSNConfigured, previewFingerprint, poolFixtureURL),
	}
}

// runtimeStoreAdapter 把 gorm.DB 适配成 server/runtimemgmt.Store。
// 放装配根（不放 data 层）是为了让 data/runtimemgmt/store 只持有
// Migrate 一个职责——首切片仓储动作全部内联在此，避免为一个 upsert
// 建一层只有两个调用者的 service。后续切片仓储变复杂时再下沉。
type runtimeStoreAdapter struct {
	db *gorm.DB
}

func (a runtimeStoreAdapter) List() ([]runtimemgmtmodel.RuntimeRecord, error) {
	var recs []runtimemgmtmodel.RuntimeRecord
	if err := a.db.Order("runtime_id asc").Find(&recs).Error; err != nil {
		return nil, err
	}
	return recs, nil
}

func (a runtimeStoreAdapter) Get(runtimeID string) (runtimemgmtmodel.RuntimeRecord, error) {
	var rec runtimemgmtmodel.RuntimeRecord
	if err := a.db.Where("runtime_id = ?", runtimeID).First(&rec).Error; err != nil {
		return runtimemgmtmodel.RuntimeRecord{}, errs.ErrNotFound
	}
	return rec, nil
}

func (a runtimeStoreAdapter) Upsert(rec runtimemgmtmodel.RuntimeRecord) (runtimemgmtmodel.RuntimeRecord, error) {
	if err := a.db.Save(&rec).Error; err != nil {
		return runtimemgmtmodel.RuntimeRecord{}, err
	}
	return rec, nil
}

func (a runtimeStoreAdapter) Save(rec runtimemgmtmodel.RuntimeRecord) error {
	return a.db.Save(&rec).Error
}
