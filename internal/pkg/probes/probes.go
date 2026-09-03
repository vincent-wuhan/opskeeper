// Package probes implements split health/readiness checks for the
// platform-base-ha HA story.
//
// Liveness (/healthz) is always 200 — the process is alive. Readiness
// (/readyz) runs every registered Checker in parallel and returns 200
// only when all pass, so K8s stops routing traffic to a pod that has
// lost its DB or Redis connection.
//
// Design: docs/superpowers/specs/2026-07-15-platform-base-ha-design.md §3.3
package probes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	redis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/leader"
)

// Role is the leader election role label. Mirrors leader.Role so this
// package doesn't need to import leader directly for the JSON marshaling
// path — but we do import it for the WorkersChecker implementation.
type Role = leader.Role

// Checker is the single-method health-check contract. Check must be
// safe to call concurrently and must respect ctx (timeouts are enforced
// by the caller via ReadinessHandler).
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

// Probes holds the set of Checkers that drive /readyz. Liveness does
// not consult them — liveness only proves the process is alive.
type Probes struct {
	checkers []Checker
}

// NewProbes builds a Probes from the given checkers. Duplicate names
// are allowed (last-write-wins in the result map, which is fine for
// diagnostics).
func NewProbes(checkers ...Checker) *Probes {
	return &Probes{checkers: checkers}
}

// CheckResult is one checker's outcome. Serialized into the /readyz
// JSON response so operators and K8s can distinguish "DB down" from
// "Redis down" without reading logs.
type CheckResult struct {
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// ReadinessResponse is the /readyz body. Role / InstanceID / LeaderID
// are populated by the handler closure (which closes over the leader
// Manager); Checks is populated by running every Checker.
type ReadinessResponse struct {
	Ready      bool                   `json:"ready"`
	Role       string                 `json:"role"`        // leader / follower / draining
	LeaderID   string                 `json:"leader_id"`   // current leader instance_id (best-effort)
	InstanceID string                 `json:"instance_id"` // this pod's instance_id
	Checks     map[string]CheckResult `json:"checks"`
}

// LivenessHandler always returns 200 + "ok". It does NOT consult
// checkers — liveness only proves the HTTP server goroutine is
// schedulable. A deadlocked process that can't serve /healthz is
// caught by K8s's timeout, not by a checker.
func (p *Probes) LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

// ReadinessHandler runs every Checker in parallel with a per-check
// timeout (default 500ms; configurable via the ctx passed to each
// Check). Returns 200 + JSON when all pass, 503 + JSON when any fail.
//
// role / isLeader / workersRunning / instanceID / leaderID are passed
// as closures so the handler doesn't need a direct leader.Manager
// reference (keeping the package free of a hard dep on leader for
// the HTTP path). When isLeader is nil the role is derived from
// draining state.
func (p *Probes) ReadinessHandler(
	instanceID string,
	role func() string,
	leaderID func() string,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		results := p.RunChecks(r.Context())

		allOK := true
		for _, res := range results {
			if !res.OK {
				allOK = false
				break
			}
		}

		resp := ReadinessResponse{
			Ready:      allOK,
			Checks:     results,
			InstanceID: instanceID,
		}
		if role != nil {
			resp.Role = role()
		}
		if leaderID != nil {
			resp.LeaderID = leaderID()
		}

		status := http.StatusOK
		if !allOK {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// runCheckers executes every checker in parallel, each with a 500ms
// timeout. Returns a map keyed by checker Name.
func (p *Probes) RunChecks(parent context.Context) map[string]CheckResult {
	if len(p.checkers) == 0 {
		return map[string]CheckResult{}
	}

	type result struct {
		name string
		res  CheckResult
	}

	var wg sync.WaitGroup
	ch := make(chan result, len(p.checkers))

	for _, c := range p.checkers {
		wg.Add(1)
		go func(c Checker) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(parent, 500*time.Millisecond)
			defer cancel()

			start := time.Now()
			err := c.Check(ctx)
			latency := time.Since(start).Milliseconds()

			res := CheckResult{
				OK:        err == nil,
				LatencyMs: latency,
			}
			if err != nil {
				res.Error = err.Error()
			}
			ch <- result{name: c.Name(), res: res}
		}(c)
	}

	wg.Wait()
	close(ch)

	out := make(map[string]CheckResult, len(p.checkers))
	for r := range ch {
		out[r.name] = r.res
	}
	return out
}

// ---- Built-in Checkers ----

// DBChecker pings the database via gorm. 500ms timeout is enforced by
// the runCheckers harness; the checker itself adds no extra timeout.
func DBChecker(db *gorm.DB) Checker {
	return &dbChecker{db: db}
}

type dbChecker struct {
	db *gorm.DB
}

func (c *dbChecker) Name() string { return "db" }
func (c *dbChecker) Check(ctx context.Context) error {
	if c.db == nil {
		return fmt.Errorf("db: gorm.DB is nil")
	}
	sqlDB, err := c.db.DB()
	if err != nil {
		return fmt.Errorf("db: get *sql.DB: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("db: ping: %w", err)
	}
	return nil
}

// RedisChecker pings Redis. 200ms timeout enforced inside Check
// (Redis ping should be sub-millisecond on a healthy connection).
func RedisChecker(cli *redis.Client) Checker {
	return &redisChecker{cli: cli}
}

type redisChecker struct {
	cli *redis.Client
}

func (c *redisChecker) Name() string { return "redis" }
func (c *redisChecker) Check(ctx context.Context) error {
	if c.cli == nil {
		return fmt.Errorf("redis: client is nil")
	}
	pingCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if err := c.cli.Ping(pingCtx).Err(); err != nil {
		return fmt.Errorf("redis: ping: %w", err)
	}
	return nil
}

// WorkersChecker verifies that the local replica is healthy with
// respect to its leader-only worker obligations. A follower replica
// (IsLeaderAny=false) always passes — it has no worker obligation.
// A leader replica (IsLeaderAny=true) passes only if every registered
// role reports running=true. This catches the "elected but worker
// crashed" failure mode that a pure DB/Redis ping would miss.
func WorkersChecker(mgr *leader.Manager) Checker {
	return &workersChecker{mgr: mgr}
}

type workersChecker struct {
	mgr *leader.Manager
}

func (c *workersChecker) Name() string { return "workers" }
func (c *workersChecker) Check(_ context.Context) error {
	if c.mgr == nil {
		return nil // no leader manager → single-replica mode, skip
	}
	// Follower replicas have no worker obligation.
	if !c.mgr.IsLeaderAny() {
		return nil
	}
	// Leader replica: every registered role must be running.
	running := c.mgr.WorkersRunning()
	for role, isRunning := range running {
		if !isRunning {
			return fmt.Errorf("workers: leader role %q not running", role)
		}
	}
	return nil
}
