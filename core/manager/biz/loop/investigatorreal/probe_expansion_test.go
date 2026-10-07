package investigatorreal

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	loop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

// argRecordingTools is a stub that remembers the ARGUMENTS of every call,
// which is the only thing a ForEach expansion can be wrong about in a way
// that matters: the tool name will always be right, and the question is
// whether each call was aimed at the claim it was derived from.
type argRecordingTools struct {
	specs   map[string]loop.ToolSpec
	results map[string]any
	// perKey lets one tool answer differently depending on the argument it
	// was given, which is what a real measurement does.
	perKey  map[string]map[string]any
	seen    []call
	failOn  string
	readErr error
}

type call struct {
	tool string
	args map[string]any
}

func (s *argRecordingTools) LookupTool(name string) (loop.ToolSpec, bool) {
	spec, ok := s.specs[name]
	return spec, ok
}

func (s *argRecordingTools) CallTool(_ context.Context, name string, args map[string]any) (any, error) {
	s.seen = append(s.seen, call{tool: name, args: args})
	if name == s.failOn {
		return nil, fmt.Errorf("stubbed failure")
	}
	if s.readErr != nil {
		return nil, s.readErr
	}
	if byKey, ok := s.perKey[name]; ok {
		key := fmt.Sprint(args["pvc"])
		if result, ok := byKey[key]; ok {
			return result, nil
		}
	}
	return s.results[name], nil
}

func (s *argRecordingTools) callsTo(tool string) []call {
	var out []call
	for _, c := range s.seen {
		if c.tool == tool {
			out = append(out, c)
		}
	}
	return out
}

func volumePlan(sourceResult any, sourceErr bool) *argRecordingTools {
	tools := &argRecordingTools{
		specs: map[string]loop.ToolSpec{
			"k8s.pvc_list":  readOnly("k8s.pvc_list"),
			"k8s.pvc_usage": diagnostic("k8s.pvc_usage"),
		},
		results: map[string]any{
			"k8s.pvc_list": sourceResult,
			"k8s.pvc_usage": rowsOf(
				map[string]any{"name": "x", "namespace": "y", "measured": true, "used_percent": 50.0},
			),
		},
	}
	if sourceErr {
		tools.failOn = "k8s.pvc_list"
	}
	return tools
}

// The expansion is the point: a plan entry becomes one call per claim the
// cluster named, and each call carries the name and namespace from the very
// row that produced it.
func TestProbeForEach_MeasuresEveryClaimTheClusterNamed(t *testing.T) {
	tools := volumePlan(rowsOf(
		map[string]any{"name": "data-pvc", "namespace": "test"},
		map[string]any{"name": "logs-pvc", "namespace": "test"},
	), false)
	tools.perKey = map[string]map[string]any{
		"k8s.pvc_usage": {
			"data-pvc": rowsOf(map[string]any{"name": "data-pvc", "namespace": "test", "measured": true, "used_percent": 99.0}),
			"logs-pvc": rowsOf(map[string]any{"name": "logs-pvc", "namespace": "test", "measured": true, "used_percent": 12.0}),
		},
	}

	items := probe(context.Background(), tools, "k8s", timeZero)

	calls := tools.callsTo("k8s.pvc_usage")
	if len(calls) != 2 {
		t.Fatalf("made %d measurements, want one per claim: %#v", len(calls), calls)
	}
	got := make([]string, 0, len(calls))
	for _, c := range calls {
		if c.args["pvc"] == nil || c.args["pvc"] == "" {
			t.Fatalf("a measurement was made with no claim name: %#v", c.args)
		}
		if c.args["namespace"] != "test" {
			t.Errorf("namespace = %v, want test — the namespace comes from the same row that named the claim", c.args["namespace"])
		}
		got = append(got, fmt.Sprint(c.args["pvc"]))
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "data-pvc,logs-pvc" {
		t.Errorf("measured %v, want both claims", got)
	}
	// And both measurements must survive into the chain, not just the first.
	full := loop.FullClaimRows(items)
	if len(full) != 1 || full[0]["name"] != "data-pvc" {
		t.Fatalf("chain carried %d full volumes, want the one that measured 99%%: %#v", len(full), full)
	}
}

// A source probe that failed has produced no rows, so there is nothing to
// iterate. The expansion must not call the dependent tool at all — and
// certainly must not call it with a blank name, which would measure whatever
// namespace the credential happens to default to.
func TestProbeForEach_MakesNoCallWhenTheSourceProducedNoRows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result any
		fails  bool
	}{
		{"the source failed", nil, true},
		{"the source returned nothing", rowsOf(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := volumePlan(tc.result, tc.fails)
			probe(context.Background(), tools, "k8s", timeZero)
			if calls := tools.callsTo("k8s.pvc_usage"); len(calls) != 0 {
				t.Fatalf("made %d measurements with no claim to measure: %#v", len(calls), calls)
			}
		})
	}
}

// A row missing the column the expansion needs is skipped, not called with
// an empty argument.
func TestProbeForEach_SkipsARowThatCannotSupplyTheArgument(t *testing.T) {
	tools := volumePlan(rowsOf(
		map[string]any{"name": "data-pvc", "namespace": "test"},
		map[string]any{"namespace": "test"}, // no name
	), false)
	probe(context.Background(), tools, "k8s", timeZero)
	calls := tools.callsTo("k8s.pvc_usage")
	if len(calls) != 1 {
		t.Fatalf("made %d measurements, want only the row that named a claim: %#v", len(calls), calls)
	}
	if calls[0].args["pvc"] != "data-pvc" {
		t.Errorf("measured %v, want data-pvc", calls[0].args["pvc"])
	}
}

// The expansion does not buy an exemption from the read-only rule. A
// dependent entry inherits the ceiling, and one that grades itself above it
// is refused however many rows it would have been expanded over.
func TestProbeForEach_DoesNotBypassTheReadOnlyCeiling(t *testing.T) {
	tools := volumePlan(rowsOf(map[string]any{"name": "data-pvc", "namespace": "test"}), false)
	tools.specs["k8s.pvc_usage"] = loop.ToolSpec{Name: "k8s.pvc_usage", RiskLevel: "L3"}
	probe(context.Background(), tools, "k8s", timeZero)
	if calls := tools.callsTo("k8s.pvc_usage"); len(calls) != 0 {
		t.Fatalf("an L3 tool was called %d times by a read-only probe plan", len(calls))
	}
}

// The plan's own Args map is never written to. Two entries sharing a literal
// must not see each other's arguments, or the second call silently inherits
// the first call's claim.
func TestExpandProbe_DoesNotMutateThePlanEntrysArgs(t *testing.T) {
	shared := map[string]any{"limit": 5}
	entry := DomainProbe{
		Tool: "k8s.pvc_usage",
		Args: shared,
		ForEach: &ForEachRow{
			Source:  "k8s.pvc_list",
			Columns: map[string]string{"pvc": "name"},
		},
	}
	rows := map[string][]map[string]any{
		"k8s.pvc_list": {{"name": "data-pvc"}},
	}
	calls := expandProbe(entry, rows)
	if len(calls) != 1 {
		t.Fatalf("expanded to %d calls, want 1", len(calls))
	}
	if _, leaked := shared["pvc"]; leaked {
		t.Error("expandProbe wrote the expanded argument into the plan's own map")
	}
}

// A probe whose grade cannot be parsed is treated as exceeding the ceiling.
// A tool that will not say what it does is not one to call unprompted, and
// that has to hold for the expanded calls too.
func TestProbeForEach_AnUngradeableToolIsNotCalled(t *testing.T) {
	tools := volumePlan(rowsOf(map[string]any{"name": "data-pvc", "namespace": "test"}), false)
	tools.specs["k8s.pvc_usage"] = loop.ToolSpec{Name: "k8s.pvc_usage", RiskLevel: "spicy"}
	probe(context.Background(), tools, "k8s", timeZero)
	if calls := tools.callsTo("k8s.pvc_usage"); len(calls) != 0 {
		t.Fatalf("a tool that grades itself %q was called %d times by the probe plan", "spicy", len(calls))
	}
}

var _ = time.Time{}

// The skew read is expanded over the topics the lag read named, and only
// those. Asking about a topic nobody is behind on would produce a skew
// finding for a topic that is fine, which is a proposal to copy a partition
// for nothing.
func TestProbeForEach_SkewIsMeasuredOnlyForTopicsThatAreBehind(t *testing.T) {
	tools := &argRecordingTools{
		specs: map[string]loop.ToolSpec{
			"mq.inspect_consumer_lag": diagnostic("mq.inspect_consumer_lag"),
			"kafka.partition_skew":    readOnly("kafka.partition_skew"),
		},
		results: map[string]any{
			"mq.inspect_consumer_lag": rowsOf(
				map[string]any{"group": "order-svc", "topic": "order.events", "total_lag": 900},
				map[string]any{"group": "order-svc", "topic": "cart.events", "total_lag": 800},
			),
			"kafka.partition_skew": rowsOf(map[string]any{"topic": "x", "partition": 0}),
		},
	}
	probe(context.Background(), tools, "mq", time.Time{})

	calls := tools.callsTo("kafka.partition_skew")
	if len(calls) != 2 {
		t.Fatalf("made %d skew reads, want one per topic the lag read named: %#v", len(calls), calls)
	}
	seen := map[string]bool{}
	for _, c := range calls {
		seen[fmt.Sprint(c.args["topic"])] = true
	}
	if !seen["order.events"] || !seen["cart.events"] {
		t.Errorf("measured %v, want both lagging topics", seen)
	}
}

// A deployment with only the neutral mq DSN does not register the
// kafka-namespaced tools. The probe must then make no call and the proposal
// must simply be absent — which is the same observable state as a cluster
// with no skewed partitions, and not an error.
func TestProbeForEach_AnUnregisteredProductToolIsNotCalled(t *testing.T) {
	tools := &argRecordingTools{
		specs: map[string]loop.ToolSpec{
			"mq.inspect_consumer_lag": diagnostic("mq.inspect_consumer_lag"),
			// kafka.partition_skew is deliberately absent.
		},
		results: map[string]any{
			"mq.inspect_consumer_lag": rowsOf(map[string]any{"group": "g", "topic": "order.events", "total_lag": 900}),
		},
	}
	items := probe(context.Background(), tools, "mq", time.Time{})
	if calls := tools.callsTo("kafka.partition_skew"); len(calls) != 0 {
		t.Fatalf("an unregistered tool was called %d times", len(calls))
	}
	if rows := loop.SkewedPartitionRows(items); len(rows) != 0 {
		t.Fatalf("the chain claims %d skewed partitions from a tool that was never called", len(rows))
	}
}
