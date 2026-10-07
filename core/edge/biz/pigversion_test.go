package biz_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/edge/biz"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// The node half of the PiG version contract.
//
// The control plane answers "can this node host a package needing PiG
// X?" from two numbers it was told, and this file is where one of them
// comes from. The design decision being pinned here is that the value
// rides the heartbeat rather than register_edge: a node is upgraded, the
// process restarts onto a different binary, and from the next beat on the
// control plane knows what the node actually hosts. Captured at connect
// time it would have frozen at the build the node started with, which is
// precisely the number an operator does not want when they are deciding
// whether a fleet can take a package.

// A configured version reaches the wire, under its JSON name.
//
// Asserting on the decoded struct rather than the raw bytes would pass
// even if the tag were wrong and the field silently vanished in
// production, because the two ends are decoded by the same struct. The
// round trip through a generic map is what actually proves the field is
// named pig_version on the wire, which is the only name the manager — and
// every log line and packet capture an operator might read — will ever see.
func TestTheHeartbeatCarriesTheConfiguredPigVersionOnTheWire(t *testing.T) {
	fc := newFakeClient()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := biz.NewAgent(fc, &fakeCollector{}, biz.Config{
		HeartbeatInterval: 50 * time.Millisecond,
		MetricsInterval:   time.Hour, // keep the test on the heartbeat path only
		MetricsBatchSize:  1000,
		AgentVersion:      "0.8.0",
		PigVersion:        "0.3.0",
	}, discard)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	req, ok := fc.lastReq(tunnel.MethodHeartbeat).(tunnel.HeartbeatRequest)
	if !ok {
		t.Fatalf("no heartbeat was sent; last %s payload = %#v",
			tunnel.MethodHeartbeat, fc.lastReq(tunnel.MethodHeartbeat))
	}
	if req.PigVersion != "0.3.0" {
		t.Errorf("HeartbeatRequest.PigVersion = %q, want %q", req.PigVersion, "0.3.0")
	}

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal heartbeat: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal heartbeat: %v", err)
	}
	got, present := wire["pig_version"]
	if !present {
		t.Fatalf("heartbeat JSON has no pig_version key: %s", raw)
	}
	if got != "0.3.0" {
		t.Errorf("wire pig_version = %v, want %q", got, "0.3.0")
	}
}

// A node that knows no version says so, rather than guessing one.
//
// The empty case is the security-relevant one, and it is worth stating
// plainly which way it fails. The control plane compares a package's
// min_pig_version against whatever arrives here and fails closed on a
// value it cannot read, so a node that invented a plausible-looking
// version would convert "this node cannot tell" into "this node is
// compatible" across the whole fleet — the silent all-clear on the one
// axis that decides whether an agent can load a package's extensions at
// all. An absent Config field, or one that is only whitespace, must
// therefore stay absent on the wire.
func TestANodeWithNoPigVersionReportsNoneRatherThanAGuess(t *testing.T) {
	for name, configured := range map[string]string{
		"unset":      "",
		"whitespace": "   \t\n ",
	} {
		t.Run(name, func(t *testing.T) {
			fc := newFakeClient()
			discard := slog.New(slog.NewTextHandler(io.Discard, nil))
			a := biz.NewAgent(fc, &fakeCollector{}, biz.Config{
				HeartbeatInterval: 50 * time.Millisecond,
				MetricsInterval:   time.Hour,
				MetricsBatchSize:  1000,
				PigVersion:        configured,
			}, discard)

			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- a.Run(ctx) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not return after ctx cancel")
			}

			req, ok := fc.lastReq(tunnel.MethodHeartbeat).(tunnel.HeartbeatRequest)
			if !ok {
				t.Fatalf("no heartbeat was sent")
			}
			if req.PigVersion != "" {
				t.Errorf("HeartbeatRequest.PigVersion = %q, want empty", req.PigVersion)
			}
			raw, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal heartbeat: %v", err)
			}
			var wire map[string]any
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatalf("unmarshal heartbeat: %v", err)
			}
			if _, present := wire["pig_version"]; present {
				t.Errorf("heartbeat JSON carries a pig_version key for an unknown version: %s", raw)
			}
		})
	}
}
