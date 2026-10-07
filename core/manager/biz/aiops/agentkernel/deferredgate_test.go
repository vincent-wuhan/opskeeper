package agentkernel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

var _ ports.ApprovalGate = (*DeferredGate)(nil)

// TestADeferredToolIsGrantedWithItsOwnDigest proves the two properties the
// kernel checks: the call runs, and the grant is bound to the call rather
// than to nothing. A deferral that returned an empty digest would be refused
// by the kernel and the tool would never see its own approval, so the
// operator's card would sit in the inbox with nothing behind it.
func TestADeferredToolIsGrantedWithItsOwnDigest(t *testing.T) {
	inner := &fakeInbox{proposeID: "row-1"}
	g := NewDeferredGate(newTestGate(inner), []string{"cloud_bash"})

	req := approvalRequest()
	req.ToolName = "cloud_bash"

	d, err := g.Request(context.Background(), req)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if d.Decision != ports.ApprovalGranted {
		t.Fatalf("decision = %q, want a grant", d.Decision)
	}
	if d.Digest != req.Digest {
		t.Fatalf("digest = %q, want the request's own", d.Digest)
	}
	if d.RequestID != req.ID {
		t.Fatalf("request id = %q, want %q", d.RequestID, req.ID)
	}
	// The wrapped gate must not have been consulted: consulting it would
	// queue a second card for a call the tool is about to queue itself.
	if len(inner.proposed) != 0 {
		t.Fatalf("the inner gate queued %d rows for a deferred tool", len(inner.proposed))
	}
}

// TestAnUndeclaredToolIsAskedThroughTheWrappedGate is the fail-closed half:
// a mutating tool nobody declared reaches a human rather than running.
func TestAnUndeclaredToolIsAskedThroughTheWrappedGate(t *testing.T) {
	inner := &fakeInbox{proposeID: "row-7", decision: ports.Decision{
		RequestID: "row-7", Decision: ports.ApprovalGranted}}
	g := NewDeferredGate(newTestGate(inner), []string{"cloud_bash"})

	req := approvalRequest()
	req.ToolName = "a_tool_nobody_declared"

	d, err := g.Request(context.Background(), req)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if len(inner.proposed) != 1 {
		t.Fatalf("the inner gate queued %d rows, want 1", len(inner.proposed))
	}
	if d.Decision != ports.ApprovalGranted || d.Digest != req.Digest {
		t.Fatalf("decision = %+v, want the inner gate's grant rebound to the request", d)
	}
}

// TestADeclaredToolRenamedIsRefused proves the set is matched by exact name.
// A prefix or substring match would keep granting a tool after it was renamed
// out of the list — the one drift this gate cannot detect on its own.
func TestADeclaredToolRenamedIsRefused(t *testing.T) {
	g := NewDeferredGate(nil, []string{"cloud_bash"})
	req := approvalRequest()
	req.ToolName = "cloud_bash_v2"

	_, err := g.Request(context.Background(), req)
	if err == nil {
		t.Fatal("a renamed tool was granted")
	}
	var ge *ports.GateError
	if !errors.As(err, &ge) || ge.Reason != ports.GateDenied {
		t.Fatalf("err = %v, want a denial", err)
	}
}

// TestWithNoInnerGateAnUndeclaredToolIsRefused documents the fail-closed
// reading of an unassembled gate: no human to ask means no approval, not a
// pass-through.
func TestWithNoInnerGateAnUndeclaredToolIsRefused(t *testing.T) {
	g := NewDeferredGate(nil, []string{"cloud_bash"})
	req := approvalRequest()
	req.ToolName = "apply_config_change"

	_, err := g.Request(context.Background(), req)
	var ge *ports.GateError
	if !errors.As(err, &ge) || ge.Reason != ports.GateDenied {
		t.Fatalf("err = %v, want a denial", err)
	}
	// The reason must name the tool: an operator reading a refusal has to be
	// able to tell which call had no approval path.
	if ge.Err == nil || !strings.Contains(ge.Err.Error(), "apply_config_change") {
		t.Fatalf("reason = %v, want it to name the tool", ge.Err)
	}
}

// TestPendingIsTheWrappedGatesQueue proves the deferral does not invent a
// queue of its own: the settled tools' cards live in the tool, and telling a
// reconnecting console about an empty queue here would hide them.
func TestPendingIsTheWrappedGatesQueue(t *testing.T) {
	inner := &fakeInbox{open: []ports.ApprovalRequest{approvalRequest()}}
	g := NewDeferredGate(newTestGate(inner), []string{"cloud_bash"})

	got, err := g.Pending(context.Background(), "s-1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(got) != 1 || inner.openSess != "s-1" {
		t.Fatalf("pending = %v, asked %q", got, inner.openSess)
	}

	if _, err := NewDeferredGate(nil, nil).Pending(context.Background(), "s-1"); !errors.Is(err, ErrGateNotWired) {
		t.Fatalf("err = %v, want ErrGateNotWired", err)
	}
}

// TestAddDeclaresAToolAfterConstruction proves the second declaration site
// works: MCP tools are created from the configured servers long after the
// gate exists, and a declaration that only worked at construction would
// leave every one of them raising a duplicate approval card.
func TestAddDeclaresAToolAfterConstruction(t *testing.T) {
	g := NewDeferredGate(nil, []string{"cloud_bash"})
	if g.Declares("mcp_delete_pod") {
		t.Fatal("an undeclared tool reported as declared")
	}
	g.Add("mcp_delete_pod")
	if !g.Declares("mcp_delete_pod") {
		t.Fatal("Add did not declare the tool")
	}
	// The declaration is what makes the call run rather than raise a card.
	req := approvalRequest()
	req.ToolName = "mcp_delete_pod"
	d, err := g.Request(context.Background(), req)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if d.Decision != ports.ApprovalGranted || d.Digest != req.Digest {
		t.Fatalf("decision = %+v, want a bound grant", d)
	}
	// Empty names are dropped rather than stored as a matchable "".
	g.Add("", "")
	if g.Declares("") {
		t.Fatal("an empty name was declared")
	}
	if got := g.Declared(); len(got) != 2 {
		t.Fatalf("declared = %v, want the two real names", got)
	}
}

// fakeTool is the minimum BaseTool the declaration check needs.
type fakeTool struct {
	name  string
	class string
}

func (f fakeTool) Info(context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{Name: f.name, Class: f.class}, nil
}

func (f fakeTool) InvokableRun(context.Context, string, ...basetool.InvokeOption) (string, error) {
	return "", nil
}

// TestUndeclaredMutatingToolsNamesWhatWasForgotten is the drift detector: a
// new mutating tool that nobody declared is reported by name at assembly,
// instead of surfacing as a second approval card at run time.
func TestUndeclaredMutatingToolsNamesWhatWasForgotten(t *testing.T) {
	g := NewDeferredGate(nil, []string{"cloud_bash", "AgentTool"})
	bag := []basetool.BaseTool{
		fakeTool{name: "query_promql", class: "read"},
		fakeTool{name: "cloud_bash", class: "destructive"},
		fakeTool{name: "AgentTool", class: "write"},
		fakeTool{name: "brand_new_mutator", class: "write"},
		nil,
	}

	missing, err := UndeclaredMutatingTools(context.Background(), bag, g.Declares)
	if err != nil {
		t.Fatalf("UndeclaredMutatingTools: %v", err)
	}
	if len(missing) != 1 || missing[0] != "brand_new_mutator" {
		t.Fatalf("missing = %v, want exactly the undeclared mutator", missing)
	}

	// Declaring it clears the report: the check is about the declaration
	// existing somewhere, not about which list it came from.
	g.Add("brand_new_mutator")
	missing, err = UndeclaredMutatingTools(context.Background(), bag, g.Declares)
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing = %v, err = %v, want none", missing, err)
	}
}
