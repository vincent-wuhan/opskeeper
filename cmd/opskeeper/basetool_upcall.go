package main

// This file is the BaseTool half of the channel a node's agent calls up on.
//
// The control plane carries two tool surfaces for the same business
// capability: the closure registry (registry.go — a name/description/schema
// quad plus an executor) and the BaseTool bag (registry_basetool.go — the
// same capabilities assembled as objects behind the decorator chain). The
// node's upcall channel reached only the first, so every capability that
// exists solely as a BaseTool was declared in a package manifest, visible in
// the node's schema, and unreachable at call time.
//
// The fix is not a third implementation. It is a bridge onto the one that
// already exists, which is why this file owns no tool logic at all: it owns
// the *exposure* rules.
//
// Three rules, each of which is a decision rather than a convenience:
//
//   - Read-only. This channel has no approval queue. A call that arrives
//     on it is dispatched immediately and attributed to a conversation, so a
//     tool that can change something must not be reachable here at all —
//     not "reachable but gated", because the gate would have to live
//     somewhere nobody is watching.
//   - Not the node's own surface. The host_* probes are node-local skills:
//     the node serves them itself, and routing them through the control
//     plane would be a round trip to answer a question the asker can
//     answer. The bridge therefore builds its own bag rather than reusing
//     the MCP one, which has the node-local tools appended to it.
//   - The node's schema is the contract. The node's PiG extension schema is
//     generated from the BaseTool metadata, so when a name exists in both
//     surfaces the BaseTool is the one the node's arguments were shaped
//     for.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	aiopstools "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools"
	aiopstoolsbase "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	aiopstoolschat2query "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/chat2query"
	aiopstoolsconfigchange "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/configchange"
	aiopstoolsdec "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/decorators"
	aiopstoolscore "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/toolcore"
)

// errBaseToolNotHere says the bridge does not carry that name.
//
// It is not a failure the caller reports. The bridge only knows about its
// own bag, so a miss means "not here", not "does not exist" — the dispatch
// chain continues to the other surfaces, and only when none of them has the
// tool is "no such tool" the honest answer.
var errBaseToolNotHere = errors.New("no such tool on this surface")

// baseToolUpcallReserved names the read-class tools this channel still
// will not serve, with the reason.
//
// A read class is a claim about a tool's *declared* effect, and it is not
// the same question as "safe to answer without a human watching". These four
// are read-class and still do not belong here, and the reason for each is
// recorded rather than left to be rediscovered: a name in this map with no
// reason is indistinguishable from a name somebody added to make a
// complaint go away.
var baseToolUpcallReserved = map[string]string{
	aiopstools.ToolNameBash: "the shell is reserved for the control plane's own agent and reaches the host " +
		"over the same tunnel this call arrived on, so a command here would run without a second pair of eyes",
	aiopstools.ToolSearchToolName: "schema discovery is the control plane's own model-loop tool; a node's tool " +
		"inventory comes from the manifest it installed, not from asking the control plane",
	aiopstoolschat2query.ToolNameChatToQuery: "the natural-language query translator belongs to a control-plane " +
		"conversation and is not a fleet question a node would ask upward",
	aiopstoolsconfigchange.ToolNameDraftConfigChange: "a configuration draft is bound to a control-plane " +
		"conversation and is the first half of a change that needs a human; serving the draft without the " +
		"apply would leave a half-change with no gate",
}

// baseToolUpcall serves the control plane's BaseTool implementations to a
// node whose agent asked for one over the upcall channel.
type baseToolUpcall struct {
	// serve holds the read-class tools this channel will run, keyed by
	// wire name. The values are decorated: timeout, rate limit and metric
	// come from the same chain every other BaseTool call site uses.
	serve map[string]aiopstoolsbase.BaseTool

	// withheld holds the tools this channel knows about and will not run,
	// with the reason it will not run them.
	//
	// The set exists so that a refused call says *why*. "This channel does
	// not carry writes" and "there is no tool by that name" are different
	// sentences to whoever reads the ledger afterwards: the first is a
	// policy, the second is a typo, and only the second is worth chasing.
	withheld map[string]string
}

// newBaseToolUpcall builds the bridge from a tool bag.
//
// The bag MUST be the registry's own bag for this deployment — the one
// `Registry.BuildBaseTools()` assembles — and MUST NOT be one that has had
// the node-local skill tools appended to it. The host_* probes are served by
// the node itself; a bridge that carried them would turn every local
// question into a tunnel round trip.
//
// The bag is also where the untrusted-output fencing already happened
// (`markUntrustedOutputs` runs inside BuildBaseTools). The bridge does not
// fence again: two fences over the same string mark it twice, and make the
// fencing look like it has two rules when it has one.
func newBaseToolUpcall(bag *aiopstoolscore.ToolBag, deps aiopstoolsdec.Deps, log *slog.Logger) *baseToolUpcall {
	b := &baseToolUpcall{
		serve:    map[string]aiopstoolsbase.BaseTool{},
		withheld: map[string]string{},
	}
	if bag == nil {
		return b
	}
	for _, tool := range bag.AllTools() {
		if tool == nil {
			continue
		}
		info, err := tool.Info(context.Background())
		if err != nil || info == nil || info.Name == "" {
			if log != nil {
				log.Warn("agent tool bridge: inspect BaseTool metadata",
					slog.Any("err", err))
			}
			continue
		}
		if reason, reserved := baseToolUpcallReserved[info.Name]; reserved {
			b.withheld[info.Name] = reason
			continue
		}
		// The class is re-derived from the executor rather than taken
		// from the manifest, for the reason the middleware surface
		// re-derives it: a manifest is a claim, and a claim that can
		// widen a channel is worth checking twice.
		if domain.ToolClass(info.Class) != domain.ClassRead {
			b.withheld[info.Name] = "a write-classed tool was offered on a read-only channel"
			continue
		}
		b.serve[info.Name] = aiopstoolsdec.Wrap(tool, deps)
	}
	if log != nil {
		// The exposed surface is logged at boot because it is the answer to
		// the question an operator asks when a node's tool call comes back
		// refused: what does this control plane's upcall channel carry,
		// and what does it hold back. Both lists come from the bridge
		// itself rather than from a description of it, so the log cannot
		// drift away from the exposure the channel actually has.
		served := b.serviceableNames()
		withheld := b.withheldNames()
		log.Info("node upcall channel: BaseTool surface assembled",
			slog.Int("served", len(served)),
			slog.Int("withheld", len(withheld)),
			slog.Any("withheld_tools", withheld))
	}
	return b
}

// invoke runs one tool the node's agent asked for.
//
// The three outcomes are distinct on purpose: served, known-and-refused,
// and not-here. The last one is not an answer — it is the signal for the
// dispatch chain to try the next surface.
func (b *baseToolUpcall) invoke(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, bool, string, error) {
	if b == nil {
		return nil, false, "", errBaseToolNotHere
	}
	if reason, withheld := b.withheld[tool]; withheld {
		return nil, true, reason, fmt.Errorf(
			"%s is not available on this channel: %s", tool, reason)
	}
	wrapped, ok := b.serve[tool]
	if !ok {
		return nil, false, "", fmt.Errorf("%w: %s", errBaseToolNotHere, tool)
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	out, err := wrapped.InvokableRun(ctx, string(args))
	if err != nil {
		// The error is the model's to read, so it is passed through
		// rather than flattened into a transport failure. "the alert
		// service is down" and "no such tool" are different sentences
		// and the model reacts to them differently.
		return nil, false, "", fmt.Errorf("%s: %w", tool, err)
	}
	return json.RawMessage(out), false, "", nil
}

// serviceableNames lists the tools this channel will run.
//
// It is the bridge's own answer to "is this tool reachable from a node",
// and it is derived from the construction rather than written down. A
// hand-written list would be the same class of mistake this file exists to
// fix: two places that each look right and disagree.
//
// The boot log and the reconciliation gate both read it, which is why it
// answers from the bridge rather than from a list beside it.
func (b *baseToolUpcall) serviceableNames() []string {
	if b == nil {
		return nil
	}
	out := make([]string, 0, len(b.serve))
	for name := range b.serve {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// withheldNames lists the tools this channel knows about and will not run.
//
// It is the other half of the boot log's question, and like
// serviceableNames it is derived from the construction — a name that was
// reserved, or a write-classed tool that was turned away, is in here
// because the bridge turned it away, not because somebody wrote it down.
func (b *baseToolUpcall) withheldNames() []string {
	if b == nil {
		return nil
	}
	out := make([]string, 0, len(b.withheld))
	for name := range b.withheld {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
