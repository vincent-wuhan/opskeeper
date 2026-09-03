package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newStateTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "test.db") + "?_pragma=foreign_keys(1)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// 1. 新 incident Get → (0, nil)
func TestRecoveryStateStoreDB_GetMissing(t *testing.T) {
	db := newStateTestDB(t)
	s := NewRecoveryStateStoreDB(db)

	n, err := s.Get(context.Background(), "I1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 0 {
		t.Errorf("want 0, got %d", n)
	}
}

// 2. Increment 新 incident → 1
func TestRecoveryStateStoreDB_IncrementNew(t *testing.T) {
	db := newStateTestDB(t)
	s := NewRecoveryStateStoreDB(db)

	n, err := s.Increment(context.Background(), "I1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1, got %d", n)
	}
}

// 3. Increment 已有 incident → +1
func TestRecoveryStateStoreDB_IncrementExisting(t *testing.T) {
	db := newStateTestDB(t)
	s := NewRecoveryStateStoreDB(db)
	ctx := context.Background()

	// seed: retry_count=2
	if _, err := s.Increment(ctx, "I1"); err != nil {
		t.Fatalf("first increment: %v", err)
	}
	if _, err := s.Increment(ctx, "I1"); err != nil {
		t.Fatalf("second increment: %v", err)
	}

	n, err := s.Increment(ctx, "I1")
	if err != nil {
		t.Fatalf("third increment: %v", err)
	}
	if n != 3 {
		t.Errorf("want 3, got %d", n)
	}
}

// 4. Reset 已有 incident → 0
func TestRecoveryStateStoreDB_Reset(t *testing.T) {
	db := newStateTestDB(t)
	s := NewRecoveryStateStoreDB(db)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := s.Increment(ctx, "I1"); err != nil {
			t.Fatalf("increment %d: %v", i+1, err)
		}
	}

	if err := s.Reset(ctx, "I1"); err != nil {
		t.Fatalf("reset: %v", err)
	}

	n, err := s.Get(ctx, "I1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if n != 0 {
		t.Errorf("want 0, got %d", n)
	}
}

// 5. Reset 不存在 incident → 不报错（幂等）
func TestRecoveryStateStoreDB_ResetMissing(t *testing.T) {
	db := newStateTestDB(t)
	s := NewRecoveryStateStoreDB(db)

	if err := s.Reset(context.Background(), "I-missing"); err != nil {
		t.Fatalf("want nil err on reset missing, got: %v", err)
	}
}

// 6. 多 incident 隔离：I1 increment 不影响 I2
func TestRecoveryStateStoreDB_MultiIncidentIsolation(t *testing.T) {
	db := newStateTestDB(t)
	s := NewRecoveryStateStoreDB(db)
	ctx := context.Background()

	// seed: I1=2, I2=5
	for i := 0; i < 2; i++ {
		if _, err := s.Increment(ctx, "I1"); err != nil {
			t.Fatalf("I1 increment: %v", err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, err := s.Increment(ctx, "I2"); err != nil {
			t.Fatalf("I2 increment: %v", err)
		}
	}

	if _, err := s.Increment(ctx, "I1"); err != nil {
		t.Fatalf("final I1 increment: %v", err)
	}

	n1, _ := s.Get(ctx, "I1")
	n2, _ := s.Get(ctx, "I2")
	if n1 != 3 {
		t.Errorf("I1 = %d, want 3", n1)
	}
	if n2 != 5 {
		t.Errorf("I2 = %d, want 5", n2)
	}
}

// 7. 高并发 Increment 计数正确（-race）
func TestRecoveryStateStoreDB_ConcurrentIncrement(t *testing.T) {
	db := newStateTestDB(t)
	s := NewRecoveryStateStoreDB(db)
	ctx := context.Background()

	const goroutines = 50
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Increment(ctx, "I1"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent increment err: %v", err)
	}

	n, err := s.Get(ctx, "I1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if n != goroutines {
		t.Errorf("want %d, got %d", goroutines, n)
	}
}

// 8. 空 incidentID 各方法 fail fast
func TestRecoveryStateStoreDB_EmptyIncidentID(t *testing.T) {
	db := newStateTestDB(t)
	s := NewRecoveryStateStoreDB(db)
	ctx := context.Background()

	if _, err := s.Get(ctx, ""); err == nil {
		t.Error("Get: want err on empty")
	}
	if _, err := s.Increment(ctx, ""); err == nil {
		t.Error("Increment: want err on empty")
	}
	if err := s.Reset(ctx, ""); err == nil {
		t.Error("Reset: want err on empty")
	}
}

// 9. nil db 构造 panic
func TestRecoveryStateStoreDB_NilDBPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("want panic on nil db")
		}
	}()
	_ = NewRecoveryStateStoreDB(nil)
}
