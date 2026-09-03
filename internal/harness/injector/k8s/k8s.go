// Package k8s 是 Kubernetes 故障注入器（路径 A 阶段 2 任务 2.6）。
//
// 4 类 K8S 故障（覆盖黄金 case）：
//   - k8s.set_bad_image
//   - k8s.cordon_node
//   - k8s.inject_memory_pressure
//   - k8s.fill_pv
//
// 当前骨架：接口契约 + 4 个注入方法清单 + 错误处理。
// 完整实现在 Task 2.6 followup PR（client-go + Deployment patch / Memory Limit / PVC fill）。
package k8s

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/harness/injector"
)

// Injector 是 Kubernetes 故障注入器。
type Injector struct {
	mu     sync.Mutex
	active map[string]injector.InjectResult
	// connStr string  // 完整实现：admin DSN
	// adminDB *client // 完整实现：admin 连接
}

// New 创建 k8s Injector。
func New() *Injector {
	return &Injector{active: make(map[string]injector.InjectResult)}
}

// Type 返回 type prefix。
func (i *Injector) Type() string { return "k8s." }

// IsAvailable 检查注入器在当前环境是否可用（骨架：返回 true）。
func (i *Injector) IsAvailable(ctx context.Context) bool {
	// 骨架：直接返回 true。完整实现：检查依赖二进制 + 网络连通性 + 权限
	return true
}

// Inject 执行故障注入（骨架：占位返回）。
func (i *Injector) Inject(ctx context.Context, spec injector.InjectSpec) (*injector.InjectResult, error) {
	switch spec.Type {
	case "k8s.set_bad_image", "k8s.cordon_node", "k8s.inject_memory_pressure", "k8s.fill_pv":
		// 已知类型，继续
	default:
		return nil, fmt.Errorf("%w: %s", injector.ErrUnsupportedType, spec.Type)
	}
	id := spec.InjectID
	if id == "" {
		id = fmt.Sprintf("k8s-inj-%d", len(i.active)+1)
	}
	res := injector.InjectResult{
		InjectID:  id,
		Type:      spec.Type,
		StartedAt: time.Now(),
		Metadata:  map[string]string{"skeleton": "true", "duration": spec.Duration.String()},
	}
	i.mu.Lock()
	i.active[id] = res
	i.mu.Unlock()
	return &res, nil
}

// Cleanup 清理注入（骨架：从 active map 移除，幂等）。
func (i *Injector) Cleanup(ctx context.Context, injectID string) error {
	if injectID == "" {
		return injector.ErrInjectionNotFound
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.active[injectID]; !ok {
		return injector.ErrInjectionNotFound
	}
	delete(i.active, injectID)
	return nil
}

var _ injector.Injector = (*Injector)(nil)
