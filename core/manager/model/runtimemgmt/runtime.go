// runtime.go 保存 §1「Runtime Inventory and Readback」首切片的持久化
// 运行时注册记录与健康状态机（OpenSpec change runtime-management-gap）。
//
// 与 task.go 的分工：Task 描述租户内的一次工作；RuntimeRecord 描述平台
// 执行面本身。五类首切片 runtime（Edge Agent / Manager Chat Worker /
// AgentTeams Worker / TeamHarness plugin manager / preview-pg）通过
// `POST /v1/runtimes` 注册，经心跳续约，由 `GET /v1/runtimes` 读回。
package runtimemgmt

import (
	"time"
)

// RuntimeKind 是首切片承认的五类执行面。闭集：注册请求携带其它值会被
// 拒绝，避免注册表长出无人认领的“第七类”。
type RuntimeKind string

const (
	// RuntimeKindNodeEdge 是边端节点代理（biz/edge 的 Edge 行）。
	RuntimeKindNodeEdge RuntimeKind = "node_edge"
	// RuntimeKindChatWorker 是管理进程内聊天工作器（chatruntime.Runtime）。
	RuntimeKindChatWorker RuntimeKind = "chat_worker"
	// RuntimeKindAgentTeamsWorker 是 AgentTeams 团队工作器（state 账本持有者）。
	RuntimeKindAgentTeamsWorker RuntimeKind = "agentteams_worker"
	// RuntimeKindPluginManager 是 TeamHarness/插件宿主（pluginhost.Host）。
	RuntimeKindPluginManager RuntimeKind = "plugin_manager"
	// RuntimeKindPoolPreview 是一次性预览执行面（preview-pg 连接池）。
	RuntimeKindPoolPreview RuntimeKind = "pool_preview"
)

// AllRuntimeKinds 是注册校验用的闭集成员列表。
var AllRuntimeKinds = []RuntimeKind{
	RuntimeKindNodeEdge,
	RuntimeKindChatWorker,
	RuntimeKindAgentTeamsWorker,
	RuntimeKindPluginManager,
	RuntimeKindPoolPreview,
}

// IsRuntimeKind 判断 k 是否在 AllRuntimeKinds 内。
func IsRuntimeKind(k RuntimeKind) bool {
	for _, v := range AllRuntimeKinds {
		if v == k {
			return true
		}
	}
	return false
}

// RuntimeHealth 是 spec.md「Runtime health has bounded transitions」定义的
// 五态闭集。迁移语义见 EvaluateHealth：本切片健康是读时惰性推导，不起
// 后台 sweeper——「零 goroutine、零行为变化」是 ADR §4.2 的验收条件。
type RuntimeHealth string

const (
	// RuntimeHealthStarting 是注册后尚未收到第一份心跳。
	RuntimeHealthStarting RuntimeHealth = "starting"
	// RuntimeHealthOnline 是心跳落在 grace 窗口内且无版本漂移。
	RuntimeHealthOnline RuntimeHealth = "online"
	// RuntimeHealthDegraded 是心跳仍在续但版本/摘要漂移或 version_source
	// 非法：spec 规定「accepts the liveness observation, records the
	// anomaly」——降级不拒心跳。
	RuntimeHealthDegraded RuntimeHealth = "degraded"
	// RuntimeHealthRecentlyLost 是超过 reconnect grace 仍无心跳，
	// 但尚未超过离线上限。
	RuntimeHealthRecentlyLost RuntimeHealth = "recently_lost"
	// RuntimeHealthOffline 是超过 reconnect 上限仍无心跳。
	RuntimeHealthOffline RuntimeHealth = "offline"
)

// VersionSource 是 ObservedVersion 的合法来源闭集（spec「Runtime version
// provenance and drift are manager-validated」）：只接受 manifest 或
// release_metadata，hardcoded/script_constant 记漂移进 degraded。
type VersionSource string

const (
	// VersionSourceManifest 表示版本读自插件/runtime manifest。
	VersionSourceManifest VersionSource = "manifest"
	// VersionSourceReleaseMetadata 表示版本读自发布元数据（二进制
	// ldflags、自报 register 版本、workload fingerprint 等价物）。
	VersionSourceReleaseMetadata VersionSource = "release_metadata"
)

// IsVersionSource 判断 s 是否是合法的版本来源。非法来源不被拒绝——
// spec 要求接受存活观察并记降级——但调用方必须把它记入 HealthReason。
func IsVersionSource(s VersionSource) bool {
	return s == VersionSourceManifest || s == VersionSourceReleaseMetadata
}

// RuntimeRecord 是 `runtimemgmt_runtimes` 表的行。
//
// 多租户刻意不加：runtime 是平台级资产，不是租户资产——edge/pluginmgr/
// previewpg 均无 tenant 维度（model/edge post-pivot 无 org_id）。这与
// task.go 的 Task（携带 TenantID）是**刻意的不同**：Task 是租户内工作，
// Runtime 是平台执行面。后人不要“补一致性”加 TenantID；要加先过 ADR。
type RuntimeRecord struct {
	// RuntimeID 是调用方提供的稳定主键，形如 edge:12、chat-worker:manager、
	// agentteams:worker-1、pluginmgr:manager、previewpg:pool-fixture。
	// 由注册方命名而非服务端生成，因为只有注册方知道自己部署在哪。
	RuntimeID string `gorm:"primaryKey;size:128"`

	// Kind 是五类闭集之一，见 RuntimeKind。
	Kind RuntimeKind `gorm:"size:32;not null;index"`

	// Profile / Owner / DeploymentLocation / Labels 是部署档案。Labels
	// 以 JSON 对象存储，读回时原样返回。
	Profile            string `gorm:"size:64;not null;default:''"`
	Owner              string `gorm:"size:64;not null;default:''"`
	DeploymentLocation string `gorm:"size:128;not null;default:''"`
	LabelsJSON         string `gorm:"type:text;not null;default:'{}'"`

	// 读回：版本必须来自 manifest 或 release_metadata（IsVersionSource），
	// 空版本合法（spec 允许“还无法读出”），空不等于硬编码。
	ObservedVersion  string        `gorm:"size:64;not null;default:''"`
	VersionSource    VersionSource `gorm:"size:32;not null;default:''"`
	ArtifactDigest   string        `gorm:"size:64;not null;default:''"`
	CapabilityDigest string        `gorm:"size:64;not null;default:''"`
	CapabilitiesJSON string        `gorm:"type:text;not null;default:'{}'"`

	// 健康三列：Health 是最近一次写入时推导的观察值；真实健康由
	// EvaluateHealth 按 now 与 LastHeartbeatAt 惰性推导，行内值只作
	// 离线回放参考。HealthReason 记降级原因（如 version drift）。
	Health          RuntimeHealth `gorm:"size:16;not null;default:'starting';index"`
	LastHeartbeatAt *time.Time    `gorm:"column:last_heartbeat_at"`
	HealthReason    string        `gorm:"size:255;not null;default:''"`

	// 容量：首切片只存运行时自报快照，不做准入（§2.2 才接 slot）。
	TotalSlots  int `gorm:"not null;default:0"`
	FreeSlots   int `gorm:"not null;default:0"`
	ActiveTasks int `gorm:"not null;default:0"`

	// PendingCommandAcksJSON 记录 ack 契约形状（ADR §4.3：首切片 ack 只
	// 落一行记录，命令下发是 2.4–2.6 的事；不为此单独建表）。
	PendingCommandAcksJSON string `gorm:"type:text;not null;default:'{}'"`

	// CommandAckCount / LastCommandAckAt 是 ack 计数与最近时戳。
	CommandAckCount  int        `gorm:"not null;default:0"`
	LastCommandAckAt *time.Time `gorm:"column:last_command_ack_at"`

	RegisteredAt time.Time `gorm:"not null"`
	UpdatedAt    time.Time `gorm:"not null"`
}

// TableName 固定表名，迁移必须保持不变。
func (RuntimeRecord) TableName() string { return "runtimemgmt_runtimes" }

// 健康窗口默认常量。ADR §4.2：首切片先落默认常量，env 覆盖与 Manager
// 策略持有属于后续切片；runtime manifest 不可越权缩短（delta-spec-patch
// 的 profile 裁决）。
const (
	// DefaultHeartbeatInterval 是约定的上报间隔，用于解释 grace 的倍数。
	DefaultHeartbeatInterval = 30 * time.Second
	// DefaultReconnectGrace 是错过多少时长仍可视为 recently_lost 之前的
	// 宽限：错过单个心跳间隔不降级（spec「One heartbeat is missed」），
	// 超过两倍间隔才进入 recently_lost。
	DefaultReconnectGrace = 2 * DefaultHeartbeatInterval
	// DefaultOfflineAfter 是从最近心跳算起多久判 offline。
	DefaultOfflineAfter = 5 * DefaultHeartbeatInterval
)

// EvaluateHealth 按 now 与记录当前状态惰性推导健康。无心跳时只看
// RegisteredAt 之后是否一直无上报（starting 保持 starting，直到超过
// offline 窗口才判 offline——一个从未心跳过的注册不是 online，也不该
// 立刻被扫成 offline）。
//
// 惰性而非后台 sweeper 是 ADR §4.2 的显式取舍：读时推导保证零 goroutine
// 与默认部署零行为变化，且转移表可单测（runtime_health_test.go）。
func (r RuntimeRecord) EvaluateHealth(now time.Time) RuntimeHealth {
	// 写入时已记降级且仍在续约的，保持 degraded——版本漂移不会因为
	// 时间流逝而自愈。
	last := r.LastHeartbeatAt
	if last == nil {
		if now.Sub(r.RegisteredAt) > DefaultOfflineAfter {
			return RuntimeHealthOffline
		}
		return RuntimeHealthStarting
	}
	elapsed := now.Sub(last.UTC())
	switch {
	case elapsed > DefaultOfflineAfter:
		return RuntimeHealthOffline
	case elapsed > DefaultReconnectGrace:
		return RuntimeHealthRecentlyLost
	case r.Health == RuntimeHealthDegraded:
		// 仍在宽限内但记录过漂移：接受存活观察、保留异常标记。
		return RuntimeHealthDegraded
	default:
		return RuntimeHealthOnline
	}
}
