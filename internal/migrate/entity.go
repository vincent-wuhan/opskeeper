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
}

// entityRegistry 全局实体元信息注册表。
var entityRegistry = map[EntityType]EntityMeta{
	EntityUsers: {
		Type:   EntityUsers,
		Source: "users",
		Target: "users",
		FieldMap: map[string]string{
			"id":         "id",
			"email":      "email",
			"name":       "name",
			"created_at": "created_at",
		},
		DependsOn: nil,
	},
	EntityProjects: {
		Type:   EntityProjects,
		Source: "projects",
		Target: "tenants",
		FieldMap: map[string]string{
			"id":         "id",
			"name":       "name",
			"owner_id":   "owner_id",
			"created_at": "created_at",
		},
		DependsOn: []EntityType{EntityUsers},
	},
	EntityPGConnections: {
		Type:   EntityPGConnections,
		Source: "pg_connections",
		Target: "middleware_resources",
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
		FieldMap: map[string]string{
			"id":         "id",
			"project_id": "tenant_id",
			"name":       "name",
			"expr":       "expression",
			"severity":   "severity",
			"for":        "for_duration",
		},
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
