// admission.go 是 pig-runtime-adapter 任务 2.3 的三方工具对账组件：
// declared（发布清单 CapabilityManifest）/ observed（运行期
// CapabilityObservation）/ allowlisted（角色准入表）三方对齐后才把一个
// 工具暴露给角色，其余全部记 drift。
//
// 规则来源 spec「Runtime capability is Manager-authoritative」与
// 2026-11-21 评论区默认裁决（任务评论区增补注记第 4 条）：
//   - 三方交集才暴露：不在 manifest → 永不暴露、记 drift；在 manifest
//     不在角色 allowlist → 不暴露、记 drift；observed 缺席 declared →
//     照常暴露但记 drift（声明优先于观察，观察只是证据）。
//   - 观察侧漂移裁决健康与后续准入：未声明 write/destructive →
//     Rejected（运行时记 degraded，后续任务准入 fail closed，不当场杀
//     已启动的执行——未声明工具本来就不暴露，观察是证据不是授权）；
//     未声明 read_only → Degraded。计数阈值只作用于 read_only 类且只
//     能收紧不能放行——副作用类永远见一即拒，没有计数缓冲带。
//   - 角色侧漂移（not_allowlisted / unobserved）只记证据，不影响健康
//     裁决：注册时尚无任何观察是常态，把每个未观察工具记成降级是噪音。
//   - 重复 StableToolID：清单侧由 CapabilityManifest.Validate 拒绝；观察
//     流对同一稳定身份给出两份不一致观察 → 整体拒绝，与清单同一立场。
//
// allowlist 的持久化形态在本切片刻意不建表：当前没有任何生产写入方
// （无 API、无 UI、装配根的清单即声明侧），建一张无人写的表只会在
// deadcode 棘轮上欠新债。对账的 allowlist 入参是纯值 RoleAllowlist，
// 经 LifecycleSink 的 AdmissionConfig 进入生产路径（写入注册行的
// CapabilitiesJSON 暴露集）；3.x 网关落地时再决定表族形态，落点在
// ReconcileCapabilities 的入参签名，不返工。
package pigruntime

import (
	"errors"
	"sort"
	"strconv"

	domain "github.com/vincent-wuhan/opskeeper/core/domain"
)

// RoleAllowlist 是某角色对一组稳定工具身份的准入表。它只承载 Manager
// 侧策略事实，不承载 PiG 侧任何声明或观察。
type RoleAllowlist struct {
	Role  string
	Tools []string
}

// Validate 要求角色非空、准入集非空且无重复稳定身份。
func (a RoleAllowlist) Validate() error {
	if a.Role == "" || len(a.Tools) == 0 {
		return ErrInvalidContract
	}
	seen := make(map[string]struct{}, len(a.Tools))
	for _, id := range a.Tools {
		if id == "" {
			return ErrInvalidContract
		}
		if _, exists := seen[id]; exists {
			return ErrInvalidContract
		}
		seen[id] = struct{}{}
	}
	return nil
}

// allows 判断 stableToolID 是否在该角色准入表内。
func (a RoleAllowlist) allows(stableToolID string) bool {
	for _, id := range a.Tools {
		if id == stableToolID {
			return true
		}
	}
	return false
}

// DriftKind 是对账漂移的闭集。
type DriftKind string

const (
	// DriftUndeclared：观察到的工具不在发布清单——永不暴露；副作用类
	// 直接 Rejected。
	DriftUndeclared DriftKind = "undeclared"
	// DriftNotAllowlisted：声明存在但角色准入表未放行——不暴露，只记
	// 证据，不影响健康。
	DriftNotAllowlisted DriftKind = "not_allowlisted"
	// DriftUnobserved：声明在清单但运行期未观察到——照常暴露（声明
	// 优先于观察），只记证据。
	DriftUnobserved DriftKind = "unobserved"
	// DriftClassMismatch：观察类别与声明不一致——按观察侧是否升级进
	// 副作用类裁决。
	DriftClassMismatch DriftKind = "class_mismatch"
)

// Valid 判断漂移类型是否在闭集内。
func (d DriftKind) Valid() bool {
	switch d {
	case DriftUndeclared, DriftNotAllowlisted, DriftUnobserved, DriftClassMismatch:
		return true
	default:
		return false
	}
}

// CapabilityDrift 是一条对账漂移记录，进入健康理由与审计证据。
type CapabilityDrift struct {
	StableToolID string
	Kind         DriftKind
	Reason       string
}

// DriftVerdict 是观察侧漂移对健康与后续准入的裁决。角色侧漂移不参与
// 裁决，只进 Drift 证据列表。
type DriftVerdict string

const (
	// VerdictClean：无观察侧漂移。
	VerdictClean DriftVerdict = "clean"
	// VerdictDegraded：存在未声明 read_only（或变轻方向的类别漂移）——
	// 运行时记 degraded，暴露面不受影响。
	VerdictDegraded DriftVerdict = "degraded"
	// VerdictRejected：存在未声明副作用类或身份不可信——运行时记
	// degraded 且暴露集清空，后续任务准入 fail closed。
	VerdictRejected DriftVerdict = "rejected"
)

// AdmissionPolicy 是可配置的漂移裁决阈值。零值即 2026-11-21 默认裁决。
type AdmissionPolicy struct {
	// RejectAtUndeclaredReadOnly 在未声明 read_only 观察数达到该值时把
	// 裁决升级为 Rejected。零值（默认）永不升级——默认裁决里 read_only
	// 观察漂移的处置是 degraded；该阈值是运维想要更严的绊线，永远不是
	// 副作用类的缓冲带：write/destructive 见一即拒，与本值无关。
	RejectAtUndeclaredReadOnly int
}

// ExposedTool 是对某角色暴露的工具。只携带稳定身份与统一类别——
// RuntimeToolID 是 PiG 侧实现细节，结构上就不出对账层。
type ExposedTool struct {
	StableToolID  string
	Name          string
	Class         domain.ToolClass
	ResourceScope string
}

// AdmissionResult 是一次对账的完整输出。
type AdmissionResult struct {
	// Exposed 是按 StableToolID 排序的暴露集；Rejected 时为空。
	Exposed []ExposedTool
	// Drift 是本次对账的全部漂移证据（含不影响健康的角色侧漂移）。
	Drift []CapabilityDrift
	// Verdict 是观察侧漂移裁决，调用方据此写健康或拒绝后续准入。
	Verdict DriftVerdict
	// HealthReason 是可直接写入 RuntimeRecord.HealthReason 的一行摘要；
	// Verdict 为 clean 时为空（角色侧漂移不进健康理由）。
	HealthReason string
}

// ErrDuplicateObservation 表示观察流对同一稳定身份给出两份不一致的
// 观察（不同 RuntimeToolID 或不同类别）——清单或观察流已不可信。
var ErrDuplicateObservation = errors.New(
	"pigruntime: conflicting observations for one stable tool id")

// ReconcileCapabilities 执行三方对账并返回某角色可见的工具集与漂移。
//
// 输入：manifest 是发布声明的全部工具（必须通过 Validate）；observed
// 是注册/运行期观察流；allowlist 是该角色的 Manager 侧准入表；policy
// 是漂移裁决阈值。输出永远只引用 StableToolID。
func ReconcileCapabilities(
	manifest CapabilityManifest,
	observed []CapabilityObservation,
	allowlist RoleAllowlist,
	policy AdmissionPolicy,
) (AdmissionResult, error) {
	if err := manifest.Validate(); err != nil {
		return AdmissionResult{}, err
	}
	if err := allowlist.Validate(); err != nil {
		return AdmissionResult{}, err
	}
	for _, observation := range observed {
		if observation.StableToolID == "" || observation.RuntimeToolID == "" ||
			!observation.Class.Valid() {
			return AdmissionResult{}, ErrInvalidContract
		}
	}

	result := admissionState{}
	observedStable, err := result.observe(manifest, observed, policy)
	if err != nil {
		return AdmissionResult{}, err
	}
	result.admit(manifest, allowlist, observedStable)
	if result.verdict == VerdictRejected {
		result.exposed = nil
	}
	sort.Slice(result.exposed, func(i, j int) bool {
		return result.exposed[i].StableToolID < result.exposed[j].StableToolID
	})
	sort.Slice(result.drift, func(i, j int) bool {
		if result.drift[i].Kind != result.drift[j].Kind {
			return result.drift[i].Kind < result.drift[j].Kind
		}
		return result.drift[i].StableToolID < result.drift[j].StableToolID
	})
	return AdmissionResult{
		Exposed:      result.exposed,
		Drift:        result.drift,
		Verdict:      result.verdict,
		HealthReason: driftHealthReason(result.verdict, result.drift),
	}, nil
}

// admissionState 是对账的中间态，拆开只为让 observe（观察侧）与 admit
// （角色侧）两遍可读。
type admissionState struct {
	exposed      []ExposedTool
	drift        []CapabilityDrift
	verdict      DriftVerdict
	undeclaredRO int
}

// observe 处理观察侧：未声明与类别漂移裁决健康，不一致的重复观察
// 整体拒绝。
func (s *admissionState) observe(
	manifest CapabilityManifest,
	observed []CapabilityObservation,
	policy AdmissionPolicy,
) (map[string]bool, error) {
	declared := make(map[string]ManifestTool, len(manifest.Tools))
	for _, tool := range manifest.Tools {
		declared[tool.StableToolID] = tool
	}

	observedByStable := make(map[string]CapabilityObservation, len(observed))
	s.verdict = VerdictClean
	for _, observation := range observed {
		prior, exists := observedByStable[observation.StableToolID]
		if !exists {
			observedByStable[observation.StableToolID] = observation
			continue
		}
		// 同一稳定身份的完全重复观察是流重放，按幂等吞掉；不一致的
		// 重复是身份级故障，整体拒绝（与清单侧 Validate 同一立场）。
		if prior.RuntimeToolID != observation.RuntimeToolID || prior.Class != observation.Class {
			return nil, ErrDuplicateObservation
		}
	}

	for stableID, observation := range observedByStable {
		declaration, isDeclared := declared[stableID]
		if !isDeclared {
			s.drift = append(s.drift, CapabilityDrift{
				StableToolID: stableID,
				Kind:         DriftUndeclared,
				Reason:       "observed tool absent from release manifest",
			})
			if observation.Class == ToolClassReadOnly {
				s.undeclaredRO++
			} else {
				// 未声明副作用类：fail closed，与任何计数阈值无关。
				s.verdict = VerdictRejected
			}
			continue
		}
		if observation.Class == declaration.Class {
			continue
		}
		s.drift = append(s.drift, CapabilityDrift{
			StableToolID: stableID,
			Kind:         DriftClassMismatch,
			Reason: "observed class " + string(observation.Class) +
				" differs from declared class " + string(declaration.Class),
		})
		// 只有「观察比声明更危险且进入副作用类」才拒绝：声明 read_only
		// 而运行期是 write，暴露面（按声明）已经说错话了。变轻方向
		// （声明 write、观察 read_only）暴露面仍按危险的声明侧治理，
		// 降级留痕即可。
		observedDomain := toDomainToolClass(observation.Class)
		if observedDomain.Rank() > toDomainToolClass(declaration.Class).Rank() &&
			observedDomain.AtLeast(domain.ClassWrite) {
			s.verdict = VerdictRejected
		} else {
			s.verdict = degrade(s.verdict)
		}
	}

	// 计数绊线只作用于 read_only，且只能在默认裁决之上收紧。
	if s.verdict != VerdictRejected &&
		policy.RejectAtUndeclaredReadOnly > 0 &&
		s.undeclaredRO >= policy.RejectAtUndeclaredReadOnly {
		s.verdict = VerdictRejected
	}
	if s.verdict == VerdictClean && s.undeclaredRO > 0 {
		s.verdict = VerdictDegraded
	}
	observedStable := make(map[string]bool, len(observedByStable))
	for stableID := range observedByStable {
		observedStable[stableID] = true
	}
	return observedStable, nil
}

// admit 处理角色侧：未放行不暴露；放行但运行期未观察到 → 照常暴露
// （声明优先于观察）并记证据。
func (s *admissionState) admit(manifest CapabilityManifest, allowlist RoleAllowlist, observedStable map[string]bool) {
	for _, declaration := range manifest.Tools {
		if !allowlist.allows(declaration.StableToolID) {
			s.drift = append(s.drift, CapabilityDrift{
				StableToolID: declaration.StableToolID,
				Kind:         DriftNotAllowlisted,
				Reason:       "declared tool not allowlisted for role " + allowlist.Role,
			})
			continue
		}
		if !observedStable[declaration.StableToolID] {
			s.drift = append(s.drift, CapabilityDrift{
				StableToolID: declaration.StableToolID,
				Kind:         DriftUnobserved,
				Reason:       "declared tool never observed at runtime",
			})
		}
		s.exposed = append(s.exposed, ExposedTool{
			StableToolID:  declaration.StableToolID,
			Name:          declaration.Name,
			Class:         toDomainToolClass(declaration.Class),
			ResourceScope: declaration.ResourceScope,
		})
	}
}

// degrade 单调推进裁决：clean → degraded，degraded/rejected 保持。
func degrade(current DriftVerdict) DriftVerdict {
	if current == VerdictClean {
		return VerdictDegraded
	}
	return current
}

// capabilityDriftReasonPrefix 是能力 drift 健康理由的固定前缀；
// LifecycleSink 的恢复逻辑以它区分「drift 理由」与「心跳类理由」。
const capabilityDriftReasonPrefix = "pig capability drift:"

// driftHealthReason 生成一行可读的健康理由；clean 裁决返回空串，角色侧
// 漂移不进健康。
func driftHealthReason(verdict DriftVerdict, drift []CapabilityDrift) string {
	if verdict == VerdictClean {
		return ""
	}
	byKind := make(map[DriftKind]int, len(drift))
	for _, item := range drift {
		byKind[item.Kind]++
	}
	reason := capabilityDriftReasonPrefix
	for _, kind := range []DriftKind{DriftUndeclared, DriftClassMismatch} {
		if count := byKind[kind]; count > 0 {
			reason += " " + string(kind) + "=" + strconv.Itoa(count)
		}
	}
	if verdict == VerdictRejected {
		reason += " (admission rejected)"
	}
	return reason
}

// toDomainToolClass 把 pigruntime 的闭集拼写（read_only）映射到 domain
// 的闭集拼写（read）。两套拼写并存是既有事实：domain 是插件 ABI 不动，
// pigruntime 沿用 PiG 生态习惯。闭集外输入一律 ClassUnknown——domain
// 的零值语义即 fail closed。
func toDomainToolClass(class ToolClass) domain.ToolClass {
	switch class {
	case ToolClassReadOnly:
		return domain.ClassRead
	case ToolClassWrite:
		return domain.ClassWrite
	case ToolClassDestructive:
		return domain.ClassDestructive
	default:
		return domain.ClassUnknown
	}
}

// fromDomainToolClass 是 toDomainToolClass 的逆向映射，3.x 网关把
// domain 侧类别收回 PiG 拼写时使用。闭集外输入返回零值——调用方对
// 零值必须按 InvalidContract 拒绝，不得把空字符串当 read_only。
func fromDomainToolClass(class domain.ToolClass) ToolClass {
	switch class {
	case domain.ClassRead:
		return ToolClassReadOnly
	case domain.ClassWrite:
		return ToolClassWrite
	case domain.ClassDestructive:
		return ToolClassDestructive
	default:
		return ""
	}
}
