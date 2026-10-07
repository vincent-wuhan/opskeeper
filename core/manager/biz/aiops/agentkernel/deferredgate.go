package agentkernel

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// DeferredGate routes every mutating call to one of two answers: it runs now
// because its approval question is already answered elsewhere, or a human is
// asked by the wrapped gate.
//
// It exists because OpsKeeper's approval story is per tool, not per class.
// The kernel can only see a tool's class — read or not — while the host knows
// that a tool is dangerous AND knows which mechanism already settles it:
//
//   - cloud_bash / host_bash / install_skill: the tool blocks on its own
//     propose-and-await inside InvokableRun and renders the approval card
//     itself (HLD-021). Wrapping them in a second approval would ask the
//     operator twice for one call.
//   - apply_config_change: the mutating half of the draft/confirm pair. The
//     human confirmation is the user's `confirmed=true` on the exact draft
//     hash, checked inside the tool.
//   - AgentTool / SendMessage / TaskStop / serve_page / send_im_message: the
//     coordination and output primitives. They change no infrastructure —
//     they spawn a worker, post a card, or reply — and the graph path has
//     never asked for approval before running them.
//
// A tool that is in neither list is passed to the wrapped gate, which asks a
// human. That direction is deliberate: the failure mode of this list being
// incomplete is an extra question, never an unapproved change.
//
// The list is not configuration. It is assembled next to the constructors
// that decide it (see selfSettledToolNames in cmd/opskeeper/main.go), because
// a tool whose mechanism changed and whose name stayed in the list would be
// granted without its own approval ever running — the one thing this gate
// cannot detect by itself.
type DeferredGate struct {
	inner ports.ApprovalGate
	// mu guards settled. Declarations arrive from two places — the static
	// tool registry and the MCP registrar — and the gate is consulted from
	// live turns, so the map is written during assembly and read afterwards.
	mu sync.RWMutex
	// settled names the tools whose approval is already handled without the
	// kernel. A name is granted with the request's own digest, because the
	// grant is asserting a fact about the call the tool is about to put in
	// front of a human.
	settled map[string]bool
}

// NewDeferredGate wraps inner and defers the named tools. A nil inner gate is
// accepted and turns "not settled" into a refusal: a deployment that has no
// human to ask must not run an unsettled mutating call.
func NewDeferredGate(inner ports.ApprovalGate, settled []string) *DeferredGate {
	set := make(map[string]bool, len(settled))
	for _, n := range settled {
		if n != "" {
			set[n] = true
		}
	}
	return &DeferredGate{inner: inner, settled: set}
}

// Add declares more settled tools.
//
// It exists because not every tool is known when the gate is built: the MCP
// tools are created from the configured servers later in assembly, and their
// risk class is inferred from the server's own naming. Adding them where they
// are created is what keeps the declaration next to the decision it
// describes — a second static list elsewhere would drift.
func (g *DeferredGate) Add(names ...string) {
	if g == nil || len(names) == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.settled == nil {
		g.settled = make(map[string]bool, len(names))
	}
	for _, n := range names {
		if n != "" {
			g.settled[n] = true
		}
	}
}

// Declares reports whether a tool has been declared settled.
func (g *DeferredGate) Declares(name string) bool {
	if g == nil {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.settled[name]
}

// Declared returns the declared names, sorted, for boot logging and for the
// assembly check that every mutating tool was declared by someone.
func (g *DeferredGate) Declared() []string {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]string, 0, len(g.settled))
	for n := range g.settled {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// UndeclaredMutatingTools returns the names of tools in the bag that are not
// read-class and that nobody declared settled.
//
// With a wired inner gate these calls would each raise an approval card, and
// for a tool that already asks for approval itself that card is a duplicate —
// so this is reported as an assembly error rather than left to appear at run
// time. A tool that genuinely needs kernel-side approval is declared by
// leaving it out and registering an executor for it, not by forgetting it.
func UndeclaredMutatingTools(ctx context.Context, tools []basetool.BaseTool, declared func(string) bool) ([]string, error) {
	var missing []string
	for _, t := range tools {
		if t == nil {
			continue
		}
		info, err := t.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("agentkernel: tool info while checking declarations: %w", err)
		}
		if info == nil || info.Name == "" {
			continue
		}
		if domain.ToolClass(info.Class) == domain.ClassRead {
			continue
		}
		if declared != nil && declared(info.Name) {
			continue
		}
		missing = append(missing, info.Name)
	}
	sort.Strings(missing)
	return missing, nil
}

// Request answers one mutating call.
func (g *DeferredGate) Request(ctx context.Context, req ports.ApprovalRequest) (ports.Decision, error) {
	if g != nil && g.Declares(req.ToolName) {
		return ports.Decision{
			RequestID: req.ID,
			Digest:    req.Digest,
			Decision:  ports.ApprovalGranted,
			Note:      "approval is handled by the tool",
		}, nil
	}
	if g == nil || g.inner == nil {
		return ports.Decision{}, &ports.GateError{
			Reason: ports.GateDenied,
			Err:    fmt.Errorf("no approval path is wired for tool %q", req.ToolName),
		}
	}
	return g.inner.Request(ctx, req)
}

// Pending reports the queue a reconnecting console should render.
//
// It is the wrapped gate's queue, not this gate's: the settled tools keep
// their own cards (the tool renders them), and a console that asked this
// layer for them would be told about a queue that does not exist here.
func (g *DeferredGate) Pending(ctx context.Context, sessionID string) ([]ports.ApprovalRequest, error) {
	if g == nil || g.inner == nil {
		return nil, ErrGateNotWired
	}
	return g.inner.Pending(ctx, sessionID)
}

var _ ports.ApprovalGate = (*DeferredGate)(nil)
