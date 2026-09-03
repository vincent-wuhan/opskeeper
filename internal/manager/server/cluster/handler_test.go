package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/leader"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/probes"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

// stubLeaderState satisfies LeaderState for unit tests.
type stubLeaderState struct {
	instanceID string
	leaderAny  bool
	draining   bool
	workers    map[leader.Role]bool
}

func (s *stubLeaderState) InstanceID() string                   { return s.instanceID }
func (s *stubLeaderState) IsLeaderAny() bool                    { return s.leaderAny }
func (s *stubLeaderState) IsDraining() bool                     { return s.draining }
func (s *stubLeaderState) WorkersRunning() map[leader.Role]bool { return s.workers }

type stubChecker struct {
	name string
	err  error
}

func (s *stubChecker) Name() string                { return s.name }
func (s *stubChecker) Check(context.Context) error { return s.err }

func withAdmin(req *http.Request, role string) *http.Request {
	ctx := tenantctx.With(req.Context(), tenantctx.Tenant{UserID: 1, Role: role})
	return req.WithContext(ctx)
}

func TestStatusAdminLeader(t *testing.T) {
	state := &stubLeaderState{
		instanceID: "pod-a",
		leaderAny:  true,
		workers: map[leader.Role]bool{
			leader.Role("scheduler:flow"):   true,
			leader.Role("scheduler:report"): true,
		},
	}
	prb := probes.NewProbes(
		&stubChecker{name: "db"},
		&stubChecker{name: "redis"},
	)
	h := NewHandler(state, prb, time.Now().Add(-5*time.Minute))

	w := httptest.NewRecorder()
	req := withAdmin(httptest.NewRequest("GET", "/api/v1/cluster/status", nil), "admin")
	h.status(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp ClusterStatus
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.InstanceID != "pod-a" {
		t.Errorf("InstanceID = %q", resp.InstanceID)
	}
	if resp.Role != "leader" {
		t.Errorf("Role = %q, want leader", resp.Role)
	}
	if resp.LeaderInstanceID != "pod-a" {
		t.Errorf("LeaderInstanceID = %q, want pod-a", resp.LeaderInstanceID)
	}
	if resp.UptimeSeconds < 290 || resp.UptimeSeconds > 310 {
		t.Errorf("UptimeSeconds = %d, want ~300", resp.UptimeSeconds)
	}
	if len(resp.Workers) != 2 {
		t.Errorf("Workers len = %d, want 2", len(resp.Workers))
	}
	if !resp.Workers["scheduler:flow"].Running {
		t.Error("scheduler:flow should be running")
	}
	if len(resp.Dependencies) != 2 {
		t.Errorf("Dependencies len = %d, want 2", len(resp.Dependencies))
	}
}

func TestStatusFollowerRole(t *testing.T) {
	state := &stubLeaderState{
		instanceID: "pod-b",
		leaderAny:  false,
		workers:    map[leader.Role]bool{},
	}
	h := NewHandler(state, nil, time.Now())

	w := httptest.NewRecorder()
	req := withAdmin(httptest.NewRequest("GET", "/api/v1/cluster/status", nil), "admin")
	h.status(w, req)

	var resp ClusterStatus
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Role != "follower" {
		t.Errorf("Role = %q, want follower", resp.Role)
	}
	if resp.LeaderInstanceID != "" {
		t.Errorf("Follower should not report leader_instance_id, got %q", resp.LeaderInstanceID)
	}
}

func TestStatusDrainingRole(t *testing.T) {
	state := &stubLeaderState{
		instanceID: "pod-a",
		leaderAny:  true,
		draining:   true,
	}
	h := NewHandler(state, nil, time.Now())

	w := httptest.NewRecorder()
	req := withAdmin(httptest.NewRequest("GET", "/api/v1/cluster/status", nil), "admin")
	h.status(w, req)

	var resp ClusterStatus
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Role != "draining" {
		t.Errorf("Role = %q, want draining", resp.Role)
	}
}

func TestStatusStandaloneWhenLeaderNil(t *testing.T) {
	h := NewHandler(nil, nil, time.Now())

	w := httptest.NewRecorder()
	req := withAdmin(httptest.NewRequest("GET", "/api/v1/cluster/status", nil), "admin")
	h.status(w, req)

	var resp ClusterStatus
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Role != "standalone" {
		t.Errorf("Role = %q, want standalone", resp.Role)
	}
}

func TestStatusNonAdmin403(t *testing.T) {
	h := NewHandler(nil, nil, time.Now())

	w := httptest.NewRecorder()
	req := withAdmin(httptest.NewRequest("GET", "/api/v1/cluster/status", nil), "viewer")
	h.status(w, req)

	if w.Code != 403 {
		t.Errorf("status = %d, want 403 for non-admin", w.Code)
	}
}

func TestStatusUnauthenticated401(t *testing.T) {
	h := NewHandler(nil, nil, time.Now())

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/cluster/status", nil)
	// No tenantctx → 401
	h.status(w, req)

	if w.Code != 401 {
		t.Errorf("status = %d, want 401 for unauthenticated", w.Code)
	}
}
