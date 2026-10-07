package probes

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAgentTeamsControllerChecker_OK proves the checker passes when the
// controller returns 2xx (real Controller returns 200 + workers list; we
// accept any 2xx since /readyz is a liveness signal, not a payload check).
func TestAgentTeamsControllerChecker_OK(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workers" {
			t.Errorf("path = %q, want /api/v1/workers", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"workers":[]}`)
	}))
	defer srv.Close()

	c := AgentTeamsControllerChecker(srv.URL, "test-token")
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if c.Name() != "agentteams_controller" {
		t.Errorf("Name = %q, want agentteams_controller", c.Name())
	}
}

// TestAgentTeamsControllerChecker_EmptyURLDegrades proves a single-replica
// deployment without a Controller still gets /readyz=200. Important for
// dev / CI / unit-test environments.
func TestAgentTeamsControllerChecker_EmptyURLDegrades(t *testing.T) {
	t.Parallel()

	c := AgentTeamsControllerChecker("", "")
	if err := c.Check(context.Background()); err != nil {
		t.Errorf("Check with empty URL should skip, got err: %v", err)
	}
}

// TestAgentTeamsControllerChecker_5xxFails proves the checker fails when
// the controller returns a non-2xx status, including the response body
// (truncated) so operators can debug without grepping logs.
func TestAgentTeamsControllerChecker_5xxFails(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `upstream db unavailable`)
	}))
	defer srv.Close()

	c := AgentTeamsControllerChecker(srv.URL, "")
	err := c.Check(context.Background())
	if err == nil {
		t.Fatal("Check: want error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("err = %v, want mention of HTTP 500", err)
	}
	if !strings.Contains(err.Error(), "upstream db") {
		t.Errorf("err = %v, want body fragment 'upstream db' in error message", err)
	}
}

// TestAgentTeamsControllerChecker_404Fails covers the "controller not yet
// deployed" mode (e.g. new env where the route hasn't been wired yet).
func TestAgentTeamsControllerChecker_404Fails(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := AgentTeamsControllerChecker(srv.URL, "")
	if err := c.Check(context.Background()); err == nil {
		t.Fatal("Check: want error on 404, got nil")
	}
}

// TestAgentTeamsControllerChecker_NetworkErrorFails simulates a controller
// that is DNS-unreachable or has its port closed. The dial fails inside
// the http.Client.Do, which must surface as a non-nil error.
func TestAgentTeamsControllerChecker_NetworkErrorFails(t *testing.T) {
	t.Parallel()

	// Bind a listener and immediately close it so the URL points at a
	// port that refuses connections (no flakes from random high ports).
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	// Use a tighter http.Client timeout than the default 400ms so the test
	// fails fast on slow CI runners.
	c := &agentTeamsControllerChecker{
		url:         "http://" + addr,
		bearerToken: "",
		httpClient:  &http.Client{Timeout: 200 * time.Millisecond},
	}
	err = c.Check(context.Background())
	if err == nil {
		t.Fatal("Check on dead address: want error, got nil")
	}
	if !strings.Contains(err.Error(), "agentteams_controller:") {
		t.Errorf("err = %v, want prefix 'agentteams_controller:'", err)
	}
}

// TestAgentTeamsControllerChecker_BodyTruncationAt4KB proves we cap the
// captured body at 4 KB so a misbehaving controller cannot fill the
// /readyz response with megabytes of HTML.
func TestAgentTeamsControllerChecker_BodyTruncationAt4KB(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("X", 16*1024) // 16 KB of garbage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, huge)
	}))
	defer srv.Close()

	c := AgentTeamsControllerChecker(srv.URL, "")
	err := c.Check(context.Background())
	if err == nil {
		t.Fatal("Check: want error on 502, got nil")
	}
	// We can't assert the captured body is exactly 4096 bytes — that
	// depends on the http transport — but the error string must be
	// under a sane ceiling (so we don't accidentally include 16 KB in
	// every /readyz response).
	if len(err.Error()) > 8*1024 {
		t.Errorf("err message length = %d, want <= 8 KB (we cap body at 4 KB)", len(err.Error()))
	}
}

// TestAgentTeamsControllerChecker_ContextCancel proves the checker
// respects context cancellation. Otherwise a hung controller could
// wedge /readyz for 5 minutes (K8s default timeout) before giving up.
func TestAgentTeamsControllerChecker_ContextCancel(t *testing.T) {
	t.Parallel()

	// Server that hangs forever.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		select {}
	}))
	defer srv.Close()

	c := AgentTeamsControllerChecker(srv.URL, "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before request

	err := c.Check(ctx)
	if err == nil {
		t.Fatal("Check on cancelled ctx: want error, got nil")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
