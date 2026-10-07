// Package topology exposes the manager HTTP routes for the
// graph layer: nodes / relations / relation types.
//
// Routes (all under the authed /api/v1 prefix):
//
//	GET /v1/topology/nodes (any authed)
//	POST /v1/topology/nodes (admin)
//	GET /v1/topology/nodes/{id} (any authed)
//	PATCH /v1/topology/nodes/{id} (admin) — name/props
//	DELETE /v1/topology/nodes/{id} (admin)
//
//	GET /v1/topology/relations (any authed) — filter by src/dst/type/src_or_dst
//	POST /v1/topology/relations (admin)
//	GET /v1/topology/relations/{id} (any authed)
//	PATCH /v1/topology/relations/{id} (admin) — props only
//	DELETE /v1/topology/relations/{id} (admin)
//
//	GET /v1/topology/relation-types (any authed)
//	POST /v1/topology/relation-types (admin)
//	GET /v1/topology/relation-types/{name} (any authed)
//	DELETE /v1/topology/relation-types/{name} (admin)
package topology

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	biz "github.com/vincent-wuhan/opskeeper/core/manager/biz/topology"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/topology"
)

const roleAdmin = "admin"

// Handler exposes /v1/topology/*.
type Handler struct {
	uc *biz.Usecase
}

// NewHandler builds the handler around a topology biz Usecase.
func NewHandler(uc *biz.Usecase) *Handler { return &Handler{uc: uc} }

// Register attaches the topology routes on r.
func (h *Handler) Register(r chi.Router) {
	// Nodes
	r.Get("/v1/topology/nodes", h.listNodes)
	r.Get("/v1/topology/nodes/{id}", h.getNode)
	r.With(h.requireAdmin).Post("/v1/topology/nodes", h.createNode)
	r.With(h.requireAdmin).Patch("/v1/topology/nodes/{id}", h.updateNode)
	r.With(h.requireAdmin).Delete("/v1/topology/nodes/{id}", h.deleteNode)

	// Relations
	r.Get("/v1/topology/relations", h.listRelations)
	r.Get("/v1/topology/relations/{id}", h.getRelation)
	r.With(h.requireAdmin).Post("/v1/topology/relations", h.createRelation)
	r.With(h.requireAdmin).Patch("/v1/topology/relations/{id}", h.updateRelation)
	r.With(h.requireAdmin).Delete("/v1/topology/relations/{id}", h.deleteRelation)

	// Relation types
	r.Get("/v1/topology/relation-types", h.listRelationTypes)
	r.Get("/v1/topology/relation-types/{name}", h.getRelationType)
	r.With(h.requireAdmin).Post("/v1/topology/relation-types", h.createRelationType)
	r.With(h.requireAdmin).Delete("/v1/topology/relation-types/{name}", h.deleteRelationType)

	// Node types — same shape as relation-types. UI uses these to
	// label chips with display_name and to seed the tier layout.
	r.Get("/v1/topology/node-types", h.listNodeTypes)
	r.Get("/v1/topology/node-types/{name}", h.getNodeType)
	r.With(h.requireAdmin).Post("/v1/topology/node-types", h.createNodeType)
	r.With(h.requireAdmin).Delete("/v1/topology/node-types/{name}", h.deleteNodeType)
}

// requireAdmin gates write endpoints behind the admin role.
func (h *Handler) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, ok := tenantctx.From(r.Context())
		if !ok {
			writeErr(w, errs.ErrUnauthorized)
			return
		}
		if t.Role != roleAdmin {
			writeErr(w, errs.ErrForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- DTOs ------------------------------------------------------------

type nodeItem struct {
	ID        uint64 `json:"id"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	Props     any    `json:"props,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type nodeListResp struct {
	Items []nodeItem `json:"items"`
	Total int64      `json:"total"`
}

type createNodeReq struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Props any    `json:"props,omitempty"`
}

type updateNodeReq struct {
	Name  *string `json:"name,omitempty"`
	Props any     `json:"props,omitempty"`
}

type relationItem struct {
	ID        uint64 `json:"id"`
	SrcID     uint64 `json:"src_id"`
	DstID     uint64 `json:"dst_id"`
	Type      string `json:"type"`
	Props     any    `json:"props,omitempty"`
	CreatedAt string `json:"created_at"`
}

type relationListResp struct {
	Items []relationItem `json:"items"`
	Total int64          `json:"total"`
}

type createRelationReq struct {
	SrcID uint64 `json:"src_id"`
	DstID uint64 `json:"dst_id"`
	Type  string `json:"type"`
	Props any    `json:"props,omitempty"`
}

type updateRelationReq struct {
	Props any `json:"props,omitempty"`
}

type relationTypeItem struct {
	Name              string `json:"name"`
	DisplayName       string `json:"display_name"`
	DisplayNameEN     string `json:"display_name_en,omitempty"`
	Builtin           bool   `json:"builtin"`
	PropagatesFailure bool   `json:"propagates_failure"`
	Direction         string `json:"direction"`
	SemanticsTag      string `json:"semantics_tag"`
	Description       string `json:"description"`
}

type createRelationTypeReq struct {
	Name              string `json:"name"`
	DisplayName       string `json:"display_name"`
	DisplayNameEN     string `json:"display_name_en,omitempty"`
	PropagatesFailure bool   `json:"propagates_failure"`
	Direction         string `json:"direction"`
	SemanticsTag      string `json:"semantics_tag"`
	Description       string `json:"description"`
}

// ---------- Node handlers ---------------------------------------------------

func (h *Handler) listNodes(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	q := r.URL.Query()
	f := biz.NodeListFilter{
		Type: q.Get("type"),
		Q:    q.Get("q"),
	}
	if s := q.Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			f.Limit = n
		}
	}
	if s := q.Get("offset"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			f.Offset = n
		}
	}
	rows, total, err := h.uc.ListNodes(r.Context(), f)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]nodeItem, 0, len(rows))
	for _, n := range rows {
		out = append(out, toNodeItem(n))
	}
	writeJSON(w, http.StatusOK, nodeListResp{Items: out, Total: total})
}

func (h *Handler) getNode(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	id, err := parseID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	n, err := h.uc.GetNode(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toNodeItem(n))
}

func (h *Handler) createNode(w http.ResponseWriter, r *http.Request) {
	var in createNodeReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	propsStr, err := encodeProps(in.Props)
	if err != nil {
		writeErr(w, err)
		return
	}
	n, err := h.uc.CreateNode(r.Context(), in.Type, in.Name, propsStr)
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 339：节点是图上的一个点，而**它带不带外部标识**决定后面能不能自动
	// 把它和一台真机器对上。type + name 就是那对标识，所以进链；props 是
	// 业务自定义属性，可能含值班群、机房这类内部名字，与 report 面的 scope_json
	// 同理，不进链。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionTopologyNodeCreate,
		ResourceType: auditport.ResourceTopologyNode,
		ResourceID:   strconv.FormatUint(n.ID, 10),
		ResourceName: n.Name,
		Status:       auditport.StatusSuccess,
		Payload:      map[string]any{"node_type": n.Type},
	})
	writeJSON(w, http.StatusCreated, toNodeItem(n))
}

func (h *Handler) updateNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	var in updateNodeReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	cur, err := h.uc.GetNode(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	name := cur.Name
	if in.Name != nil {
		name = *in.Name
	}
	propsStr := cur.PropsJSON
	if in.Props != nil {
		s, err := encodeProps(in.Props)
		if err != nil {
			writeErr(w, err)
			return
		}
		propsStr = s
	}
	if err := h.uc.UpdateNode(r.Context(), id, name, propsStr); err != nil {
		writeErr(w, err)
		return
	}
	// 决策 339：改名要记**新旧两个名字**。图上的一条边指向的是这个节点，
	// 而事后看链的人手上只有改完之后的名字——「这条边原来连的是谁」从此答不出来。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionTopologyNodeUpdate,
		ResourceType: auditport.ResourceTopologyNode,
		ResourceID:   strconv.FormatUint(id, 10),
		ResourceName: name,
		Status:       auditport.StatusSuccess,
		Payload: map[string]any{
			"node_type": cur.Type,
			// 空串表示「这次没改名」，与「改名成空」区分得开。
			"renamed_from":  cur.Name,
			"props_changed": propsStr != cur.PropsJSON,
		},
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 339：先读名字与类型再删。删一个点等于从图上抹掉它连着的**所有边**，
	// 而事后第一个要回答的问题是「抹掉的是哪个点」——链上只剩一个自增 id 答不出来。
	name, nodeType := "", ""
	if n, err := h.uc.GetNode(r.Context(), id); err == nil && n != nil {
		name, nodeType = n.Name, n.Type
	}
	if err := h.uc.DeleteNode(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionTopologyNodeDelete,
		ResourceType: auditport.ResourceTopologyNode,
		ResourceID:   strconv.FormatUint(id, 10),
		ResourceName: name,
		Status:       auditport.StatusSuccess,
		Payload:      map[string]any{"node_type": nodeType},
	})
	w.WriteHeader(http.StatusNoContent)
}

// ---------- Relation handlers -----------------------------------------------

func (h *Handler) listRelations(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	q := r.URL.Query()
	f := biz.RelationListFilter{
		Type: q.Get("type"),
	}
	if s := q.Get("src_id"); s != "" {
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			f.SrcID = n
		}
	}
	if s := q.Get("dst_id"); s != "" {
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			f.DstID = n
		}
	}
	if s := q.Get("src_or_dst_id"); s != "" {
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			f.SrcOrDstID = n
		}
	}
	if s := q.Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			f.Limit = n
		}
	}
	if s := q.Get("offset"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			f.Offset = n
		}
	}
	rows, total, err := h.uc.ListRelations(r.Context(), f)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]relationItem, 0, len(rows))
	for _, rel := range rows {
		out = append(out, toRelationItem(rel))
	}
	writeJSON(w, http.StatusOK, relationListResp{Items: out, Total: total})
}

func (h *Handler) getRelation(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	id, err := parseID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	rel, err := h.uc.GetRelation(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRelationItem(rel))
}

func (h *Handler) createRelation(w http.ResponseWriter, r *http.Request) {
	var in createRelationReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	propsStr, err := encodeProps(in.Props)
	if err != nil {
		writeErr(w, err)
		return
	}
	rel, err := h.uc.CreateRelation(r.Context(), in.SrcID, in.DstID, in.Type, propsStr)
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 339：一条边就是**一次要走通才能查到的关联**。src/dst/type 三样合起来
	// 才是「加了一条什么样的边」——只记 id 的话，「上次那条关联是谁到谁」要靠翻库。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionTopologyRelationCreate,
		ResourceType: auditport.ResourceTopologyRelation,
		ResourceID:   strconv.FormatUint(rel.ID, 10),
		ResourceName: fmt.Sprintf("%d -%s-> %d", rel.SrcID, rel.Type, rel.DstID),
		Status:       auditport.StatusSuccess,
		Payload: map[string]any{
			"src_id":        rel.SrcID,
			"dst_id":        rel.DstID,
			"relation_type": rel.Type,
		},
	})
	writeJSON(w, http.StatusCreated, toRelationItem(rel))
}

func (h *Handler) updateRelation(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	var in updateRelationReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	propsStr, err := encodeProps(in.Props)
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 339：改 props 之前先把这条边读出来。这条路由只改属性、不改端点，
	// 而「这条边是谁到谁」正是看 props 的人最需要知道的前提。
	srcID, dstID, relType := uint64(0), uint64(0), ""
	if rel, err := h.uc.GetRelation(r.Context(), id); err == nil && rel != nil {
		srcID, dstID, relType = rel.SrcID, rel.DstID, rel.Type
	}
	if err := h.uc.UpdateRelation(r.Context(), id, propsStr); err != nil {
		writeErr(w, err)
		return
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionTopologyRelationUpdate,
		ResourceType: auditport.ResourceTopologyRelation,
		ResourceID:   strconv.FormatUint(id, 10),
		ResourceName: fmt.Sprintf("%d -%s-> %d", srcID, relType, dstID),
		Status:       auditport.StatusSuccess,
		Payload: map[string]any{
			"src_id":        srcID,
			"dst_id":        dstID,
			"relation_type": relType,
			// props 是这条边的业务属性（强弱、延迟、SLO 之类），它是这次改动的
			// **全部内容**，所以与 node 的 props 不同，它必须进链。
			"props": decodeProps(propsStr),
		},
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteRelation(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 339：先读这条边再删。**删一条边就是让一次关联查询从此走不通**，
	// 而事后要回答的是「哪条路断了」——一个自增 id 答不出来。
	srcID, dstID, relType := uint64(0), uint64(0), ""
	if rel, err := h.uc.GetRelation(r.Context(), id); err == nil && rel != nil {
		srcID, dstID, relType = rel.SrcID, rel.DstID, rel.Type
	}
	if err := h.uc.DeleteRelation(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionTopologyRelationDelete,
		ResourceType: auditport.ResourceTopologyRelation,
		ResourceID:   strconv.FormatUint(id, 10),
		ResourceName: fmt.Sprintf("%d -%s-> %d", srcID, relType, dstID),
		Status:       auditport.StatusSuccess,
		Payload: map[string]any{
			"src_id":        srcID,
			"dst_id":        dstID,
			"relation_type": relType,
		},
	})
	w.WriteHeader(http.StatusNoContent)
}

// ---------- RelationType handlers ------------------------------------------

func (h *Handler) listRelationTypes(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	rows, err := h.uc.ListRelationTypes(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]relationTypeItem, 0, len(rows))
	for _, rt := range rows {
		out = append(out, toRelationTypeItem(rt))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *Handler) getRelationType(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	name := chi.URLParam(r, "name")
	rt, err := h.uc.GetRelationType(r.Context(), name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRelationTypeItem(rt))
}

func (h *Handler) createRelationType(w http.ResponseWriter, r *http.Request) {
	var in createRelationTypeReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	rt, err := h.uc.RegisterRelationType(r.Context(), model.RelationType{
		Name:              in.Name,
		DisplayName:       in.DisplayName,
		DisplayNameEN:     in.DisplayNameEN,
		PropagatesFailure: in.PropagatesFailure,
		Direction:         in.Direction,
		SemanticsTag:      in.SemanticsTag,
		Description:       in.Description,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 339：这一行是整个拓扑面后果最重的**一条**。
	//
	// `propagates_failure` 决定这条类型的边上的故障会不会向上游传播。把一个类型
	// 注册成 false，等于让这一整类依赖在根因分析里静默消失——界面上一张图还是
	// 那张图，一条告警该找到上游却找不到，而**没有任何一个地方会报错**。
	// `direction` 决定往哪边传播；它错了，故障会朝相反的方向去找源头。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionTopologyRelationTypeCreate,
		ResourceType: auditport.ResourceTopologyRelationType,
		ResourceID:   rt.Name,
		ResourceName: rt.Name,
		Status:       auditport.StatusSuccess,
		Payload: map[string]any{
			"propagates_failure": rt.PropagatesFailure,
			"direction":          rt.Direction,
			"semantics_tag":      rt.SemanticsTag,
			"builtin":            rt.Builtin,
		},
	})
	writeJSON(w, http.StatusCreated, toRelationTypeItem(rt))
}

func (h *Handler) deleteRelationType(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	// 决策 339：删一个类型比删一条边严重**一个量级**——它下面的每一条边都还在，
	// 但都失去了名字与语义。所以这一行必须带上被删掉的传播开关：事后要回答的
	// 是「刚刚让哪一类依赖不再参与根因传播」。
	propagates, direction, wasBuiltin := false, "", false
	if rt, err := h.uc.GetRelationType(r.Context(), name); err == nil && rt != nil {
		propagates, direction, wasBuiltin = rt.PropagatesFailure, rt.Direction, rt.Builtin
	}
	if err := h.uc.DeleteRelationType(r.Context(), name); err != nil {
		writeErr(w, err)
		return
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionTopologyRelationTypeDelete,
		ResourceType: auditport.ResourceTopologyRelationType,
		ResourceID:   name,
		ResourceName: name,
		Status:       auditport.StatusSuccess,
		Payload: map[string]any{
			"propagates_failure": propagates,
			"direction":          direction,
			"builtin":            wasBuiltin,
		},
	})
	w.WriteHeader(http.StatusNoContent)
}

// ---------- NodeType handlers ----------------------------------------------

type nodeTypeItem struct {
	Name          string `json:"name"`
	DisplayName   string `json:"display_name"`
	DisplayNameEN string `json:"display_name_en,omitempty"`
	Builtin       bool   `json:"builtin"`
	Tier          int    `json:"tier"`
	Description   string `json:"description"`
}

type createNodeTypeReq struct {
	Name          string `json:"name"`
	DisplayName   string `json:"display_name"`
	DisplayNameEN string `json:"display_name_en,omitempty"`
	Tier          int    `json:"tier"`
	Description   string `json:"description"`
}

func (h *Handler) listNodeTypes(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	rows, err := h.uc.ListNodeTypes(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]nodeTypeItem, 0, len(rows))
	for _, nt := range rows {
		out = append(out, toNodeTypeItem(nt))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *Handler) getNodeType(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	name := chi.URLParam(r, "name")
	nt, err := h.uc.GetNodeType(r.Context(), name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toNodeTypeItem(nt))
}

func (h *Handler) createNodeType(w http.ResponseWriter, r *http.Request) {
	var in createNodeTypeReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	nt, err := h.uc.RegisterNodeType(r.Context(), model.NodeType{
		Name:          in.Name,
		DisplayName:   in.DisplayName,
		DisplayNameEN: in.DisplayNameEN,
		Tier:          in.Tier,
		Description:   in.Description,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// 决策 339：节点类型比关系类型轻一档——它决定**图例与分层**（tier），
	// 不参与推理。但 `builtin` 决定这一类能不能删，而删了内置类型的后果由
	// 谁承担是可以有争议的，所以它进链。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionTopologyNodeTypeCreate,
		ResourceType: auditport.ResourceTopologyNodeType,
		ResourceID:   nt.Name,
		ResourceName: nt.Name,
		Status:       auditport.StatusSuccess,
		Payload: map[string]any{
			"tier":    nt.Tier,
			"builtin": nt.Builtin,
		},
	})
	writeJSON(w, http.StatusCreated, toNodeTypeItem(nt))
}

func (h *Handler) deleteNodeType(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	// 决策 339：先读 tier 与 builtin 再删。tier 决定这一类节点画在图上的哪一层，
	// 删完之后链上只剩一个名字，而「刚刚从第几层拿掉了一类节点」答不出来。
	tier, wasBuiltin := 0, false
	if nt, err := h.uc.GetNodeType(r.Context(), name); err == nil && nt != nil {
		tier, wasBuiltin = nt.Tier, nt.Builtin
	}
	if err := h.uc.DeleteNodeType(r.Context(), name); err != nil {
		writeErr(w, err)
		return
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionTopologyNodeTypeDelete,
		ResourceType: auditport.ResourceTopologyNodeType,
		ResourceID:   name,
		ResourceName: name,
		Status:       auditport.StatusSuccess,
		Payload: map[string]any{
			"tier":    tier,
			"builtin": wasBuiltin,
		},
	})
	w.WriteHeader(http.StatusNoContent)
}

func toNodeTypeItem(nt *model.NodeType) nodeTypeItem {
	return nodeTypeItem{
		Name:          nt.Name,
		DisplayName:   nt.DisplayName,
		DisplayNameEN: nt.DisplayNameEN,
		Builtin:       nt.Builtin,
		Tier:          nt.Tier,
		Description:   nt.Description,
	}
}

// ---------- helpers ---------------------------------------------------------

func toNodeItem(n *model.Node) nodeItem {
	return nodeItem{
		ID:        n.ID,
		Type:      n.Type,
		Name:      n.Name,
		Props:     decodeProps(n.PropsJSON),
		CreatedAt: n.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt: n.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

func toRelationItem(r *model.Relation) relationItem {
	return relationItem{
		ID:        r.ID,
		SrcID:     r.SrcID,
		DstID:     r.DstID,
		Type:      r.Type,
		Props:     decodeProps(r.PropsJSON),
		CreatedAt: r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

func toRelationTypeItem(rt *model.RelationType) relationTypeItem {
	return relationTypeItem{
		Name:              rt.Name,
		DisplayName:       rt.DisplayName,
		DisplayNameEN:     rt.DisplayNameEN,
		Builtin:           rt.Builtin,
		PropagatesFailure: rt.PropagatesFailure,
		Direction:         rt.Direction,
		SemanticsTag:      rt.SemanticsTag,
		Description:       rt.Description,
	}
}

// encodeProps serialises the request-time `props` field (any JSON value
// the client sent — typically an object) back to a JSON string for
// storage. nil maps to "".
func encodeProps(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", errors.Join(errs.ErrInvalid, err)
	}
	return string(b), nil
}

// decodeProps inverts encodeProps for the response side. Returns nil
// (so the field can be omitted via omitempty) when storage is empty.
func decodeProps(s string) any {
	if s == "" {
		return nil
	}
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		// Store raw string fallback — better than dropping silently if
		// somebody bypassed the API and inserted garbage.
		return s
	}
	return out
}

func parseID(r *http.Request, key string) (uint64, error) {
	raw := chi.URLParam(r, key)
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, errors.Join(errs.ErrInvalid, err)
	}
	return id, nil
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
	status := errs.HTTPStatus(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
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
	case errors.Is(err, errs.ErrConflict):
		return "conflict"
	case errors.Is(err, errs.ErrNotWiredYet):
		return "not-wired-yet"
	default:
		return "internal"
	}
}
