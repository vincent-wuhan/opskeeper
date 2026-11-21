// tasks_http.go 是 §2.2+§2.3 任务生命周期 HTTP API（ ADR D-2）。
// 前缀 /v1/runtime-tasks：/v1/tasks 已被 report 包占用（report/http.go
// 的 oneoff 任务族），复用即路由冲突。
//
// 本文件只做 decode / 审计 / respond；状态机全部在
// biz/runtimemgmt.Usecase，SQL 在 data/runtimemgmt/store。
//
// 三条高频存活路由（lease-renewal / checkpoint / progress）不落审计行：
// settled 型无行裁决，理由在 scripts/routeaudit verdict 表；其余七条
// mutating 路由逐条入账（runtime_task_* 七词）。
package runtimemgmt

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	bizruntimemgmt "github.com/vincent-wuhan/opskeeper/core/manager/biz/runtimemgmt"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// TaskHandler 挂任务生命周期路由。usecase 是状态机；nowFn 保留与
// runtime handler 相同的可注入时钟形状。
type TaskHandler struct {
	usecase *bizruntimemgmt.Usecase
}

// NewTaskHandler 构造。
func NewTaskHandler(usecase *bizruntimemgmt.Usecase) *TaskHandler {
	return &TaskHandler{usecase: usecase}
}

// Register 挂载到 protected 路由组（admin auth + 审计中间件）。
func (h *TaskHandler) Register(r chi.Router) {
	r.Post("/v1/runtime-tasks", h.create)
	r.Get("/v1/runtime-tasks", h.list)
	r.Get("/v1/runtime-tasks/{id}", h.get)
	r.Post("/v1/runtime-tasks/{id}/claim", h.claim)
	r.Post("/v1/runtime-tasks/{id}/lease-renewal", h.leaseRenewal)
	r.Post("/v1/runtime-tasks/{id}/start", h.start)
	r.Post("/v1/runtime-tasks/{id}/checkpoint", h.checkpoint)
	r.Post("/v1/runtime-tasks/{id}/progress", h.progress)
	r.Post("/v1/runtime-tasks/{id}/result", h.result)
	r.Post("/v1/runtime-tasks/{id}/failure", h.failure)
	r.Post("/v1/runtime-tasks/{id}/cancel", h.cancel)
	r.Post("/v1/runtime-tasks/stale-recovery", h.staleRecovery)
}

// ---- 请求形状 ----

type createTaskRequest struct {
	TenantID   string `json:"tenant_id"`
	IncidentID string `json:"incident_id,omitempty"`
	Kind       string `json:"kind"`
	Payload    string `json:"payload,omitempty"` // JSON 文本，控制面不解释
	Safety     string `json:"safety"`
	Selector   string `json:"desired_runtime_selector,omitempty"`
	TraceID    string `json:"trace_id,omitempty"`
	Priority   int    `json:"priority,omitempty"`

	IdempotencyTargetSystem string `json:"idempotency_target_system,omitempty"`
	IdempotencyScope        string `json:"idempotency_scope,omitempty"`
	IdempotencyExternalID   string `json:"idempotency_external_id,omitempty"`
}

type claimRequest struct {
	RuntimeID string `json:"runtime_id"`
}

type leaseRequest struct {
	RuntimeID  string `json:"runtime_id"`
	Generation int64  `json:"generation"`
}

type checkpointRequest struct {
	RuntimeID      string `json:"runtime_id"`
	Generation     int64  `json:"generation"`
	AttemptNumber  int    `json:"attempt_number,omitempty"`
	Phase          string `json:"phase,omitempty"`
	ResumeHint     string `json:"resume_hint,omitempty"`
	PayloadRef     string `json:"payload_ref,omitempty"`
}

type resultRequest struct {
	RuntimeID       string `json:"runtime_id"`
	Generation      int64  `json:"generation"`
	AttemptNumber   int    `json:"attempt_number,omitempty"`
	OutputRef       string `json:"output_ref,omitempty"`
	OutputSize      int64  `json:"output_size,omitempty"`
	OutputDigest    string `json:"output_digest,omitempty"`
	SanitizerStatus string `json:"sanitizer_status"`
	SanitizerNotes  string `json:"sanitizer_notes,omitempty"`
}

type failureRequest struct {
	RuntimeID      string `json:"runtime_id"`
	Generation     int64  `json:"generation"`
	AttemptNumber  int    `json:"attempt_number,omitempty"`
	FailureClass   string `json:"failure_class"`
	FailureReason  string `json:"failure_reason,omitempty"`
}

type staleRecoveryRequest struct {
	Limit int `json:"limit,omitempty"`
}

// ---- handlers ----

// create 是 POST /v1/runtime-tasks：入队 + 内联准入评估（§2.2）。
func (h *TaskHandler) create(w http.ResponseWriter, r *http.Request) {
	var req createTaskRequest
	if !decodeJSON(w, r, &req) {
		auditTaskAction(r, auditport.ActionRuntimeTaskCreate, "", nil, errs.ErrInvalid)
		return
	}
	tenant := firstNonEmpty(req.TenantID, tenantFromRequest(r))
	safety := model.TaskSafety(req.Safety)
	if req.Kind == "" || !model.IsTaskSafety(safety) {
		auditTaskAction(r, auditport.ActionRuntimeTaskCreate, "", nil, errs.ErrInvalid)
		writeError(w, errs.ErrInvalid)
		return
	}
	if model.IsTaskSafety(safety) && safety.RequiresIdempotencyContract() &&
		(req.IdempotencyTargetSystem == "" || req.IdempotencyScope == "") {
		// 入队 fail-fast（ADR D-5 第一道闸）。
		auditTaskAction(r, auditport.ActionRuntimeTaskCreate, "", map[string]any{"kind": req.Kind}, model.ErrIdempotencyRequired)
		writeError(w, errs.ErrInvalid)
		return
	}
	res, err := h.usecase.Enqueue(r.Context(), bizruntimemgmt.EnqueueRequest{
		TenantID: tenant,
		IncidentID: req.IncidentID,
		Kind: req.Kind,
		Payload: []byte(req.Payload),
		Safety: safety,
		Selector: req.Selector,
		TraceID: req.TraceID,
		Priority: req.Priority,
		IdempotencyTargetSystem: req.IdempotencyTargetSystem,
		IdempotencyScope: req.IdempotencyScope,
		IdempotencyExternalID: req.IdempotencyExternalID,
		IssuedBy: "api",
	})
	if err != nil {
		auditTaskCreate(r, "", req.Kind, req.Safety, false, err)
		writeError(w, err)
		return
	}
	auditTaskCreate(r, res.Task.TaskID, req.Kind, req.Safety, res.Admitted, nil)
	code := http.StatusCreated
	if !res.Admitted {
		code = http.StatusAccepted // 留 queued：入队成立，准入未过
	}
	writeJSON(w, code, taskViewOf(res.Task, taskAdmissionView(res.Admission)))
}

// list 是 GET /v1/runtime-tasks：按租户列任务。
func (h *TaskHandler) list(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFromRequest(r)
	state, err := model.Normalize(r.URL.Query().Get("state"))
	if r.URL.Query().Get("state") != "" && err != nil {
		writeError(w, errs.ErrInvalid)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	tasks, err := h.usecase.ListTasks(r.Context(), tenant, state, limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	items := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		items = append(items, taskViewOf(t, nil))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(items), "items": items})
}

// get 是 GET /v1/runtime-tasks/{id}：读回单个任务 + 当前认领投影。
func (h *TaskHandler) get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenant := tenantFromRequest(r)
	task, err := h.usecase.GetTask(r.Context(), id, tenant)
	if err != nil {
		writeError(w, err)
		return
	}
	view := taskViewOf(task, nil)
	if claim, cerr := h.usecase.GetClaim(r.Context(), id, tenant); cerr == nil {
		view.ClaimedBy = claim.ClaimedBy
		view.Generation = claim.Generation
	}
	writeJSON(w, http.StatusOK, view)
}

// claim 是 POST /{id}/claim：admitted→claimed（§2.2 认领再校验 + §2.3
// 代际写入）。
func (h *TaskHandler) claim(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req claimRequest
	if !decodeJSON(w, r, &req) || req.RuntimeID == "" {
		auditTaskAction(r, auditport.ActionRuntimeTaskClaim, id, nil, errs.ErrInvalid)
		return
	}
	res, err := h.usecase.Claim(r.Context(), id, tenantFromRequest(r), req.RuntimeID)
	if err != nil {
		auditTaskAction(r, auditport.ActionRuntimeTaskClaim, id, map[string]any{"runtime_id": req.RuntimeID}, err)
		writeError(w, err)
		return
	}
	auditTaskAction(r, auditport.ActionRuntimeTaskClaim, id, map[string]any{
		"runtime_id": req.RuntimeID, "generation": res.Claim.Generation,
	}, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": res.Task.TaskID,
		"state": string(res.Task.State),
		"generation": res.Claim.Generation,
		"claimed_by": res.Claim.ClaimedBy,
		"prepare_deadline": res.Lease.PrepareDeadline.UTC().Format(timeFormat),
		"attempt_number": res.Attempt.AttemptNumber,
	})
}

// leaseRenewal 是 POST /{id}/lease-renewal：滚存活租约。无行裁决。
func (h *TaskHandler) leaseRenewal(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req leaseRequest
	if !decodeJSON(w, r, &req) || req.RuntimeID == "" {
		writeError(w, errs.ErrInvalid)
		return
	}
	lease, err := h.usecase.RenewLease(r.Context(), id, tenantFromRequest(r), req.RuntimeID, req.Generation)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": id,
		"generation": lease.Generation,
		"liveness_deadline": lease.LivenessDeadline.UTC().Format(timeFormat),
		"extensions": lease.Extensions,
	})
}

// start 是 POST /{id}/start：claimed→preparing→running（一次调用两笔
// 迁移，ADR D-2）。
func (h *TaskHandler) start(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req leaseRequest
	if !decodeJSON(w, r, &req) || req.RuntimeID == "" {
		auditTaskAction(r, auditport.ActionRuntimeTaskStart, id, nil, errs.ErrInvalid)
		return
	}
	task, lease, err := h.usecase.Start(r.Context(), id, tenantFromRequest(r), req.RuntimeID, req.Generation)
	if err != nil {
		auditTaskAction(r, auditport.ActionRuntimeTaskStart, id, map[string]any{"runtime_id": req.RuntimeID}, err)
		writeError(w, err)
		return
	}
	auditTaskAction(r, auditport.ActionRuntimeTaskStart, id, map[string]any{
		"runtime_id": req.RuntimeID, "state": string(task.State),
	}, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": task.TaskID,
		"state": string(task.State),
		"liveness_deadline": lease.LivenessDeadline.UTC().Format(timeFormat),
	})
}

// checkpoint 是 POST /{id}/checkpoint：追加检查点 + 滚租约。无行裁决。
func (h *TaskHandler) checkpoint(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req checkpointRequest
	if !decodeJSON(w, r, &req) || req.RuntimeID == "" {
		writeError(w, errs.ErrInvalid)
		return
	}
	cp, lease, err := h.usecase.Checkpoint(r.Context(), id, tenantFromRequest(r), req.RuntimeID, bizruntimemgmt.CheckpointInput{
		Generation: req.Generation,
		AttemptNumber: req.AttemptNumber,
		Phase: req.Phase,
		ResumeHint: req.ResumeHint,
		PayloadRef: req.PayloadRef,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": id,
		"sequence": cp.Sequence,
		"generation": cp.Generation,
		"liveness_deadline": lease.LivenessDeadline.UTC().Format(timeFormat),
	})
}

// progress 是 POST /{id}/progress：检查点的轻量别名。无行裁决。
func (h *TaskHandler) progress(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req checkpointRequest
	if !decodeJSON(w, r, &req) || req.RuntimeID == "" {
		writeError(w, errs.ErrInvalid)
		return
	}
	cp, lease, err := h.usecase.Progress(r.Context(), id, tenantFromRequest(r), req.RuntimeID, bizruntimemgmt.CheckpointInput{
		Generation: req.Generation,
		AttemptNumber: req.AttemptNumber,
		Phase: req.Phase,
		ResumeHint: req.ResumeHint,
		PayloadRef: req.PayloadRef,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": id,
		"phase": cp.Phase,
		"liveness_deadline": lease.LivenessDeadline.UTC().Format(timeFormat),
	})
}

// result 是 POST /{id}/result：→succeeded。
func (h *TaskHandler) result(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req resultRequest
	if !decodeJSON(w, r, &req) || req.RuntimeID == "" {
		auditTaskAction(r, auditport.ActionRuntimeTaskResult, id, nil, errs.ErrInvalid)
		return
	}
	sanitizer := model.SanitizerStatus(req.SanitizerStatus)
	if !model.IsSanitizerStatus(sanitizer) {
		auditTaskAction(r, auditport.ActionRuntimeTaskResult, id, nil, errs.ErrInvalid)
		writeError(w, errs.ErrInvalid)
		return
	}
	task, result, err := h.usecase.ReportResult(r.Context(), id, tenantFromRequest(r), req.RuntimeID, bizruntimemgmt.ResultInput{
		Generation: req.Generation,
		AttemptNumber: req.AttemptNumber,
		OutputRef: req.OutputRef,
		OutputSize: req.OutputSize,
		OutputDigest: req.OutputDigest,
		SanitizerStatus: sanitizer,
		SanitizerNotes: req.SanitizerNotes,
	})
	if err != nil {
		auditTaskAction(r, auditport.ActionRuntimeTaskResult, id, map[string]any{"runtime_id": req.RuntimeID}, err)
		writeError(w, err)
		return
	}
	auditTaskAction(r, auditport.ActionRuntimeTaskResult, id, map[string]any{
		"runtime_id": req.RuntimeID, "state": string(task.State),
		"sanitizer_status": string(result.SanitizerStatus),
	}, nil)
	writeJSON(w, http.StatusOK, taskViewOf(task, nil))
}

// failure 是 POST /{id}/failure：→failed 或可重试分类回 admitted。
func (h *TaskHandler) failure(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req failureRequest
	if !decodeJSON(w, r, &req) || req.RuntimeID == "" {
		auditTaskAction(r, auditport.ActionRuntimeTaskFailure, id, nil, errs.ErrInvalid)
		return
	}
	fc := model.FailureClass(req.FailureClass)
	if !model.IsFailureClass(fc) {
		auditTaskAction(r, auditport.ActionRuntimeTaskFailure, id, nil, errs.ErrInvalid)
		writeError(w, errs.ErrInvalid)
		return
	}
	task, retried, err := h.usecase.ReportFailure(r.Context(), id, tenantFromRequest(r), req.RuntimeID, bizruntimemgmt.FailureInput{
		Generation: req.Generation,
		AttemptNumber: req.AttemptNumber,
		FailureClass: fc,
		FailureReason: req.FailureReason,
	})
	if err != nil {
		auditTaskAction(r, auditport.ActionRuntimeTaskFailure, id, map[string]any{"runtime_id": req.RuntimeID}, err)
		writeError(w, err)
		return
	}
	auditTaskAction(r, auditport.ActionRuntimeTaskFailure, id, map[string]any{
		"runtime_id": req.RuntimeID, "failure_class": string(fc),
		"requeued": retried,
	}, nil)
	writeJSON(w, http.StatusOK, taskViewOf(task, nil))
}

// cancel 是 POST /{id}/cancel：操作员取消 →cancelled。
func (h *TaskHandler) cancel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	task, err := h.usecase.Cancel(r.Context(), id, tenantFromRequest(r), actorFromRequest(r))
	if err != nil {
		auditTaskAction(r, auditport.ActionRuntimeTaskCancel, id, nil, err)
		writeError(w, err)
		return
	}
	auditTaskAction(r, auditport.ActionRuntimeTaskCancel, id, map[string]any{
		"state": string(task.State),
	}, nil)
	writeJSON(w, http.StatusOK, taskViewOf(task, nil))
}

// staleRecovery 是 POST /v1/runtime-tasks/stale-recovery：清扫陈旧认领。
// 幂等，可重复调；runtime_task_recover 入账（recover 行记 recovered/
// terminated 计数，不给每个任务单开一行——批次操作一条汇总行，明细
// 在任务表与 attempt 表里，与批量升级路由的 AddAuditEvent 模式互补：
// 这里每行都是任务自身的状态变化，get 视图可见）。
func (h *TaskHandler) staleRecovery(w http.ResponseWriter, r *http.Request) {
	var req staleRecoveryRequest
	_ = decodeJSONLoose(r, &req)
	res, err := h.usecase.RecoverStale(r.Context(), req.Limit)
	if err != nil {
		auditTaskAction(r, auditport.ActionRuntimeTaskRecover, "", nil, err)
		writeError(w, err)
		return
	}
	auditTaskAction(r, auditport.ActionRuntimeTaskRecover, "", map[string]any{
		"scanned": res.Scanned,
		"recovered": len(res.Recovered),
		"terminated": len(res.Terminated),
	}, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"scanned": res.Scanned,
		"recovered": res.Recovered,
		"terminated": res.Terminated,
	})
}

// ---- 视图与辅助 ----

const timeFormat = "2006-01-02T15:04:05.999999999Z07:00"

type taskView struct {
	TaskID    string `json:"task_id"`
	TenantID  string `json:"tenant_id"`
	IncidentID string `json:"incident_id,omitempty"`
	Kind      string `json:"kind"`
	Safety    string `json:"safety"`
	State     string `json:"state"`
	PayloadDigest string `json:"payload_digest,omitempty"`
	DesiredRuntimeSelector string `json:"desired_runtime_selector,omitempty"`
	RuntimeID string `json:"runtime_id,omitempty"`
	Priority  int    `json:"priority,omitempty"`
	TraceID   string `json:"trace_id,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	ClaimedBy string `json:"claimed_by,omitempty"`
	Generation int64 `json:"generation,omitempty"`
	Admission *admissionView `json:"admission,omitempty"`
}

type admissionView struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	ServerLimit int `json:"server_limit,omitempty"`
	RuntimeLimit int `json:"runtime_limit,omitempty"`
}

func taskViewOf(t model.Task, adm *admissionView) taskView {
	return taskView{
		TaskID: t.TaskID,
		TenantID: t.TenantID,
		IncidentID: t.IncidentID,
		Kind: t.Kind,
		Safety: string(t.Safety),
		State: string(t.State),
		PayloadDigest: t.PayloadDigest,
		DesiredRuntimeSelector: t.DesiredRuntimeSelector,
		RuntimeID: t.RuntimeID,
		Priority: t.Priority,
		TraceID: t.TraceID,
		CreatedAt: t.CreatedAt.UTC().Format(timeFormat),
		UpdatedAt: t.UpdatedAt.UTC().Format(timeFormat),
		Admission: adm,
	}
}

func taskAdmissionView(d model.AdmissionDecision) *admissionView {
	return &admissionView{
		Outcome: string(d.Outcome),
		Reason: d.Reason,
		ServerLimit: d.ServerLimit,
		RuntimeLimit: d.RuntimeLimit,
	}
}

// tenantFromRequest 从 JWT 上下文解析租户（与 loop/http.go 的
// tenantFromContext 同形：单租户 MVP 用 user-N，缺省 default）。
func tenantFromRequest(r *http.Request) string {
	if t, ok := tenantctx.From(r.Context()); ok {
		if t.UserID != 0 {
			return "user-" + strconv.FormatUint(t.UserID, 10)
		}
	}
	return "default"
}

func actorFromRequest(r *http.Request) string {
	if t, ok := tenantctx.From(r.Context()); ok && t.UserID != 0 {
		return strconv.FormatUint(t.UserID, 10)
	}
	return ""
}

// decodeJSONLoose 只解析可选体（stale-recovery 的空 POST 也合法）。
func decodeJSONLoose(r *http.Request, target any) bool {
	if r.Body == nil {
		return true
	}
	raw := make([]byte, 0)
	buf := make([]byte, 4096)
	total := 0
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			total += n
			if total > maxRequestBodyBytes {
				return false
			}
			raw = append(raw, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	if len(raw) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return false
	}
	return true
}

// ServerLimitFromEnv 读 OPSKEEPER_RUNTIME_TASK_SERVER_LIMIT（默认 64）。
// 装配根调用一次，不在请求路径解析。
func ServerLimitFromEnv() int {
	v := os.Getenv("OPSKEEPER_RUNTIME_TASK_SERVER_LIMIT")
	if v == "" {
		return bizruntimemgmt.DefaultServerLimit
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return bizruntimemgmt.DefaultServerLimit
	}
	return n
}
