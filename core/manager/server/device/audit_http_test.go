package device

import (
	"context"
	"encoding/json"
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

	devicebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/device"
	devicestore "github.com/vincent-wuhan/opskeeper/core/manager/data/device/store"
	devicemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/device"
)

// nopRevoker stands in for the edge store during a device delete. The real one
// tombstones edge credentials; here we only need it to not be nil.
type nopRevoker struct{ revoked [][]uint64 }

func (n *nopRevoker) RevokeIdentities(_ context.Context, _ *gorm.DB, ids []uint64) error {
	n.revoked = append(n.revoked, ids)
	return nil
}

func newAuditHandler(t *testing.T) (*Handler, *nopRevoker, uint64) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := devicestore.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rev := &nopRevoker{}
	repo := devicestore.NewRepo(db, rev)
	uc := devicebiz.NewUsecase(repo, nil, slog.New(slog.DiscardHandler))
	h := NewHandler(uc)

	d, err := repo.FindOrCreateByFingerprint(context.Background(), &devicemodel.Device{
		Fingerprint: "fp-abc",
		Name:        "db-node-01",
		Description: "华东主库",
		Hostname:    "db-node-01.bj.internal",
		OS:          "linux",
		Arch:        "arm64",
		Roles:       devicemodel.EncodeRoles([]string{devicemodel.RoleServer}),
	})
	if err != nil {
		t.Fatalf("seed device: %v", err)
	}
	return h, rev, d.ID
}

func auditCall(t *testing.T, h *Handler, method, path, body, role string) (*httptest.ResponseRecorder, auditport.Event, bool) {
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

func pload(t *testing.T, ev auditport.Event) map[string]any {
	t.Helper()
	p, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map", ev.Payload)
	}
	return p
}

func setRoles(t *testing.T, h *Handler, id uint64, names ...string) {
	t.Helper()
	quoted, _ := json.Marshal(names)
	if rec, _, _ := auditCall(t, h, "PATCH", "/v1/devices/"+strconv.FormatUint(id, 10)+"/roles",
		`{"roles":`+string(quoted)+`}`, "admin"); rec.Code != http.StatusNoContent {
		t.Fatalf("set roles = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func readRoles(t *testing.T, h *Handler, id uint64) []string {
	t.Helper()
	rec, _, _ := auditCall(t, h, "GET", "/v1/devices/"+strconv.FormatUint(id, 10), "", "user")
	var item deviceItem
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	return item.Roles
}

// TestDeviceRolesRowCarriesBothSides is the load-bearing one for 决策 342.
//
// This is the only write route in the repository that changes what a thing is
// **allowed to do**: a device's roles decide which tools it carries and which
// assets it can see. The question asked after any incident is "what role did
// it have at the time", and a row carrying only the new set cannot answer it.
func TestDeviceRolesRowCarriesBothSides(t *testing.T) {
	h, _, id := newAuditHandler(t)

	rec, ev, set := auditCall(t, h, "PATCH", "/v1/devices/"+strconv.FormatUint(id, 10)+"/roles",
		`{"roles":["database","storage"]}`, "admin")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("roles = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("role change left no audit event")
	}
	if ev.Action != auditport.ActionDeviceRolesSet {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionDeviceRolesSet)
	}
	if ev.ResourceType != auditport.ResourceDevice {
		t.Fatalf("resource type = %q, want %q", ev.ResourceType, auditport.ResourceDevice)
	}
	if ev.ResourceID != strconv.FormatUint(id, 10) {
		t.Errorf("resource id = %q, want %d", ev.ResourceID, id)
	}
	if ev.ResourceName != "db-node-01" {
		t.Errorf("name = %q, want the device name", ev.ResourceName)
	}
	p := pload(t, ev)

	before, _ := json.Marshal(p["roles_before"])
	if string(before) != `["server"]` {
		t.Errorf("roles_before = %s, want [\"server\"]", before)
	}
	after, _ := json.Marshal(p["roles_after"])
	// The order is DecodeRoles' canonical one (server, storage, network,
	// database), **not** the order the request happened to use. That is the
	// right shape for a chain: two requests naming the same roles produce
	// byte-identical rows, so "did the roles change" is answerable by reading
	// rather than by comparing sets.
	if string(after) != `["storage","database"]` {
		t.Errorf("roles_after = %s, want the canonical [\"storage\",\"database\"]", after)
	}
	// Granting and revoking are opposite events with opposite follow-ups, and
	// the row has to say which one this was.
	granted, _ := json.Marshal(p["granted"])
	if string(granted) != `["storage","database"]` {
		t.Errorf("granted = %s, want the two newly added roles in canonical order", granted)
	}
	revoked, _ := json.Marshal(p["revoked"])
	if string(revoked) != `["server"]` {
		t.Errorf("revoked = %s, want [\"server\"]", revoked)
	}
	if got, ok := p["changed"].(bool); !ok || !got {
		t.Errorf("changed = %v (present=%v), want true", p["changed"], ok)
	}
}

// TestDeviceRolesRowSaysWhenNothingChanged: setting the roles a device already
// has is still an operation somebody performed, and an absent `changed` key
// reads as "nobody has ever looked at this device's roles".
func TestDeviceRolesRowSaysWhenNothingChanged(t *testing.T) {
	h, _, id := newAuditHandler(t)
	rec, ev, set := auditCall(t, h, "PATCH", "/v1/devices/"+strconv.FormatUint(id, 10)+"/roles",
		`{"roles":["server"]}`, "admin")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("roles = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("role change left no audit event")
	}
	p := pload(t, ev)
	raw, ok := p["changed"]
	if !ok {
		t.Fatal("row has no changed key")
	}
	if got, isBool := raw.(bool); !isBool || got {
		t.Errorf("changed = %v (bool=%v), want false", raw, isBool)
	}
	// The order of the request must not read as a change.
	setRoles(t, h, id, "server")
}

// TestTheRequestOrderNeverReachesTheChain pins a property of the chain
// rather than of the comparison: two requests naming the same roles produce
// byte-identical rows, whatever order they were written in.
//
// Worth being precise about what this does **not** prove. Making `sameRoles`
// order-*sensitive* still leaves this test green, because `DecodeRoles`
// canonicalises both sides before the comparison ever runs — the two
// properties are separate, and this one only sees the first. That is why
// `sameRoles` stays set-based: it defends the case where `roles_before` came
// from somewhere that does not canonicalise, and a test that cannot see that
// case is not a reason to weaken the helper.
func TestTheRequestOrderNeverReachesTheChain(t *testing.T) {
	h, _, id := newAuditHandler(t)
	setRoles(t, h, id, "server", "database")

	rec, ev, set := auditCall(t, h, "PATCH", "/v1/devices/"+strconv.FormatUint(id, 10)+"/roles",
		`{"roles":["database","server"]}`, "admin")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("roles = %d", rec.Code)
	}
	if !set {
		t.Fatal("no audit row")
	}
	if got, _ := pload(t, ev)["changed"].(bool); got {
		t.Error("reordering the same roles is not a change")
	}
}

// TestDeviceDeleteRowNamesTheDeviceAndSaysCredentialsWentWithIt: the delete
// path revokes the credentials of every linked edge, so the machines behind
// them stop being able to connect. "Why did that host drop off" is answered
// here and nowhere else.
func TestDeviceDeleteRowNamesTheDeviceAndSaysCredentialsWentWithIt(t *testing.T) {
	h, _, id := newAuditHandler(t)
	setRoles(t, h, id, "database")

	rec, ev, set := auditCall(t, h, "DELETE", "/v1/devices/"+strconv.FormatUint(id, 10), "", "admin")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("delete left no audit event")
	}
	if ev.Action != auditport.ActionDeviceDelete {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionDeviceDelete)
	}
	if ev.ResourceName != "db-node-01" {
		t.Fatalf("row name = %q, want the device name", ev.ResourceName)
	}
	p := pload(t, ev)
	if p["hostname"] != "db-node-01.bj.internal" {
		t.Errorf("hostname = %v, want the host that just went dark", p["hostname"])
	}
	roles, _ := json.Marshal(p["roles"])
	if string(roles) != `["database"]` {
		t.Errorf("roles = %s, want what it could do at the moment it was removed", roles)
	}
	for _, key := range []string{"linked_edges_removed", "linked_credentials_revoked"} {
		raw, ok := p[key]
		if !ok {
			t.Errorf("row has no %s key", key)
			continue
		}
		if got, isBool := raw.(bool); !isBool || !got {
			t.Errorf("%s = %v, want true", key, raw)
		}
	}
}

// TestDeviceUpdateRowCarriesTheOldName: the fingerprint and the id survive a
// rename, so every edge still points at this device — but a reader of the
// chain only has the new name.
func TestDeviceUpdateRowCarriesTheOldName(t *testing.T) {
	h, _, id := newAuditHandler(t)
	rec, ev, set := auditCall(t, h, "PATCH", "/v1/devices/"+strconv.FormatUint(id, 10),
		`{"name":"db-node-01（已下线）"}`, "admin")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("update = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !set {
		t.Fatal("update left no audit event")
	}
	if ev.Action != auditport.ActionDeviceUpdate {
		t.Fatalf("action = %q, want %q", ev.Action, auditport.ActionDeviceUpdate)
	}
	if ev.ResourceName != "db-node-01（已下线）" {
		t.Errorf("name = %q, want the new name", ev.ResourceName)
	}
	p := pload(t, ev)
	if p["renamed_from"] != "db-node-01" {
		t.Errorf("renamed_from = %v, want db-node-01", p["renamed_from"])
	}
	if p["hostname"] != "db-node-01.bj.internal" {
		t.Errorf("hostname = %v, want the host facts unchanged by a rename", p["hostname"])
	}
	// This request changed the name and nothing else, so the flag must read
	// false rather than be absent.
	raw, ok := p["description_changed"]
	if !ok {
		t.Fatal("row has no description_changed key")
	}
	if got, isBool := raw.(bool); !isBool || got {
		t.Errorf("description_changed = %v (bool=%v), want false", raw, isBool)
	}
}

// TestDeniedDeviceWriteClaimsNothing: a 403 must not look like an audited
// permission change.
func TestDeniedDeviceWriteClaimsNothing(t *testing.T) {
	h, _, id := newAuditHandler(t)
	rec, ev, set := auditCall(t, h, "PATCH", "/v1/devices/"+strconv.FormatUint(id, 10)+"/roles",
		`{"roles":["database"]}`, "user")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin roles = %d, want 403", rec.Code)
	}
	if set {
		t.Fatalf("denied write claimed an audit row: %+v", ev)
	}
}

var _ = readRoles
