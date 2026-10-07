package grafana

import (
	"embed"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

//go:embed dashboards/*.json
var testEmbedded embed.FS

// TestEmbeddedDashboardJSONShape verifies every JSON file shipped in the
// dashboards/ directory is well-formed, has a unique Grafana `uid`, and
// every panel's PromQL targets reference metric names this repo actually
// emits. Guards against:
//   - broken JSON causing Grafana to reject the dashboard on push
//   - two dashboards with the same uid (Grafana upsert would clobber one)
//   - typo'd PromQL names that silently render "No data" forever
func TestEmbeddedDashboardJSONShape(t *testing.T) {
	t.Parallel()

	entries, err := testEmbedded.ReadDir("dashboards")
	if err != nil {
		t.Fatalf("read dashboards dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("dashboards/ is empty; nothing to ship")
	}

	uidRe := regexp.MustCompile(`^[a-z][a-z0-9-]{2,80}$`)
	// Metric names we know exist (regenerated if the package ever
	// renames them). Keep this list tight: it intentionally does NOT
	// include legacy or removed metric names. The list only covers
	// metrics the manager itself emits — node_exporter / cadvisor /
	// external Prometheus exporters are out of scope here.
	knownMetrics := map[string]struct{}{
		"agentteams_mcp_call_total":              {},
		"agentteams_mcp_call_duration_seconds":   {},
		"agentteams_higress_resolve_total":       {},
		"agentteams_plugin_sync_total":           {},
		"loop_phase_total":                       {},
		"loop_phase_duration_seconds":            {},
		"loop_db_approved_decision_lookup_total": {},
	}

	// Only validate metric names that look like they're owned by this
	// project (one of these prefixes). node_exporter / cadvisor /
	// alertmanager / Prometheus internals all fall outside the check.
	ownedPrefixes := []string{"agentteams_", "loop_", "opskeeper_", "opskeeper_"}

	seenUIDs := map[string]string{} // uid → filename
	// Match any *_<total|seconds> candidate token, then we filter by
	// prefix below. Single capture group = metric name.
	exprMetricRe := regexp.MustCompile(`\b([a-z][a-z0-9_]+_(?:total|seconds))\b`)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := e.Name()
		raw, err := testEmbedded.ReadFile("dashboards/" + name)
		if err != nil {
			t.Errorf("%s: read: %v", name, err)
			continue
		}

		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Errorf("%s: invalid JSON: %v", name, err)
			continue
		}

		uid, _ := doc["uid"].(string)
		if uid == "" {
			t.Errorf("%s: missing uid", name)
		} else if !uidRe.MatchString(uid) {
			t.Errorf("%s: uid %q does not match %s", name, uid, uidRe)
		} else if prev, dup := seenUIDs[uid]; dup {
			t.Errorf("%s: uid %q already used by %s — Grafana upsert would clobber one", name, uid, prev)
		} else {
			seenUIDs[uid] = name
		}

		title, _ := doc["title"].(string)
		if title == "" {
			t.Errorf("%s: missing title", name)
		}

		panels, _ := doc["panels"].([]any)
		if len(panels) == 0 {
			t.Errorf("%s: panels is empty", name)
		}

		// Walk every panel and every target, scrape metric names from
		// each PromQL `expr` and assert they exist in knownMetrics.
		for pi, raw := range panels {
			panel, _ := raw.(map[string]any)
			targets, _ := panel["targets"].([]any)
			for ti, traw := range targets {
				tgt, _ := traw.(map[string]any)
				expr, _ := tgt["expr"].(string)
				if expr == "" {
					continue
				}
				for _, m := range exprMetricRe.FindAllString(expr, -1) {
					owned := false
					for _, p := range ownedPrefixes {
						if strings.HasPrefix(m, p) {
							owned = true
							break
						}
					}
					if !owned {
						continue // node_exporter / external — skip
					}
					if _, ok := knownMetrics[m]; !ok {
						t.Errorf("%s: panel[%d].targets[%d] expr=%q references unknown metric %q — check pkg/prom or extend knownMetrics", name, pi, ti, expr, m)
					}
				}
			}
		}
	}
}

// TestAgentTeamsLoopDashboardSpecifically guards the OPSKEEPER-24 companion
// dashboard: it must surface the three new metric families introduced
// in commits 627e754 / 7520b85 / 1aead7d. If anyone removes this file
// or strips its panels without updating knownMetrics, this fails.
func TestAgentTeamsLoopDashboardSpecifically(t *testing.T) {
	t.Parallel()

	raw, err := testEmbedded.ReadFile("dashboards/agentteams-loop-overview.json")
	if err != nil {
		t.Fatalf("read: %v (OPSKEEPER-24 companion dashboard must exist)", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if doc["uid"] != "opskeeper-agentteams-loop" {
		t.Errorf("uid = %v, want opskeeper-agentteams-loop", doc["uid"])
	}

	panels, _ := doc["panels"].([]any)
	if len(panels) < 5 {
		t.Fatalf("panels = %d, want >= 5 (companion dashboard should cover the three new metric families)", len(panels))
	}

	// Each of the three must appear at least once across all panel exprs.
	mustHave := []string{
		"loop_phase_total",                       // 627e754 + 7520b85 (severity escalation label)
		"agentteams_mcp_call_total",              // 627e754
		"loop_db_approved_decision_lookup_total", // 1aead7d (tenant_mismatch label)
	}
	allExprs := ""
	for _, raw := range panels {
		panel, _ := raw.(map[string]any)
		targets, _ := panel["targets"].([]any)
		for _, traw := range targets {
			tgt, _ := traw.(map[string]any)
			if e, _ := tgt["expr"].(string); e != "" {
				allExprs += e + "\n"
			}
		}
	}
	for _, m := range mustHave {
		if !strings.Contains(allExprs, m) {
			t.Errorf("companion dashboard missing any reference to %q in panel exprs; cannot claim OPSKEEPER-24 coverage", m)
		}
	}
}
