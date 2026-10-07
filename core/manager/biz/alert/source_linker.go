// Package alert - L5 Source Linker（借鉴 v1 monitoring-source-linker）
//
// 本文件实现 zero-manual-ops-loop Day 2 Task 2.4：移植 v1 monitoring-source-linker
// 的 12 条 DIAGNOSIS_SKILL_MAP 路径映射（PG 4 + Redis 2 + K8s 3 + Host 3）。
//
// 集成位置（设计 D.3）：
//
//	correlated_alert
//	    │
//	    ▼
//	orchestrator.Run() → phase=correlated
//	    │
//	    ▼
//	investigator/usecase.go::Enqueue
//	    │
//	    ▼
//	worker spawn → chatruntime
//	    │
//	    ▼ (worker 输出 current_skill)
//	source_linker.Next(ResourceType, CurrentSkill, Incident, RootCauseHint, MessageContent)
//	    │
//	    ├ 命中 12 条 → 产 SourceLinkOutput.NextPhaseHint="link_runtime_to_commit"
//	    │           → orchestrator 调度 PhaseWorker.Planner() → Executor() = linkRuntimeToCommit
//	    │
//	    └ 未命中 → NextPhaseHint="skip"
//
// Day 2 仅做 schema + 路由表，不实际调用 linkRuntimeToCommit（Day 4 落地）。
package alert

import (
	"context"
	"fmt"
	"time"
)

// HostAdapter host 资源诊断工具接口（设计 D.12 + 路径 A 集成 Day 3）。
//
// 三个方法对齐 v1 host-cpu-spike / host-memory-burst / host-disk-full 工具，
// 在 SourceLinker 路由表命中 host.* skill 后由 wire-up 流水线同步调用。
//
// 真实实现由主 agent 在 wire-up 阶段接入 ops-keeper opsadaptor 或自研 host_exporter；
// 本文件提供 NoopHostAdapter 占位，保证 12 条路径默认可达（返 noop finding + nil err）。
type HostAdapter interface {
	InvestigateCPU(ctx context.Context, target string) (finding string, evidenceRaw []byte, err error)
	InvestigateMemory(ctx context.Context, target string) (finding string, evidenceRaw []byte, err error)
	InvestigateDisk(ctx context.Context, target string) (finding string, evidenceRaw []byte, err error)
}

// NoopHostAdapter 默认占位实现。
//
// 所有 Investigate* 返回 finding="noop host adapter, <metric> investigation pending wire-up"
// + 空 evidence + nil error，保证 DIAGNOSIS_SKILL_MAP 12 条路径默认命中；真实指标采集
// 由主 agent 接入 opsadaptor / 自研 host_exporter 后通过 SourceLinker.WithHostAdapter 替换。
type NoopHostAdapter struct{}

// InvestigateCPU 占位：返 noop finding + nil err。
func (NoopHostAdapter) InvestigateCPU(ctx context.Context, target string) (string, []byte, error) {
	return "noop host adapter, CPU investigation pending wire-up", nil, nil
}

// InvestigateMemory 占位：返 noop finding + nil err。
func (NoopHostAdapter) InvestigateMemory(ctx context.Context, target string) (string, []byte, error) {
	return "noop host adapter, memory investigation pending wire-up", nil, nil
}

// InvestigateDisk 占位：返 noop finding + nil err。
func (NoopHostAdapter) InvestigateDisk(ctx context.Context, target string) (string, []byte, error) {
	return "noop host adapter, disk investigation pending wire-up", nil, nil
}

// ResourceType 资源类型枚举（与 alert model.RuleScope* 对齐）。
//
// Day 2 实现 postgresql / redis / kubernetes / host；其他类型 Next() 返 skip。
type ResourceType string

const (
	ResourceTypePostgreSQL ResourceType = "postgresql"
	ResourceTypeRedis      ResourceType = "redis"
	ResourceTypeKubernetes ResourceType = "kubernetes"

	// ResourceTypePG / ResourceTypeHost / ResourceTypeK8s 是 alert model 里的别名，
	// source linker 同时支持两套命名（设计兼容）。
	ResourceTypePG   ResourceType = "pg"
	ResourceTypeHost ResourceType = "host"
	ResourceTypeK8s  ResourceType = "k8s"
)

// normalizeResourceType 把 alert model 命名归一到 source linker 命名。
func normalizeResourceType(rt string) ResourceType {
	switch ResourceType(rt) {
	case ResourceTypePG:
		return ResourceTypePostgreSQL
	case ResourceTypeK8s:
		return ResourceTypeKubernetes
	default:
		return ResourceType(rt)
	}
}

// SkillLink 单条 DIAGNOSIS_SKILL_MAP 路径。
//
// 与 v1 monitoring-source-linker 的 SkillLink 接口对齐：
//
//	{
//	  next_phase: "link_runtime_to_commit",
//	  tool:       "linkRuntimeToCommit",
//	  args:       {"resourceScope": "postgresql", "selector": {...}},
//	}
type SkillLink struct {
	// NextPhase 提示 orchestrator 下一步走哪个 sub-phase。
	// Day 2 固定为 "link_runtime_to_commit"。
	NextPhase string

	// Tool 第二跳要调用的工具名。Day 2 仅声明，Day 4 才实现。
	Tool string

	// Args 已 binding 的入参（resourceScope / selector 等）。
	// Day 4 linkRuntimeToCommit 工具实现时按此 schema 解析。
	Args map[string]any

	// Timeout 工具调用超时（借鉴 v1 verifier 30s 上限）。
	Timeout time.Duration

	// HarnessCaseID 关联 Harness 黄金事故 case（Day 7 跑通）。
	HarnessCaseID string
}

// DiagnosisSkillMap 12 条 DIAGNOSIS_SKILL_MAP 路径全集（设计 D.2）。
//
// 资源 → skill → SkillLink 嵌套字典，驱动"诊断 skill 第二跳"。
//
// PG 4 条 / Redis 2 条 / K8s 3 条 / Host 3 条 = 12 条（与 v1 spec REQ-AUTODIAG-001 一致）。
//
// Host 3 条：通过 SourceLinker 注入的 HostAdapter 调用（Interface stub 在文件下方），
// 真实实现由主 agent 在 wire-up 阶段接 ops-keeper opsadaptor 或自研 host_exporter；
// 本文件仅提供 NoopHostAdapter 占位，保证 12 条路径默认可达。
//
// Day 2：所有路径 NextPhase=link_runtime_to_commit / Tool=linkRuntimeToCommit，
// 实际工具调用留 Day 4 任务 4.4。
var defaultDiagnosisSkillMap = map[ResourceType]map[string]SkillLink{
	ResourceTypePostgreSQL: {
		"investigateSlowQueries": {
			NextPhase: "link_runtime_to_commit",
			Tool:      "linkRuntimeToCommit",
			Args: map[string]any{
				"resourceScope": "postgresql",
				"selector":      map[string]any{"queryId": "$root_cause_hint.query_id"},
			},
			Timeout:       30 * time.Second,
			HarnessCaseID: "pg/long-running-tx",
		},
		"investigateHighCpuUsage": {
			NextPhase: "link_runtime_to_commit",
			Tool:      "linkRuntimeToCommit",
			Args: map[string]any{
				"resourceScope": "postgresql",
			},
			Timeout:       30 * time.Second,
			HarnessCaseID: "host/cpu-spike",
		},
		"investigateLowMemory": {
			NextPhase: "link_runtime_to_commit",
			Tool:      "linkRuntimeToCommit",
			Args: map[string]any{
				"resourceScope": "postgresql",
			},
			Timeout: 30 * time.Second,
		},
		"investigateConnectionPoolIssues": {
			NextPhase: "link_runtime_to_commit",
			Tool:      "linkRuntimeToCommit",
			Args: map[string]any{
				"resourceScope": "postgresql",
				"selector":      map[string]any{"poolName": "$root_cause_hint.pool_name"},
			},
			Timeout: 30 * time.Second,
		},
	},
	ResourceTypeRedis: {
		"investigateRedisHighMemoryUsage": {
			NextPhase: "link_runtime_to_commit",
			Tool:      "linkRuntimeToCommit",
			Args: map[string]any{
				"resourceScope": "redis",
				"selector":      map[string]any{"command": "$message_content"},
			},
			Timeout:       30 * time.Second,
			HarnessCaseID: "redis/memory-burst",
		},
		"investigateRedisSlowCommands": {
			NextPhase: "link_runtime_to_commit",
			Tool:      "linkRuntimeToCommit",
			Args: map[string]any{
				"resourceScope": "redis",
				"selector":      map[string]any{"command": "$message_content"},
			},
			Timeout: 30 * time.Second,
		},
	},
	ResourceTypeKubernetes: {
		"kubernetesInvestigatePodCrash": {
			NextPhase: "link_runtime_to_commit",
			Tool:      "linkRuntimeToCommit",
			Args: map[string]any{
				"resourceScope": "kubernetes",
				"selector":      map[string]any{"labelSelector": "$incident.labels.app"},
			},
			Timeout: 30 * time.Second,
		},
		"kubernetesInvestigateOom": {
			NextPhase: "link_runtime_to_commit",
			Tool:      "linkRuntimeToCommit",
			Args: map[string]any{
				"resourceScope": "kubernetes",
			},
			Timeout: 30 * time.Second,
		},
		"kubernetesInvestigatePodPending": {
			NextPhase: "link_runtime_to_commit",
			Tool:      "linkRuntimeToCommit",
			Args: map[string]any{
				"resourceScope": "kubernetes",
			},
			Timeout: 30 * time.Second,
		},
	},
	ResourceTypeHost: {
		"host.cpu": {
			NextPhase: "link_runtime_to_commit",
			Tool:      "host.cpu.investigateCPU",
			Args: map[string]any{
				"resourceScope": "host",
				"selector":      map[string]any{"target": "$incident.labels.instance"},
			},
			Timeout:       30 * time.Second,
			HarnessCaseID: "host/cpu-spike",
		},
		"host.memory": {
			NextPhase: "link_runtime_to_commit",
			Tool:      "host.memory.investigateMemory",
			Args: map[string]any{
				"resourceScope": "host",
				"selector":      map[string]any{"target": "$incident.labels.instance"},
			},
			Timeout: 30 * time.Second,
		},
		"host.disk": {
			NextPhase: "link_runtime_to_commit",
			Tool:      "host.disk.investigateDisk",
			Args: map[string]any{
				"resourceScope": "host",
				"selector":      map[string]any{"target": "$incident.labels.instance"},
			},
			Timeout: 30 * time.Second,
		},
	},
}

// SourceLinkInput SourceLinker.Next 输入参数。
type SourceLinkInput struct {
	// ResourceType 资源类型（host / pg / redis / k8s / mq / ...）。
	ResourceType string

	// CurrentSkill investigator worker 当前定位的 skill 名。
	CurrentSkill string

	// RootCauseHint 来自 static-rules-v1 的 root_cause_category（如
	// "pg.subscription.long_running_tx"）。
	RootCauseHint string

	// MessageContent incident.summary + description 拼接。
	MessageContent string

	// TenantID 多租户隔离（设计 H.3）。
	TenantID uint64
}

// SourceLinkOutput SourceLinker.Next 输出。
type SourceLinkOutput struct {
	// NextPhaseHint 提示 orchestrator 下一步走哪个 sub-phase。
	//   - "link_runtime_to_commit"  命中 12 条
	//   - "skip"                     未命中（资源类型 / skill 未注册）
	NextPhaseHint string

	// Tool 第二跳要调用的工具 spec（参数已 binding）。
	Tool ToolSpec

	// UnmatchedReason NextPhaseHint="skip" 时填原因：
	//   - "unknown_resource_type"  资源类型未注册
	//   - "unknown_skill"          skill 未命中
	UnmatchedReason string
}

// ToolSpec 工具调用规格。
type ToolSpec struct {
	ToolName string
	Args     map[string]any
	Timeout  time.Duration
}

// SourceLinker 路由服务。
//
// Day 2：纯内存路由表（12 条）。Day 11+ 走 YAML loader + gitartifact 反查。
type SourceLinker struct {
	skillMap    map[ResourceType]map[string]SkillLink
	hostAdapter HostAdapter
}

// NewSourceLinker 构造默认 source linker（Host 3 条绑定 NoopHostAdapter）。
//
// 主 agent 在 wire-up 阶段通过 WithHostAdapter 注入真实 opsadaptor。
func NewSourceLinker() *SourceLinker {
	return &SourceLinker{
		skillMap:    defaultDiagnosisSkillMap,
		hostAdapter: NoopHostAdapter{},
	}
}

// WithHostAdapter 注入真实 host adapter（默认 NoopHostAdapter）。
func (s *SourceLinker) WithHostAdapter(a HostAdapter) *SourceLinker {
	if a == nil {
		return s
	}
	s.hostAdapter = a
	return s
}

// HostAdapter 返当前注入的 host adapter（观察者路径 / 测试用）。
func (s *SourceLinker) HostAdapter() HostAdapter {
	if s == nil || s.hostAdapter == nil {
		return NoopHostAdapter{}
	}
	return s.hostAdapter
}

// WithSkillMap 注入自定义路由表（测试 / Day 11+ 外部加载）。
//
// 注意：保留当前 hostAdapter 绑定（不在本方法内重置），避免双注入顺序混乱。
func (s *SourceLinker) WithSkillMap(m map[ResourceType]map[string]SkillLink) *SourceLinker {
	s.skillMap = m
	return s
}

// Lookup 按 (resourceType, currentSkill) 查 SkillLink。
//
// 返回 nil 表示未命中（资源类型或 skill 未注册）。
func (s *SourceLinker) Lookup(resourceType, currentSkill string) (*SkillLink, error) {
	if s == nil || s.skillMap == nil {
		return nil, fmt.Errorf("source linker not initialized")
	}
	rt := normalizeResourceType(resourceType)
	if rt == "" {
		return nil, fmt.Errorf("resource type required")
	}
	if currentSkill == "" {
		return nil, fmt.Errorf("current skill required")
	}
	skills, ok := s.skillMap[rt]
	if !ok {
		return nil, fmt.Errorf("no skill map for resource_type=%s", resourceType)
	}
	link, ok := skills[currentSkill]
	if !ok {
		return nil, fmt.Errorf("no skill map for %s/%s", resourceType, currentSkill)
	}
	cp := link
	return &cp, nil
}

// Next 设计 D.2 接口契约：返 (SourceLinkOutput, error)。
//
// 命中：NextPhaseHint="link_runtime_to_commit" + Tool 已 binding。
// 未命中：NextPhaseHint="skip" + UnmatchedReason 填原因。
//
// SourceLinker 是只读路由表，错误仅在自身未初始化时返回；查表未命中通过
// NextPhaseHint="skip" 表达，不返 error（便于 orchestrator 主流程无分支处理）。
func (s *SourceLinker) Next(ctx context.Context, in SourceLinkInput) (SourceLinkOutput, error) {
	if err := ctx.Err(); err != nil {
		return SourceLinkOutput{}, err
	}
	if in.ResourceType == "" || in.CurrentSkill == "" {
		return SourceLinkOutput{
			NextPhaseHint:   "skip",
			UnmatchedReason: "unknown_resource_type",
		}, nil
	}
	link, err := s.Lookup(in.ResourceType, in.CurrentSkill)
	if err != nil {
		// 区分"资源类型未注册"和"skill 未命中"。
		rt := normalizeResourceType(in.ResourceType)
		if _, ok := s.skillMap[rt]; !ok {
			return SourceLinkOutput{
				NextPhaseHint:   "skip",
				UnmatchedReason: "unknown_resource_type",
			}, nil
		}
		return SourceLinkOutput{
			NextPhaseHint:   "skip",
			UnmatchedReason: "unknown_skill",
		}, nil
	}
	return SourceLinkOutput{
		NextPhaseHint: link.NextPhase,
		Tool: ToolSpec{
			ToolName: link.Tool,
			Args:     link.Args,
			Timeout:  link.Timeout,
		},
	}, nil
}

// ResolveFromStaticHit 静态规则命中后调源链接。
//
// Day 2 仅做映射，不实际调用 linkRuntimeToCommit（Day 4 实现）。
//
// 当 static rule ID 不在 12 条路径里（host/cpu-spike 已映射到 host.cpu，其余
// 未知 ID 走 default 分支返 error），本方法仍能正确路由。
func (s *SourceLinker) ResolveFromStaticHit(ctx context.Context, hit *StaticRuleHit) (*SkillLink, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if hit == nil {
		return nil, fmt.Errorf("static rule hit required")
	}
	switch hit.Rule.ID {
	case "pg/long-running-tx":
		return s.Lookup(string(ResourceTypePostgreSQL), "investigateSlowQueries")
	case "redis/memory-burst":
		return s.Lookup(string(ResourceTypeRedis), "investigateRedisHighMemoryUsage")
	case "host/cpu-spike":
		// 路径 A 集成 Day 3：host 资源已纳入 DIAGNOSIS_SKILL_MAP 共 12 条；此处
		// 路由回 host.cpu skill 走 HostAdapter.InvestigateCPU 抓取主机 CPU 证据。
		// 真实 host adapter 由主 agent wire-up 注入；当前 NewSourceLinker 默认 Noop。
		return s.Lookup(string(ResourceTypeHost), "host.cpu")
	default:
		return nil, fmt.Errorf("no skill map for static rule %s", hit.Rule.ID)
	}
}

// ListSupportedResourceTypes 返回当前路由表覆盖的资源类型清单。
//
// 用于单测硬编码清单（设计 G.1：TestAllExpectedMappingsExist）。
func (s *SourceLinker) ListSupportedResourceTypes() []ResourceType {
	if s == nil || s.skillMap == nil {
		return nil
	}
	out := make([]ResourceType, 0, len(s.skillMap))
	for rt := range s.skillMap {
		out = append(out, rt)
	}
	return out
}

// CountMappings 返回当前路由表的总路径数。
//
// Day 2 期望 12（PG 4 + Redis 2 + K8s 3 + Host 3）。
func (s *SourceLinker) CountMappings() int {
	if s == nil || s.skillMap == nil {
		return 0
	}
	total := 0
	for _, skills := range s.skillMap {
		total += len(skills)
	}
	return total
}
