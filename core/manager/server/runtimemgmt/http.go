// Package runtimemgmt 暴露运行时注册表 HTTP API（runtime-management-gap §1.2
// 首切片）：注册、心跳、命令 ack 与只读 inventory。
//
// 域边界（ADR 决策 449 / 方案 A）：本包与 model/runtimemgmt、
// data/runtimemgmt 同属 runtimemgmt 域，不 import 五个被观察域
// （edge / chatruntime / agentteams / pluginhost / demo）。五类内置观察者
// 由装配根 cmd/opskeeper/runtimemgmt_wiring.go 经 ReadonlyRuntimeSource
// 端口注入；本包只见端口，不见任何被观察实体。
package runtimemgmt

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// Store 是本 handler 依赖的最窄持久化端口。仓储实现放
// data/runtimemgmt/store；这里只声明 HTTP 层需要的四个动作。
type Store interface {
	// List 返回全部注册记录，按 RuntimeID 排序，保证响应稳定。
	List() ([]model.RuntimeRecord, error)
	// Upsert 幂等注册：同 RuntimeID 更新读回字段并刷新 RegisteredAt 语义。
	Upsert(rec model.RuntimeRecord) (model.RuntimeRecord, error)
	// Get 按 RuntimeID 取单条。
	Get(runtimeID string) (model.RuntimeRecord, error)
	// Save 整行写回（心跳/ack 后的记录）。
	Save(rec model.RuntimeRecord) error
}

// ReadonlyRuntimeSource 是内置观察者端口（ADR §4.4）：每个实现 Describe
// 出一份当前实时投影，注册记录与投影在 inventory 里合并。接口刻意只有
// 一个读方法——观察者不得携带任何写方法，编译期窄接口即只读性硬保证。
type ReadonlyRuntimeSource interface {
	Describe() []RuntimeObservation
}

// RuntimeObservation 是观察者投影出的单条 runtime 快照。全部字段为值
// 拷贝：观察者不许把被观察域的实体指针放进这里（方案 B 的复发形状）。
type RuntimeObservation struct {
	RuntimeID          string              `json:"runtime_id"`
	Kind               model.RuntimeKind   `json:"kind"`
	Profile            string              `json:"profile,omitempty"`
	Owner              string              `json:"owner,omitempty"`
	DeploymentLocation string              `json:"deployment_location,omitempty"`
	ObservedVersion    string              `json:"observed_version,omitempty"`
	VersionSource      model.VersionSource `json:"version_source,omitempty"`
	ArtifactDigest     string              `json:"artifact_digest,omitempty"`
	CapabilityDigest   string              `json:"capability_digest,omitempty"`
	Capabilities       map[string]any      `json:"capabilities,omitempty"`
	Health             model.RuntimeHealth `json:"health"`
	HealthReason       string              `json:"health_reason,omitempty"`
	TotalSlots         int                 `json:"total_slots,omitempty"`
	FreeSlots          int                 `json:"free_slots,omitempty"`
	ActiveTasks        int                 `json:"active_tasks,omitempty"`
}

// Handler 挂四条路由。now 是可注入时钟，测试用固定时刻验证惰性健康推导。
type Handler struct {
	store   Store
	sources []ReadonlyRuntimeSource
	now     func() time.Time
}

// NewHandler 构造。sources 可为空（默认部署无观察者时 inventory 仍 200）。
func NewHandler(store Store, sources []ReadonlyRuntimeSource, now func() time.Time) *Handler {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Handler{store: store, sources: sources, now: now}
}

// Register 挂载到 protected（admin auth）路由组。
func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/runtimes", h.inventory)
	r.Post("/v1/runtimes", h.register)
	r.Post("/v1/runtimes/{id}/heartbeat", h.heartbeat)
	r.Post("/v1/runtimes/{id}/commands/{cid}/ack", h.ackCommand)
}

// ---- 请求/响应形状 ----

type registerRequest struct {
	RuntimeID          string            `json:"runtime_id"`
	Kind               string            `json:"kind"`
	Profile            string            `json:"profile,omitempty"`
	Owner              string            `json:"owner,omitempty"`
	DeploymentLocation string            `json:"deployment_location,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
	ObservedVersion    string            `json:"observed_version,omitempty"`
	VersionSource      string            `json:"version_source,omitempty"`
	ArtifactDigest     string            `json:"artifact_digest,omitempty"`
	CapabilityDigest   string            `json:"capability_digest,omitempty"`
	Capabilities       map[string]any    `json:"capabilities,omitempty"`
}

type heartbeatRequest struct {
	ObservedVersion  string `json:"observed_version,omitempty"`
	VersionSource    string `json:"version_source,omitempty"`
	ArtifactDigest   string `json:"artifact_digest,omitempty"`
	CapabilityDigest string `json:"capability_digest,omitempty"`
	TotalSlots       int    `json:"total_slots,omitempty"`
	FreeSlots        int    `json:"free_slots,omitempty"`
	ActiveTasks      int    `json:"active_tasks,omitempty"`
}

type ackRequest struct {
	Result  string `json:"result,omitempty"`
	Message string `json:"message,omitempty"`
}

type runtimeView struct {
	RuntimeID          string            `json:"runtime_id"`
	Kind               string            `json:"kind"`
	Profile            string            `json:"profile,omitempty"`
	Owner              string            `json:"owner,omitempty"`
	DeploymentLocation string            `json:"deployment_location,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
	ObservedVersion    string            `json:"observed_version,omitempty"`
	VersionSource      string            `json:"version_source,omitempty"`
	ArtifactDigest     string            `json:"artifact_digest,omitempty"`
	CapabilityDigest   string            `json:"capability_digest,omitempty"`
	Health             string            `json:"health"`
	HealthReason       string            `json:"health_reason,omitempty"`
	LastHeartbeatAt    *time.Time        `json:"last_heartbeat_at,omitempty"`
	TotalSlots         int               `json:"total_slots,omitempty"`
	FreeSlots          int               `json:"free_slots,omitempty"`
	ActiveTasks        int               `json:"active_tasks,omitempty"`
	CommandAckCount    int               `json:"command_ack_count,omitempty"`
	Source             string            `json:"source"` // registry | projection
}

type inventoryResponse struct {
	Total int           `json:"total"`
	Items []runtimeView `json:"items"`
}

// ---- handlers ----

// register 是 POST /v1/runtimes：注册/upsert，幂等。
func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !decodeJSON(w, r, &req) {
		auditRegister(r, "", "", auditport.StatusFailure, errs.ErrInvalid)
		return
	}
	kind := model.RuntimeKind(req.Kind)
	if req.RuntimeID == "" || !model.IsRuntimeKind(kind) {
		auditRegister(r, req.RuntimeID, req.Kind, auditport.StatusFailure, errs.ErrInvalid)
		writeError(w, errs.ErrInvalid)
		return
	}
	src := model.VersionSource(req.VersionSource)
	if src != "" && !model.IsVersionSource(src) {
		// spec「Runtime reports drift or a forbidden source」：接受存活观察，
		// 记异常——这里拒绝的是“来源闭集之外的值当作合法来源登记”，
		// 与心跳里同值导致 degraded 不同：注册表不接受一个自称合法的
		// 未知来源作为基准。空来源合法（“还无法读出”）。
		auditRegister(r, req.RuntimeID, req.Kind, auditport.StatusFailure, errs.ErrInvalid)
		writeError(w, errs.ErrInvalid)
		return
	}
	now := h.now()
	existing, err := h.store.Get(req.RuntimeID)
	if err != nil && !isNotFound(err) {
		auditRegister(r, req.RuntimeID, req.Kind, auditport.StatusFailure, err)
		writeError(w, err)
		return
	}
	rec := existing
	if isNotFound(err) {
		rec = model.RuntimeRecord{
			RuntimeID:    req.RuntimeID,
			Kind:         kind,
			RegisteredAt: now,
			Health:       model.RuntimeHealthStarting,
		}
	}
	rec.Kind = kind
	rec.Profile = req.Profile
	rec.Owner = req.Owner
	rec.DeploymentLocation = req.DeploymentLocation
	rec.LabelsJSON = marshalOrEmptyObject(req.Labels)
	if rec.ObservedVersion != req.ObservedVersion || rec.VersionSource != src {
		// 版本变化在注册路径上重置健康观察；漂移判定属于心跳路径。
		rec.ObservedVersion = req.ObservedVersion
		rec.VersionSource = src
		rec.HealthReason = ""
	}
	rec.ArtifactDigest = req.ArtifactDigest
	rec.CapabilityDigest = req.CapabilityDigest
	rec.CapabilitiesJSON = marshalOrEmptyObject(req.Capabilities)
	rec.UpdatedAt = now
	saved, err := h.store.Upsert(rec)
	if err != nil {
		auditRegister(r, req.RuntimeID, req.Kind, auditport.StatusFailure, err)
		writeError(w, err)
		return
	}
	auditRegister(r, req.RuntimeID, req.Kind, auditport.StatusSuccess, nil)
	writeJSON(w, http.StatusOK, viewOf(saved, h.now(), "registry"))
}

// heartbeat 是 POST /v1/runtimes/{id}/heartbeat：更新存活、容量并按 spec
// 记录版本漂移。漂移不拒收（「accepts the liveness observation, records
// the anomaly」），只降级为 degraded。
func (h *Handler) heartbeat(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req heartbeatRequest
	if !decodeJSON(w, r, &req) {
		auditHeartbeat(r, id, auditport.StatusFailure, errs.ErrInvalid)
		return
	}
	rec, err := h.store.Get(id)
	if err != nil {
		auditHeartbeat(r, id, auditport.StatusFailure, err)
		writeError(w, err)
		return
	}
	now := h.now()
	reason := ""
	src := model.VersionSource(req.VersionSource)
	if src != "" && !model.IsVersionSource(src) {
		reason = "version_source not in {manifest, release_metadata}"
	} else if req.ObservedVersion != "" && rec.ObservedVersion != "" &&
		req.ObservedVersion != rec.ObservedVersion {
		reason = "observed version drift: registered=" + rec.ObservedVersion +
			" reported=" + req.ObservedVersion
	}
	if req.ObservedVersion != "" {
		rec.ObservedVersion = req.ObservedVersion
	}
	if src != "" {
		rec.VersionSource = src
	}
	if req.ArtifactDigest != "" {
		if req.ArtifactDigest != rec.ArtifactDigest && rec.ArtifactDigest != "" && reason == "" {
			reason = "artifact digest drift"
		}
		rec.ArtifactDigest = req.ArtifactDigest
	}
	if req.CapabilityDigest != "" {
		rec.CapabilityDigest = req.CapabilityDigest
	}
	rec.TotalSlots = req.TotalSlots
	rec.FreeSlots = req.FreeSlots
	rec.ActiveTasks = req.ActiveTasks
	rec.LastHeartbeatAt = &now
	if reason != "" {
		rec.Health = model.RuntimeHealthDegraded
		rec.HealthReason = reason
	} else if rec.Health == model.RuntimeHealthDegraded && rec.HealthReason != "" {
		// 同一报告不再漂移：观察被接受，异常标记保留到注册基准刷新。
		// 首切片不自愈（spec 要求 drift 事件与显式恢复，属下一切片）。
		rec.Health = model.RuntimeHealthDegraded
	} else {
		rec.Health = model.RuntimeHealthOnline
		rec.HealthReason = ""
	}
	rec.UpdatedAt = now
	if err := h.store.Save(rec); err != nil {
		auditHeartbeat(r, id, auditport.StatusFailure, err)
		writeError(w, err)
		return
	}
	auditHeartbeat(r, id, auditport.StatusSuccess, nil)
	writeJSON(w, http.StatusOK, viewOf(rec, h.now(), "registry"))
}

// ackCommand 是 POST /v1/runtimes/{id}/commands/{cid}/ack。首切片是契约
// 占位（ADR §4.3）：接受 ack、累计 CommandAckCount 并记最近时戳，命令
// 下发本身是 §2.4–2.6 的事。
func (h *Handler) ackCommand(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	cid := chi.URLParam(r, "cid")
	if id == "" || cid == "" {
		auditAck(r, id, cid, auditport.StatusFailure, errs.ErrInvalid)
		writeError(w, errs.ErrInvalid)
		return
	}
	var req ackRequest
	if !decodeJSON(w, r, &req) {
		auditAck(r, id, cid, auditport.StatusFailure, errs.ErrInvalid)
		return
	}
	rec, err := h.store.Get(id)
	if err != nil {
		auditAck(r, id, cid, auditport.StatusFailure, err)
		writeError(w, err)
		return
	}
	now := h.now()
	rec.CommandAckCount++
	rec.LastCommandAckAt = &now
	rec.UpdatedAt = now
	var acks map[string]any
	if rec.PendingCommandAcksJSON != "" {
		_ = json.Unmarshal([]byte(rec.PendingCommandAcksJSON), &acks)
	}
	if acks == nil {
		acks = map[string]any{}
	}
	acks[cid] = map[string]any{
		"result":          firstNonEmpty(req.Result, "accepted"),
		"acknowledged_at": now.Format(time.RFC3339Nano),
	}
	if b, err := json.Marshal(acks); err == nil {
		rec.PendingCommandAcksJSON = string(b)
	}
	if err := h.store.Save(rec); err != nil {
		auditAck(r, id, cid, auditport.StatusFailure, err)
		writeError(w, err)
		return
	}
	auditAck(r, id, cid, auditport.StatusSuccess, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"runtime_id":        rec.RuntimeID,
		"command_id":        cid,
		"command_ack_count": rec.CommandAckCount,
		"acknowledged_at":   now.Format(time.RFC3339Nano),
	})
}

// inventory 是 GET /v1/runtimes：注册记录 + 五类内置观察者实时投影的合并
// 视图。默认部署（无注册无观察者）返回 200 + 空数组，零行为变化。
func (h *Handler) inventory(w http.ResponseWriter, r *http.Request) {
	now := h.now()
	items := []runtimeView{}
	seen := map[string]bool{}
	if h.store != nil {
		recs, err := h.store.List()
		if err != nil {
			writeError(w, err)
			return
		}
		for _, rec := range recs {
			items = append(items, viewOf(rec, now, "registry"))
			seen[rec.RuntimeID] = true
		}
	}
	for _, src := range h.sources {
		if src == nil {
			continue
		}
		for _, obs := range src.Describe() {
			if seen[obs.RuntimeID] {
				continue // 注册记录优先：心跳持有更新鲜的存活时戳
			}
			items = append(items, viewOfObservation(obs))
			seen[obs.RuntimeID] = true
		}
	}
	writeJSON(w, http.StatusOK, inventoryResponse{Total: len(items), Items: items})
}

// ---- 辅助 ----

func viewOf(rec model.RuntimeRecord, now time.Time, source string) runtimeView {
	labels := map[string]string{}
	_ = json.Unmarshal([]byte(rec.LabelsJSON), &labels)
	return runtimeView{
		RuntimeID:          rec.RuntimeID,
		Kind:               string(rec.Kind),
		Profile:            rec.Profile,
		Owner:              rec.Owner,
		DeploymentLocation: rec.DeploymentLocation,
		Labels:             labels,
		ObservedVersion:    rec.ObservedVersion,
		VersionSource:      string(rec.VersionSource),
		ArtifactDigest:     rec.ArtifactDigest,
		CapabilityDigest:   rec.CapabilityDigest,
		Health:             string(rec.EvaluateHealth(now)),
		HealthReason:       rec.HealthReason,
		LastHeartbeatAt:    rec.LastHeartbeatAt,
		TotalSlots:         rec.TotalSlots,
		FreeSlots:          rec.FreeSlots,
		ActiveTasks:        rec.ActiveTasks,
		CommandAckCount:    rec.CommandAckCount,
		Source:             source,
	}
}

func viewOfObservation(obs RuntimeObservation) runtimeView {
	return runtimeView{
		RuntimeID:          obs.RuntimeID,
		Kind:               string(obs.Kind),
		Profile:            obs.Profile,
		Owner:              obs.Owner,
		DeploymentLocation: obs.DeploymentLocation,
		ObservedVersion:    obs.ObservedVersion,
		VersionSource:      string(obs.VersionSource),
		ArtifactDigest:     obs.ArtifactDigest,
		CapabilityDigest:   obs.CapabilityDigest,
		Health:             string(obs.Health),
		HealthReason:       obs.HealthReason,
		TotalSlots:         obs.TotalSlots,
		FreeSlots:          obs.FreeSlots,
		ActiveTasks:        obs.ActiveTasks,
		Source:             "projection",
	}
}

func isNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, errs.ErrNotFound)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.More() {
		writeError(w, errs.ErrInvalid)
		return false
	}
	return true
}

const maxRequestBodyBytes = 32 << 10

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, err error) {
	writeJSON(w, errs.HTTPStatus(err), map[string]string{"error": err.Error()})
}

func marshalOrEmptyObject(v any) string {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return "{}"
	}
	return string(b)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
