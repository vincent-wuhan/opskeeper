package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/auth"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

func TestNewGitArtifactRuntimeExposesOnlyRuntimeLink(t *testing.T) {
	runtime, err := newGitArtifactRuntime("", nil)
	if err != nil {
		t.Fatalf("newGitArtifactRuntime: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	info, err := runtime.tool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != runtimeLinkToolName || info.Class != "read" {
		t.Fatalf("unexpected tool: %#v", info)
	}
	if strings.Contains(string(info.Parameters), "commit_history") {
		t.Fatalf("placeholder git tools leaked into schema: %s", info.Parameters)
	}
}

func TestNewGitArtifactRuntimeRejectsDirectoryStore(t *testing.T) {
	if _, err := newGitArtifactRuntime(t.TempDir(), nil); err == nil {
		t.Fatal("expected directory store error")
	}
}

func TestGitArtifactRoutesRequireJWT(t *testing.T) {
	runtime, err := newGitArtifactRuntime("", nil)
	if err != nil {
		t.Fatalf("newGitArtifactRuntime: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	signer := auth.NewSigner("test-secret", time.Minute, time.Minute)
	router := chi.NewRouter()
	router.Route("/api", func(api chi.Router) {
		api.Group(func(protected chi.Router) {
			protected.Use(auth.Middleware(signer))
			runtime.RegisterProtected(protected)
		})
	})

	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/v1/runtime-link", strings.NewReader(`{}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	token, err := signer.SignAccess(auth.Claims{UserID: 7, Role: "viewer"})
	if err != nil {
		t.Fatalf("SignAccess: %v", err)
	}
	authorizedReq := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-link", strings.NewReader(`{}`))
	authorizedReq.Header.Set("Authorization", "Bearer "+token)
	authorized := httptest.NewRecorder()
	router.ServeHTTP(authorized, authorizedReq)
	if authorized.Code != http.StatusBadRequest {
		t.Fatalf("authorized status = %d, want %d; body=%s", authorized.Code, http.StatusBadRequest, authorized.Body.String())
	}
}

func TestGitArtifactRuntimeEndToEndAndTenantIsolation(t *testing.T) {
	runtime, err := newGitArtifactRuntime("", nil)
	if err != nil {
		t.Fatalf("newGitArtifactRuntime: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	signer := auth.NewSigner("test-secret", time.Minute, time.Minute)
	router := chi.NewRouter()
	router.Route("/api", func(api chi.Router) {
		api.Group(func(protected chi.Router) {
			protected.Use(auth.Middleware(signer))
			runtime.RegisterProtected(protected)
		})
	})
	tokenFor := func(userID uint64) string {
		t.Helper()
		token, signErr := signer.SignAccess(auth.Claims{UserID: userID, Role: "viewer"})
		if signErr != nil {
			t.Fatalf("SignAccess: %v", signErr)
		}
		return token
	}

	body := `{
  "artifact": {
    "repo_url": "https://example.com/orders.git",
    "commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "branch": "main",
    "artifact_url": "s3://artifacts/orders.bundle",
    "build_at": "2026-07-13T10:00:00Z",
    "meta": {
      "build_id": "build-7",
      "extracted_symbols": [{
        "type": "pg_query",
        "input": {"query": "SELECT * FROM orders WHERE id = $1"},
        "file_path": "internal/orders/repo.go",
        "line_start": 42,
        "line_end": 44,
        "confidence": 0.93
      }]
    }
  }
}`
	post := httptest.NewRequest(http.MethodPost, "/api/v1/git-artifacts", strings.NewReader(body))
	post.Header.Set("Authorization", "Bearer "+tokenFor(7))
	post.Header.Set("X-GitArtifact-Version", "v0")
	postRec := httptest.NewRecorder()
	router.ServeHTTP(postRec, post)
	if postRec.Code != http.StatusCreated {
		t.Fatalf("post status = %d; body=%s", postRec.Code, postRec.Body.String())
	}
	var response struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(postRec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		linkCtx := tenantctx.With(context.Background(), tenantctx.Tenant{UserID: 7})
		result, runErr := runtime.tool.InvokableRun(linkCtx, `{
          "symbol_type":"pg_query",
          "input":{"query":"SELECT * FROM orders WHERE id = $1","database":"orders"}
        }`)
		if runErr != nil {
			t.Fatalf("runtime link: %v", runErr)
		}
		if strings.Contains(result, `"hit":true`) && strings.Contains(result, `"file_path":"internal/orders/repo.go"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runtime link not indexed in time: %s", result)
		}
		time.Sleep(10 * time.Millisecond)
	}
	otherTenantCtx := tenantctx.With(context.Background(), tenantctx.Tenant{UserID: 8})
	otherTenantResult, err := runtime.tool.InvokableRun(otherTenantCtx, `{
      "symbol_type":"pg_query",
      "input":{"query":"SELECT * FROM orders WHERE id = $1"}
    }`)
	if err != nil {
		t.Fatalf("cross-tenant runtime link: %v", err)
	}
	if !strings.Contains(otherTenantResult, `"hit":false`) {
		t.Fatalf("cross-tenant runtime link leaked: %s", otherTenantResult)
	}

	get := httptest.NewRequest(http.MethodGet, "/api/v1/git-artifacts/"+response.Data.ID, nil)
	get.Header.Set("Authorization", "Bearer "+tokenFor(8))
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, get)
	if getRec.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant GET status = %d, want %d; body=%s", getRec.Code, http.StatusForbidden, getRec.Body.String())
	}
}
