// Package flow exposes the workflow-orchestration HTTP surface
// (HLD-016). Mirrors server/report's lean pattern — chi-mounted
// Handler, reads open to any authed user, writes gated on role via
// requireWriter (ADR-022: viewer is read-only).
package flow

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	bizflow "github.com/vincent-wuhan/opskeeper/core/domains/biz/flow"
	model "github.com/vincent-wuhan/opskeeper/core/domains/model/flow"
)

const roleViewer = "viewer"

// Handler carries the biz facade.
type Handler struct {
	uc *bizflow.Usecase
}

// NewHandler constructs the HTTP layer.
func NewHandler(uc *bizflow.Usecase) *Handler { return &Handler{uc: uc} }

// Register mounts the authed routes.
func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/flows", h.list)
	r.With(h.requireWriter).Post("/v1/flows", h.create)
	r.With(h.requireWriter).Post("/v1/flows/generate", h.generate)
	r.Get("/v1/flows/{id}", h.get)
	r.With(h.requireWriter).Put("/v1/flows/{id}", h.update)
	r.With(h.requireWriter).Delete("/v1/flows/{id}", h.del)
	r.With(h.requireWriter).Post("/v1/flows/{id}/toggle", h.toggle)
	r.With(h.requireWriter).Post("/v1/flows/{id}/run", h.run)
	r.With(h.requireWriter).Post("/v1/flows/{id}/test-node", h.testNode)
	r.Get("/v1/flows/{id}/runs", h.listRuns)
	r.Get("/v1/flow-runs/{run_id}", h.getRun)
	r.Get("/v1/flow-tools", h.listTools)
	r.Get("/v1/flow-node-types", h.listNodeTypes)
}

func (h *Handler) requireWriter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, ok := tenantctx.From(r.Context())
		if !ok {
			writeErr(w, errs.ErrUnauthorized)
			return
		}
		if t.Role == roleViewer {
			writeErr(w, errs.ErrForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- wire DTOs ---

type flowDTO struct {
	ID          uint64          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Graph       json.RawMessage `json:"graph"`
	Enabled     bool            `json:"enabled"`
	Version     int             `json:"version"`
	NodeCount   int             `json:"node_count"`
	TriggerType string          `json:"trigger_type,omitempty"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
}

func toFlowDTO(f *model.Flow, withGraph bool) flowDTO {
	d := flowDTO{
		ID:          f.ID,
		Name:        f.Name,
		Description: f.Description,
		Enabled:     f.Enabled,
		Version:     f.Version,
		CreatedAt:   f.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   f.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	// Cheap graph summary so the list can show node count + trigger without
	// shipping the whole graph. GraphJSON is on the model even when the list
	// omits the serialized graph.
	if len(f.GraphJSON) > 0 {
		var g struct {
			Nodes []struct {
				Type string `json:"type"`
			} `json:"nodes"`
		}
		if json.Unmarshal([]byte(f.GraphJSON), &g) == nil {
			d.NodeCount = len(g.Nodes)
			for _, n := range g.Nodes {
				if strings.HasPrefix(n.Type, "trigger.") {
					d.TriggerType = n.Type
					break
				}
			}
		}
	}
	if withGraph {
		d.Graph = json.RawMessage(f.GraphJSON)
	}
	return d
}

type runDTO struct {
	ID          string `json:"id"`
	FlowID      uint64 `json:"flow_id"`
	FlowVersion int    `json:"flow_version"`
	Status      string `json:"status"`
	TriggerType string `json:"trigger_type"`
	Error       string `json:"error,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	FinishedAt  string `json:"finished_at,omitempty"`
	CreatedAt   string `json:"created_at"`
}

func toRunDTO(r *model.FlowRun) runDTO {
	d := runDTO{
		ID:          r.ID,
		FlowID:      r.FlowID,
		FlowVersion: r.FlowVersion,
		Status:      r.Status,
		TriggerType: r.TriggerType,
		Error:       r.Error,
		CreatedAt:   r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if r.StartedAt != nil {
		d.StartedAt = r.StartedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if r.FinishedAt != nil {
		d.FinishedAt = r.FinishedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return d
}

type runNodeDTO struct {
	NodeID     string          `json:"node_id"`
	NodeType   string          `json:"node_type"`
	NodeName   string          `json:"node_name"`
	Status     string          `json:"status"`
	Input      json.RawMessage `json:"input"`
	Output     json.RawMessage `json:"output"`
	FiredPort  string          `json:"fired_port"`
	Error      string          `json:"error,omitempty"`
	StartedAt  string          `json:"started_at,omitempty"`
	FinishedAt string          `json:"finished_at,omitempty"`
}

// --- handlers ---

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if !h.authed(w, r) {
		return
	}
	q := r.URL.Query()
	rows, total, err := h.uc.List(r.Context(), atoiDefault(q.Get("limit"), 50), atoiDefault(q.Get("offset"), 0))
	if err != nil {
		writeErr(w, err)
		return
	}
	items := make([]flowDTO, 0, len(rows))
	for _, f := range rows {
		items = append(items, toFlowDTO(f, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

type writeBody struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Graph       json.RawMessage `json:"graph"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	t, _ := tenantctx.From(r.Context())
	var in writeBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10)).Decode(&in); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	f, err := h.uc.Create(r.Context(), bizflow.CreateInput{
		Name:        in.Name,
		Description: in.Description,
		GraphJSON:   string(in.Graph),
		CreatedBy:   &t.UserID,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 335：编排定义落库。graph 不进 payload —— 它是一份会被反复改写的大块
	// JSON，而链上留一份「当时的图」只会让人误以为那是事实；要回查就查 flows 表。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionFlowCreate,
		ResourceType: auditport.ResourceFlow,
		ResourceID:   strconv.FormatUint(f.ID, 10),
		ResourceName: f.Name,
		Status:       auditport.StatusSuccess,
	})
	writeJSON(w, http.StatusCreated, toFlowDTO(f, true))
}

// generate turns a natural-language prompt into a workflow: the model drafts a
// graph from the live tool catalog, we validate + persist it, and return the
// new flow so the SPA opens it in the editor for review.
func (h *Handler) generate(w http.ResponseWriter, r *http.Request) {
	t, _ := tenantctx.From(r.Context())
	var in struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	draft, err := h.uc.GenerateGraph(r.Context(), in.Prompt)
	if err != nil {
		writeErr(w, err)
		return
	}
	draft.CreatedBy = &t.UserID
	f, err := h.uc.Create(r.Context(), draft)
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 335：这一行的价值不在「创建了一个编排」，而在「**是模型根据这句话
	// 决定了这个自动化该长什么样**」。prompt 进 payload：它是这次决定的全部输入，
	// 且不长；而 graph 不进——它已经落库，而且会被人接着改。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionFlowGenerate,
		ResourceType: auditport.ResourceFlow,
		ResourceID:   strconv.FormatUint(f.ID, 10),
		ResourceName: f.Name,
		Status:       auditport.StatusSuccess,
		Payload:      map[string]any{"prompt": in.Prompt},
	})
	writeJSON(w, http.StatusCreated, toFlowDTO(f, true))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	if !h.authed(w, r) {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	f, err := h.uc.Get(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toFlowDTO(f, true))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in writeBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10)).Decode(&in); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	f, err := h.uc.Update(r.Context(), id, bizflow.CreateInput{
		Name:        in.Name,
		Description: in.Description,
		GraphJSON:   string(in.Graph),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionFlowUpdate,
		ResourceType: auditport.ResourceFlow,
		ResourceID:   strconv.FormatUint(id, 10),
		ResourceName: f.Name,
		Status:       auditport.StatusSuccess,
		Payload:      map[string]any{"version": f.Version},
	})
	writeJSON(w, http.StatusOK, toFlowDTO(f, true))
}

func (h *Handler) del(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	// 名字先读：删除之后行里剩下的只有数字 id，而「删的是哪份自动化」正是
	// 事后第一个要回答的问题。读不到不是不写这行的理由。
	existing, _ := h.uc.Get(r.Context(), id)
	name := ""
	if existing != nil {
		name = existing.Name
	}
	if err := h.uc.Delete(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionFlowDelete,
		ResourceType: auditport.ResourceFlow,
		ResourceID:   strconv.FormatUint(id, 10),
		ResourceName: name,
		Status:       auditport.StatusSuccess,
	})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (h *Handler) toggle(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	if err := h.uc.SetEnabled(r.Context(), id, in.Enabled); err != nil {
		writeErr(w, err)
		return
	}
	// 决策 335：开与关不是同一个动词的两面。**enable=true 决定这份自动化此后
	// 会不会自己动手**（告警与 cron 触发器扫的是 enabled 的编排），所以这一行
	// 单独可查；disable 记在同一个动作的 payload 里，靠 enabled 字段区分。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionFlowToggle,
		ResourceType: auditport.ResourceFlow,
		ResourceID:   strconv.FormatUint(id, 10),
		Status:       auditport.StatusSuccess,
		Payload:      map[string]any{"enabled": in.Enabled},
	})
	writeJSON(w, http.StatusOK, map[string]any{"enabled": in.Enabled})
}

func (h *Handler) run(w http.ResponseWriter, r *http.Request) {
	t, _ := tenantctx.From(r.Context())
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		Input map[string]any `json:"input"`
	}
	// Body is optional for a bare manual run.
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in)
	run, err := h.uc.Trigger(r.Context(), id, in.Input, &t.UserID)
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 335：整个平台里后果最重的一行——** somebody 按下了一次执行**。
	// input 不进 payload：运行输入本身已经作为 run 的 TriggerJSON 落库，
	// 而它也可能含凭据；链上记的是「谁在什么时候让哪份编排跑了起来」，run_id
	// 让执行细节可以被查到，而不必把它复制一份到链上。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionFlowRun,
		ResourceType: auditport.ResourceFlow,
		ResourceID:   strconv.FormatUint(id, 10),
		Status:       auditport.StatusSuccess,
		Payload:      map[string]any{"run_id": run.ID, "trigger": bizflow.NodeTriggerManual},
	})
	writeJSON(w, http.StatusAccepted, toRunDTO(run))
}

// testNode runs one node in isolation and returns its output (or the
// execution error in-band) so the editor can show real output before the
// node is wired in. Execution errors are 200 + {error} — only malformed
// requests are HTTP errors.
func (h *Handler) testNode(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		NodeType     string          `json:"node_type"`
		Config       json.RawMessage `json:"config"`
		TriggerInput map[string]any  `json:"trigger_input"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&in); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	out, runErr := h.uc.TestNode(r.Context(), id, in.NodeType, in.Config, in.TriggerInput)
	if runErr != nil {
		// 决策 335：这个失败是**带内**的——HTTP 200，错误在响应体的 error 字段里。
		// 于是任何靠状态码判断成败的东西（中间件、网关、指标、告警）都会把它当成
		// 一次成功。**而它其实真的动了手**：testNode 会执行那个节点，会触到工具。
		// 所以这里必须显式写一行 failure；否则「试了一下，没成」这件事在链上
		// 一次也不会出现，而它恰恰是最值得被看见的那种试探。
		auditport.SetAuditEvent(r, auditport.Event{
			Action:       auditport.ActionFlowTestNode,
			ResourceType: auditport.ResourceFlow,
			ResourceID:   strconv.FormatUint(id, 10),
			Status:       auditport.StatusFailure,
			ErrorMessage: runErr.Error(),
			Payload:      map[string]any{"node_type": in.NodeType},
		})
		writeJSON(w, http.StatusOK, map[string]any{"error": runErr.Error()})
		return
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionFlowTestNode,
		ResourceType: auditport.ResourceFlow,
		ResourceID:   strconv.FormatUint(id, 10),
		Status:       auditport.StatusSuccess,
		Payload:      map[string]any{"node_type": in.NodeType},
	})
	writeJSON(w, http.StatusOK, map[string]any{"output": out})
}

func (h *Handler) listRuns(w http.ResponseWriter, r *http.Request) {
	if !h.authed(w, r) {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	runs, err := h.uc.ListRuns(r.Context(), id, atoiDefault(r.URL.Query().Get("limit"), 20))
	if err != nil {
		writeErr(w, err)
		return
	}
	items := make([]runDTO, 0, len(runs))
	for _, rr := range runs {
		items = append(items, toRunDTO(rr))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) getRun(w http.ResponseWriter, r *http.Request) {
	if !h.authed(w, r) {
		return
	}
	run, nodes, err := h.uc.GetRun(r.Context(), chi.URLParam(r, "run_id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	nds := make([]runNodeDTO, 0, len(nodes))
	for _, n := range nodes {
		d := runNodeDTO{
			NodeID:    n.NodeID,
			NodeType:  n.NodeType,
			NodeName:  n.NodeName,
			Status:    n.Status,
			Input:     json.RawMessage(n.InputJSON),
			Output:    json.RawMessage(n.OutputJSON),
			FiredPort: n.FiredPort,
			Error:     n.Error,
		}
		if n.StartedAt != nil {
			d.StartedAt = n.StartedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		if n.FinishedAt != nil {
			d.FinishedAt = n.FinishedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		nds = append(nds, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": toRunDTO(run), "nodes": nds})
}

type toolMetaDTO struct {
	Name          string          `json:"name"`
	DisplayZh     string          `json:"display_zh,omitempty"`
	Description   string          `json:"description"`
	DescriptionZh string          `json:"description_zh,omitempty"`
	WhenToUse     string          `json:"when_to_use,omitempty"`
	Class         string          `json:"class"`
	Category      string          `json:"category"`
	Parameters    json.RawMessage `json:"parameters,omitempty"`
}

// listTools returns the tool-node palette (every registered BaseTool as
// a draggable, form-driven node source). Read-open to any authed user
// since the editor needs it; an empty list means the tools runtime
// isn't wired (LLM provider absent) — the canvas degrades gracefully.
func (h *Handler) listTools(w http.ResponseWriter, r *http.Request) {
	if !h.authed(w, r) {
		return
	}
	metas := h.uc.ListTools()
	items := make([]toolMetaDTO, 0, len(metas))
	for _, m := range metas {
		items = append(items, toolMetaDTO{
			Name:          m.Name,
			DisplayZh:     m.DisplayZh,
			Description:   m.Description,
			DescriptionZh: m.DescriptionZh,
			WhenToUse:     m.WhenToUse,
			Class:         m.Class,
			Category:      m.Category,
			Parameters:    m.Parameters,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type nodeTypeDTO struct {
	Type         string                    `json:"type"`
	Kind         string                    `json:"kind"`
	Category     string                    `json:"category"`
	LabelZh      string                    `json:"label_zh"`
	LabelEn      string                    `json:"label_en"`
	Ports        []string                  `json:"ports"`
	ConfigFields []bizflow.ConfigFieldSpec `json:"config_fields"`
	OutputShape  []string                  `json:"output_shape"`
}

// listNodeTypes returns every registered node-type spec so the editor can
// render the palette + config drawer from data, not hardcoded tables.
// Read-open to any authed user.
func (h *Handler) listNodeTypes(w http.ResponseWriter, r *http.Request) {
	if !h.authed(w, r) {
		return
	}
	specs := h.uc.ListNodeTypes()
	items := make([]nodeTypeDTO, 0, len(specs))
	for _, s := range specs {
		items = append(items, nodeTypeDTO{
			Type:         s.Type,
			Kind:         string(s.Kind),
			Category:     s.Category,
			LabelZh:      s.LabelZh,
			LabelEn:      s.LabelEn,
			Ports:        s.Ports,
			ConfigFields: s.ConfigFields,
			OutputShape:  s.OutputShape,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// --- helpers (mirror server/report) ---

func (h *Handler) authed(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return false
	}
	return true
}

func pathID(r *http.Request) (uint64, error) {
	id, err := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		return 0, errors.Join(errs.ErrInvalid, err)
	}
	return id, nil
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeErr(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(errs.HTTPStatus(err))
	_ = json.NewEncoder(w).Encode(errorBody{Error: err.Error(), Code: errCode(err)})
}

func errCode(err error) string {
	switch {
	case errors.Is(err, errs.ErrNotFound):
		return "not-found"
	case errors.Is(err, errs.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, errs.ErrForbidden):
		return "forbidden"
	case errors.Is(err, errs.ErrInvalid):
		return "invalid"
	case errors.Is(err, errs.ErrNotWiredYet):
		return "not-wired-yet"
	default:
		return "internal"
	}
}
