package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	edgebiz "github.com/vincent-wuhan/opskeeper/core/edge/biz"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// Two numbers, one product, and three ways to get them wrong: a typo that
// silently leaves the node on defaults it was not configured for, an extreme
// value that means something different from what it looks like, and a knob
// that is read, logged, and then never reaches the run loop. The third is
// the one a unit test on a parse function cannot see, which is why
// TestTheConfiguredNumbersReachTheRunLoop exists at the bottom of this file
// and drives a real Agent.

// quietLogger discards, for the cases that are not about what was logged.
func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)) }

// captureLogger returns the logger and the buffer it writes to.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &buf
}

func TestAnUnconfiguredNodeKeepsTheProductionNumbers(t *testing.T) {
	// The regression that matters more than any other case here: a knob
	// added for operators must not move the fleet. An unset variable means
	// zero, which means default, and this asserts the whole chain lands on
	// 30s and 5 rather than on something nearby.
	tun, err := loadTunables(quietLogger())
	if err != nil {
		t.Fatalf("loadTunables: %v", err)
	}
	if tun.heartbeat != 0 || tun.stuck != 0 {
		t.Errorf("tunables = %+v, want the zero value so NewAgent applies its own defaults", tun)
	}
	if got := tun.tolerance(); got != 150*time.Second {
		t.Errorf("tolerance = %s, want 150s (30s x 5)", got)
	}
}

func TestTheConfiguredNumbersAreReadAsWritten(t *testing.T) {
	t.Setenv(heartbeatIntervalEnv, "5s")
	t.Setenv(tunnelStuckEnv, "12")
	tun, err := loadTunables(quietLogger())
	if err != nil {
		t.Fatalf("loadTunables: %v", err)
	}
	if tun.heartbeat != 5*time.Second {
		t.Errorf("heartbeat = %s, want 5s", tun.heartbeat)
	}
	if tun.stuck != 12 {
		t.Errorf("stuck = %d, want 12", tun.stuck)
	}
	// The product, because an operator who set only one of the two has no
	// other way to know what they actually bought.
	if got := tun.tolerance(); got != time.Minute {
		t.Errorf("tolerance = %s, want 60s (5s x 12)", got)
	}
}

func TestATypoIsARefusalNotASilentDefault(t *testing.T) {
	// The failure this exists for: a unit file with a typo used to be
	// indistinguishable from one that never mentioned the setting, and the
	// node booted on numbers its operator believed they had changed. A node
	// that refuses to boot is an outage somebody notices; a node that
	// silently ignores a tuning knob is a tuning that never happened.
	for _, tc := range []struct{ env, value string }{
		{heartbeatIntervalEnv, "30"},
		{heartbeatIntervalEnv, "thirty seconds"},
		{heartbeatIntervalEnv, "1m30"},
		{tunnelStuckEnv, "5x"},
		{tunnelStuckEnv, "5.0"},
		{tunnelStuckEnv, "five"},
	} {
		t.Run(tc.env+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			_, err := loadTunables(quietLogger())
			if err == nil {
				t.Fatalf("loadTunables accepted %q; the node would boot on numbers nobody chose", tc.value)
			}
			// The message has to name the variable and the value. "invalid
			// config" sends an operator looking through every file in the
			// unit directory instead of at the one line that is wrong.
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error does not name the variable: %v", err)
			}
			if !strings.Contains(err.Error(), tc.value) {
				t.Errorf("error does not name the value: %v", err)
			}
		})
	}
}

func TestAnExtremeButParseableValueIsClampedAndSaidOutLoud(t *testing.T) {
	// A number is a tuning decision, and refusing would strand a node with
	// a working link over a knob. But clamping silently is the same lie as
	// ignoring it, so the clamp is a WARN that names both numbers.
	cases := []struct {
		name         string
		env          string
		value        string
		wantInForce  string
		wantContains string
	}{
		{"a sub-second heartbeat", heartbeatIntervalEnv, "10ms", "1s", heartbeatIntervalEnv},
		{"a zero heartbeat", heartbeatIntervalEnv, "0s", "1s", heartbeatIntervalEnv},
		{"a negative heartbeat", heartbeatIntervalEnv, "-1s", "1s", heartbeatIntervalEnv},
		{"a zero stuck threshold", tunnelStuckEnv, "0", "1", tunnelStuckEnv},
		{"a negative stuck threshold", tunnelStuckEnv, "-3", "1", tunnelStuckEnv},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := captureLogger()
			t.Setenv(tc.env, tc.value)
			if _, err := loadTunables(log); err != nil {
				t.Fatalf("loadTunables refused a parseable value: %v", err)
			}
			out := buf.String()
			if !strings.Contains(out, "clamped") {
				t.Errorf("the clamp was not logged; the operator cannot see the number in force:\n%s", out)
			}
			if !strings.Contains(out, tc.wantContains) {
				t.Errorf("the WARN does not name the variable:\n%s", out)
			}
			if !strings.Contains(out, tc.wantInForce) {
				t.Errorf("the WARN does not say what is in force (want %q):\n%s", tc.wantInForce, out)
			}
		})
	}
}

func TestTheFloorItselfIsNotRewritten(t *testing.T) {
	// The other direction. A clamp with no test above it is a clamp that
	// quietly rewrites every legal configuration, and the operator who asked
	// for exactly the documented minimum is the one who can least afford to
	// be given something else.
	log, buf := captureLogger()
	t.Setenv(heartbeatIntervalEnv, edgebiz.MinHeartbeatInterval.String())
	t.Setenv(tunnelStuckEnv, "1")
	tun, err := loadTunables(log)
	if err != nil {
		t.Fatalf("loadTunables: %v", err)
	}
	if strings.Contains(buf.String(), "clamped") {
		t.Errorf("a value at the documented floor was rewritten:\n%s", buf.String())
	}
	if tun.heartbeat != edgebiz.MinHeartbeatInterval || tun.stuck != 1 {
		t.Errorf("tunables = %+v, want the values as written", tun)
	}
}

func TestWhitespaceIsNotAValue(t *testing.T) {
	// A systemd Environment line that gains a trailing space is a common
	// enough edit that treating " 5 " as a refusal would be right, and
	// treating it as unset would be a silent default. Trimmed is right for
	// the former; the test is here because the choice is invisible.
	t.Setenv(tunnelStuckEnv, "  7  ")
	tun, err := loadTunables(quietLogger())
	if err != nil {
		t.Fatalf("loadTunables: %v", err)
	}
	if tun.stuck != 7 {
		t.Errorf("stuck = %d, want 7", tun.stuck)
	}
}

// --- the part a parse test cannot see -----------------------------------

// deadClient fails every call, so every heartbeat is a failure and the node
// is on its way to giving up. It is the smallest thing that satisfies
// tunnel.Client; the run loop is the only consumer in this test.
type deadClient struct{ heartbeats atomic.Int32 }

func (d *deadClient) Dial(context.Context) error             { return nil }
func (d *deadClient) RegisterHandler(string, tunnel.Handler) {}
func (d *deadClient) OnReconnect(func())                     {}
func (d *deadClient) Close() error                           { return nil }
func (d *deadClient) AcceptStream() (tunnel.StreamConn, error) {
	return nil, errors.New("deadClient: no streams")
}
func (d *deadClient) Call(_ context.Context, method string, _, _ any) error {
	if method == tunnel.MethodHeartbeat {
		d.heartbeats.Add(1)
	}
	return errors.New("frontier: connection refused")
}

// noCollector is an agent with nothing to sample, so the only loop that can
// end the test is the heartbeat one.
type noCollector struct{}

func (noCollector) CollectAll(context.Context) ([]edgebiz.CollectorOutput, error) {
	return nil, nil
}
func (noCollector) HostInfo(context.Context) (tunnel.HostInfo, error) {
	return tunnel.HostInfo{}, errors.New("noCollector: no host")
}
func (noCollector) GetHostLoad(context.Context) (tunnel.GetHostLoadResponse, error) {
	return tunnel.GetHostLoadResponse{}, errors.New("noCollector: no host")
}
func (noCollector) GetProcessList(context.Context, int, string) (tunnel.GetProcessListResponse, error) {
	return tunnel.GetProcessListResponse{}, errors.New("noCollector: no host")
}

// TestTheConfiguredNumbersReachTheRunLoop is the anti-vacuity test.
//
// Everything above it would still pass if apply were empty and main.go went
// on passing a struct literal with the two fields left at zero — the parse
// would be right, the logs would be right, and the node would still give up
// after 5 heartbeats regardless of what its unit file said. So this drives
// the whole chain an operator's edit goes through: environment, parse,
// apply, NewAgent, Run. The interval is set to the one-second floor and the
// threshold to one, which costs about a second of wall clock and is the
// cheapest configuration that still proves the node gave up on the
// configured count rather than the default five.
func TestTheConfiguredNumbersReachTheRunLoop(t *testing.T) {
	t.Setenv(heartbeatIntervalEnv, "1s")
	t.Setenv(tunnelStuckEnv, "1")

	tun, err := loadTunables(quietLogger())
	if err != nil {
		t.Fatalf("loadTunables: %v", err)
	}
	cfg := edgebiz.Config{MetricsInterval: time.Hour}
	tun.apply(&cfg)
	// Reported, not fatal. The precondition is worth knowing about, but the
	// evidence that matters is below: if apply silently did nothing, the
	// node would still give up after the *default* five heartbeats, and
	// stopping at the precondition would hide the only number that says so.
	if cfg.HeartbeatInterval != time.Second || cfg.TunnelStuckThreshold != 1 {
		t.Errorf("apply left the config at %+v", cfg)
	}

	client := &deadClient{}
	agent := edgebiz.NewAgent(client, noCollector{}, cfg, quietLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := agent.Run(ctx); !errors.Is(err, edgebiz.ErrTunnelStuck) {
		t.Fatalf("Run = %v, want the tunnel-stuck sentinel", err)
	}
	if got := int(client.heartbeats.Load()); got != 1 {
		t.Errorf("heartbeats = %d, want 1: the configured threshold reached the run loop", got)
	}
}
