package policygate

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// scriptedPolicy answers from a table, so a test states the policy it is
// exercising rather than reproducing one.
type scriptedPolicy struct {
	permitted     bool
	reason        string
	needsApproval bool
	// calls counts Admit-time lookups, so a test can assert the gate
	// consulted policy once per call rather than per decision.
	mu    sync.Mutex
	calls int
}

func (p *scriptedPolicy) Permitted(Call) (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.permitted, p.reason
}

func (p *scriptedPolicy) NeedsApproval(Call) bool { return p.needsApproval }

// EffectiveClass is the class the scripted policy is pretending to have
// judged. A gate test that cared about the class would set it; the
// scripted policy is for tests about the round trip, and destructive is
// the honest reading of a policy nobody configured.
func (p *scriptedPolicy) EffectiveClass(Call) domain.ToolClass { return domain.ClassDestructive }

// MaxRadius lets the scripted policy approve anything it is asked about.
// Narrowing it here would only test the clamp, which the registry tests
// cover directly.
func (p *scriptedPolicy) MaxRadius(Call) domain.BlastRadius { return domain.RadiusCluster }

// recordingAudit keeps every ledger row the gate wrote.
type recordingAudit struct {
	mu      sync.Mutex
	entries []ports.AuditEntry
	// fail makes Record error, so a test can prove a broken ledger does
	// not fail the call.
	fail bool
}

func (a *recordingAudit) Record(_ context.Context, e ports.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		return context.DeadlineExceeded
	}
	a.entries = append(a.entries, e)
	return nil
}

func (a *recordingAudit) Verify(context.Context) error { return nil }

func (a *recordingAudit) actions() []ports.AuditAction {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]ports.AuditAction, 0, len(a.entries))
	for _, e := range a.entries {
		out = append(out, e.Action)
	}
	return out
}

func (a *recordingAudit) count(action ports.AuditAction) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, e := range a.entries {
		if e.Action == action {
			n++
		}
	}
	return n
}

func (a *recordingAudit) last() (ports.AuditEntry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.entries) == 0 {
		return ports.AuditEntry{}, false
	}
	return a.entries[len(a.entries)-1], true
}

// frameRecorder keeps the approval frames the gate pushed.
type frameRecorder struct {
	mu sync.Mutex
	// frames and sessions are parallel: the console has to be able to
	// route a frame to a conversation, and a frame that arrives without
	// one is dropped by the control plane rather than guessed at.
	frames   []wire.ApprovalFrame
	sessions []string
}

func (f *frameRecorder) sink() FrameSink {
	return func(sessionID string, fr wire.ApprovalFrame) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.frames = append(f.frames, fr)
		f.sessions = append(f.sessions, sessionID)
	}
}

// sessionOf reports the conversation the nth pushed frame belonged to.
func (f *frameRecorder) sessionOf(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[i]
}

func (f *frameRecorder) all() []wire.ApprovalFrame {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wire.ApprovalFrame(nil), f.frames...)
}

// countOf reports how many frames of a kind were pushed. A pending request
// is one frame and a resolution is another, so a test that wants "exactly
// one of each" says so rather than counting a total.
func (f *frameRecorder) countOf(decision string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, fr := range f.frames {
		if fr.Decision == decision {
			n++
		}
	}
	return n
}

// countingIDs mints predictable, distinct request ids so a test can name
// one. Distinctness is the point: a double that handed out the same id
// twice would make the gate's collision handling look like the thing under
// test.
type countingIDs struct {
	mu sync.Mutex
	n  int
}

func (c *countingIDs) next() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return fmt.Sprintf("ar-%d", c.n)
}

// collidingIDs hands out the same id every time, so a test can prove the
// gate refuses to admit a call under a live request's handle.
type collidingIDs struct{}

func (collidingIDs) next() string { return "ar-fixed" }

// readCall is a convenient read-only call.
func readCall() Call {
	return Call{SessionID: "s-1", ToolName: "get_process_list", Class: domain.ClassRead, Actor: "op-1"}
}

// writeCall is a mutating call that must reach a human.
func writeCall() Call {
	return Call{
		SessionID: "s-1", ToolName: "restart_service", Class: domain.ClassDestructive,
		Actor: "op-1", Target: "orders-api-7d9", Summary: "restart orders-api",
		Arguments: json.RawMessage(`{"service":"orders-api"}`),
	}
}
