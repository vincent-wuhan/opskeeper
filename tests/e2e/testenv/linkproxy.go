//go:build e2e

package testenv

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// LinkProxy is a TCP forwarder a test can cut, standing between one node and
// the tunnel broker.
//
// It exists because the frontier broker is a process-wide singleton in this
// harness: one container per `go test` process, shared by every test. Stopping
// it to simulate an outage would take every other test's node offline too, and
// "the broker is gone" is a different failure from "this node lost its link" —
// the first says nothing about whether a node survives the second, which is
// the only question an outage test has.
//
// So the outage is scoped to one edge: its address is this proxy, the proxy
// forwards to the broker, and Cut() drops the live connection and refuses the
// next one. The node then experiences exactly what a node behind a yanked
// network cable experiences — an established connection that stops answering,
// followed by dials that do not complete — while everything else in the test
// keeps running against the same broker.
type LinkProxy struct {
	listener net.Listener
	upstream string

	mu        sync.Mutex
	severed   bool
	closed    bool
	live      map[net.Conn]struct{}
	accepted  int
	forwarded int
	refused   int
}

// NewLinkProxy starts a forwarder in front of upstreamAddr.
//
// The proxy is torn down with the test.
func NewLinkProxy(t *testing.T, upstreamAddr string) *LinkProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("testenv: link proxy listen: %v", err)
	}
	proxy := &LinkProxy{
		listener: listener,
		upstream: upstreamAddr,
		live:     map[net.Conn]struct{}{},
	}
	go proxy.serve()
	t.Cleanup(proxy.Close)
	return proxy
}

// Addr is the address to hand a node in place of the broker's edgebound
// listener.
func (p *LinkProxy) Addr() string { return p.listener.Addr().String() }

// Cut severs the link and keeps it severed.
//
// Existing connections are closed rather than left hanging: a node that sees
// a socket go quiet may keep writing into a buffer for minutes, and the test
// would then be asserting on a node that believes it is still connected. A
// dial made while severed is accepted and closed immediately, so the client
// gets a definite failure instead of a timeout, which is what a real outage
// looks like to a reconnecting client.
func (p *LinkProxy) Cut() {
	p.mu.Lock()
	p.severed = true
	live := make([]net.Conn, 0, len(p.live))
	for conn := range p.live {
		live = append(live, conn)
	}
	p.mu.Unlock()
	for _, conn := range live {
		_ = conn.Close()
	}
}

// Heal restores forwarding. Connections the node opens from here on are
// carried to the broker.
func (p *LinkProxy) Heal() {
	p.mu.Lock()
	p.severed = false
	p.mu.Unlock()
}

// Severed reports whether the link is currently cut.
func (p *LinkProxy) Severed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.severed
}

// Counts reports how many connections were accepted, how many are currently
// established, and how many dials were turned away while severed.
//
// The refused count is what tells a test that the node actually retried
// rather than sitting still: a node that stops dialling is a node whose
// reconnect story is broken, and without this number that failure looks
// identical to a node that is merely patient.
func (p *LinkProxy) Counts() (accepted, live, refused int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepted, len(p.live), p.refused
}

// WaitForRefusedDials blocks until at least n dials have been turned away.
//
// It exists because "the link is down" is not observable from outside until
// the client tries to use it, and a test that sleeps a fixed interval is
// asserting on the clock rather than on the node.
func (p *LinkProxy) WaitForRefusedDials(t *testing.T, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, _, refused := p.Counts(); refused >= n {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_, _, refused := p.Counts()
	t.Fatalf("testenv: %d refused dial(s) wanted, saw %d within %s — the node did not "+
		"try to reconnect while the link was cut", n, refused, within)
}

// WaitForLive blocks until at least n connections are established.
func (p *LinkProxy) WaitForLive(t *testing.T, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, live, _ := p.Counts(); live >= n {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	accepted, live, _ := p.Counts()
	t.Fatalf("testenv: %d live connection(s) wanted, saw %d (of %d accepted) within %s",
		n, live, accepted, within)
}

// Close shuts the proxy down.
func (p *LinkProxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	live := make([]net.Conn, 0, len(p.live))
	for conn := range p.live {
		live = append(live, conn)
	}
	p.live = map[net.Conn]struct{}{}
	p.mu.Unlock()
	_ = p.listener.Close()
	for _, conn := range live {
		_ = conn.Close()
	}
}

func (p *LinkProxy) serve() {
	for {
		downstream, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = downstream.Close()
			return
		}
		if p.severed {
			p.refused++
			p.mu.Unlock()
			_ = downstream.Close()
			continue
		}
		p.accepted++
		p.live[downstream] = struct{}{}
		p.mu.Unlock()
		go p.forward(downstream)
	}
}

func (p *LinkProxy) forward(downstream net.Conn) {
	defer p.forget(downstream)

	upstream, err := net.DialTimeout("tcp", p.upstream, 5*time.Second)
	if err != nil {
		_ = downstream.Close()
		return
	}
	p.track(upstream)
	defer func() {
		p.forget(upstream)
		_ = upstream.Close()
	}()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, downstream); done <- struct{}{} }()
	go func() { _, _ = io.Copy(downstream, upstream); done <- struct{}{} }()
	<-done
	_ = downstream.Close()
	_ = upstream.Close()
}

func (p *LinkProxy) track(conn net.Conn) {
	p.mu.Lock()
	if !p.closed {
		p.live[conn] = struct{}{}
	}
	p.mu.Unlock()
}

func (p *LinkProxy) forget(conn net.Conn) {
	p.mu.Lock()
	delete(p.live, conn)
	p.mu.Unlock()
	_ = conn.Close()
}
