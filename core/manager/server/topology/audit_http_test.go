package topology

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	biz "github.com/vincent-wuhan/opskeeper/core/manager/biz/topology"
	store "github.com/vincent-wuhan/opskeeper/core/manager/data/topology/store"
)

func newHandler(t *testing.T) *Handler {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	uc := biz.NewUsecase(
		store.NewNodeRepo(db), store.NewRelationRepo(db),
		store.NewRelationTypeRepo(db), store.NewNodeTypeRepo(db),
		slog.New(slog.DiscardHandler),
	)
	return NewHandler(uc)
}

// call runs one request through the real router with an audit slot installed,
// and hands back the row the handler declared.
func call(t *testing.T, h *Handler, method, path, body, role string) (*httptest.ResponseRecorder, auditport.Event, bool) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	ctx := auditport.WithSlot(r.Context())
	ctx = tenantctx.With(ctx, tenantctx.Tenant{UserID: 7, Role: role})
	r = r.WithContext(ctx)
	router := chi.NewRouter()
	h.Register(router)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	ev, set := auditport.GetAuditEvent(r.Context())
	return rec, ev, set
}

func payloadMap(t *testing.T, ev auditport.Event) map[string]any {
	t.Helper()
	p, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map", ev.Payload)
	}
	return p
}

// mkRelationType registers a custom (non-builtin) relation type, which
// CreateRelation requires and DeleteRelationType refuses to remove.
func mkRelationType(t *testing.T, h *Handler, name string, propagates bool, direction string) {
	t.Helper()
	body := fmt.Sprintf(
		`{"name":%q,"display_name":%q,"propagates_failure":%t,"direction":%q,"semantics_tag":"hard_dep"}`,
		name, name, propagates, direction,
	)
	rec, _, _ := call(t, h, "POST", "/v1/topology/relation-types", body, "admin")
	if rec.Code != http.StatusCreated {
		t.Fatalf("register relation type %s: status = %d, body=%s", name, rec.Code, rec.Body.String())
	}
}

func mkNode(t *testing.T, h *Handler, typ, name string) uint64 {
	t.Helper()
	rec, _, _ := call(t, h, "POST", "/v1/topology/nodes",
		fmt.Sprintf(`{"type":%q,"name":%q}`, typ, name), "admin")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create node status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var item nodeItem
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	return item.ID
}

// TestCreateRelationAuditRowCarriesTheWholeEdge is the load-bearing one for
// 决策 339.
//
// A relation *is* the answer to "does A depend on B". Its src, dst and type
// together are the edge — an id on its own says nothing a reader can use, and
// a name that reads "12 -calls-> 34" is the same fact in a form that survives
// being read aloud.
func TestCreateRelationAuditRowCarriesTheWholeEdge(t *testing.T) {
	h := newHandler(t)
	mkRelationType(t, h, "calls", true, "src_to_dst")
	src := mkNode(t, h, "service", "order-api")
	dst := mkNode(t, h, "service", "pg-primary")

	rec, ev, set := call(t, h, "POST", "/v1/topology/relations",
		`{"src_id":`+strconv.FormatUint(src, 10)+`,"dst_id":`+strconv.FormatUint(dst, 10)+`,"type":"calls"}`, "admin")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create relation status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("create relation left no audit event")
	}
	if ev.Action != auditport.ActionTopologyRelationCreate {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionTopologyRelationCreate)
	}
	if ev.ResourceType != auditport.ResourceTopologyRelation {
		t.Fatalf("resource type = %q, want %q", ev.ResourceType, auditport.ResourceTopologyRelation)
	}
	p := payloadMap(t, ev)
	if got, _ := p["src_id"].(uint64); got != src {
		t.Errorf("src_id = %v, want %d", p["src_id"], src)
	}
	if got, _ := p["dst_id"].(uint64); got != dst {
		t.Errorf("dst_id = %v, want %d", p["dst_id"], dst)
	}
	if p["relation_type"] != "calls" {
		t.Errorf("relation_type = %v, want calls", p["relation_type"])
	}
	if ev.ResourceName == "" || !strings.Contains(ev.ResourceName, "calls") {
		t.Errorf("name = %q, want the edge spelled out", ev.ResourceName)
	}
}

// TestDeleteRelationAuditRowRemembersWhichPathBroke: deleting an edge is what
// makes a correlation query stop reaching. Afterwards the chain holds only an
// autoincrement id.
func TestDeleteRelationAuditRowRemembersWhichPathBroke(t *testing.T) {
	h := newHandler(t)
	mkRelationType(t, h, "reads", true, "src_to_dst")
	src := mkNode(t, h, "service", "checkout")
	dst := mkNode(t, h, "service", "redis-cache")
	rec, _, _ := call(t, h, "POST", "/v1/topology/relations",
		`{"src_id":`+strconv.FormatUint(src, 10)+`,"dst_id":`+strconv.FormatUint(dst, 10)+`,"type":"reads"}`, "admin")
	var rel relationItem
	if err := json.Unmarshal(rec.Body.Bytes(), &rel); err != nil {
		t.Fatal(err)
	}

	rec2, ev, set := call(t, h, "DELETE", "/v1/topology/relations/"+strconv.FormatUint(rel.ID, 10), "", "admin")
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("delete relation status = %d", rec2.Code)
	}
	if !set {
		t.Fatal("delete relation left no audit event")
	}
	if ev.Action != auditport.ActionTopologyRelationDelete {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionTopologyRelationDelete)
	}
	p := payloadMap(t, ev)
	if got, _ := p["src_id"].(uint64); got != src {
		t.Errorf("src_id = %v, want %d —— 删掉之后「哪条路断了」就答不出来了", p["src_id"], src)
	}
	if p["relation_type"] != "reads" {
		t.Errorf("relation_type = %v, want reads", p["relation_type"])
	}
}

// TestCreateRelationTypeAuditRowCarriesThePropagationSwitch is the heaviest row
// in this whole family. `propagates_failure: false` makes an entire class of
// dependency vanish from root-cause analysis with nothing erroring anywhere.
func TestCreateRelationTypeAuditRowCarriesThePropagationSwitch(t *testing.T) {
	h := newHandler(t)
	rec, ev, set := call(t, h, "POST", "/v1/topology/relation-types",
		`{"name":"shards_by","display_name":"分片归属","propagates_failure":false,"direction":"src_to_dst","semantics_tag":"hard_dep"}`, "admin")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create relation type status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("create relation type left no audit event")
	}
	if ev.Action != auditport.ActionTopologyRelationTypeCreate {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionTopologyRelationTypeCreate)
	}
	if ev.ResourceID != "shards_by" {
		t.Fatalf("resource id = %q, want shards_by", ev.ResourceID)
	}
	p := payloadMap(t, ev)
	// **Key presence**: a missing key and a `false` value are both "does not
	// propagate" to anyone scanning the chain, and only one is a fact.
	raw, ok := p["propagates_failure"]
	if !ok {
		t.Fatal("row has no propagates_failure key —— 链上答不出「这一类故障会不会传播」")
	}
	got, isBool := raw.(bool)
	if !isBool {
		t.Fatalf("propagates_failure type = %T, want bool", raw)
	}
	if got {
		t.Error("propagates_failure = true, want false as created")
	}
	if p["direction"] != "src_to_dst" {
		t.Errorf("direction = %v, want src_to_dst", p["direction"])
	}
}

// TestDeleteRelationTypeAuditRowRemembersTheSemanticsThatJustDied: deleting a
// type is one order of magnitude worse than deleting an edge — every relation
// of that type survives but loses its meaning.
func TestDeleteRelationTypeAuditRowRemembersTheSemanticsThatJustDied(t *testing.T) {
	h := newHandler(t)
	rec, _, _ := call(t, h, "POST", "/v1/topology/relation-types",
		`{"name":"replicated_to","display_name":"复制到","propagates_failure":true,"direction":"src_to_dst","semantics_tag":"runtime_dep"}`, "admin")
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed relation type status = %d, body=%s", rec.Code, rec.Body.String())
	}

	rec2, ev, set := call(t, h, "DELETE", "/v1/topology/relation-types/replicated_to", "", "admin")
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("delete relation type status = %d, body=%s", rec2.Code, rec2.Body.String())
	}
	if !set {
		t.Fatal("delete relation type left no audit event")
	}
	if ev.Action != auditport.ActionTopologyRelationTypeDelete {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionTopologyRelationTypeDelete)
	}
	p := payloadMap(t, ev)
	if got, ok := p["propagates_failure"].(bool); !ok || !got {
		t.Errorf("propagates_failure = %v (present=%v), want true —— 删掉的正是这个语义", p["propagates_failure"], ok)
	}
	if p["direction"] != "src_to_dst" {
		t.Errorf("direction = %v, want src_to_dst", p["direction"])
	}
}

// TestDeniedAdminRouteClaimsNothing: a 403 must not look like an audited write.
func TestDeniedAdminRouteClaimsNothing(t *testing.T) {
	h := newHandler(t)
	rec, ev, set := call(t, h, "POST", "/v1/topology/relation-types",
		`{"name":"x","display_name":"x"}`, "user")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403", rec.Code)
	}
	if set {
		t.Fatalf("denied write claimed an audit row: %+v", ev)
	}
}

// TestUpdateNodeAuditRowCarriesTheOldName: a renamed node keeps its id, so every
// edge still points at it — but the chain, read later, only knows the new name.
func TestUpdateNodeAuditRowCarriesTheOldName(t *testing.T) {
	h := newHandler(t)
	id := mkNode(t, h, "service", "order-api-v1")

	rec, ev, set := call(t, h, "PATCH", "/v1/topology/nodes/"+strconv.FormatUint(id, 10),
		`{"name":"order-api-v2"}`, "admin")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("update node status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("update node left no audit event")
	}
	if ev.Action != auditport.ActionTopologyNodeUpdate {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionTopologyNodeUpdate)
	}
	if ev.ResourceName != "order-api-v2" {
		t.Errorf("name = %q, want the new name", ev.ResourceName)
	}
	p := payloadMap(t, ev)
	if p["renamed_from"] != "order-api-v1" {
		t.Errorf("renamed_from = %v, want order-api-v1 —— 边还指着这个 id，链上却只有新名字", p["renamed_from"])
	}
	if p["node_type"] != "service" {
		t.Errorf("node_type = %v, want service", p["node_type"])
	}
	// Key presence again: this request changed neither the name nor the props,
	// so the flag must read `false` rather than be absent. An absent key reads
	// as "nobody has ever looked", which is a different claim.
	raw, ok := p["props_changed"]
	if !ok {
		t.Fatal("row has no props_changed key")
	}
	if got, isBool := raw.(bool); !isBool || got {
		t.Errorf("props_changed = %v (bool=%v), want false", raw, isBool)
	}
}

// TestDeleteNodeAuditRowNamesTheNode: deleting a node erases every edge touching
// it. An id does not say which point disappeared.
func TestDeleteNodeAuditRowNamesTheNode(t *testing.T) {
	h := newHandler(t)
	id := mkNode(t, h, "service", "order-api")

	rec, ev, set := call(t, h, "DELETE", "/v1/topology/nodes/"+strconv.FormatUint(id, 10), "", "admin")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete node status = %d", rec.Code)
	}
	if !set {
		t.Fatal("delete node left no audit event")
	}
	if ev.ResourceName != "order-api" {
		t.Fatalf("row name = %q, want the node name", ev.ResourceName)
	}
	p := payloadMap(t, ev)
	if p["node_type"] != "service" {
		t.Errorf("node_type = %v, want service", p["node_type"])
	}
}
