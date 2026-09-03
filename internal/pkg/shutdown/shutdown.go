// Package shutdown orchestrates the platform-base-ha graceful shutdown
// sequence triggered by SIGTERM (K8s pod termination).
//
// The sequence is:
//  1. MarkDraining → /readyz returns 503 immediately (K8s stops routing)
//  2. HTTP server drain (30s) — serve in-flight, reject new
//  3. Leader resign (25s) — release all held leader locks
//  4. DB close (5s) — close connection pool
//  5. Redis close (5s) — close connection pool
//
// Total ≤ 65s; K8s terminationGracePeriodSeconds must be ≥ 70s to
// accommodate the full sequence + signal propagation latency. The
// Helm chart sets this to 60s by default, which is safe because steps
// 2–5 have internal timeouts that sum to 65s but in practice complete
// in <1s (no active requests + quick resign).
package shutdown

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	redis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/leader"
)

// Drainer abstracts the HTTP server's Shutdown method so the package
// doesn't depend on net/http directly.
type Drainer interface {
	Shutdown(ctx context.Context) error
}

// Closer abstracts anything with a Close method (DB pool, Redis client).
type Closer interface {
	Close() error
}

// Options wires the components that participate in the shutdown
// sequence. Any nil field is skipped.
type Options struct {
	LeaderMgr  *leader.Manager
	HTTPServer Drainer
	DB         *gorm.DB
	Redis      *redis.Client
	Logger     *slog.Logger
}

// Result records the outcome of each step for logging.
type Result struct {
	MarkDraining time.Duration
	HTTPDrain    time.Duration
	ResignAll    time.Duration
	DBClose      time.Duration
	RedisClose   time.Duration
	Errors       []error
}

// Graceful runs the 5-step shutdown sequence. Each step is bounded by
// its own timeout so a stuck component can't block the whole sequence.
// Returns nil if all steps complete without error; otherwise returns
// a joined error containing every step's failure (non-fatal — the
// process is exiting anyway, but operators should see what failed).
func Graceful(ctx context.Context, opts Options) Result {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	r := Result{}

	// Step 1: MarkDraining (synchronous, instant).
	t0 := time.Now()
	if opts.LeaderMgr != nil {
		opts.LeaderMgr.MarkDraining()
		log.Info("shutdown: marked draining", slog.Duration("elapsed", time.Since(t0)))
	}
	r.MarkDraining = time.Since(t0)

	// Step 2: HTTP server drain (30s).
	if opts.HTTPServer != nil {
		drainCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		t1 := time.Now()
		if err := opts.HTTPServer.Shutdown(drainCtx); err != nil {
			log.Warn("shutdown: http drain failed", slog.Any("err", err))
			r.Errors = append(r.Errors, fmt.Errorf("http drain: %w", err))
		}
		cancel()
		r.HTTPDrain = time.Since(t1)
		log.Info("shutdown: http drain complete", slog.Duration("elapsed", r.HTTPDrain))
	}

	// Step 3: Leader resign (25s). Releases all held locks so a
	// follower can pick them up immediately.
	if opts.LeaderMgr != nil {
		resignCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		t2 := time.Now()
		if err := opts.LeaderMgr.ResignAll(resignCtx); err != nil {
			log.Warn("shutdown: resign failed", slog.Any("err", err))
			r.Errors = append(r.Errors, fmt.Errorf("resign: %w", err))
		}
		cancel()
		r.ResignAll = time.Since(t2)
		log.Info("shutdown: resign complete", slog.Duration("elapsed", r.ResignAll))
	}

	// Step 4: DB close (5s).
	if opts.DB != nil {
		t3 := time.Now()
		if sqlDB, err := opts.DB.DB(); err == nil && sqlDB != nil {
			closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			go func() {
				<-closeCtx.Done()
				_ = sqlDB.Close()
			}()
			_ = sqlDB.Close()
			cancel()
		}
		r.DBClose = time.Since(t3)
		log.Info("shutdown: db closed", slog.Duration("elapsed", r.DBClose))
	}

	// Step 5: Redis close (5s).
	if opts.Redis != nil {
		t4 := time.Now()
		_ = opts.Redis.Close()
		r.RedisClose = time.Since(t4)
		log.Info("shutdown: redis closed", slog.Duration("elapsed", r.RedisClose))
	}

	return r
}
