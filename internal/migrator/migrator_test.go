package migrator

import (
	"context"
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// newTestDB 创建 SQLite 内存数据库。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return db
}

// fakeMigration 测试用假迁移，含自定义 up/down 行为。
type fakeMigration struct {
	version      string
	description  string
	upCalls      *int
	downCalls    *int
	upErr        error
	downErr      error
	irreversible bool
}

func (f *fakeMigration) Version() string     { return f.version }
func (f *fakeMigration) Description() string { return f.description }

func (f *fakeMigration) Up(_ context.Context, _ *gorm.DB) error {
	if f.upCalls != nil {
		*f.upCalls++
	}
	return f.upErr
}

func (f *fakeMigration) Down(_ context.Context, _ *gorm.DB) error {
	if f.downCalls != nil {
		*f.downCalls++
	}
	if f.irreversible {
		return ErrIrreversible
	}
	return f.downErr
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	m := &fakeMigration{version: "20260713100000", description: "init"}
	r.Register(m)
	if r.Count() != 1 {
		t.Errorf("Count=%d want 1", r.Count())
	}
	got, ok := r.Get("20260713100000")
	if !ok || got.Version() != "20260713100000" {
		t.Errorf("Get failed: ok=%v got=%v", ok, got)
	}
	if _, ok := r.Get("not_found"); ok {
		t.Error("Get(not_found) must fail")
	}
}

func TestRegistry_MustRegister_DuplicatePanics(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeMigration{version: "v1", description: "a"})
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustRegister duplicate must panic")
		}
	}()
	r.MustRegister(&fakeMigration{version: "v1", description: "b"})
}

func TestRegistry_AllSorted(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeMigration{version: "20260713", description: "c"})
	r.Register(&fakeMigration{version: "20260711", description: "a"})
	r.Register(&fakeMigration{version: "20260712", description: "b"})
	all := r.All()
	if len(all) != 3 {
		t.Fatalf("All len=%d want 3", len(all))
	}
	if all[0].Version() != "20260711" || all[2].Version() != "20260713" {
		t.Errorf("sort wrong: %v", []string{all[0].Version(), all[1].Version(), all[2].Version()})
	}
}

func TestRunner_ApplyMigrations(t *testing.T) {
	db := newTestDB(t)
	r := NewRegistry()

	upCalls1, upCalls2 := 0, 0
	r.Register(&fakeMigration{version: "20260711100000", description: "create users", upCalls: &upCalls1})
	r.Register(&fakeMigration{version: "20260712100000", description: "create projects", upCalls: &upCalls2})

	runner := NewRunner(db, r)
	ctx := context.Background()

	// 初次 status：2 pending
	status, err := runner.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if len(status.Pending) != 2 {
		t.Errorf("pending=%d want 2", len(status.Pending))
	}

	// 跑 up
	result, err := runner.Run(ctx, RunOptions{Direction: Up})
	if err != nil {
		t.Fatalf("Run up: %v", err)
	}
	if len(result.Applied) != 2 {
		t.Errorf("applied=%d want 2", len(result.Applied))
	}
	if upCalls1 != 1 || upCalls2 != 1 {
		t.Errorf("upCalls: %d, %d want 1, 1", upCalls1, upCalls2)
	}

	// 二次 status：0 pending
	status, _ = runner.GetStatus(ctx)
	if len(status.Pending) != 0 {
		t.Errorf("after up pending=%d want 0", len(status.Pending))
	}
	if len(status.Applied) != 2 {
		t.Errorf("after up applied=%d want 2", len(status.Applied))
	}

	// 二次 up：幂等（不重复执行）
	result2, _ := runner.Run(ctx, RunOptions{Direction: Up})
	if len(result2.Applied) != 0 {
		t.Errorf("idempotent applied=%d want 0", len(result2.Applied))
	}
	if upCalls1 != 1 || upCalls2 != 1 {
		t.Errorf("idempotency: upCalls still %d, %d (must not increment)", upCalls1, upCalls2)
	}
}

func TestRunner_DryRun(t *testing.T) {
	db := newTestDB(t)
	r := NewRegistry()
	upCalls := 0
	r.Register(&fakeMigration{version: "v1", description: "init", upCalls: &upCalls})

	runner := NewRunner(db, r)
	result, err := runner.Run(context.Background(), RunOptions{
		Direction: Up,
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("Run dry-run: %v", err)
	}
	if len(result.Applied) != 1 {
		t.Errorf("dry-run applied=%d want 1", len(result.Applied))
	}
	if upCalls != 0 {
		t.Errorf("dry-run must NOT call Up: calls=%d", upCalls)
	}

	// 实际 schema 未变更：status 仍是 pending
	status, _ := runner.GetStatus(context.Background())
	if len(status.Pending) != 1 {
		t.Errorf("dry-run must not change schema, pending=%d", len(status.Pending))
	}
}

func TestRunner_DownRollback(t *testing.T) {
	db := newTestDB(t)
	r := NewRegistry()
	upCalls, downCalls := 0, 0
	r.Register(&fakeMigration{version: "v1", description: "init", upCalls: &upCalls, downCalls: &downCalls})

	runner := NewRunner(db, r)
	ctx := context.Background()

	// 先 up
	if _, err := runner.Run(ctx, RunOptions{Direction: Up}); err != nil {
		t.Fatalf("up: %v", err)
	}
	if upCalls != 1 {
		t.Errorf("upCalls=%d want 1", upCalls)
	}

	// 再 down
	result, err := runner.Run(ctx, RunOptions{Direction: Down})
	if err != nil {
		t.Fatalf("down: %v", err)
	}
	if len(result.Applied) != 1 {
		t.Errorf("down applied=%d want 1", len(result.Applied))
	}
	if downCalls != 1 {
		t.Errorf("downCalls=%d want 1", downCalls)
	}

	// history 应有 rolled_back
	var rec MigrationRecord
	if err := db.Where("version = ?", "v1").First(&rec).Error; err != nil {
		t.Fatalf("read history: %v", err)
	}
	if rec.Status != "rolled_back" {
		t.Errorf("status=%s want rolled_back", rec.Status)
	}
}

func TestRunner_DownIrreversible(t *testing.T) {
	db := newTestDB(t)
	r := NewRegistry()
	r.Register(&fakeMigration{version: "v1", description: "init", irreversible: true})

	runner := NewRunner(db, r)
	ctx := context.Background()

	if _, err := runner.Run(ctx, RunOptions{Direction: Up}); err != nil {
		t.Fatalf("up: %v", err)
	}
	_, err := runner.Run(ctx, RunOptions{Direction: Down})
	if err == nil {
		t.Fatal("down on irreversible must fail")
	}
	if !errors.Is(err, ErrIrreversible) {
		t.Errorf("error must wrap ErrIrreversible, got: %v", err)
	}
}

func TestRunner_UpStepLimit(t *testing.T) {
	db := newTestDB(t)
	r := NewRegistry()
	r.Register(&fakeMigration{version: "v1", description: "a"})
	r.Register(&fakeMigration{version: "v2", description: "b"})
	r.Register(&fakeMigration{version: "v3", description: "c"})

	runner := NewRunner(db, r)
	result, err := runner.Run(context.Background(), RunOptions{
		Direction: Up,
		Steps:     2, // 只跑前 2 个
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Applied) != 2 {
		t.Errorf("Steps=2 applied=%d want 2", len(result.Applied))
	}

	status, _ := runner.GetStatus(context.Background())
	if len(status.Pending) != 1 {
		t.Errorf("after Steps=2 pending=%d want 1", len(status.Pending))
	}
}

func TestRunner_UpFailure(t *testing.T) {
	db := newTestDB(t)
	r := NewRegistry()
	r.Register(&fakeMigration{version: "v1", description: "ok"})
	r.Register(&fakeMigration{version: "v2", description: "fail", upErr: errors.New("boom")})

	runner := NewRunner(db, r)
	_, err := runner.Run(context.Background(), RunOptions{Direction: Up})
	if err == nil {
		t.Fatal("up must fail when migration errors")
	}

	// v1 应已应用，v2 失败（history 标 failed）
	status, _ := runner.GetStatus(context.Background())
	applied := 0
	for _, r := range status.Applied {
		if r.Version == "v1" {
			applied++
		}
	}
	if applied != 1 {
		t.Errorf("v1 should be applied even though v2 fails, applied=%d", applied)
	}
}
