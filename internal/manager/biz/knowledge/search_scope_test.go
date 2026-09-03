package knowledge

import (
	"context"
	"reflect"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/qdrantx"
)

func TestSearchUsesSignedTenantScopeFilter(t *testing.T) {
	vec := &fakeVec{}
	u := &Usecase{vec: vec, embed: fakeEmbed{}}

	if _, err := u.Search(context.Background(), "dns 排查", SearchOptions{TenantID: "tenant-a", Limit: 3}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(vec.searchOpts) != 1 {
		t.Fatalf("search calls = %d, want 1", len(vec.searchOpts))
	}
	got := vec.searchOpts[0].MustMatch["tenant_scopes"]
	want := []string{"global", "tenant:tenant-a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tenant_scopes filter = %#v, want %#v", got, want)
	}
}

func TestSearchRejectsMissingTenant(t *testing.T) {
	u := &Usecase{vec: &fakeVec{}, embed: fakeEmbed{}}
	if _, err := u.Search(context.Background(), "dns", SearchOptions{}); err == nil {
		t.Fatal("Search() without tenant succeeded")
	}
}

func TestManualDocWriteCarriesTenantScope(t *testing.T) {
	vec := &fakeVec{}
	u := &Usecase{vec: vec, embed: fakeEmbed{}}
	if _, err := u.CreateManualDoc(context.Background(), CreateManualDocInput{
		TenantID: "tenant-a", Title: "DNS SOP", Content: "dig example.com",
	}); err != nil {
		t.Fatalf("CreateManualDoc() error = %v", err)
	}
	if len(vec.upserts) != 1 || len(vec.upserts[0]) != 1 {
		t.Fatalf("upserts = %#v", vec.upserts)
	}
	if got := vec.upserts[0][0].Payload["tenant_scopes"]; !reflect.DeepEqual(got, []string{"tenant:tenant-a"}) {
		t.Fatalf("tenant_scopes payload = %#v", got)
	}
}

type scopeMigratingVec struct {
	fakeVec
	points        []qdrantx.SearchHit
	payload       map[string]any
	payloadFilter map[string]any
}

func (v *scopeMigratingVec) Scroll(_ context.Context, _ string, _ qdrantx.ScrollOpts) (*qdrantx.ScrollResult, error) {
	return &qdrantx.ScrollResult{Points: v.points}, nil
}

func (v *scopeMigratingVec) SetPayloadByFilter(_ context.Context, _ string, payload, mustMatch map[string]any) error {
	v.payload = payload
	v.payloadFilter = mustMatch
	return nil
}

func TestMigrateBuiltinVaultTenantScope(t *testing.T) {
	vec := &scopeMigratingVec{points: []qdrantx.SearchHit{{
		ID:      1,
		Payload: map[string]any{"source_type": "vault"},
	}}}
	u := &Usecase{vec: vec}

	migrated, err := u.MigrateBuiltinVaultTenantScope(context.Background())
	if err != nil {
		t.Fatalf("MigrateBuiltinVaultTenantScope() error = %v", err)
	}
	if !migrated {
		t.Fatal("expected migration to run")
	}
	if got := vec.payload["tenant_scopes"]; !reflect.DeepEqual(got, []string{"global"}) {
		t.Fatalf("tenant_scopes payload = %#v", got)
	}
	if got := vec.payloadFilter["source_type"]; got != "vault" {
		t.Fatalf("source filter = %#v", got)
	}
}

func TestMigrateBuiltinVaultTenantScopeSkipsCurrentPoints(t *testing.T) {
	vec := &scopeMigratingVec{points: []qdrantx.SearchHit{{
		ID:      1,
		Payload: map[string]any{"tenant_scopes": []any{"global"}},
	}}}
	u := &Usecase{vec: vec}

	migrated, err := u.MigrateBuiltinVaultTenantScope(context.Background())
	if err != nil {
		t.Fatalf("MigrateBuiltinVaultTenantScope() error = %v", err)
	}
	if migrated || vec.payload != nil {
		t.Fatalf("migration unexpectedly ran: migrated=%v payload=%#v", migrated, vec.payload)
	}
}
