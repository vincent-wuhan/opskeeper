package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/edge/autonomy"
	"github.com/vincent-wuhan/opskeeper/core/edge/cmdpolicy"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill/builtin"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// The node's self-heal capability, assembled.
//
// It is built here rather than inside the autonomy package because this is
// the one file that knows all four of its inputs: which packages were
// admitted, what the heartbeat has to say about the control plane, what the
// collector last scraped, and which sandbox the node already runs its shell
// commands under. A component that knew all four would be the node plane in
// one package, which is the shape this composition root exists to avoid.
//
// Nothing here runs unless a package actually asked for autonomy. A node
// whose installed manifests have no autonomy block gets no registry, no
// spool, no pump and no runner, and the tool it exposes refuses. That is
// the state of every package that ships today, and it is the reason adding
// this file does not change what any existing node can do.

// autonomySpoolFile is where the node keeps the decisions it made on its
// own, under its own working directory rather than somewhere global: it
// belongs to this installation, and an installation that is deleted takes
// its evidence with it.
const autonomySpoolFile = "autonomy-audit.jsonl"

// autonomyObservations is what the stack needs from the node's agent: the
// control plane's reachability, and this host's last reading of a metric.
//
// It is an interface because the alternative is a test that has to stand up a
// tunnel and a clock to answer two questions. *edgebiz.Agent satisfies it,
// and the seam is the honest shape: these are observations, and an
// observation is something you can have recorded rather than dialled.
type autonomyObservations interface {
	// LinkReach reports whether the control plane is answering, and when it
	// stopped.
	LinkReach() (online bool, offlineSince time.Time)
	// MetricValue returns this host's most recent reading of a metric.
	MetricValue(name string) (value float64, ok bool)
	// EdgeID is this node's control-plane identity, as register_edge last
	// established it. It is read at send time rather than captured when the
	// stack is built, because the stack is built before the tunnel has
	// registered and the batch that matters is the one sent after it has.
	//
	// A zero means "not registered yet", which the center answers with
	// "took none of it" — the sentence the pump reads as "keep the rows
	// and try again", which is exactly right.
	EdgeID() uint64
}

// autonomyLink reads the control plane's reachability from the agent.
//
// The agent watches it through the heartbeat, which is the only witness that
// actually proves the manager is there: a socket that has not failed yet
// shows that the network stack accepted a write.
type autonomyLink struct{ agent autonomyObservations }

func (l autonomyLink) Reach() autonomy.Reach {
	online, since := l.agent.LinkReach()
	return autonomy.Reach{Online: online, OfflineSince: since}
}

// autonomyRunner runs a declared action through the node's own sandbox.
//
// The sandbox is the same one the bash tool uses, which is the point: an
// autonomy action is not a privileged path, it is an ordinary command that
// happens to have been signed in advance. If it needed its own executor
// with its own allow-list, the two lists would drift, and the day they did
// the signed argv would be running under a rule nobody reviewed.
type autonomyRunner struct{ sandbox *cmdpolicy.Sandbox }

func (r autonomyRunner) RunArgv(ctx context.Context, argv []string) (autonomy.Outcome, error) {
	res, err := r.sandbox.ExecArgv(ctx, argv)
	if err != nil {
		return autonomy.Outcome{ExitCode: -1}, err
	}
	if !res.Allowed {
		// The sandbox refused. That is a refusal of the *node's policy*,
		// not a failure of the action, and it must not look like a run:
		// the audit row has to say the command never started.
		return autonomy.Outcome{ExitCode: -1, Stderr: res.Reason}, nil
	}
	return autonomy.Outcome{
		ExitCode:  res.ExitCode,
		Stdout:    res.Stdout,
		Stderr:    res.Stderr,
		Truncated: res.Truncated,
	}, nil
}

// autonomyTool adapts the arbiter to the shape the host tool declares.
type autonomyTool struct {
	arbiter *autonomy.Arbiter
	runner  autonomy.Runner
}

func (t autonomyTool) PerformAutonomy(ctx context.Context, action, target, window string) (builtin.AutonomyOutcome, error) {
	claim := autonomy.Claim{
		Action: action,
		Target: target,
		Window: window,
		// The trigger is filled in from the declaration by the arbiter
		// itself when the caller names none, because the caller is not the
		// party that knows it: the manifest is, and a claim that supplied
		// its own would be checked against itself.
	}
	res, err := t.arbiter.Perform(ctx, claim, t.runner)
	if err != nil {
		return builtin.AutonomyOutcome{Verdict: autonomy.Refuse.String(), Reason: err.Error()}, nil
	}
	return builtin.AutonomyOutcome{
		Verdict:  res.Decision.Verdict.String(),
		Reason:   res.Decision.Reason,
		Ran:      res.Ran,
		ExitCode: res.Outcome.ExitCode,
		Stdout:   res.Outcome.Stdout,
		Stderr:   res.Outcome.Stderr,
	}, nil
}

// autonomyReplaySender hands one batch of the node's own decisions to the
// control plane, which appends them to the tamper-evident audit chain.
//
// It is the transport half of the plan's "隧道恢复后回传，补写中心审计链". The
// policy half — when to drain, how much per drain, and that a batch is all
// or nothing — already lives in autonomy.Pump and in core/edge/spool, and
// this type deliberately adds none of its own. A sender that reordered,
// batched differently, or decided which rows were worth sending would be a
// second policy about a chain whose whole value is that its order was not
// anybody's convenience.
//
// The refusals are the interesting half, because there are three answers
// the center can give and they mean three different things to the node:
//
//   - The call itself fails: the tunnel is down again. Error, keep the
//     batch, retry.
//   - The center took none of it and refused none of it: nobody there can
//     place the rows yet — the node has not registered, so the manager has
//     no identity to file them under. This is decision 100's sentence, and
//     reading it as a permanent refusal is how a backlog dies in the first
//     message after a reconnect. Error, keep the batch, retry.
//   - The center refused some rows for shape. Retrying is the node asking
//     the same question forever, so those rows are counted and passed over
//     — the same rule the telemetry path uses, and for the same reason: a
//     single permanently-refusable row must not wedge a queue that is
//     otherwise fine. It is loud, it is counted, and it is on the node's
//     health line, because the rows that go this way are rows that will
//     never be evidence.
type autonomyReplaySender struct {
	client tunnel.Client
	edgeID func() uint64
	log    *slog.Logger
	// refused counts rows the center will never take. It is a counter
	// rather than a log line because a node that has been replaying for an
	// hour should not have to be grepped to find out whether it has been
	// throwing rows away.
	refused *atomic.Uint64
}

// Send delivers rows in order, or reports that the batch has to come again.
func (s autonomyReplaySender) Send(ctx context.Context, rows []autonomy.Row) error {
	if len(rows) == 0 {
		return nil
	}
	req := tunnel.AutonomyAuditReplayRequest{
		EdgeID: s.edgeID(),
		Rows:   make([]tunnel.AutonomyAuditRow, 0, len(rows)),
	}
	for _, r := range rows {
		req.Rows = append(req.Rows, autonomyAuditRow(r))
	}
	var resp tunnel.AutonomyAuditReplayResponse
	if err := s.client.Call(ctx, tunnel.MethodAgentAuditReplay, req, &resp); err != nil {
		// A transport failure and a refused batch must not collapse into
		// one branch: one is retried, the other is not. The pump's
		// contract is that a returned error keeps the whole batch.
		return fmt.Errorf("autonomy replay: send %d rows: %w", len(rows), err)
	}
	switch {
	case resp.Accepted+resp.Rejected == len(rows):
		if resp.Rejected > 0 {
			s.refused.Add(uint64(resp.Rejected))
			s.log.Warn("the center refused autonomy rows for shape; they will not be retried",
				slog.Int("accepted", resp.Accepted),
				slog.Int("rejected", resp.Rejected),
				slog.String("reason", resp.Reason))
		}
		return nil
	case resp.Accepted == 0 && resp.Rejected == 0:
		// Decision 100: "took none of it" is not "refused all of it". The
		// center is not ready to place these rows — usually because the
		// node has not registered yet — and the honest instruction is to
		// keep them.
		return fmt.Errorf("autonomy replay: the center accepted none of %d rows; the batch stays on disk", len(rows))
	default:
		// A count that is neither "all" nor "none" is a center this build
		// does not understand. Guessing which half it took is how rows are
		// lost; keeping the whole batch costs one more round trip.
		return fmt.Errorf("autonomy replay: the center reported %d accepted and %d rejected of %d rows",
			resp.Accepted, resp.Rejected, len(rows))
	}
}

// autonomyAuditRow converts the arbiter's row into the wire shape.
//
// The trigger is flattened rather than nested, because the wire type is
// shared with the center and a nested struct there would be a second place
// for the trigger vocabulary to drift. Unknown-kind triggers do not occur —
// the Registry refuses a manifest whose kind nobody implements — so the
// kind is copied verbatim and the center records it as it was declared.
func autonomyAuditRow(r autonomy.Row) tunnel.AutonomyAuditRow {
	return tunnel.AutonomyAuditRow{
		At:        r.At,
		Action:    r.Action,
		Package:   r.Package,
		Tool:      r.Tool,
		Target:    r.Target,
		Argv:      r.Argv,
		Kind:      string(r.Trigger.Kind),
		Metric:    r.Trigger.Metric,
		Threshold: r.Trigger.Threshold,
		Key:       r.Key,
		Verdict:   r.Verdict,
		Reason:    r.Reason,
		Phase:     r.Phase,
		Result:    r.Result,
		ExitCode:  r.ExitCode,
	}
}

// autonomyStack is what a node with autonomy installed holds.
type autonomyStack struct {
	registry *autonomy.Registry
	arbiter  *autonomy.Arbiter
	spool    *autonomy.Spool
	// pump is the replay loop. It is built here and started by the caller,
	// because "the stack exists" and "the node is running" are two
	// different moments: buildAutonomy runs before the tunnel registers,
	// and a pump started here would race the registration it depends on.
	pump *autonomy.Pump
	// refused counts rows the center took one look at and would not keep.
	// It is read by Health so the number exists even when nobody is
	// reading logs.
	refused *atomic.Uint64
}

// buildAutonomy assembles the stack, or returns nil when no installed
// package asked for autonomy.
//
// A nil stack is not an error and not a warning. It is the shape of every
// fleet running today's packages, and treating it as a problem would put a
// line in every node's boot log about a capability nobody asked for.
func buildAutonomy(
	ctx context.Context,
	client tunnel.Client,
	admitted []pluginmanifest.Plugin,
	obs autonomyObservations,
	runner autonomy.Runner,
	cwd string,
	log *slog.Logger,
) (*autonomyStack, error) {
	registry, err := autonomy.NewRegistry(manifestsOf(admitted), time.Now())
	if err != nil {
		// Two packages declaring one action name is refused rather than
		// resolved, for the same reason the tool allow-list is: an action
		// name that resolves differently by install order is a name whose
		// argv nobody read.
		return nil, err
	}
	if registry.Empty() {
		return nil, nil
	}

	spool, err := autonomy.OpenSpool(filepath.Join(cwd, autonomySpoolFile), 0)
	if err != nil {
		// The stack is optional but not optional-once-declared. A package
		// that asked for autonomy on a node that cannot record what it
		// does is a node acting without a record, and the plan's phrase for
		// that is exactly what must not happen.
		return nil, fmt.Errorf("autonomy audit spool: %w", err)
	}

	arbiter, err := autonomy.New(autonomy.Options{
		Registry: registry,
		Link:     autonomyLink{agent: obs},
		Audit:    spool,
		Detector: autonomy.ValueDetector{Value: obs.MetricValue},
		Log:      log,
	})
	if err != nil {
		spool.Close()
		return nil, err
	}
	// The replay pump: the rows go to the center's audit chain once there
	// is a center to take them.
	//
	// It is built here and started by the caller, which is the same split
	// the telemetry WAL uses. The split is not cosmetic: this function
	// runs before the tunnel has registered, and a pump that started now
	// would immediately drain into a manager that cannot yet place the
	// rows. The sender handles that case correctly — it reads "took none"
	// as "keep them" — but a loop that begins with a guaranteed failure is
	// a loop whose boot log says something is wrong when nothing is.
	refused := &atomic.Uint64{}
	pump, err := autonomy.NewPump(autonomy.PumpOptions{
		Spool:  spool,
		Sender: autonomyReplaySender{client: client, edgeID: obs.EdgeID, log: log, refused: refused},
		Link:   autonomyLink{agent: obs},
		Log:    log,
	})
	if err != nil {
		spool.Close()
		return nil, fmt.Errorf("autonomy replay pump: %w", err)
	}

	builtin.SetAutonomyRunner(autonomyTool{arbiter: arbiter, runner: runner})
	log.Info("node autonomy is installed",
		slog.Int("actions", len(registry.Actions())),
		slog.Duration("offline_after", registry.OfflineAfter()),
		slog.String("spool", spool.Path()))
	return &autonomyStack{registry: registry, arbiter: arbiter, spool: spool, pump: pump, refused: refused}, nil
}

// autonomyHealth is the shape a node's health page renders, so the numbers
// exist even before anything reads them.
type autonomyHealth struct {
	Actions   []string `json:"actions"`
	Deferred  uint64   `json:"deferred"`
	Run       uint64   `json:"run"`
	Refused   uint64   `json:"refused"`
	Replays   uint64   `json:"replays"`
	SpoolPath string   `json:"spool_path,omitempty"`
	Spooled   int      `json:"spooled,omitempty"`
	// ReplayRefused is rows the center refused after the outage. It is
	// separate from Refused, which is the arbiter turning down an action:
	// one means "the node decided not to act", the other means "the node
	// acted and the record of it was not accepted", and an operator
	// reading only one of them would draw the wrong conclusion.
	ReplayRefused uint64 `json:"replay_refused,omitempty"`
}

func (s *autonomyStack) Health() autonomyHealth {
	if s == nil {
		return autonomyHealth{}
	}
	h := autonomyHealth{SpoolPath: s.spool.Path()}
	stats := s.arbiter.Snapshot()
	h.Deferred, h.Run, h.Refused, h.Replays = stats.Deferred, stats.Run, stats.Refused, stats.Replays
	if s.refused != nil {
		h.ReplayRefused = s.refused.Load()
	}
	for _, a := range s.registry.Actions() {
		h.Actions = append(h.Actions, a.Name)
	}
	if n, err := s.spool.Len(); err == nil {
		h.Spooled = n
	}
	return h
}
