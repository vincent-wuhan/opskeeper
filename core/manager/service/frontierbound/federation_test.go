package frontierbound

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// Tests for the two things Install now does for the cluster channel:
// dispatching cluster.hello to whoever answers it, and telling subscribers
// when a caller has gone away.
//
// The second one exists because the broker accepts exactly one EdgeOffline
// callback. A second RegisterEdgeOffline would replace the first, taking the
// offline bookkeeping with it — so the hook list is a list, and the tests
// below are about the list behaving like one.

type fakeClusterLink struct {
	mu      sync.Mutex
	calls   int
	gotEdge uint64
	gotBody []byte
	answer  []byte
	err     error
}

func (f *fakeClusterLink) HandleHello(_ context.Context, edgeID uint64, body []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotEdge = edgeID
	f.gotBody = append([]byte(nil), body...)
	return f.answer, f.err
}

func (f *fakeClusterLink) seen() (int, uint64, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.gotEdge, f.gotBody
}

func TestInstall_ClusterHello_ReachesTheLink(t *testing.T) {
	link := &fakeClusterLink{answer: []byte(`{"accepted":true}`)}
	fs, _ := installWith(t, Wiring{ClusterLink: link})

	rpc, ok := fs.rpcs[tunnel.MethodClusterHello]
	if !ok {
		t.Fatal("cluster.hello was not registered")
	}
	// A real hello body, because the point of this test is that the bytes
	// a child cluster sends arrive at the link unmangled.
	id, err := federation.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	body, _ := json.Marshal(tunnel.ClusterHelloRequest{
		Cluster:           federation.Cluster{ID: id, Name: "north"},
		ProvisioningToken: "tok-north",
	})
	// The caller id here is the transport's opaque number, not an edge id.
	const transportID = uint64(7634846078675816708)
	rsp := &fakeResp{}
	rpc(context.Background(), &fakeReq{data: body, clientID: transportID}, rsp)

	if rsp.err != nil {
		t.Fatalf("cluster.hello returned an error: %v", rsp.err)
	}
	calls, edge, got := link.seen()
	if calls != 1 {
		t.Fatalf("the link was called %d times, want 1", calls)
	}
	if edge != transportID {
		t.Errorf("the link saw caller %d, want the transport id %d", edge, transportID)
	}
	var req tunnel.ClusterHelloRequest
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("the link received unreadable bytes: %v", err)
	}
	if req.Cluster.ID != id || req.ProvisioningToken != "tok-north" {
		t.Errorf("the link received %+v, want the hello the caller sent", req)
	}
	if string(rsp.data) != `{"accepted":true}` {
		t.Errorf("the link's answer was not returned to the child: %q", rsp.data)
	}
}

// TestInstall_WithoutClusterLink_NoHelloMethod: a root that was never wired
// for federation must not answer a hello, and the absence is a clean
// "method not registered" rather than a handler that panics.
func TestInstall_WithoutClusterLink_NoHelloMethod(t *testing.T) {
	fs, _ := installWith(t, Wiring{})
	if _, ok := fs.rpcs[tunnel.MethodClusterHello]; ok {
		t.Error("cluster.hello was registered with no link behind it")
	}
}

// TestEdgeOffline_ReachesTheSubscribers is the reason the list exists. A
// cluster binding is keyed by the caller that proved its token, and that is
// the number the broker recycles — so a subscriber handed the canonical edge
// id instead would miss every time the broker had not yet canonicalised the
// dial, and the stale binding would outlive the connection.
func TestEdgeOffline_ReachesTheSubscribers(t *testing.T) {
	fs, c := installWith(t, Wiring{})

	var mu sync.Mutex
	var got []uint64
	c.OnEdgeOffline(func(edgeID uint64) {
		mu.Lock()
		got = append(got, edgeID)
		mu.Unlock()
	})

	const transportID = uint64(4242)
	if err := fs.offline(transportID, nil, &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 40011}); err != nil {
		t.Fatalf("offline callback: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != transportID {
		t.Errorf("subscribers saw %v, want exactly [%d]", got, transportID)
	}
}

// TestOneSubscriberPanickingDoesNotSilenceTheRest: the hooks run inside the
// lifecycle notification the broker is waiting on, so a subscriber that
// panics would otherwise keep every other node from being marked offline.
// The panic is recovered and logged, not swallowed silently — a silently
// dropped hook is a binding nobody ever forgets to release.
func TestOneSubscriberPanickingDoesNotSilenceTheRest(t *testing.T) {
	fs, c := installWith(t, Wiring{})

	var mu sync.Mutex
	var reached bool
	c.OnEdgeOffline(func(uint64) { panic("this subscriber is broken") })
	c.OnEdgeOffline(func(uint64) {
		mu.Lock()
		reached = true
		mu.Unlock()
	})

	if err := fs.offline(1, nil, nil); err != nil {
		t.Fatalf("offline callback returned an error instead of containing the panic: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reached {
		t.Error("a panicking subscriber stopped the ones after it from running")
	}
}

func TestANilSubscriberIsNotRegistered(t *testing.T) {
	_, c := installWith(t, Wiring{})
	c.OnEdgeOffline(nil)
	// The list stays empty rather than holding a nil that panics on the
	// first disconnection.
	c.offlineMu.Lock()
	n := len(c.offlineHooks)
	c.offlineMu.Unlock()
	if n != 0 {
		t.Errorf("offlineHooks = %d after registering nil, want 0", n)
	}
}
