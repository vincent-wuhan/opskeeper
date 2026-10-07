package runner

import (
	"context"
	"sync"
	"testing"
)

// TestHarnessSupervisorLifecycle 验证 HarnessStart / HarnessStop 正确翻转
// IsActive flag 且调用是线程安全的(高并发读写不 panic)。
func TestHarnessSupervisorLifecycle(t *testing.T) {
	// 重置状态:Deactivate 保证测试从确定起点开始
	Deactivate()
	if IsActive() {
		t.Fatal("IsActive should be false after Deactivate")
	}

	if err := HarnessStart(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !IsActive() {
		t.Fatal("IsActive should be true after Start")
	}

	if err := HarnessStop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if IsActive() {
		t.Fatal("IsActive should be false after Stop")
	}

	// 二次 Start/Stop 应当幂等
	if err := HarnessStart(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if err := HarnessStop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestHarnessSupervisorConcurrentToggle(t *testing.T) {
	// 20 writer 各自跑 100 次 Start/Stop,主线程持续读 — 不能 race / panic
	const writers = 20
	const iters = 100

	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				_ = HarnessStart(context.Background())
				_ = IsActive()
				_ = HarnessStop(context.Background())
				_ = IsActive()
			}
		}()
	}

	// 主线程也读
	for i := 0; i < writers*iters; i++ {
		_ = IsActive()
	}
	wg.Wait()
}

func TestActivateDeactivate(t *testing.T) {
	Deactivate()
	if IsActive() {
		t.Fatal("Deactivate should clear active")
	}
	Activate()
	if !IsActive() {
		t.Fatal("Activate should set active")
	}
	// 对称性
	Deactivate()
	if IsActive() {
		t.Fatal("second Deactivate should clear active")
	}
}
