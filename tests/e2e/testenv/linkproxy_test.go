//go:build e2e

package testenv

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// The proxy is a fixture, but it is the fixture the outage test's conclusion
// rests on: if Cut() does not actually stop traffic, or Heal() does not
// actually restore it, the end-to-end run still goes green while measuring
// nothing. So it is tested on its own, against a real socket, with no
// container involved — the failure mode being guarded against here is a
// fixture that appears to work.

// echoServer is the upstream: one connection, one line, one line back.
func echoServer(t *testing.T) (addr string, stop func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("linkproxy: upstream listen: %v", err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				reader := bufio.NewReader(c)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if _, err := c.Write([]byte("echo:" + line)); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return listener.Addr().String(), func() { _ = listener.Close() }
}

func TestTheProxyCarriesTrafficWhileHealthy(t *testing.T) {
	upstream, stop := echoServer(t)
	defer stop()
	proxy := NewLinkProxy(t, upstream)

	conn, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("linkproxy: dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("linkproxy: write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("linkproxy: read: %v", err)
	}
	if line != "echo:ping\n" {
		t.Errorf("read %q, want %q", line, "echo:ping\n")
	}
}

// Cut has to break an *established* connection, not merely refuse future
// ones. A proxy that only refused dials would leave a node writing into a
// socket that goes quiet, and the outage test would then be asserting on a
// node that still believes it is connected.
//
// The probe exchanges one line *before* the cut and then requires the *next*
// exchange to fail. That ordering is not incidental: the reply to a line
// already in flight is sitting in the client's socket buffer, so cutting and
// then reading proves nothing — it reads the answer to a question asked
// while the link was still up.
func TestCuttingTheProxyBreaksAnEstablishedConnection(t *testing.T) {
	upstream, stop := echoServer(t)
	defer stop()
	proxy := NewLinkProxy(t, upstream)

	conn, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("linkproxy: dial: %v", err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	exchange := func(line string) (string, error) {
		if _, err := conn.Write([]byte(line + "\n")); err != nil {
			return "", err
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		return reader.ReadString('\n')
	}
	if got, err := exchange("before"); err != nil || got != "echo:before\n" {
		t.Fatalf("before the cut: got (%q, %v), want the link working", got, err)
	}
	proxy.WaitForLive(t, 1, 5*time.Second)

	proxy.Cut()
	if !proxy.Severed() {
		t.Fatal("Severed() = false right after Cut()")
	}
	if got, err := exchange("after"); err == nil {
		t.Errorf("the established connection still carried %q after Cut(); an outage that only "+
			"refuses new dials leaves a node writing into a socket that goes quiet", got)
	}
}

// The other half, and the one an outage test silently depends on: after the
// link comes back, a client that retries gets through. A proxy that cannot
// heal turns "the node lost its link" into "the node is gone", and the test
// would report the second while believing it measured the first.
func TestHealingTheProxyLetsAClientBackIn(t *testing.T) {
	upstream, stop := echoServer(t)
	defer stop()
	proxy := NewLinkProxy(t, upstream)

	before, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("linkproxy: dial: %v", err)
	}
	defer before.Close()
	proxy.WaitForLive(t, 1, 5*time.Second)

	proxy.Cut()
	// A dial made *after* the cut is what the counter counts. A connection
	// that was already up when the cut landed is severed, not refused, and
	// waiting on the refusal without dialling again waits for something that
	// was never going to happen.
	refused, err := net.Dial("tcp", proxy.Addr())
	if err == nil {
		defer refused.Close()
	}
	proxy.WaitForRefusedDials(t, 1, 5*time.Second)

	proxy.Heal()
	if proxy.Severed() {
		t.Error("Severed() = true right after Heal()")
	}

	conn, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("linkproxy: dial after heal: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("back\n")); err != nil {
		t.Fatalf("linkproxy: write after heal: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("linkproxy: a client could not get through after Heal(): %v", err)
	}
	if !strings.HasPrefix(line, "echo:") {
		t.Errorf("read %q after heal, want an echo", line)
	}
}
