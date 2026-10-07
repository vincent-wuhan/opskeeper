package main

import (
	"context"
	"errors"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/domains/biz/nodeagent"
	"github.com/vincent-wuhan/opskeeper/core/domains/biz/nodefleet"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	frontierbound "github.com/vincent-wuhan/opskeeper/core/manager/service/frontierbound"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// This is the file decision 282 needed and could not write inside either
// domain. nodeagent drives nodefleet through nine methods; making that a port
// rather than a package import required two things neither domain may know
// about the other, and both of them are the same two things:
//
//   - Open's result. The fleet returns a *TunelledProcess handle for the node
//     it just opened a conversation with. nodeagent discards it — there is one
//     call site and it reads `if _, err := ...`. A handle the console never
//     sees, in a signature two packages had to agree on, is not worth a
//     dependency; so the port drops it and this adapter drops it on the way
//     through, while the concrete fleet keeps returning it for the callers
//     that do use it.
//
//   - Open's error. The fleet's cap error is ErrFleetFull, which carries a
//     LimitError saying whether the per-edge or the whole-fleet budget ran
//     out. That distinction belongs in the log and in the wrapped error, where
//     an operator debugging a fleet needs it. It does not belong in a
//     signature the console's HTTP layer would have to import to read, and it
//     certainly does not belong in nodeagent's own vocabulary. So the port
//     says ErrConversationLimit and this adapter says which one it was.
//
// Both translations live at the composition root, which is the only place that
// legitimately knows there are two domains here.

// nodeAgentFleetAdapter presents a *nodefleet.Fleet as a nodeagent.Fleet.
type nodeAgentFleetAdapter struct {
	fleet *nodefleet.Fleet
}

func (a nodeAgentFleetAdapter) Open(req domain.AgentPrompt, sink ports.EventSink) error {
	if _, err := a.fleet.Open(req, sink); err != nil {
		return translateFleetError(err)
	}
	return nil
}

// Prompt and the four methods below are pass-throughs, and they are written
// without conversions on purpose. The fleet's PromptRequest, Decision and
// SessionStats are now ALIASES of the domain types, not separate declarations,
// so there is nothing to convert — and if a future edit ever turns one back
// into a distinct struct, the compiler will say so here rather than quietly
// letting the port and the implementation drift apart.
func (a nodeAgentFleetAdapter) Prompt(ctx context.Context, req domain.AgentPrompt) error {
	return a.fleet.Prompt(ctx, req)
}

func (a nodeAgentFleetAdapter) Steer(ctx context.Context, edgeID uint64, sessionID, text string) error {
	return a.fleet.Steer(ctx, edgeID, sessionID, text)
}

func (a nodeAgentFleetAdapter) Abort(ctx context.Context, edgeID uint64, sessionID string) error {
	return a.fleet.Abort(ctx, edgeID, sessionID)
}

func (a nodeAgentFleetAdapter) Decide(ctx context.Context, edgeID uint64, sessionID string, d domain.AgentDecision) error {
	return a.fleet.Decide(ctx, edgeID, sessionID, d)
}

func (a nodeAgentFleetAdapter) State(ctx context.Context, edgeID uint64) (*ports.ProcessState, error) {
	ps, err := a.fleet.State(ctx, edgeID)
	return ps, translateBrokerError(err)
}

func (a nodeAgentFleetAdapter) Health(ctx context.Context, edgeID uint64) (*tunnel.AgentHealthResponse, error) {
	h, err := a.fleet.Health(ctx, edgeID)
	return h, translateBrokerError(err)
}

func (a nodeAgentFleetAdapter) Close(edgeID uint64, sessionID string) {
	a.fleet.Close(edgeID, sessionID)
}

func (a nodeAgentFleetAdapter) AllStats() []domain.AgentSessionStats {
	return a.fleet.AllStats()
}

// translateFleetError maps the fleet's vocabulary onto the console surface's.
// Everything that is not the cap travels through unchanged, because a node's
// own refusal and a transport failure are answers the console already renders
// correctly and re-wrapping them here would only cost a reader the type.
func translateFleetError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, nodefleet.ErrFleetFull) {
		// Join rather than Replace: the LimitError underneath says which
		// budget ran out, and an operator reading the log needs that. Replacing
		// it with a bare sentinel would have made the console's answer right
		// and the diagnosis impossible.
		return errors.Join(nodeagent.ErrConversationLimit, err)
	}
	return err
}

// translateBrokerError maps the frontier broker's "I am disabled" answer onto
// nodeagent's own vocabulary, so the console can tell a node that refused
// (a real answer) from a control plane that cannot reach any node at all.
// Without this the broker's sentinel reaches the HTTP layer unrecognised and
// renders as a 500 "internal" error — which reads as a server bug when it is
// a configuration state. Join rather than Replace so ErrDisabled stays in the
// chain for the log.
func translateBrokerError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, frontierbound.ErrDisabled) {
		return errors.Join(nodeagent.ErrBrokerUnavailable, err)
	}
	return err
}
