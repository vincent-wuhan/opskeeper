package biz_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	edgebiz "github.com/vincent-wuhan/opskeeper/core/edge/biz"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// The tunnel-stuck path is the node's last resort: when the control plane
// has been unreachable long enough that waiting stopped being sensible, the
// agent stops and lets systemd respawn it with a clean dial. Every operator
// who has ever had a node quietly disappear from the console has this path to
// thank, and it is the only thing standing between a wedged process and a
// fleet that has to be restarted by hand.
//
// It also had no test. `errTunnelStuck` is returned from exactly one place,
// and nothing anywhere asserted that it is ever returned, that it is returned
// at the configured number of failures rather than one earlier or one later,
// or that a healthy link never returns it. A threshold that fired on the
// first dropped packet and a threshold that never fired at all produce the
// same green suite.
//
// These tests pin the count, which is the number an operator reasons about:
// five heartbeats is 150s at the production interval and one heartbeat at
// the floor, and a node has to give up after the one its operator chose.

// stuckClient fails every call, so every heartbeat is a failure.
func stuckClient(t *testing.T, threshold int) (*edgebiz.Agent, *fakeClient) {
	t.Helper()
	fc := newFakeClient()
	fc.pushAccepted.err = errors.New("frontier: connection refused")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return edgebiz.NewAgent(fc, &fakeCollector{}, edgebiz.Config{
		// Sub-second on purpose. The heartbeat floor belongs to the edge's
		// env parsing, not to the agent, and a test that had to wait 30s
		// per case to observe a count would not get run.
		HeartbeatInterval:    time.Millisecond,
		TunnelStuckThreshold: threshold,
		MetricsInterval:      10 * time.Second,
	}, logger), fc
}

func TestTheNodeGivesUpAfterExactlyTheConfiguredNumberOfFailures(t *testing.T) {
	for _, threshold := range []int{1, 2, 5} {
		t.Run(strconv.Itoa(threshold)+" failures", func(t *testing.T) {
			a, fc := stuckClient(t, threshold)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := a.Run(ctx)
			if !errors.Is(err, edgebiz.ErrTunnelStuck) {
				t.Fatalf("Run = %v, want the tunnel-stuck sentinel", err)
			}
			// Exactly N, not N-1 (giving up on the failure before the
			// threshold) and not N+1 (tolerating one blip past what the
			// operator asked for). A node that exits early is an outage
			// during a manager restart; a node that exits late is a node
			// that is down for a reason nobody is still looking for.
			if got := int(fc.countOf(tunnel.MethodHeartbeat)); got != threshold {
				t.Errorf("heartbeats attempted = %d, want %d", got, threshold)
			}
		})
	}
}

func TestAHealthyLinkNeverGivesUp(t *testing.T) {
	// The other direction, and the one a threshold that always fires would
	// fail: a node whose heartbeats succeed must run until its context ends
	// rather than exiting on a count of successes.
	fc := newFakeClient()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	agent := edgebiz.NewAgent(fc, &fakeCollector{}, edgebiz.Config{
		HeartbeatInterval:    time.Millisecond,
		TunnelStuckThreshold: 2,
		MetricsInterval:      10 * time.Second,
	}, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := agent.Run(ctx); err != nil {
		t.Fatalf("Run = %v on a healthy link, want a clean context stop", err)
	}
	if n := fc.countOf(tunnel.MethodHeartbeat); n < 3 {
		t.Errorf("heartbeats = %d in 150ms at 1ms, want many: the loop stopped early", n)
	}
}

func TestAZeroThresholdFallsBackToTheProductionValue(t *testing.T) {
	// A zero must mean "unset", not "give up on the first packet". The
	// distinction is the whole reason the field is read as a count: a
	// threshold of zero is a respawn loop, and a config struct that
	// cannot say "unset" forces a caller to know that.
	a, fc := stuckClient(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := a.Run(ctx); !errors.Is(err, edgebiz.ErrTunnelStuck) {
		t.Fatalf("Run = %v, want the tunnel-stuck sentinel", err)
	}
	want := edgebiz.DefaultTunnelStuckThreshold
	if got := int(fc.countOf(tunnel.MethodHeartbeat)); got != want {
		t.Errorf("heartbeats = %d, want the production default %d; a zero threshold must not mean 0", got, want)
	}
}

func TestTheProductionDefaultsAreUnchanged(t *testing.T) {
	// The knob was added for a reason, and the reason must not have
	// changed the fleet. A node that sets neither env var runs exactly the
	// numbers it ran before, and this is the assertion that says so.
	if edgebiz.DefaultHeartbeatInterval != 30*time.Second {
		t.Errorf("DefaultHeartbeatInterval = %s, want 30s", edgebiz.DefaultHeartbeatInterval)
	}
	if edgebiz.DefaultTunnelStuckThreshold != 5 {
		t.Errorf("DefaultTunnelStuckThreshold = %d, want 5", edgebiz.DefaultTunnelStuckThreshold)
	}
	if edgebiz.MinHeartbeatInterval != time.Second {
		t.Errorf("MinHeartbeatInterval = %s, want 1s", edgebiz.MinHeartbeatInterval)
	}
}
