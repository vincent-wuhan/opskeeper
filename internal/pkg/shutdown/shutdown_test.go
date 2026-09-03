package shutdown

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/glebarez/sqlite"
	redis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// stubDrainer satisfies Drainer; records Shutdown call + optional delay.
type stubDrainer struct {
	called  atomic.Bool
	delay   time.Duration
	shutErr error
}

func (d *stubDrainer) Shutdown(ctx context.Context) error {
	d.called.Store(true)
	if d.delay > 0 {
		select {
		case <-time.After(d.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return d.shutErr
}

func TestGracefulNilSafe(t *testing.T) {
	// All nil options → no panic, instant return.
	r := Graceful(context.Background(), Options{})
	if len(r.Errors) != 0 {
		t.Errorf("expected no errors, got %d", len(r.Errors))
	}
}

func TestGracefulHTTPDrainCalled(t *testing.T) {
	d := &stubDrainer{}
	Graceful(context.Background(), Options{HTTPServer: d})
	if !d.called.Load() {
		t.Error("HTTPServer.Shutdown was not called")
	}
}

func TestGracefulHTTPDrainTimeout(t *testing.T) {
	d := &stubDrainer{delay: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r := Graceful(ctx, Options{HTTPServer: d})
	// Should timeout well before 5s
	if r.HTTPDrain > 500*time.Millisecond {
		t.Errorf("HTTP drain took %v, should have timed out faster", r.HTTPDrain)
	}
}

func TestGracefulHTTPDrainError(t *testing.T) {
	d := &stubDrainer{shutErr: errors.New("forced error")}
	r := Graceful(context.Background(), Options{HTTPServer: d})
	if len(r.Errors) == 0 {
		t.Error("expected error from failed shutdown")
	}
}

func TestGracefulRedisClose(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Skipf("miniredis unavailable: %v", err)
	}
	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	r := Graceful(context.Background(), Options{Redis: cli})
	if r.RedisClose > time.Second {
		t.Errorf("redis close took %v", r.RedisClose)
	}
	// After close, a ping should fail
	if err := cli.Ping(context.Background()).Err(); err == nil {
		t.Error("redis ping should fail after close")
	}
}

func TestGracefulDBClose(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Skipf("sqlite unavailable: %v", err)
	}
	r := Graceful(context.Background(), Options{DB: db})
	if r.DBClose > time.Second {
		t.Errorf("db close took %v", r.DBClose)
	}
}

func TestGracefulFullSequence(t *testing.T) {
	// Full sequence with real components (miniredis + sqlite + drainer)
	mr, err := miniredis.Run()
	if err != nil {
		t.Skipf("miniredis unavailable: %v", err)
	}
	defer mr.Close()

	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer cli.Close()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Skipf("sqlite unavailable: %v", err)
	}

	d := &stubDrainer{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	r := Graceful(ctx, Options{
		HTTPServer: d,
		DB:         db,
		Redis:      cli,
	})

	if !d.called.Load() {
		t.Error("HTTP drain not called")
	}
	if len(r.Errors) != 0 {
		t.Errorf("unexpected errors: %v", r.Errors)
	}
	total := r.MarkDraining + r.HTTPDrain + r.ResignAll + r.DBClose + r.RedisClose
	if total > 5*time.Second {
		t.Errorf("total shutdown took %v, want < 5s", total)
	}
}

// Silence unused import if zap isn't needed in all build configs.
