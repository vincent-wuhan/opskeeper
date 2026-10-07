package decorators

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
)

// countingGate records what it was asked and answers a fixed way.
type countingGate struct {
	asked   int
	lastTyp string
	lastID  string
	err     error
}

func (g *countingGate) Check(_ context.Context, resourceType, resourceID string) error {
	g.asked++
	g.lastTyp, g.lastID = resourceType, resourceID
	return g.err
}

// A refusal must stop the tool, and must say which resource and why: a
// refusal that says "forbidden" is a support ticket.
func TestTheGateRefusesBeforeTheToolRuns(t *testing.T) {
	tool := &fakeTool{name: "get_device", result: "ok"}
	gate := &countingGate{err: errors.New("device/db-7 is labelled Restricted and your reader tier does not reach it")}
	wrapped := WithSensitivity(tool, gate)

	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{UserID: 7, Role: "user"})
	out, err := wrapped.InvokableRun(ctx, `{"device_id":"db-7"}`)
	if err == nil {
		t.Fatalf("the call was served: %q", out)
	}
	if !strings.Contains(err.Error(), "device/db-7") || !strings.Contains(err.Error(), "Restricted") {
		t.Errorf("the refusal does not name the resource and the level: %s", err)
	}
}

// A call the gate allows is a call the tool sees, unchanged. The gate is not
// allowed to become a filter that reshapes the tool's world.
func TestAnAllowedCallReachesTheTool(t *testing.T) {
	gate := &countingGate{}
	wrapped := WithSensitivity(&fakeTool{name: "get_device", result: "ok"}, gate)

	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{UserID: 7})
	out, err := wrapped.InvokableRun(ctx, `{"device_id":"db-7"}`)
	if err != nil {
		t.Fatalf("an allowed call failed: %v", err)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("output = %q, want the tool's own", out)
	}
	if gate.lastTyp != "device" || gate.lastID != "db-7" {
		t.Errorf("the gate was asked about %s/%s, want device/db-7", gate.lastTyp, gate.lastID)
	}
}

// Two kinds of caller carry no tier and cannot ever satisfy one. A gate that
// demanded one from them would refuse the public path and every service
// identity, which is not a security control but an outage.
func TestCallersWithoutAHumanIdentityAreNotGated(t *testing.T) {
	cases := map[string]context.Context{
		"no tenant at all": context.Background(),
		"a service identity": tenantctx.With(context.Background(), tenantctx.Tenant{
			Role: "worker",
			AgentTeams: &tenantctx.AgentTeamsIdentity{
				TenantID: "acme", Service: "opskeeper", Worker: "opskeeper-sre", Role: "sre",
			},
		}),
	}
	for name, ctx := range cases {
		gate := &countingGate{err: errors.New("refused")}
		wrapped := WithSensitivity(&fakeTool{name: "get_device", result: "ok"}, gate)
		if _, err := wrapped.InvokableRun(ctx, `{"device_id":"db-7"}`); err != nil {
			t.Errorf("%s: the call was refused: %v", name, err)
		}
		if gate.asked != 0 {
			t.Errorf("%s: the gate was asked %d times; a caller with no tier was checked anyway",
				name, gate.asked)
		}
	}
}

// A tool whose arguments name no resource is asked with an empty pair rather
// than skipped, so the gate — not this decorator — owns the decision about
// unlabeled resources.
func TestAToolThatNamesNoResourceStillAsksTheGate(t *testing.T) {
	gate := &countingGate{}
	wrapped := WithSensitivity(&fakeTool{name: "get_topology", result: "ok"}, gate)

	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{UserID: 7})
	if _, err := wrapped.InvokableRun(ctx, `{"format":"json"}`); err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if gate.asked != 1 {
		t.Fatalf("the gate was asked %d times, want once", gate.asked)
	}
	if gate.lastTyp != "" || gate.lastID != "" {
		t.Errorf("the gate was asked about %s/%s, want an empty pair", gate.lastTyp, gate.lastID)
	}
}

// A list argument names several resources and the gate is asked about one of
// them. Which one is a decision: the first is chosen so the answer is stable,
// and the gap — that a call naming twelve devices is checked against the
// first one only — is stated here rather than left to be discovered.
func TestAListArgumentIsCheckedInFullNotByItsFirstEntry(t *testing.T) {
	gate := &recordingGate{}
	wrapped := WithSensitivity(&fakeTool{name: "list_devices", result: "ok"}, gate)

	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{UserID: 7})
	if _, err := wrapped.InvokableRun(ctx, `{"device_ids":["db-1","db-2"]}`); err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if got := gate.seen; len(got) != 2 || got[0] != "db-1" || got[1] != "db-2" {
		t.Errorf("the gate was asked about %v, want every entry in order", got)
	}
}

// The order of the list is the caller's to choose, so deciding the set by
// its first member is a gate that is walked past by moving one argument. A
// gate that answers a set in one call is asked once, and refuses when any
// member of the set refuses.
func TestABatchGateIsAskedOnceAndARefusalAnywhereRefuses(t *testing.T) {
	gate := &batchGate{failOn: "db-2"}
	wrapped := WithSensitivity(&fakeTool{name: "list_devices", result: "ok"}, gate)

	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{UserID: 7})
	out, err := wrapped.InvokableRun(ctx, `{"device_ids":["db-1","db-2","db-3"]}`)
	if err == nil {
		t.Fatalf("the call was served: %q", out)
	}
	if gate.calls != 1 {
		t.Errorf("the batch gate was asked %d times, want once for the whole set", gate.calls)
	}
	if !strings.Contains(err.Error(), "db-2") {
		t.Errorf("the refusal does not name the entry that failed: %s", err)
	}
}

// Numbers are ids too. A numeric id compared as a string would never match a
// label row and would quietly pass everything.
func TestANumericIdIsNotComparedAsNothing(t *testing.T) {
	gate := &countingGate{}
	wrapped := WithSensitivity(&fakeTool{name: "get_device", result: "ok"}, gate)

	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{UserID: 7})
	if _, err := wrapped.InvokableRun(ctx, `{"device_id":42}`); err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if gate.lastID != "42" {
		t.Errorf("the gate was asked about %q, want \"42\"", gate.lastID)
	}
}

// Unwired is unwired, not broken: a deployment without Data-Guard keeps the
// tools it had.
func TestANilGateLeavesTheBagAlone(t *testing.T) {
	tool := &fakeTool{name: "get_device", result: "ok"}
	if got := WithSensitivity(tool, nil); got != basetool.BaseTool(tool) {
		t.Errorf("a nil gate still wrapped the tool: %T", got)
	}
	tools := []basetool.BaseTool{tool}
	if got := WithSensitivityAll(tools, nil); len(got) != 1 || got[0] != tools[0] {
		t.Error("WithSensitivityAll rebuilt the bag with no gate configured")
	}
	if got := WithSensitivityAll(nil, &countingGate{}); got != nil {
		t.Error("WithSensitivityAll invented tools for an empty bag")
	}
}

// recordingGate remembers every id it was asked about, in order.
type recordingGate struct {
	seen []string
}

func (g *recordingGate) Check(_ context.Context, _, resourceID string) error {
	g.seen = append(g.seen, resourceID)
	return nil
}

// batchGate answers a set in one call and refuses when the set contains
// failOn — the shape a real implementation has, so the decorator's use of it
// is exercised rather than assumed.
type batchGate struct {
	calls  int
	failOn string
}

func (g *batchGate) Check(_ context.Context, _, resourceID string) error {
	if resourceID == g.failOn {
		return errors.New("tier does not reach it")
	}
	return nil
}

func (g *batchGate) CheckAll(_ context.Context, _ string, resourceIDs []string) error {
	g.calls++
	for _, id := range resourceIDs {
		if err := g.Check(context.Background(), "", id); err != nil {
			return err
		}
	}
	return nil
}
