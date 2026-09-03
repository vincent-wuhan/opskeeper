// Package heuristic 实现自动打标启发式规则。
//
// 路径 A P1-3 阶段 1 任务 1.3 — 自动打标引擎。
//
// 置信度阈值：
//   - ≥ 0.85：自动打标 + 写 audit
//   - 0.70-0.85：自动打标 + 通知 admin 复核
//   - < 0.70：仅标记 pending_label，不生效
package heuristic

import (
	"context"
	"strings"

	"github.com/vincent-wuhan/opskeeper/internal/dataguard"
)

// ResourceType 资源类型枚举。
type ResourceType string

const (
	ResourcePostgres ResourceType = "pg"
	ResourceRedis    ResourceType = "redis"
	ResourceKafka    ResourceType = "kafka"
	ResourceRabbitMQ ResourceType = "rabbitmq"
	ResourceK8s      ResourceType = "k8s"
	ResourceGit      ResourceType = "git"
)

// Resource 资源描述（用于启发式判定）。
type Resource struct {
	Type   ResourceType
	ID     string
	Parent string            // 父资源 ID（用于继承）
	Name   string            // 资源名（PG table / Redis key / K8s namespace 等）
	Extra  map[string]string // 扩展字段（列名、镜像 tag 等）
}

// Match 启发式匹配结果。
type Match struct {
	Sensitivity dataguard.Sensitivity
	Confidence  float64
	Reason      string
}

// Engine 启发式引擎接口。
type Engine interface {
	Match(ctx context.Context, res Resource) (Match, bool)
}

// CompositeEngine 组合多种启发式引擎（PG + Redis + K8s + Git）。
type CompositeEngine struct {
	pg    Engine
	redis Engine
	k8s   Engine
	git   Engine
}

// NewCompositeEngine 创建组合引擎。
func NewCompositeEngine() *CompositeEngine {
	return &CompositeEngine{
		pg:    &pgEngine{},
		redis: &redisEngine{},
		k8s:   &k8sEngine{},
		git:   &gitEngine{},
	}
}

// Match 依次调用各类型引擎，返回第一个匹配。
func (c *CompositeEngine) Match(ctx context.Context, res Resource) (Match, bool) {
	switch res.Type {
	case ResourcePostgres:
		return c.pg.Match(ctx, res)
	case ResourceRedis:
		return c.redis.Match(ctx, res)
	case ResourceK8s:
		return c.k8s.Match(ctx, res)
	case ResourceGit:
		return c.git.Match(ctx, res)
	}
	return Match{}, false
}

// pgEngine PostgreSQL 资源启发式。
type pgEngine struct{}

// Match PG 启发式规则：
//   - table 名匹配 *_pii / *_personal → Confidential (0.85)
//   - column 名匹配 id_card / credit_card / ssn → Restricted (0.95)
func (e *pgEngine) Match(ctx context.Context, res Resource) (Match, bool) {
	name := strings.ToLower(res.Name)

	// 列名匹配（更敏感）
	for col, sens := range map[string]dataguard.Sensitivity{
		"id_card":     dataguard.Restricted,
		"credit_card": dataguard.Restricted,
		"ssn":         dataguard.Restricted,
		"passport":    dataguard.Restricted,
		"tax_id":      dataguard.Restricted,
	} {
		if colName, ok := res.Extra["column"]; ok && strings.EqualFold(colName, col) {
			return Match{
				Sensitivity: sens,
				Confidence:  0.95,
				Reason:      "PG column matches sensitive pattern: " + col,
			}, true
		}
	}

	// table 名匹配
	if strings.HasSuffix(name, "_pii") || strings.HasSuffix(name, "_personal") {
		return Match{
			Sensitivity: dataguard.Confidential,
			Confidence:  0.85,
			Reason:      "PG table name matches PII pattern",
		}, true
	}

	if strings.Contains(name, "payment") || strings.Contains(name, "billing") {
		return Match{
			Sensitivity: dataguard.Confidential,
			Confidence:  0.80,
			Reason:      "PG table name contains payment/billing",
		}, true
	}

	return Match{}, false
}

// redisEngine Redis 资源启发式。
type redisEngine struct{}

// Match Redis 启发式规则：
//   - key 匹配 payment:* / session:* / user:* → Confidential (0.80)
func (e *redisEngine) Match(ctx context.Context, res Resource) (Match, bool) {
	key := strings.ToLower(res.Name)

	for _, prefix := range []string{"payment:", "session:", "user:", "auth:", "token:"} {
		if strings.HasPrefix(key, prefix) {
			return Match{
				Sensitivity: dataguard.Confidential,
				Confidence:  0.80,
				Reason:      "Redis key prefix matches sensitive pattern: " + prefix,
			}, true
		}
	}

	return Match{}, false
}

// k8sEngine Kubernetes 资源启发式。
type k8sEngine struct{}

// Match K8s 启发式规则：
//   - Secret 资源 → TopSecret (1.00)
//   - namespace kube-system / istio-system → Restricted (0.90)
func (e *k8sEngine) Match(ctx context.Context, res Resource) (Match, bool) {
	kind := strings.ToLower(res.Extra["kind"])

	if kind == "secret" {
		return Match{
			Sensitivity: dataguard.TopSecret,
			Confidence:  1.00,
			Reason:      "K8s Secret resource",
		}, true
	}

	ns := strings.ToLower(res.Name)
	if ns == "kube-system" || ns == "istio-system" {
		return Match{
			Sensitivity: dataguard.Restricted,
			Confidence:  0.90,
			Reason:      "K8s system namespace",
		}, true
	}

	return Match{}, false
}

// gitEngine Git 资源启发式。
type gitEngine struct{}

// Match Git 启发式规则：
//   - repo name 包含 payment- → Confidential (0.75)
func (e *gitEngine) Match(ctx context.Context, res Resource) (Match, bool) {
	name := strings.ToLower(res.Name)

	if strings.Contains(name, "payment") {
		return Match{
			Sensitivity: dataguard.Confidential,
			Confidence:  0.75,
			Reason:      "Git repo name contains payment",
		}, true
	}

	return Match{}, false
}
