// crystallized.go — the review surface for cost crystallisation.
//
// The ledger (biz/aiops/crystallize) decides which (fault, fix) patterns have
// earned a runbook, and DraftFor renders the exact pig-ops.yaml a node would
// install. What did not exist until this file was a place for the person who
// has to answer "should this run without me?" to see the claim. The ledger was
// reachable only from tests, which is the same shape the whole crystallisation
// path had before decision 159: a mechanism with no observer.
//
// The surface lives in the aiops domain, next to the crystalliser it reads,
// rather than in the loop domain that feeds it. The dependency direction is
// aiops -> loop (declared, decision 117), and the loop server putting a
// review endpoint on a package in the aiops domain would have added the
// reverse edge and closed a cycle between two things that then cannot evolve
// independently. The route paths keep the /v1/loops prefix because that is
// what an operator is looking at — the crystallised patterns are the loop's
// own history — but the code sits where the domain edge points.
//
// Routes, all admin-only and mounted by aiops.Handler.Register:
//
//	GET  /v1/loops/crystallized                 — every promoted pattern.
//	GET  /v1/loops/crystallized/{name}          — one pattern + its document.
//	POST /v1/loops/crystallized/{name}/promote  — write the draft for review.
//
// Rendering and admitting are separate acts. The promote step writes into the
// package-review root the operator configured and stops there; the draft then
// travels the same review and signature channel as every other package.
package aiops

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/crystallize"
	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
)

// PatternReader is the crystalliser's promotion surface.
//
// It is a narrow interface rather than *crystallize.Ledger for the same
// reason the rest of this handler uses seams: the endpoint needs to list
// promoted patterns and render one draft, and nothing about the ledger's
// mutation half should be reachable from an HTTP handler. *crystallize.Ledger
// satisfies it; a test can satisfy it with a literal.
type PatternReader interface {
	// Promoted returns the patterns that earned a draft and still hold it.
	// Retired patterns are not listed: a claim that was taken back is a
	// different question from a claim that is live.
	Promoted() []crystallize.Run
	// DraftFor renders one promoted pattern as the package it would install.
	DraftFor(r crystallize.Run) (crystallize.Draft, error)
	// Policy is the promotion rule in force, so the console can say "three
	// clean runs" without hardcoding a number the operator may have changed.
	Policy() crystallize.Policy
}

// SetPatterns wires the crystalliser's promotion surface after construction.
// Nil is allowed and leaves the crystallised routes at 503: the pattern list
// is a property of this process's ledger, and a manager that never populated
// one must say so rather than serve an empty list that reads as "nothing has
// been promoted anywhere".
//
// The moment of wiring is recorded, because it is the start of this
// manager's observation. That sentence is doing real work: the 503 above
// covers one of the two ways this list comes back empty, and the other one
// is a manager that restarted and is now serving a perfectly well-formed
// empty list whose true reading is "nothing in the window that began when
// this process booted". Before this, an operator who let a pattern be
// promoted, deployed, and came back to an empty list had no way to tell a
// lost ledger from a fleet that had never earned one — and the two call for
// opposite responses. Recording the window is what turns the second reading
// from a guess into a statement the response makes on its own.
func (h *Handler) SetPatterns(p PatternReader) {
	h.patterns = p
	h.patternsSince = time.Time{}
	if p != nil {
		h.patternsSince = time.Now().UTC()
	}
}

// SetDraftRoot names the directory a promoted draft is written into for
// review. Empty leaves the promote route at 503: a draft is a file an
// operator reads, and a file has to go where an operator is looking.
func (h *Handler) SetDraftRoot(dir string) { h.draftRoot = dir }

// CrystallizedTrigger is the detection signal as wire JSON.
type CrystallizedTrigger struct {
	Kind      string  `json:"kind"`
	Metric    string  `json:"metric,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
}

// CrystallizedPattern is one promoted (fault, fix) pair, as an approver reads
// it. Every field is either observed evidence or the frozen grant; none of it
// is re-derived here.
type CrystallizedPattern struct {
	// Name is the package the pattern would emit, and the {name} the detail
	// route addresses. It is the stable identity across requests.
	Name string `json:"name"`
	// Action is the declaration's action name, which is what appears in the
	// node's audit rows when it fires.
	Action string `json:"action"`

	FaultKind string `json:"fault_kind"`
	Family    string `json:"family,omitempty"`

	Tool  string   `json:"tool"`
	Class string   `json:"class"`
	Argv  []string `json:"argv"`
	// Target is the resource the fix was proved on. A runbook is for the
	// target it was proved on, so it is shown, not summarised.
	Target  string              `json:"target"`
	Trigger CrystallizedTrigger `json:"trigger"`

	// BlastRadius and TTLSeconds are the grant frozen at promotion. A later,
	// wider grant is a new review rather than a new observation, so these do
	// not move.
	BlastRadius string `json:"blast_radius"`
	TTLSeconds  int    `json:"ttl_seconds"`
	SafetyLevel string `json:"safety_level"`

	Streak     int `json:"streak"`
	Verified   int `json:"verified"`
	Attempts   int `json:"attempts"`
	Rejections int `json:"rejections"`

	FirstSeen  string `json:"first_seen"`
	LastSeen   string `json:"last_seen"`
	PromotedAt string `json:"promoted_at"`
	// Evidence is the provenance ids, so an approver reads the runs the
	// promotion rests on rather than trusting the count.
	Evidence []string `json:"evidence,omitempty"`
}

// CrystallizedPolicy is the promotion rule in force.
type CrystallizedPolicy struct {
	MinCleanStreak int    `json:"min_clean_streak"`
	MaxTTLSeconds  int    `json:"max_ttl_seconds"`
	PackagePrefix  string `json:"package_prefix"`
	Version        string `json:"version"`
	Vendor         string `json:"vendor"`
}

// CrystallizedListResponse is the GET /v1/loops/crystallized body.
type CrystallizedListResponse struct {
	Items  []CrystallizedPattern `json:"items"`
	Total  int                   `json:"total"`
	Policy CrystallizedPolicy    `json:"policy"`
	// ObservingSince is when this manager attached its ledger, and so the
	// start of the window Total counts. It is present on every response, not
	// only the empty one, because a list that is non-empty now is still a
	// list that was empty a moment ago and a reader comparing two snapshots
	// needs the same baseline for both.
	//
	// Empty is written rather than omitted so that a client cannot mistake
	// "this build does not send it" for "this ledger has been observing
	// since the beginning of time". The ledger is in-memory — see
	// crystallize.Ledger for why, and for the replay seam a durable one
	// would use — so the honest bound on this number is this process.
	ObservingSince string `json:"observing_since"`
}

// CrystallizedDetailResponse is the GET /v1/loops/crystallized/{name} body.
type CrystallizedDetailResponse struct {
	Pattern CrystallizedPattern `json:"pattern"`
	// YAML is the exact document an operator would review. It is a string
	// rather than an object because the file, including its provenance
	// comments, is what a reviewer reads and what admission consumes.
	YAML string `json:"yaml"`
}

// CrystallizedPromoteResponse is the POST .../promote body.
type CrystallizedPromoteResponse struct {
	Name string `json:"name"`
	// Dir is where the draft was written. The operator reviews the package
	// there and publishes it through the release routes; this endpoint does
	// not admit it.
	Dir string `json:"dir"`
}

// errCrystallizationNotWired is the 503 the crystallised routes answer before
// SetPatterns is called.
type errCrystallizationNotWired struct{}

func (errCrystallizationNotWired) Error() string {
	return "cost crystallisation is not wired on this manager: no ledger was configured, so there is no pattern list to serve"
}

// crystallized godoc
// @Summary List crystallised patterns awaiting review
// @Description Returns every (fault, fix) pattern the ledger has promoted, with the evidence it was promoted on and the promotion policy in force. Read-only: promotion is a claim the platform may run this fix with no model in the path, and this is where an operator reads it. Answers 503 on a manager that never wired a ledger.
// @Router /v1/loops/crystallized [get]
// @Success 200 {object} aiops.CrystallizedListResponse
func (h *Handler) crystallized(w http.ResponseWriter, r *http.Request) {
	if !requireAdminRole(w, r) {
		return
	}
	if h.patterns == nil {
		writeNotWired(w)
		return
	}
	promoted := h.patterns.Promoted()
	items := make([]CrystallizedPattern, 0, len(promoted))
	for _, run := range promoted {
		items = append(items, h.wirePattern(run))
	}
	p := h.patterns.Policy()
	writeJSON(w, http.StatusOK, CrystallizedListResponse{
		Items:          items,
		Total:          len(items),
		ObservingSince: formatCrystallizedTime(h.patternsSince),
		Policy: CrystallizedPolicy{
			MinCleanStreak: p.MinCleanStreak,
			MaxTTLSeconds:  int(p.MaxTTL / time.Second),
			PackagePrefix:  p.PackagePrefix,
			Version:        p.Version,
			Vendor:         p.Vendor,
		},
	})
}

// crystallizedOne godoc
// @Summary Render one crystallised pattern as a package document
// @Description Returns the pattern and the exact pig-ops.yaml it would install, including its provenance comments. The document is what a reviewer reads and what admission consumes; this route does not admit it.
// @Router /v1/loops/crystallized/{name} [get]
// @Success 200 {object} aiops.CrystallizedDetailResponse
func (h *Handler) crystallizedOne(w http.ResponseWriter, r *http.Request) {
	if !requireAdminRole(w, r) {
		return
	}
	if h.patterns == nil {
		writeNotWired(w)
		return
	}
	name := chi.URLParam(r, "name")
	if name == "" {
		writeErr(w, errs.ErrInvalid)
		return
	}
	for _, run := range h.patterns.Promoted() {
		draft, err := h.patterns.DraftFor(run)
		if err != nil {
			// A run the ledger listed as promoted but refuses to render is
			// a bug in the ledger, not a missing resource.
			writeErr(w, err)
			return
		}
		if draft.Name() != name {
			continue
		}
		yaml, err := draft.YAML()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, CrystallizedDetailResponse{
			Pattern: h.wirePattern(run),
			YAML:    string(yaml),
		})
		return
	}
	writeErr(w, errs.ErrNotFound)
}

// promoteCrystallized godoc
// @Summary Write a crystallised draft into the package-review root
// @Description Renders one promoted pattern into OPSKEEPER_PLUGIN_IMPORT_DIR, where it travels the same review and signature channel as every other package. Refuses to overwrite an existing package. Answers 503 when no review root is configured.
// @Router /v1/loops/crystallized/{name}/promote [post]
// @Success 200 {object} aiops.CrystallizedPromoteResponse
//
// It is the "and now a human can look at it" step. Writing is the only
// side-effecting part of the crystallisation surface, and it is bounded: one
// directory per pattern, refused if it already exists. A promoted pattern can
// be re-promoted only after an operator has moved or deleted the previous
// draft, which is the point — a re-promotion is a second claim, not an
// overwrite of the first one.
func (h *Handler) promoteCrystallized(w http.ResponseWriter, r *http.Request) {
	if !requireAdminRole(w, r) {
		return
	}
	if h.patterns == nil {
		writeNotWired(w)
		return
	}
	if h.draftRoot == "" {
		writeJSON(w, http.StatusServiceUnavailable, errorBody{
			Error: "crystallized drafts have nowhere to go: set OPSKEEPER_PLUGIN_IMPORT_DIR to the package-review root",
			Code:  "not-wired",
		})
		return
	}
	name := chi.URLParam(r, "name")
	if name == "" {
		writeErr(w, errs.ErrInvalid)
		return
	}
	for _, run := range h.patterns.Promoted() {
		draft, err := h.patterns.DraftFor(run)
		if err != nil {
			writeErr(w, err)
			return
		}
		if draft.Name() != name {
			continue
		}
		dir, err := draft.Write(h.draftRoot)
		if err != nil {
			// Draft.Write uses os.Mkdir, so an existing package surfaces as
			// EEXIST; a re-promotion is a conflict, not a silent overwrite
			// of a draft a human may already have edited.
			if errors.Is(err, os.ErrExist) {
				auditPromote(r, auditport.StatusFailure, "conflict", err.Error(),
					ptr(h.wirePattern(run)))
				writeErr(w, errors.Join(errs.ErrConflict, err))
				return
			}
			auditPromote(r, auditport.StatusFailure, "write_failed", err.Error(),
				ptr(h.wirePattern(run)))
			writeErr(w, err)
			return
		}
		auditPromote(r, auditport.StatusSuccess, "", "", ptr(h.wirePattern(run)))
		writeJSON(w, http.StatusOK, CrystallizedPromoteResponse{Name: name, Dir: dir})
		return
	}
	// 名字对不上任何已晋升的模式：这也是一次尝试，而且是最值得看到的一种——
	// 它说明有人在晋升一个不存在的东西（手写错了名字，或者模式已被退役）。
	// 不入账的话，链上只剩"什么都没发生"，而真实发生过一次请求。
	auditPromote(r, auditport.StatusFailure, "not_found", "no promoted pattern has that name", nil)
	writeErr(w, errs.ErrNotFound)
}

// auditPromote puts a promotion on the audit chain.
//
// 载荷直接取自 **wirePattern**——审核页读的那一份视图，而不是另取一套字段。
// 两个来源各写一遍字段，是"审计行说的"和"人看到的"有一天开始不一致的
// 标准做法；这里让它们**只能是同一份**。
//
// 载荷里带 **argv**：那是节点将要逐词执行的那份文档，也就是这个模式
// "不再经过模型"之后系统到底会做什么。事后追问"这个不停重启的服务是谁
// 决定改成现在这样的"，答案就在这一行里——而没有这一行，链上根本没有
// 这个事件。
//
// 它审计的是**晋升**而不是**发布**：草稿落到审核目录之后仍然什么也没发生，
// 真正的上线要走 release 路由，那条路自己已经入账。两行合起来才读得出
// "谁把这份文档变成了 40 台机器上的行为"；分开读则各自成立。
func auditPromote(r *http.Request, status, code, message string, pattern *CrystallizedPattern) {
	payload := map[string]any{}
	var name string
	if pattern != nil {
		name = pattern.Name
		payload["tool"] = pattern.Tool
		payload["class"] = pattern.Class
		// argv 逐词进链：一份文档与它实际会执行的命令之间不应该有转换。
		payload["argv"] = pattern.Argv
		payload["target"] = pattern.Target
		payload["blast_radius"] = pattern.BlastRadius
		payload["ttl_seconds"] = pattern.TTLSeconds
		payload["streak"] = pattern.Streak
		payload["fault_kind"] = pattern.FaultKind
	}
	if name == "" {
		name = chi.URLParam(r, "name")
		payload["name"] = name
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionCrystallizePromote,
		ResourceType: auditport.ResourcePlugin,
		ResourceID:   name,
		ResourceName: name,
		Status:       status,
		Payload:      payload,
		ErrorCode:    code,
		ErrorMessage: message,
	})
}

// ptr is a one-line helper so the audit call sites read as values rather
// than as addresses. A pointer here means "there is a pattern behind this
// event"; nil means the request never reached one.
func ptr[T any](v T) *T { return &v }

// writePatternsRoutes mounts the crystallised surface. Called from Register.
func (h *Handler) writePatternsRoutes(r chi.Router) {
	r.Get("/v1/loops/crystallized", h.crystallized)
	r.Get("/v1/loops/crystallized/{name}", h.crystallizedOne)
	r.Post("/v1/loops/crystallized/{name}/promote", h.promoteCrystallized)
	r.Post("/v1/loops/crystallized/{name}/release", h.releaseCrystallized)
}

// writeNotWired answers 503 for a crystallised route on a manager that never
// wired a ledger.
func writeNotWired(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, errorBody{
		Error: errCrystallizationNotWired{}.Error(),
		Code:  "not-wired",
	})
}

// requireAdminRole gates the crystallised routes on the admin role. The
// pattern list is a claim the platform runs fixes without a model, and the
// document it renders is the exact program a node would execute; reading
// either is an admin act. It is a local helper rather than the loop
// handler's, because the loop package is the other side of the declared
// domain edge.
func requireAdminRole(w http.ResponseWriter, r *http.Request) bool {
	t, ok := tenantctx.From(r.Context())
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return false
	}
	if t.Role != "admin" {
		writeErr(w, errs.ErrForbidden)
		return false
	}
	return true
}

// wirePattern projects a Run into the review view.
//
// It reads the names and the safety level off the rendered draft rather than
// re-deriving them, so the listing and the document can never disagree about
// what would be installed. A ledger that cannot render a draft it just listed
// is a ledger bug; the projection leaves the name empty there and the detail
// route surfaces the render error, rather than inventing a name that would
// address nothing.
// formatCrystallizedTime renders a wire timestamp the way the rest of this
// file does, with one difference in intent: a zero time is an empty string
// rather than year one. ObservingSince is zero only when no ledger is
// attached, and that case answers 503 before reaching here — so an empty
// string means "this build sent nothing", which is the reading a client
// should fall back to, rather than a date in 0001 that looks like data.
func formatCrystallizedTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (h *Handler) wirePattern(run crystallize.Run) CrystallizedPattern {
	a := run.Pattern.Action
	out := CrystallizedPattern{
		FaultKind: run.Pattern.Fault.Kind,
		Family:    run.Pattern.Fault.Family,
		Tool:      a.Tool,
		Class:     string(a.Class),
		Argv:      append([]string(nil), a.Argv...),
		Target:    a.Target,
		Trigger: CrystallizedTrigger{
			Kind:      string(a.Trigger.Kind),
			Metric:    a.Trigger.Metric,
			Threshold: a.Trigger.Threshold,
		},
		BlastRadius: string(run.GrantedRadius),
		TTLSeconds:  int(run.GrantedTTL / time.Second),
		Streak:      run.Streak,
		Verified:    run.Verified,
		Attempts:    run.Attempts,
		Rejections:  run.Rejections,
		Evidence:    append([]string(nil), run.Evidence...),
	}
	if d, err := h.patterns.DraftFor(run); err == nil {
		out.Name = d.Name()
		out.Action = d.ActionName()
		out.SafetyLevel = string(d.Manifest.Spec.SafetyLevel)
	}
	if !run.FirstSeen.IsZero() {
		out.FirstSeen = run.FirstSeen.UTC().Format(time.RFC3339)
	}
	if !run.LastSeen.IsZero() {
		out.LastSeen = run.LastSeen.UTC().Format(time.RFC3339)
	}
	if !run.PromotedAt.IsZero() {
		out.PromotedAt = run.PromotedAt.UTC().Format(time.RFC3339)
	}
	return out
}
