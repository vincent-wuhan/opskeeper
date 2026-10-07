// Package pigrpc adapts PiG's RPC client to the AgentProcess port.
//
// It is the only place that knows a node agent is a `pig --mode rpc`
// subprocess speaking JSONL over stdio. Everything above it — the edge
// supervisor, the control plane's node fleet — depends on ports.AgentProcess
// and never sees a PiG type, so a change in PiG's RPC surface is absorbed
// here rather than in every node.
package pigrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/MichaelKinsy/PiG/coding/rpcclient"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Options configures the agent process.
type Options struct {
	// Binary is the agent executable. Empty selects "pig".
	Binary string
	// Cwd is the agent's working directory. It is also the package root:
	// the agent discovers its extensions, skills and MCP servers relative
	// to where it was launched, so this is how a node is pointed at the
	// plugin bundle it is allowed to load.
	Cwd string
	// Env is added to, and overrides, the edge's own environment.
	Env map[string]string
	// Provider and Model pin the agent's model. Empty leaves the agent's
	// own configuration in charge.
	Provider string
	Model    string
	// Args are appended after the mode, provider and model arguments.
	Args []string
	// Version is the agent build this process is known to be. It is
	// reported so the control plane can refuse a capability the node's
	// binary does not have, rather than discovering it as a protocol
	// error mid-incident.
	Version string
}

// Client drives one agent process.
//
// A Client is a single-use lifecycle holder: Start opens a process, Exited
// closes when it ends, and a later Start opens a new one. The control plane
// treats a restart as a new session — a process that died mid-turn cannot
// resume it — so the client deliberately keeps no transcript across starts.
type Client struct {
	opts Options

	mu      sync.Mutex
	client  *rpcclient.RpcClient
	running bool
	exited  chan struct{}
	lastErr error
	// listeners are the port-level subscribers, applied to every process
	// this client starts. A map keyed on an id rather than a single field:
	// the supervisor rebinds every one of its subscriptions on each
	// restart, and a single slot meant the second one bound would
	// silently starve the first — a bug that shows up as a console
	// quietly receiving nothing while the node looks healthy.
	listeners map[int]func(ports.ProcessEvent)
	nextSub   int
	// stopped distinguishes an operator-requested stop from a crash. A
	// supervisor must not restart a process it deliberately stopped.
	stopped bool
}

// Compile-time proof the client satisfies the port.
var _ ports.AgentProcess = (*Client)(nil)

// New returns a Client that has not started a process yet.
func New(opts Options) *Client {
	if opts.Binary == "" {
		opts.Binary = "pig"
	}
	return &Client{
		opts:      opts,
		exited:    closedChan(),
		listeners: make(map[int]func(ports.ProcessEvent)),
	}
}

// closedChan is the Exited channel of a client that has never run. It is
// already closed so a supervisor polling before the first Start sees a
// consistent "not running" rather than blocking on a channel with no
// process behind it.
func closedChan() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// Start spawns the agent and begins relaying its event stream.
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("pigrpc: process already running")
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return err
	}

	client := rpcclient.NewRpcClient(rpcclient.RpcClientOptions{
		CliPath:  c.opts.Binary,
		Cwd:      c.opts.Cwd,
		Env:      c.opts.Env,
		Provider: c.opts.Provider,
		Model:    c.opts.Model,
		Args:     c.opts.Args,
	})
	// The subscription is taken before Start so no event emitted during
	// the process's own startup can land in the gap between a successful
	// start and a subscription attached afterwards.
	unsubscribe := client.OnEvent(c.relay)

	exited := make(chan struct{})
	c.client = client
	c.exited = exited
	c.running = true
	c.stopped = false
	c.lastErr = nil
	c.mu.Unlock()

	if err := client.Start(); err != nil {
		// A process that failed to come up still has a client holding its
		// (empty) pipes. Subscribing first means the unsubscribe has to
		// happen here or the listener outlives the client.
		unsubscribe()
		c.mu.Lock()
		c.running = false
		c.lastErr = err
		c.client = nil
		c.mu.Unlock()
		close(exited)
		return fmt.Errorf("pigrpc: start %s: %w", c.opts.Binary, err)
	}

	go c.watch(client, unsubscribe, exited)
	return nil
}

// watch flips liveness off when the process ends, for any reason.
//
// Wait returns once the client's own readers and writers have drained, which
// happens when the process's pipes close — that is, when it exits. Polling
// Running instead would miss a process that died and was respawned between
// two samples, and would add a heartbeat to a loop that has an exact edge
// available.
func (c *Client) watch(client *rpcclient.RpcClient, unsubscribe func(), exited chan struct{}) {
	client.Wait()

	c.mu.Lock()
	// A later Start may already own the client. Only the start that
	// launched this process may retire its state.
	owns := c.client == client
	if owns {
		c.running = false
		if !c.stopped {
			c.lastErr = exitError(client)
		}
		unsubscribe()
	}
	c.mu.Unlock()
	// The channel is closed either way. When a later Start already replaced
	// this process, that start's own watch owns the liveness flag, but the
	// channel handed out by *this* start still describes *this* process and
	// must report its end.
	close(exited)
}

// exitError renders why a process ended, preferring the client's stderr
// because a PiG agent usually explains its own fatal error there before
// exiting, and "exit status 1" tells an operator nothing.
func exitError(client *rpcclient.RpcClient) error {
	stderr := client.GetStderr()
	if stderr != "" {
		return fmt.Errorf("agent process exited: %s", stderr)
	}
	return errors.New("agent process exited")
}

// Stop terminates the process.
func (c *Client) Stop() error {
	c.mu.Lock()
	client := c.client
	if !c.running || client == nil {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	c.mu.Unlock()

	client.Stop()
	return nil
}

// Running reports whether the process is up.
func (c *Client) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// Exited is closed once the process is gone.
func (c *Client) Exited() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exited
}

// LastError reports the most recent start failure or unclean exit.
func (c *Client) LastError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

// live returns the running client, or an error naming what to do instead.
// Every operation that talks to the process goes through here so a call
// against a dead agent fails with one clear message rather than a nil
// dereference or a hang on a closed pipe.
func (c *Client) live(op string) (*rpcclient.RpcClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running || c.client == nil {
		return nil, fmt.Errorf("pigrpc: cannot %s: agent process is not running", op)
	}
	return c.client, nil
}

// Prompt sends a user turn and returns once it is accepted.
//
// The text passes through guardPrompt first. The agent behind this client
// treats a leading "/" as a request to run a command rather than to open an
// investigation, and the control plane has no feature that means it, so the
// text is neutralised here — after the liveness check, because a notice
// about a turn that was never submitted is noise. See promptguard.go.
func (c *Client) Prompt(ctx context.Context, text string) error {
	client, err := c.live("prompt")
	if err != nil {
		return err
	}
	// v0.4.0 widened Prompt: it takes the images and an explicit streaming
	// behaviour, and it reports what the host did with the message
	// (handled / queued / started) instead of only whether the send worked.
	// Both new arguments are nil on purpose -- OpsKeeper submits text only,
	// and a nil streaming behaviour leaves the host's default. The
	// disposition is deliberately not propagated: this method's contract is
	// "the message was accepted", and every disposition above is an
	// acceptance. What it would buy -- telling a caller that a prompt
	// submitted during a running turn was queued rather than started -- is a
	// question about SSE delivery, and it belongs on the edge's frame
	// contract rather than hidden in a return value nobody reads.
	if _, err := client.Prompt(c.guardPrompt("prompt", text), nil, nil); err != nil {
		return fmt.Errorf("pigrpc: prompt: %w", err)
	}
	return nil
}

// Steer injects a message into the turn already running.
//
// Guarded for the same reason Prompt is, and it matters more here. A steer
// that names a command is not reinterpreted but refused outright — the
// agent answers "Extension command cannot be queued" (PiG
// cmd/pig/rpc_admission.go:141-142) — so an un-neutralised leading slash
// fails the operator's correction of a running investigation instead of
// delivering it.
func (c *Client) Steer(ctx context.Context, text string) error {
	client, err := c.live("steer")
	if err != nil {
		return err
	}
	// v0.4.0 also made Steer report a QueuedInputDisposition. Same reasoning
	// as Prompt above: `handled` and `queued` are both deliveries, and which
	// one happened is visible in the turn's event stream.
	if _, err := client.Steer(c.guardPrompt("steer", text), nil); err != nil {
		return fmt.Errorf("pigrpc: steer: %w", err)
	}
	return nil
}

// Abort stops the current turn.
func (c *Client) Abort(ctx context.Context) error {
	client, err := c.live("abort")
	if err != nil {
		return err
	}
	if err := client.Abort(); err != nil {
		return fmt.Errorf("pigrpc: abort: %w", err)
	}
	return nil
}

// SetModel pins the running process to a provider and model.
//
// The pin lives in the agent's own session state, so it survives the rest
// of this process's life and is lost on a restart — which is the intended
// behaviour. A supervisor that restored pins across respawns would have to
// persist them, and a persisted model choice is a cost decision somebody
// should make on purpose, not a detail of a crash.
func (c *Client) SetModel(ctx context.Context, provider, model string) error {
	client, err := c.live("set model")
	if err != nil {
		return err
	}
	if _, err := client.SetModel(provider, model); err != nil {
		return fmt.Errorf("pigrpc: set_model: %w", err)
	}
	return nil
}

// State reports what the process is doing right now.
func (c *Client) State(ctx context.Context) (*ports.ProcessState, error) {
	client, err := c.live("read state")
	if err != nil {
		return nil, err
	}
	state, err := client.GetState()
	if err != nil {
		return nil, fmt.Errorf("pigrpc: get_state: %w", err)
	}
	return projectState(state, c.opts.Version), nil
}

// projectState converts PiG's session state into the port's shape.
//
// The port carries only what a fleet view needs, so that a change in PiG's
// state object does not ripple into the control plane. Everything dropped
// here is available by asking the node directly.
func projectState(state rpcclient.RpcSessionState, version string) *ports.ProcessState {
	out := &ports.ProcessState{
		SessionID:        state.SessionID,
		Running:          state.IsStreaming,
		Version:          version,
		PendingToolCalls: state.PendingMessageCount,
	}
	if state.Model != nil {
		out.Model = state.Model.ID
		out.Provider = state.Model.Provider
	}
	return out
}

// OnEvent subscribes to the process's event stream.
//
// The subscription is remembered across restarts. A supervisor installs one
// listener for its whole life rather than one per process, so a crash does
// not silently disconnect the console from the node. There is exactly one
// subscription to the underlying client — the relay — and this is the
// fan-out point above it; subscribing per caller would deliver every frame
// once per caller.
func (c *Client) OnEvent(fn func(ports.ProcessEvent)) func() {
	if fn == nil {
		return func() {}
	}
	c.mu.Lock()
	id := c.nextSub
	c.nextSub++
	c.listeners[id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.listeners, id)
		c.mu.Unlock()
	}
}

// relay forwards one raw agent event to every subscriber.
//
// It reads the subscriber set at delivery time rather than capturing it, so
// a restart that rebinds mid-stream cannot leave the outgoing process
// writing into a connection that has moved on.
func (c *Client) relay(ev rpcclient.JsonAgentSessionEvent) {
	c.mu.Lock()
	subs := make([]func(ports.ProcessEvent), 0, len(c.listeners))
	for _, fn := range c.listeners {
		subs = append(subs, fn)
	}
	c.mu.Unlock()
	if len(subs) == 0 {
		return
	}
	projected := projectEvent(ev)
	for _, fn := range subs {
		fn(projected)
	}
}

// projectEvent converts one raw agent record into the port's frame.
//
// The payload is forwarded verbatim rather than reshaped. A control plane
// pinned to a decoded copy of the agent's schema would break the day the
// agent grows a field, and the console is the only component that needs to
// understand the payload anyway.
func projectEvent(ev rpcclient.JsonAgentSessionEvent) ports.ProcessEvent {
	out := ports.ProcessEvent{
		Type:     ev.Type,
		Payload:  append([]byte(nil), ev.Raw...),
		Terminal: terminalEvent(ev.Type),
	}
	// The envelope fields are best-effort. A record that does not carry
	// them is still relayed: dropping an event because its envelope did
	// not parse would lose the very output an operator is watching for.
	var envelope struct {
		SessionID string `json:"sessionId"`
		Iteration int    `json:"iteration"`
		Seq       int64  `json:"seq"`
	}
	if len(ev.Raw) > 0 && json.Unmarshal(ev.Raw, &envelope) == nil {
		out.SessionID = envelope.SessionID
		out.Iteration = envelope.Iteration
		out.Seq = envelope.Seq
	}
	return out
}

// terminalEvent reports whether an event ends a turn.
//
// The set is the agent's own terminal names. A relay needs to know when a
// turn is over to close its bookkeeping; it does not need to understand
// every other event, and an unrecognised name is treated as non-terminal so
// a new intermediate event cannot be mistaken for the end of a turn.
func terminalEvent(eventType string) bool {
	switch eventType {
	case "agent_end", "agent_settled", "turn_end":
		return true
	}
	return false
}
