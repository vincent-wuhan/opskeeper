package main

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/vincent-wuhan/opskeeper/core/edge/auditlog"
	"github.com/vincent-wuhan/opskeeper/core/edge/auditwire"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// The node's own ledger, assembled (决策 126).
//
// This file exists because a node could already decide what mattered — the
// gate, the PiG run state and the plugin installer all write through
// core/ports.AuditSink, and the sink they were handed was nil — and could
// not put it anywhere. The chain lives in the manager's database on another
// machine, so "write a row" was only ever half of what those three call
// sites meant by it.
//
// The shape of the answer is autonomy's, down to the three refusals, and
// for the same reason: both are writes to one ordered append-only chain with
// no dedupe key, and the node is the only place that can tell the
// difference between "took none of it" and "refused all of it". The
// difference is the whole ballgame — the first means keep the rows, the
// second means stop counting them.
//
// What is NOT autonomy's is the optionality. buildAutonomy returns nil when
// no manifest asked for self-healing, and that is right for a capability
// nobody declared. The ledger has no such declaration: every gated tool
// call on this host belongs in it, so a node that cannot open one refuses
// to start rather than starting an agent on the host with nowhere to record
// what it did. That is a stronger stance than the rest of the node takes
// and it is deliberate — see the note on buildAuditLedger.

// auditLedgerFile is where the node keeps its own rows, under its own
// working directory for the same reason the autonomy spool is: the evidence
// belongs to this installation and leaves with it.
const auditLedgerFile = "audit-ledger.jsonl"

// auditStack is what a node holds for its own ledger.
type auditStack struct {
	sink *auditlog.Sink
	pump *auditlog.Pump
	// sender is kept because it owns the refusal counter: the number is
	// the sender's state, not the stack's, and holding the counter
	// separately was the shape that let the two drift apart.
	sender *auditwire.Sender
}

// buildAuditLedger opens the node's ledger and builds its drain.
//
// Unlike buildAutonomy this never returns a nil stack and never treats the
// ledger as optional. The reasoning is the reverse of autonomy's, and it
// turns on what is missing when the file will not open: a node with no
// ledger is a node whose policy gate records nothing, so the tool it blocks
// and the tool it allows leave the same trace. Every other safety property
// on this node — the role ceiling, the allow-list, the socket — still holds,
// which is exactly what makes the missing record dangerous: the node looks
// like it is guarding the host while the evidence of what it did is on a
// disk that does not exist. Failing the boot says "this node cannot be
// trusted to record", which is a sentence an operator can act on; running
// anyway says nothing at all.
//
// The pump is built here and started by the caller, which is autonomy's
// split for autonomy's reason: this runs before the tunnel has registered,
// and a loop that begins by draining into a manager that cannot yet place
// the rows has a boot log that claims something is wrong when nothing is.
func buildAuditLedger(
	client tunnel.Client,
	obs autonomyObservations,
	cwd string,
	log *slog.Logger,
) (*auditStack, error) {
	sink, err := auditlog.Open(filepath.Join(cwd, auditLedgerFile), 0)
	if err != nil {
		return nil, fmt.Errorf("node audit ledger: %w", err)
	}
	sender := auditwire.NewSender(client, obs.EdgeID, log, nil)
	pump, err := auditlog.NewPump(auditlog.PumpOptions{
		Sink:   sink,
		Sender: sender,
		// A function rather than a Link: the only question this pump asks
		// is whether the tunnel is answering. The heartbeat is the witness
		// that actually proves the manager is there — a socket that has
		// not failed yet only shows that the network stack accepted a
		// write.
		Reachable: func() bool {
			online, _ := obs.LinkReach()
			return online
		},
		Log: log,
	})
	if err != nil {
		sink.Close()
		return nil, fmt.Errorf("node audit replay pump: %w", err)
	}
	log.Info("node audit ledger open", slog.String("path", sink.Path()))
	return &auditStack{sink: sink, pump: pump, sender: sender}, nil
}

// Health is the shape a node's health page renders, so the numbers exist
// even before anything reads them.
func (a *auditStack) Health() map[string]any {
	pending, err := a.pump.Pending()
	out := map[string]any{
		"pending": pending,
		"refused": a.sender.Refused(),
		"path":    a.sink.Path(),
	}
	if err != nil {
		out["error"] = err.Error()
	}
	return out
}
