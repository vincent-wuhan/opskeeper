package middleware

import (
	"testing"
)

func TestTableNames(t *testing.T) {
	// 验证表名符合设计 §2.1.3 + §2.3.2
	if got, want := (MiddlewareResource{}).TableName(), "middleware_resources"; got != want {
		t.Errorf("MiddlewareResource.TableName() = %q, want %q", got, want)
	}
	if got, want := (MiddlewareConnSpec{}).TableName(), "middleware_resource_conn_specs"; got != want {
		t.Errorf("MiddlewareConnSpec.TableName() = %q, want %q", got, want)
	}
	if got, want := (MiddlewareResourceHealth{}).TableName(), "middleware_resource_health"; got != want {
		t.Errorf("MiddlewareResourceHealth.TableName() = %q, want %q", got, want)
	}
}

func TestResourceTypeConstants(t *testing.T) {
	// 验证 6 类资源类型常量
	cases := []struct {
		got, want string
	}{
		{ResourceTypePostgres, "postgres"},
		{ResourceTypeRedis, "redis"},
		{ResourceTypeRabbitMQ, "rabbitmq"},
		{ResourceTypeKafka, "kafka"},
		{ResourceTypeK8sCluster, "k8s_cluster"},
		{ResourceTypeGitRepository, "git_repository"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("ResourceType = %q, want %q", c.got, c.want)
		}
	}
}

func TestResourceStatusConstants(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{ResourceStatusHealthy, "healthy"},
		{ResourceStatusDegraded, "degraded"},
		{ResourceStatusDown, "down"},
		{ResourceStatusUntested, "untested"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("ResourceStatus = %q, want %q", c.got, c.want)
		}
	}
}
