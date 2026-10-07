package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	edgebiz "github.com/vincent-wuhan/opskeeper/core/edge/biz"
)

// The two numbers that decide how long a node tolerates losing its control
// plane, and why they are configurable now rather than not at all.
//
// They used to be a duration defaulted inside NewAgent and a package-level
// constant, which read as one number and were two. The only reason to
// discover the pair was to retune it, and the only reason to retune it was a
// deployment whose manager restart cycle did not match the 2.5 minutes the
// defaults imply — at which point the operator's choices were to edit Go, or
// to give up. An e2e that had to keep a network outage inside that window had
// the same problem, and solved it by being lucky.
//
// Both are read as durations and counts rather than as a single "tolerance",
// because they multiply. That product is printed at boot: a threshold of 5
// is 150s at the default heartbeat and 5s at the fastest one this allows,
// and an operator who set only the interval has no other way to know that.
const (
	heartbeatIntervalEnv = "OPSKEEPER_EDGE_HEARTBEAT_INTERVAL"
	tunnelStuckEnv       = "OPSKEEPER_EDGE_TUNNEL_STUCK_THRESHOLD"
)

// edgeTunables is the link's resilience posture, as the operator configured
// it. The zero value means "production default" for both fields, so a node
// that sets neither env var behaves exactly as it did before this existed.
type edgeTunables struct {
	heartbeat time.Duration
	stuck     int
}

// tolerance is the wall-clock the node will spend failing heartbeats before
// it gives up and lets systemd respawn it. It is the number the operator is
// actually tuning, and it is not configurable directly because it is not a
// decision — it is a consequence of two.
func (t edgeTunables) tolerance() time.Duration {
	hb := t.heartbeat
	if hb <= 0 {
		hb = edgebiz.DefaultHeartbeatInterval
	}
	stuck := t.stuck
	if stuck <= 0 {
		stuck = edgebiz.DefaultTunnelStuckThreshold
	}
	return hb * time.Duration(stuck)
}

// apply writes the two numbers onto the agent's config.
//
// It exists as a named function so the mapping from "what the operator
// wrote in the unit file" to "what the run loop uses" is one testable
// statement rather than two fields in a struct literal that a test can
// only reach by reading the source. A knob nobody can demonstrate reaching
// the code it configures is a knob in a comment.
func (t edgeTunables) apply(cfg *edgebiz.Config) {
	cfg.HeartbeatInterval = t.heartbeat
	cfg.TunnelStuckThreshold = t.stuck
}

// loadTunables reads the two env vars, and is the only place that does.
//
// The two failure modes are handled differently, following the precedent
// MinMetricsInterval set: a value that is *not a number at all* is a typo
// and is refused, because silently running on the default would leave an
// operator believing they configured something; a value that *is* a number
// but sits outside the safe range is a tuning decision, and it is clamped
// with a WARN rather than refused, because refusing would strand a node with
// a working link over a knob.
//
// The distinction is the whole policy. A node that boots is a node somebody
// has to notice is misconfigured; a node that refuses to boot is an outage
// caused by a semicolon.
func loadTunables(log *slog.Logger) (edgeTunables, error) {
	t := edgeTunables{}

	if raw := strings.TrimSpace(os.Getenv(heartbeatIntervalEnv)); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return t, fmt.Errorf("%s=%q is not a duration (try 30s, 1m, 500ms): %w",
				heartbeatIntervalEnv, raw, err)
		}
		if d < edgebiz.MinHeartbeatInterval {
			log.Warn("edge: heartbeat interval below the floor; clamped",
				slog.String("env", heartbeatIntervalEnv),
				slog.Duration("configured", d),
				slog.Duration("in_force", edgebiz.MinHeartbeatInterval),
				slog.String("reason", "below this the heartbeat stops being a liveness check and becomes load on the control plane, from every node at once"))
			d = edgebiz.MinHeartbeatInterval
		}
		t.heartbeat = d
	}

	if raw := strings.TrimSpace(os.Getenv(tunnelStuckEnv)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return t, fmt.Errorf("%s=%q is not a whole number of heartbeats: %w",
				tunnelStuckEnv, raw, err)
		}
		if n < 1 {
			// Zero and negatives are not "be strict", they are
			// "exit on the first dropped packet", which reads like a
			// setting and behaves like a respawn loop. One is the
			// strictest posture that can be meant.
			log.Warn("edge: tunnel stuck threshold below one; clamped",
				slog.String("env", tunnelStuckEnv),
				slog.Int("configured", n),
				slog.Int("in_force", 1),
				slog.String("reason", "a threshold of zero makes any dropped packet fatal, which is a respawn loop rather than a policy"))
			n = 1
		}
		t.stuck = n
	}

	return t, nil
}
