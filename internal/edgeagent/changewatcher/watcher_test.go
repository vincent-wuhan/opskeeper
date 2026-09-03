package changewatcher

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestChannelSink_PushReceive(t *testing.T) {
	sink := NewChannelSink(1)
	ev := ChangeEvent{Source: SourceJournald, Kind: KindSSHLogin, Subject: "alice"}
	if err := sink.Push(context.Background(), ev); err != nil {
		t.Fatalf("Push: %v", err)
	}
	got := <-sink.C
	if got.Subject != "alice" {
		t.Errorf("Subject = %q, want %q", got.Subject, "alice")
	}
}

func TestChannelSink_ContextCancel(t *testing.T) {
	sink := NewChannelSink(0) // unbuffered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ev := ChangeEvent{Subject: "alice"}
	if err := sink.Push(ctx, ev); !errors.Is(err, context.Canceled) {
		t.Errorf("Push with cancelled ctx: got %v, want context.Canceled", err)
	}
}

func TestNew_PanicsOnNilSink(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New(nil sink) should panic")
		}
	}()
	_ = New(nil, nil)
}

func TestWatcher_StartStop(t *testing.T) {
	sink := NewChannelSink(8)
	w := New(sink, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := w.Start(ctx)
	if stop == nil {
		t.Fatal("Start returned nil stop")
	}
	// Calling stop should not panic and should be idempotent.
	stop()
	stop()
}

func TestWatcher_StartIdempotent(t *testing.T) {
	sink := NewChannelSink(8)
	w := New(sink, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop1 := w.Start(ctx)
	stop2 := w.Start(ctx)
	if stop1 == nil || stop2 == nil {
		t.Fatal("Start returned nil stop")
	}
	stop1()
	stop2()
}

func TestWatcher_StopCancelsTailers(t *testing.T) {
	sink := NewChannelSink(8)
	w := New(sink, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := w.Start(ctx)
	// Give tailers a moment to start.
	time.Sleep(50 * time.Millisecond)
	stop()
	// Wait for goroutines to exit.
	doneCh := make(chan struct{})
	go func() {
		w.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return within 2s after Stop")
	}
}

func TestWatcher_StartNeverPanicsOnNonLinux(t *testing.T) {
	// 跨平台验证：Start 在 non-linux 上只启 packagemgr（macOS
	// 上 dpkg/dnf 都不存在, packagemgr tailer 内部直接返回 nil）。
	sink := NewChannelSink(8)
	w := New(sink, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := w.Start(ctx)
	defer stop()
	var counter atomic.Int32
	_ = counter.Load() // touch atomic
}
