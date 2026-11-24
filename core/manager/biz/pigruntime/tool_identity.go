// tool_identity.go 是 pig-runtime-adapter 任务 3.1 的身份翻译层：把
// PiG 侧 (RuntimeToolID, 进程上报的类别) 翻译为稳定 OpsKeeper 工具身份
// 与 domain 类别。
//
// 翻译只消费两份 Manager 侧事实，不做第二次映射（任务书第 1 条「网关只
// 消费不再翻译第二次」）：
//   - 发布清单 CapabilityManifest：RuntimeToolID → StableToolID 的唯一
//     映射来源；
//   - 对账暴露集 []ExposedTool（ReconcileCapabilities 的输出）：该角色
//     此刻可见的稳定身份及其 domain 类别。
//
// RuntimeToolID 到本文件为止：ResolvedTool 不携带它，网关、持久化与
// 审计行都只见 StableToolID。
package pigruntime

import (
	"errors"

	domain "github.com/vincent-wuhan/opskeeper/core/domain"
)

// ErrUndeclaredTool 表示调用的运行时工具不在发布清单，或不在对账暴露
// 集内（未放行 / 对账后被清空）。两者都是 fail closed：不分发。
var ErrUndeclaredTool = errors.New("pigruntime: tool not declared or not exposed")

// ErrDuplicateRuntimeToolID 表示发布清单内两条声明撞同一 RuntimeToolID
// ——清单的身份映射已不可信，与观察侧 ErrDuplicateObservation 同一
// 立场（身份级故障，整体拒绝）。
var ErrDuplicateRuntimeToolID = errors.New(
	"pigruntime: conflicting manifest entries for one runtime tool id")

// ErrDriftedClass 表示进程上报的类别与对账暴露面记录的类别不一致。
// 暴露面按声明侧治理，调用侧却在说另一个类别——两者分叉即拒，与 S2
// 的 class_mismatch 裁决同向。
var ErrDriftedClass = errors.New("pigruntime: reported tool class drifts from exposed class")

// ErrInvalidReportedClass 表示调用携带的类别不在 pigruntime 闭集内。
var ErrInvalidReportedClass = errors.New("pigruntime: reported tool class is not in the closed set")

// ResolvedTool 是翻译结果：只剩稳定身份与 domain 类别。
type ResolvedTool struct {
	StableToolID  string
	Name          string
	Class         domain.ToolClass
	ResourceScope string
}

// TranslateToolCall 把一次 PiG 调用翻译为稳定身份。
//
// manifest 必须通过 Validate（清单侧重复 StableToolID 已在那里拒绝）；
// exposed 是该角色的对账暴露集——为空表示该角色此刻无任何可见工具，
// 一切调用都 undeclared。
func TranslateToolCall(
	manifest CapabilityManifest,
	exposed []ExposedTool,
	runtimeToolID string,
	reportedClass ToolClass,
) (ResolvedTool, error) {
	if runtimeToolID == "" {
		return ResolvedTool{}, ErrUndeclaredTool
	}
	if !reportedClass.Valid() {
		return ResolvedTool{}, ErrInvalidReportedClass
	}
	if err := manifest.Validate(); err != nil {
		return ResolvedTool{}, err
	}

	// 清单侧：RuntimeToolID 必须唯一命中一条声明。撞名是身份级故障。
	var declaration ManifestTool
	hits := 0
	for _, tool := range manifest.Tools {
		if tool.RuntimeToolID == runtimeToolID {
			declaration = tool
			hits++
		}
	}
	switch hits {
	case 0:
		return ResolvedTool{}, ErrUndeclaredTool
	case 1:
	default:
		return ResolvedTool{}, ErrDuplicateRuntimeToolID
	}

	// 对账侧：声明的稳定身份必须在暴露集内，且类别一致。
	for _, tool := range exposed {
		if tool.StableToolID != declaration.StableToolID {
			continue
		}
		if !tool.Class.Valid() {
			// 暴露集类别异常（fromDomainToolClass 闭集外的零值语义）：
			// 不把空串当 read_only。
			return ResolvedTool{}, ErrInvalidContract
		}
		if fromDomainToolClass(tool.Class) != reportedClass {
			return ResolvedTool{}, ErrDriftedClass
		}
		return ResolvedTool{
			StableToolID:  tool.StableToolID,
			Name:          tool.Name,
			Class:         tool.Class,
			ResourceScope: tool.ResourceScope,
		}, nil
	}
	return ResolvedTool{}, ErrUndeclaredTool
}
