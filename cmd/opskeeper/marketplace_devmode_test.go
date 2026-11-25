package main

// S-1 of the marketplace-imbridge-fail-closed change: DevMode is fail
// closed. These tests pin the truth table of marketplaceDevModeFromEnv —
// the whole point of extracting it from main.go's bare statement is that
// the statement itself could not be tested, and the previous reading
// (unset == true) is exactly the S-level finding the change removes.
//
// What each case protects:
//   - unset/empty → false: the default deployment posture. Before the
//     inversion this was true, which meant the source allowlist and the
//     opskeeper-official signature gate were silently bypassed on every
//     install that never mentioned the variable.
//   - true/1/TRUE → true: the explicit dev escape hatch keeps working, so
//     the fix cannot be "rolled forward" into a hard block that forces
//     every dev cluster onto a fork.
//   - garbage → false: no truthy heuristic. "flase", "yes please", and
//     "0" must not widen the attack surface by accident of spelling.

import "testing"

func TestMarketplaceDevModeDefaultIsFalse(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		if got := marketplaceDevModeFromEnv(raw); got {
			t.Errorf("marketplaceDevModeFromEnv(%q) = true, want false (fail closed default)", raw)
		}
	}
}

func TestMarketplaceDevModeExplicitTrue(t *testing.T) {
	for _, raw := range []string{"true", "TRUE", "True", "1", " 1 ", " true "} {
		if got := marketplaceDevModeFromEnv(raw); !got {
			t.Errorf("marketplaceDevModeFromEnv(%q) = false, want true (explicit opt-in)", raw)
		}
	}
}

func TestMarketplaceDevModeGarbageIsFalse(t *testing.T) {
	for _, raw := range []string{"false", "FALSE", "0", "flase", "yes please", "2", "on", "enabled"} {
		if got := marketplaceDevModeFromEnv(raw); got {
			t.Errorf("marketplaceDevModeFromEnv(%q) = true, want false (no truthy heuristics)", raw)
		}
	}
}
