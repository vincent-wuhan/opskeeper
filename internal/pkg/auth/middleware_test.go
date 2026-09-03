package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

func TestMiddlewareVerifiesAgentTeamsServiceToken(t *testing.T) {
	signer := NewSigner("test-secret", time.Minute, time.Hour)
	token, err := signer.SignAgentTeamsService(AgentTeamsServiceClaims{
		TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
		AllowedTools: []string{"loop.investigate"},
	}, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	var got tenantctx.Tenant
	handler := Middleware(signer)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		caller, ok := tenantctx.From(r.Context())
		if !ok {
			t.Fatal("authenticated caller missing")
		}
		got = caller
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if got.AgentTeams == nil {
		t.Fatal("AgentTeams identity missing from request context")
	}
	if got.UserID != 0 || got.Role != "" || got.IsSuperuser {
		t.Fatalf("service token leaked human identity: %+v", got)
	}
	if got.AgentTeams.TenantID != "tenant-a" || got.AgentTeams.Service != "agentteams" || got.AgentTeams.Worker != "opskeeper-investigator" || got.AgentTeams.Role != "investigator" {
		t.Fatalf("AgentTeams identity = %+v", *got.AgentTeams)
	}
}

func TestMiddlewareRejectsAgentTeamsServiceTokenOutsideMCP(t *testing.T) {
	signer := NewSigner("test-secret", time.Minute, time.Hour)
	token, err := signer.SignAgentTeamsService(AgentTeamsServiceClaims{
		TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
		AllowedTools: []string{"loop.investigate"},
	}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/companies", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	Middleware(signer)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
}

func TestMiddlewareAllowsAgentTeamsServiceTokenOnMountedMCPath(t *testing.T) {
	signer := NewSigner("test-secret", time.Minute, time.Hour)
	token, err := signer.SignAgentTeamsService(AgentTeamsServiceClaims{
		TenantID: "tenant-a", Service: "agentteams", Worker: "opskeeper-investigator", Role: "investigator",
		AllowedTools: []string{"loop.investigate"},
	}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	Middleware(signer)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

func TestSignAccessRejectsAgentTeamsServiceClaims(t *testing.T) {
	signer := NewSigner("test-secret", time.Minute, time.Hour)
	_, err := signer.SignAccess(Claims{AgentTeams: &AgentTeamsServiceClaims{
		TenantID: "tenant-a", Service: "agentteams", Worker: "investigator-1", Role: "investigator",
		AllowedTools: []string{"loop.investigate"},
	}})
	if err == nil {
		t.Fatal("SignAccess accepted service claims")
	}
}

func TestMiddlewareRejectsWrongServiceTokenSignature(t *testing.T) {
	signer := NewSigner("trusted-secret", time.Minute, time.Hour)
	forgingSigner := NewSigner("attacker-secret", time.Minute, time.Hour)
	token, err := forgingSigner.SignAccess(Claims{UserID: 77, Role: "user"})
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	Middleware(signer)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", recorder.Code)
	}
}
