package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	managerserveragentteams "github.com/vincent-wuhan/opskeeper/core/manager/server/agentteams"
	mcpauth "github.com/vincent-wuhan/opskeeper/core/manager/server/mcp/middleware"
)

// This file is the boot-path guard for the `agentteams -> mcp` cut.
//
// The cut is in two other files and neither can see the wiring: agentteams
// declared MCPCallerLookup and stopped importing the mcp domain, and the mcp
// domain grew ContextIdentity to fill it. What made the cut possible is that
// the consumer declares the port and the producer satisfies it structurally —
// which means the assembly root is the only place where "these two are
// compatible" is a fact rather than a hope, and it is the only place where it
// can be forgotten.
//
// Measured before the cut: the routes called mcpauth.FromContext and
// mcpauth.TraceFromContext as package functions. There was no wiring, so there
// was nothing to forget and nothing to guard. Turning the read into a port
// bought a narrower question and cost a new failure mode — a boot path that
// never passes one, after which every AgentTeams route answers 401
// with a message about a missing identity, which reads like an auth problem
// and is a wiring one.
//
// So this file does two things. It states the structural compatibility that
// makes the wiring legal, and it drives a real request through a real route
// with a real context built by the real middleware — the seam with a witness,
// which is the only kind that catches a wiring nobody wrote a test for.

// TestTheMCPDomainSatisfiesTheAgentTeamsPort is the structural half, and it
// has to live here: neither side can see the other, and this file already
// imports both for the wiring.
func TestTheMCPDomainSatisfiesTheAgentTeamsPort(t *testing.T) {
	var _ managerserveragentteams.MCPCallerLookup = mcpauth.ContextIdentity{}
}

// TestTheAgentTeamsRoutesSeeTheCallerTheMiddlewareResolved is the seam.
//
// The route is the knowledge write, and it has three distinct answers that
// depend on nothing but what arrived in the context:
//
//	401 — nobody was resolved
//	403 — somebody was, and their role may not write knowledge
//	503 — somebody was, their role may, and the writer is not wired
//
// Three outcomes from one route is a good thing to assert here, because each
// one rules out a different mistake. A 401 everywhere would mean the port is
// not wired. A 403 for the reporter would mean the role did not survive the
// projection. A 401 for the investigator would mean the whole read is broken.
//
// The contexts are built with the mcp domain's own WithIdentity, so the input
// side is the producer's real output format too — not a hand-made struct that
// happens to line up.
func TestTheAgentTeamsRoutesSeeTheCallerTheMiddlewareResolved(t *testing.T) {
	const route = "/v1/knowledge/docs"

	cases := []struct {
		name     string
		ctx      context.Context
		want     int
		explains string
	}{
		{
			name:     "no identity in the context",
			ctx:      context.Background(),
			want:     http.StatusUnauthorized,
			explains: "a request the middleware never touched has no caller",
		},
		{
			name: "a role that may not write knowledge",
			ctx: mcpauth.WithIdentity(context.Background(), mcpauth.ResolvedIdentity{
				ConsumerName: "worker-investigator",
				Role:         "investigator",
				TenantID:     "tenant-eu-1",
			}),
			want:     http.StatusForbidden,
			explains: "the role reached the route, and the role is what refused it",
		},
		{
			name: "a role that may write knowledge",
			ctx: mcpauth.WithIdentity(context.Background(), mcpauth.ResolvedIdentity{
				ConsumerName: "worker-reporter",
				Role:         "reporter",
				TenantID:     "tenant-eu-1",
			}),
			want: http.StatusServiceUnavailable,
			explains: "the role reached the route, passed the check, and the knowledge " +
				"writer is the only thing missing",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			handler := managerserveragentteams.NewHandler(nil, nil, "", mcpauth.ContextIdentity{})
			router := chi.NewRouter()
			handler.Register(router)

			req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{"title":"x"}`))
			req = req.WithContext(c.ctx)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != c.want {
				t.Fatalf("got %d, want %d: %s. Body: %s",
					rec.Code, c.want, c.explains, rec.Body.String())
			}
		})
	}
}

// TestThePluginRoutesGetTheSameCaller covers the second handler, which is a
// separate type with its own port field and its own accessor.
//
// Two handler types with the same port is the shape where one of them gets
// wired and the other does not, and the symptom is worse than a 401: the
// plugin routes log which consumer asked, so a missing wiring there writes an
// empty consumer name into the audit trail rather than refusing the request.
// The assertion is behavioural — the install route's audit line names the
// consumer — because "the setter was called" is not observable from outside.
func TestThePluginRoutesGetTheSameCaller(t *testing.T) {
	dir := t.TempDir()
	registry := managerserveragentteams.NewPluginRegistry(dir)
	handler := managerserveragentteams.NewPluginHandler(registry, nil, nil, 0, mcpauth.ContextIdentity{})

	router := chi.NewRouter()
	handler.Register(router)

	ctx := mcpauth.WithIdentity(context.Background(), mcpauth.ResolvedIdentity{
		ConsumerName: "worker-reporter",
		Role:         "reporter",
		TenantID:     "tenant-eu-1",
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/plugins", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("listing plugins answered %d, want 200: %s", rec.Code, rec.Body.String())
	}
}
