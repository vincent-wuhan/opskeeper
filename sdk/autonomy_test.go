package sdk

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// autonomyYAML is a package asking for one self-heal action. It is the
// smallest thing that is still an autonomy request, and it is here as a
// literal rather than a builder so that a change to the wire shape shows up
// as a diff in this file.
const autonomyYAML = `
apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: opskeeper-sre-autonomy
  version: 0.1.0
  vendor: opskeeper
spec:
  targets: [edge]
  safety_level: L2
  capabilities: [write]
  tools:
    - { name: host_restart_service, class: write }
  autonomy:
    offline_after: 2m
    actions:
      - name: restart-orders-on-disk-full
        tool: host_restart_service
        trigger: { kind: metric_above, metric: node_disk_used_ratio, threshold: 0.92 }
        argv: [systemctl, restart, orders-api]
        blast_radius: pod
        ttl: 30m
        idempotency_key: "restart-orders:{{target}}:{{window}}"
`

func loadAutonomy(t *testing.T, spec string) (domain.PluginManifest, error) {
	t.Helper()
	return parseManifest(t, []byte(spec))
}

// parseManifest runs a manifest literal through the same decode path Load
// uses, so these tests cover the codecs rather than a hand-built struct.
func parseManifest(t *testing.T, data []byte) (domain.PluginManifest, error) {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	var m domain.PluginManifest
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m, Validate(m)
}

// TestAnAutonomyBlockSurvivesTheManifestCodecs pins the wire format.
//
// It is here because the durations are the fragile part: this module has no
// dependencies, so domain.Duration cannot implement UnmarshalYAML and relies
// on both decoders honouring the text interfaces instead. That is an
// assumption about two libraries, and an assumption about a library is a
// test or it is nothing.
func TestAnAutonomyBlockSurvivesTheManifestCodecs(t *testing.T) {
	m, err := loadAutonomy(t, autonomyYAML)
	if err != nil {
		t.Fatalf("a well-formed autonomy block was refused: %v", err)
	}
	policy := m.Spec.Autonomy
	if len(policy.Actions) != 1 {
		t.Fatalf("actions = %d, want 1", len(policy.Actions))
	}
	a := policy.Actions[0]
	if got := time.Duration(a.TTL); got != 30*time.Minute {
		t.Errorf("ttl = %s, want 30m: the duration codec lost the unit", got)
	}
	if got := time.Duration(policy.OfflineAfter); got != 2*time.Minute {
		t.Errorf("offline_after = %s, want 2m", got)
	}
	if a.Trigger.Kind != domain.TriggerMetricAbove || a.Trigger.Metric != "node_disk_used_ratio" || a.Trigger.Threshold != 0.92 {
		t.Errorf("trigger = %+v, want the declared metric_above comparison", a.Trigger)
	}
	if !a.Trigger.SameTrigger(a.Trigger) {
		t.Error("a trigger does not match itself")
	}
	if key := a.DeriveKey("orders-api-7d9", "2026-10-02T04"); key != "restart-orders:orders-api-7d9:2026-10-02T04" {
		t.Errorf("derived key = %q, want the template rendered with the host's own values", key)
	}
}

// TestTheAutonomyRefusalsAreNamed pins the load-time refusals.
//
// Each row is a way a package could ask for more than the node will grant,
// and the assertion is on the field the operator is pointed at: a load error
// that says "autonomy is wrong" and not which line is a support ticket.
func TestTheAutonomyRefusalsAreNamed(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(spec *string)
		field string
	}{
		{
			name: "a shell string instead of an argument vector",
			mut: func(spec *string) {
				replaceIn(spec, "argv: [systemctl, restart, orders-api]", `argv: ["systemctl restart orders-api | tee /tmp/x"]`)
			},
			field: "spec.autonomy.actions[0].argv",
		},
		{
			name: "a metacharacter smuggled into one argument",
			mut: func(spec *string) {
				replaceIn(spec, "argv: [systemctl, restart, orders-api]", "argv: [systemctl, restart, orders-api; curl evil]")
			},
			field: "spec.autonomy.actions[0].argv",
		},
		{
			name: "an empty argument vector",
			mut: func(spec *string) {
				replaceIn(spec, "argv: [systemctl, restart, orders-api]", "argv: []")
			},
			field: "spec.autonomy.actions[0].argv",
		},
		{
			name: "a namespace-wide blast radius",
			mut: func(spec *string) {
				replaceIn(spec, "blast_radius: pod", "blast_radius: namespace")
			},
			field: "spec.autonomy.actions[0].blast_radius",
		},
		{
			name: "a TTL beyond the host's cap",
			mut: func(spec *string) {
				replaceIn(spec, "ttl: 30m", "ttl: 24h")
			},
			field: "spec.autonomy.actions[0].ttl",
		},
		{
			name: "a constant idempotency key, spent by the first run",
			mut: func(spec *string) {
				replaceIn(spec, `idempotency_key: "restart-orders:{{target}}:{{window}}"`, `idempotency_key: "restart-orders"`)
			},
			field: "spec.autonomy.actions[0].idempotency_key",
		},
		{
			name: "an unknown trigger kind",
			mut: func(spec *string) {
				replaceIn(spec, "kind: metric_above", "kind: whenever_it_feels_right")
			},
			field: "spec.autonomy.actions[0].trigger",
		},
		{
			name: "an action aimed at an undeclared tool",
			mut: func(spec *string) {
				replaceIn(spec, "tool: host_restart_service", "tool: host_drop_database")
			},
			field: "spec.autonomy.actions[0].tool",
		},
		{
			name: "an action on a read, which needs no autonomy at all",
			mut: func(spec *string) {
				*spec = strings.Replace(*spec, "{ name: host_restart_service, class: write }",
					"{ name: host_restart_service, class: write }\n    - { name: host_dmesg, class: read }", 1)
				replaceIn(spec, "tool: host_restart_service", "tool: host_dmesg")
			},
			field: "spec.autonomy.actions[0].tool",
		},
		{
			name: "an offline threshold below the tunnel's reconnect cycle",
			mut: func(spec *string) {
				replaceIn(spec, "offline_after: 2m", "offline_after: 2s")
			},
			field: "spec.autonomy.offline_after",
		},
		{
			name: "a threshold with nothing to run under it",
			mut: func(spec *string) {
				i := strings.Index(*spec, "  autonomy:")
				if i < 0 {
					panic("the fixture has no autonomy block")
				}
				*spec = (*spec)[:i] + "  autonomy:\n    offline_after: 2m\n"
			},
			field: "spec.autonomy.offline_after",
		},
		{
			name: "two actions driving one tool",
			mut: func(spec *string) {
				*spec = strings.Replace(*spec,
					`        idempotency_key: "restart-orders:{{target}}:{{window}}"`,
					`        idempotency_key: "restart-orders:{{target}}:{{window}}"
      - name: restart-orders-again
        tool: host_restart_service
        trigger: { kind: metric_above, metric: node_disk_used_ratio, threshold: 0.95 }
        argv: [systemctl, restart, orders-api]
        blast_radius: pod
        ttl: 30m
        idempotency_key: "restart-again:{{target}}:{{window}}"`, 1)
			},
			field: "spec.autonomy.actions[1].tool",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := autonomyYAML
			tc.mut(&spec)
			_, err := loadAutonomy(t, spec)
			if err == nil {
				t.Fatalf("the manifest was admitted: %s is a way to ask for more than the node grants", tc.name)
			}
			le, ok := err.(*LoadError)
			if !ok {
				t.Fatalf("error = %v, want a *LoadError", err)
			}
			if le.Field != tc.field {
				t.Errorf("field = %q, want %q (reason: %s)", le.Field, tc.field, le.Reason)
			}
		})
	}
}

// TestAPackageWithoutAutonomyIsUnchanged is the regression that matters
// most: the block is new, and every package that ships today omits it.
//
// If adding autonomy changed how an ordinary manifest is read, the failure
// would show up as a fleet of nodes refusing to install packages that were
// working an hour ago.
func TestAPackageWithoutAutonomyIsUnchanged(t *testing.T) {
	m, err := loadAutonomy(t, strings.Replace(autonomyYAML, autonomyBlock(t), "", 1))
	if err != nil {
		t.Fatalf("a manifest with no autonomy block was refused: %v", err)
	}
	if len(m.Spec.Autonomy.Actions) != 0 {
		t.Errorf("actions = %d, want 0", len(m.Spec.Autonomy.Actions))
	}
	// And the node's own default is what an empty block falls back to, not
	// "no threshold", which would fire the whole list on a blip.
	if got := m.Spec.Autonomy.OfflineDelay(); got != domain.DefaultAutonomyOfflineAfter {
		t.Errorf("offline delay = %s, want the host default %s", got, domain.DefaultAutonomyOfflineAfter)
	}
}

// autonomyBlock is the manifest's autonomy section, located rather than
// pasted so the two literals cannot drift apart.
func autonomyBlock(t *testing.T) string {
	t.Helper()
	i := strings.Index(autonomyYAML, "  autonomy:")
	if i < 0 {
		t.Fatal("the fixture has no autonomy block")
	}
	return autonomyYAML[i:]
}

func replaceIn(spec *string, old, new string) {
	if !strings.Contains(*spec, old) {
		panic("the fixture no longer contains " + old)
	}
	*spec = strings.Replace(*spec, old, new, 1)
}
