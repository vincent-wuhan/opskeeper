package changewatcher

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/tunnel"
)

// fakeClient 实现 tunnel.Client 接口, 记录所有 Call 接收到的请求.
type fakeClient struct {
	mu       sync.Mutex
	calls    []*tunnel.PushChangeEventsRequest
	err      error
	callsCh  chan *tunnel.PushChangeEventsRequest
	failNext atomic.Bool
}

func newFakeClient() *fakeClient {
	return &fakeClient{callsCh: make(chan *tunnel.PushChangeEventsRequest, 32)}
}

func (f *fakeClient) Dial(ctx context.Context) error                  { return nil }
func (f *fakeClient) RegisterHandler(method string, h tunnel.Handler) {}
func (f *fakeClient) AcceptStream() (tunnel.StreamConn, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeClient) Call(ctx context.Context, method string, req, resp any) error {
	if f.failNext.Swap(false) {
		return errors.New("fake: induced failure")
	}
	if f.err != nil {
		return f.err
	}
	if method != tunnel.MethodPushChangeEvents {
		return errors.New("fake: wrong method")
	}
	in, ok := req.(*tunnel.PushChangeEventsRequest)
	if !ok {
		return errors.New("fake: wrong req type")
	}
	f.mu.Lock()
	f.calls = append(f.calls, in)
	cp := *in
	f.mu.Unlock()
	select {
	case f.callsCh <- &cp:
	default:
	}
	if r, ok := resp.(*tunnel.PushChangeEventsResponse); ok {
		r.Accepted = uint32(len(in.Events))
	}
	return nil
}
func (f *fakeClient) OnReconnect(fn func()) {}
func (f *fakeClient) Close() error          { return nil }
func (f *fakeClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestTunnelSink_PushBuffersEvents(t *testing.T) {
	fc := newFakeClient()
	sink := NewTunnelSink(fc, newTestLogger(t), TunnelSinkSinkConfig{BatchSize: 5, BufferSize: 10, FlushInterval: 10 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sink.Run(ctx)

	for i := 0; i < 3; i++ {
		_ = sink.Push(ctx, ChangeEvent{Source: SourceJournald, Kind: KindSSHLogin, Subject: "alice"})
	}
	// 还没到 batchSize, 不应 flush.
	if got := fc.callCount(); got != 0 {
		t.Errorf("callCount = %d, want 0 (under batch size)", got)
	}
}

func TestTunnelSink_FlushOnFull(t *testing.T) {
	fc := newFakeClient()
	sink := NewTunnelSink(fc, newTestLogger(t), TunnelSinkSinkConfig{BatchSize: 3, BufferSize: 10, FlushInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sink.Run(ctx)

	for i := 0; i < 3; i++ {
		_ = sink.Push(ctx, ChangeEvent{Source: SourceJournald, Kind: KindSSHLogin, Subject: "u"})
	}
	// 等满批 flush.
	select {
	case got := <-fc.callsCh:
		if len(got.Events) != 3 {
			t.Errorf("len(events) = %d, want 3", len(got.Events))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for flush, callCount=%d", fc.callCount())
	}
}

func TestTunnelSink_FlushOnInterval(t *testing.T) {
	fc := newFakeClient()
	sink := NewTunnelSink(fc, newTestLogger(t), TunnelSinkSinkConfig{BatchSize: 100, BufferSize: 10, FlushInterval: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sink.Run(ctx)

	_ = sink.Push(ctx, ChangeEvent{Source: SourceJournald, Kind: KindSSHLogin, Subject: "u"})

	select {
	case <-fc.callsCh:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatalf("interval flush not fired, callCount=%d", fc.callCount())
	}
}

func TestTunnelSink_DropsOnFull(t *testing.T) {
	fc := newFakeClient()
	sink := NewTunnelSink(fc, newTestLogger(t), TunnelSinkSinkConfig{BatchSize: 100, BufferSize: 4, FlushInterval: time.Hour})
	// 不启动 Run, Push 直接落 buf.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 填满 buf (cap=4). 第 5 个起开始 drop.
	for i := 0; i < 10; i++ {
		_ = sink.Push(ctx, ChangeEvent{Source: SourceJournald, Kind: KindSSHLogin, Subject: "u"})
	}
	if got := sink.Dropped(); got == 0 {
		t.Errorf("Dropped = 0, want > 0 after overflowing")
	}
}

func TestTunnelSink_CloseFlushesRemainder(t *testing.T) {
	fc := newFakeClient()
	sink := NewTunnelSink(fc, newTestLogger(t), TunnelSinkSinkConfig{BatchSize: 100, BufferSize: 10, FlushInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sink.Run(ctx)

	for i := 0; i < 5; i++ {
		_ = sink.Push(ctx, ChangeEvent{Source: SourceJournald, Kind: KindSSHLogin, Subject: "u"})
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	// 等 Close drain 完成的 call.
	deadline := time.After(2 * time.Second)
	for fc.callCount() == 0 {
		select {
		case <-deadline:
			t.Fatalf("Close did not flush, callCount=%d", fc.callCount())
		case <-fc.callsCh:
		}
	}
	if got := fc.callCount(); got == 0 {
		t.Errorf("callCount = 0, want >= 1 after Close")
	}
}

func TestTunnelSink_ContextCancel(t *testing.T) {
	fc := newFakeClient()
	sink := NewTunnelSink(fc, newTestLogger(t), TunnelSinkSinkConfig{BatchSize: 5, BufferSize: 10, FlushInterval: 10 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sink.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on ctx cancel")
	}
}

func TestTunnelSink_PushAfterCloseNoop(t *testing.T) {
	fc := newFakeClient()
	sink := NewTunnelSink(fc, newTestLogger(t), TunnelSinkSinkConfig{BatchSize: 5, BufferSize: 10, FlushInterval: time.Hour})
	_ = sink.Close()
	// Push 在 closed 后应直接 return nil, 不 panic 不增加 drop.
	_ = sink.Push(context.Background(), ChangeEvent{Subject: "x"})
}

func TestTunnelSink_ClientErrorDoesNotPanic(t *testing.T) {
	fc := newFakeClient()
	fc.failNext.Store(true)
	sink := NewTunnelSink(fc, newTestLogger(t), TunnelSinkSinkConfig{BatchSize: 2, BufferSize: 10, FlushInterval: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sink.Run(ctx)

	_ = sink.Push(ctx, ChangeEvent{Subject: "u"})
	_ = sink.Push(ctx, ChangeEvent{Subject: "u2"})
	// 等 ticker 触发 flush, Client 返回错误, Run 不应退出.
	time.Sleep(200 * time.Millisecond)
	// 后续 push 仍应工作.
	_ = sink.Push(ctx, ChangeEvent{Subject: "u3"})
}
