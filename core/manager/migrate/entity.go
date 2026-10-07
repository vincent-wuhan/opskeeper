// Package migrate 提供 ops-keeper → opskeeper 数据迁移核心能力。
//
// 支持 9 类实体的导入导出 + 幂等 + 回滚 + 限速 + 多租户隔离。
// 设计依据：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.5
// 关联 spec：docs/integration-guide.md §四
package migrate

import (
	"fmt"
	"sort"
	"strings"
)

// EntityType 标识一类可迁移实体。9 类与 ops-keeper 1:1 对应。
type EntityType string

const (
	EntityUsers           EntityType = "users"
	EntityProjects        EntityType = "projects"
	EntityPGConnections   EntityType = "pg_connections"
	EntityRedisConns      EntityType = "redis_connections"
	EntityMQConnections   EntityType = "mq_connections"
	EntityK8sClusters     EntityType = "k8s_clusters"
	EntityGitRepos        EntityType = "git_repos"
	EntityInspectionSched EntityType = "inspection_schedules"
	EntityAlertRules      EntityType = "alert_rules"
)

// AllEntityTypes 返回全部支持的实体类型，按字母序排列（确定性）。
func AllEntityTypes() []EntityType {
	all := []EntityType{
		EntityUsers,
		EntityProjects,
		EntityPGConnections,
		EntityRedisConns,
		EntityMQConnections,
		EntityK8sClusters,
		EntityGitRepos,
		EntityInspectionSched,
		EntityAlertRules,
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	return all
}

// ParseEntityType 把字符串解析为 EntityType，未识别返回错误。
func ParseEntityType(s string) (EntityType, error) {
	for _, t := range AllEntityTypes() {
		if string(t) == s {
			return t, nil
		}
	}
	return "", fmt.Errorf("未知实体类型: %q（支持: %s）", s, strings.Join(AllEntityTypeStrings(), ", "))
}

// AllEntityTypeStrings 返回全部实体类型字符串列表。
func AllEntityTypeStrings() []string {
	all := AllEntityTypes()
	out := make([]string, len(all))
	for i, t := range all {
		out[i] = string(t)
	}
	return out
}

// EntityMeta 描述一类实体的元信息：源/目标、字段映射、依赖、依赖顺序。
type EntityMeta struct {
	Type       EntityType
	Source     string                              // ops-keeper 表名 / API endpoint
	Target     string                              // opskeeper 表名 / API endpoint
	FieldMap   map[string]string                   // ops-keeper field → opskeeper field
	DependsOn  []EntityType                        // 依赖的前置实体（先迁）
	Encryption bool                                // 目标是否含加密凭据
	VerifyFn   func(src, dst map[string]any) error // 导入后校验函数（可选）

	// TargetRoute 是这一类实体在 opskeeper 上真实存在的写入端点，形如
	// "/v1/users"。客户端把它挂在 "/api" 组下再拼上 baseURL。
	//
	// 它为空表示目标端在这个代码库里不存在，导入必须拒绝，而不是发一个
	// 必然 404 的请求再把 404 记成一行 "失败"。决策 291 之前这里只有一个
	// 自由文本 Target，于是 "tenants" / "schedules" /
	// "middleware_resources" 被 import、verify、rollback 三个命令一律
	// 当成端点拼进 URL，而它们在 manager 的路由表里没有对应物。
	TargetRoute string

	// TargetNote 记录这条实体迁过去时**丢掉了什么**。TargetRoute 存在只
	// 说明端点在，丢字段是另一件事，不写下来的话迁移报告会显示"全部命中"。
	TargetNote string

	// IdempotencyNote 说明幂等查询为什么在真实 opskeeper 上不成立。
	//
	// import 靠 GET {TargetRoute}/by-source-id/{id} 判断这条迁过了没有，
	// 而这条读路由 manager 没有注册，于是重复导入会重复创建。空 = 路由在。
	IdempotencyNote string

	// TargetMissing 说明 TargetRoute 为什么为空，写给人看：迁进来的连接
	// 配置存哪张表、巡检计划与报告计划是不是一回事，这是一次产品决定，
	// 不是重构可以替谁做的选择。
	TargetMissing string
}

// IsImportable 报告这一类实体当前能否真的导入 opskeeper。
func (m *EntityMeta) IsImportable() bool { return m != nil && m.TargetRoute != "" }

// entityRegistry 全局实体元信息注册表。
var entityRegistry = map[EntityType]EntityMeta{
	EntityUsers: {
		Type:        EntityUsers,
		Source:      "users",
		Target:      "users",
		TargetRoute: "/v1/users",
		IdempotencyNote: "manager 没有注册这条实体的 by-source-id 读路由，" +
			"所以幂等判断在真实 opskeeper 上永远返回「不存在」，重复导入会重复创建。",
		FieldMap: map[string]string{
			// POST /v1/users 的请求体是 createUserReq（iam/server/orgs.go），
			// 它收 display_name 而不是 name。id 与 created_at 由服务端分配，
			// 不在请求体里，所以不映射——映过去也只会被 json 解码丢掉，
			// 却让闸门以为对得上。
			"email": "email",
			"name":  "display_name",
		},
		DependsOn: nil,
	},
	EntityProjects: {
		Type:        EntityProjects,
		Source:      "projects",
		Target:      "orgs",
		TargetRoute: "/v1/orgs",
		IdempotencyNote: "manager 没有注册这条实体的 by-source-id 读路由，" +
			"所以幂等判断在真实 opskeeper 上永远返回「不存在」，重复导入会重复创建。",
		FieldMap: map[string]string{
			// POST /v1/orgs 的请求体是 createOrgReq（name / description /
			// parent_id）。owner_id 没有位置：ops-keeper 的项目负责人对应的是
			// 组织成员关系，走另一个端点 /v1/orgs/{id}/members，导入这一步
			// 不写它——见 TargetNote。
			"name": "name",
		},
		DependsOn: []EntityType{EntityUsers},
	},
	EntityPGConnections: {
		Type:   EntityPGConnections,
		Source: "pg_connections",
		Target: "middleware_resources",
		TargetMissing: "manager 没有承载连接配置的写入端点——中间件适配器读的是 DSN " +
			"环境变量而不是数据库（决策 287 删掉了从未接线的 middleware_resources 表）。",
		FieldMap: map[string]string{
			"id":         "id",
			"project_id": "tenant_id",
			"name":       "name",
			"host":       "host",
			"port":       "port",
			"database":   "database",
			"username":   "username",
			"password":   "password_sealed", // 加密重存
			"ssl_mode":   "ssl_mode",
		},
		DependsOn:  []EntityType{EntityProjects},
		Encryption: true,
	},
	EntityRedisConns: {
		Type:   EntityRedisConns,
		Source: "redis_connections",
		Target: "middleware_resources",
		TargetMissing: "manager 没有承载连接配置的写入端点——中间件适配器读的是 DSN " +
			"环境变量而不是数据库（决策 287 删掉了从未接线的 middleware_resources 表）。",
		FieldMap: map[string]string{
			"id":         "id",
			"project_id": "tenant_id",
			"name":       "name",
			"host":       "host",
			"port":       "port",
			"password":   "password_sealed",
			"cluster":    "cluster_mode",
		},
		DependsOn:  []EntityType{EntityProjects},
		Encryption: true,
	},
	EntityMQConnections: {
		Type:   EntityMQConnections,
		Source: "mq_connections",
		Target: "middleware_resources",
		TargetMissing: "manager 没有承载连接配置的写入端点——中间件适配器读的是 DSN " +
			"环境变量而不是数据库（决策 287 删掉了从未接线的 middleware_resources 表）。",
		FieldMap: map[string]string{
			"id":         "id",
			"project_id": "tenant_id",
			"name":       "name",
			"type":       "mq_type", // rabbitmq / kafka
			"host":       "host",
			"port":       "port",
			"username":   "username",
			"password":   "password_sealed",
			"vhost":      "vhost",
		},
		DependsOn:  []EntityType{EntityProjects},
		Encryption: true,
	},
	EntityK8sClusters: {
		Type:   EntityK8sClusters,
		Source: "k8s_clusters",
		Target: "middleware_resources",
		TargetMissing: "manager 没有承载连接配置的写入端点——中间件适配器读的是 DSN " +
			"环境变量而不是数据库（决策 287 删掉了从未接线的 middleware_resources 表）。",
		FieldMap: map[string]string{
			"id":         "id",
			"project_id": "tenant_id",
			"name":       "name",
			"kubeconfig": "kubeconfig_sealed",
			"context":    "context",
		},
		DependsOn:  []EntityType{EntityProjects},
		Encryption: true,
	},
	EntityGitRepos: {
		Type:   EntityGitRepos,
		Source: "git_repos",
		Target: "middleware_resources",
		TargetMissing: "manager 没有承载连接配置的写入端点——中间件适配器读的是 DSN " +
			"环境变量而不是数据库（决策 287 删掉了从未接线的 middleware_resources 表）。",
		FieldMap: map[string]string{
			"id":         "id",
			"project_id": "tenant_id",
			"name":       "name",
			"url":        "url",
			"token":      "token_sealed",
		},
		DependsOn:  []EntityType{EntityProjects},
		Encryption: true,
	},
	EntityInspectionSched: {
		Type:   EntityInspectionSched,
		Source: "inspection_schedules",
		Target: "schedules",
		TargetMissing: "manager 唯一的计划类端点是 /v1/report-schedules（报告计划），" +
			"与巡检计划不是同一件事；落哪张表是一次产品决定。",
		FieldMap: map[string]string{
			"id":         "id",
			"project_id": "tenant_id",
			"name":       "name",
			"cron":       "cron_expression",
			"target_id":  "middleware_resource_id",
			"enabled":    "enabled",
		},
		DependsOn: []EntityType{EntityProjects, EntityPGConnections, EntityRedisConns},
	},
	EntityAlertRules: {
		Type:   EntityAlertRules,
		Source: "alert_rules",
		Target: "alert_rules",
		TargetMissing: "POST /v1/alert-rules 的请求体是 ruleReq" +
			"（server/alert/http.go），它要 rule_key / kind / scope_type /" +
			" join_mode / conditions 这几组字段，而 ops-keeper 的 expr / for /" +
			" severity 到它们的翻译是一次规则语义决定（一条 PromQL 表达式拆成" +
			"哪几个 condition、scope 取什么），不是字段改名。照旧映射写出去" +
			"只会得到一条 400，而 400 读起来像源数据不合法。",
		DependsOn: []EntityType{EntityProjects},
	},
}

// GetEntityMeta 返回实体元信息；未注册返回 nil。
func GetEntityMeta(t EntityType) *EntityMeta {
	if m, ok := entityRegistry[t]; ok {
		return &m
	}
	return nil
}

// MigrationOrder 按依赖顺序返回实体列表（拓扑排序）。
// 无依赖 → 先迁；依赖项必在前。
func MigrationOrder() []EntityType {
	visited := make(map[EntityType]bool)
	var order []EntityType

	var visit func(t EntityType) error
	visit = func(t EntityType) error {
		if visited[t] {
			return nil
		}
		visited[t] = true
		meta := GetEntityMeta(t)
		if meta == nil {
			return fmt.Errorf("未知实体: %s", t)
		}
		for _, dep := range meta.DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		order = append(order, t)
		return nil
	}

	for _, t := range AllEntityTypes() {
		if err := visit(t); err != nil {
			// 不应发生；返回部分排序便于诊断
			return order
		}
	}
	return order
}
