// Package crystallizehook adapts the closed loop's recovery evidence to the
// crystalliser's ledger.
//
// It is the production wiring the plan's item 7 was missing. The crystallise
// package already knows how to decide "this pattern has earned a runbook",
// and the loop package already knows how to say "this run verified cleanly
// with this argv". What no build had was the object that holds one and is
// told by the other across runs, which is why the ledger existed with a
// Record method and no caller.
//
// The adapter is deliberately the only place that assembles a Trial from a
// RecoveryEvidence, so the questions that need judgement live in one file:
//
//   - Which runs are evidence? A first-try verification is; a pass that
//     needed a rollback is not (it contradicts the fix's own claim), and a
//     failed verification is negative evidence. That mapping is decided by
//     crystallize.OutcomeOf, not re-derived here.
//   - Which tool class? It comes from the tool registry the fix dispatched
//     through, never from the action's name: an L2 soft write and an L3 hard
//     write can share a verb in different adapters, and a runbook's safety
//     level is set from the class.
//   - Which reach and window? They are a policy input. The ledger only saw a
//     human approve an action with no explicit radius, so the adapter stamps
//     the operator's configured default and says so.
package crystallizehook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/crystallize"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// ToolSpecSource answers a tool's declared risk and class.
//
// It is ports.ToolCaller's LookupTool half, taken as its own interface
// because the hook never runs a tool — it only reads what the tool says
// about itself. A hook that could dispatch would be a second path to
// execution independent of the approval gate, which is the one thing the
// platform does not have twice.
type ToolSpecSource interface {
	LookupTool(name string) (ports.ToolSpec, bool)
}

// Config is the operator's policy for what a crystallised action may carry.
//
// The zero value is not usable: without a radius the emitted declaration
// would be refused by the ledger's own validation, so Default's values are
// required rather than optional.
type Config struct {
	// ToolClass maps a tool's declared risk level to the class stamped on
	// the trial. A tool the registry does not know is refused, not guessed.
	ToolClass func(riskLevel string) (domain.ToolClass, bool)

	// BlastRadius is the reach an operator is willing to grant a pattern
	// proven on this deployment. It is policy rather than evidence: the
	// loop's approval carries a target, not a radius.
	BlastRadius domain.BlastRadius

	// TTL is how long a crystallised action may stay live. The ledger
	// caps it further; this is the operator's own ceiling.
	TTL time.Duration

	// Policy is the ledger's promotion rule. Zero values take the
	// crystallise package's defaults (three clean runs).
	Policy crystallize.Policy

	// StatePath is where the ledger's runs are kept across a restart. The
	// empty string means they are not, and that is a working
	// configuration rather than a broken one: the drafts this feature
	// produces are files on disk either way, and only the promotion
	// progress behind them is lost. Set it when the control plane's
	// restart rate is low enough that a three-run streak is reachable.
	StatePath string
}

// runStore is what the learner needs from a place to keep the ledger.
//
// It is an interface rather than a concrete *FileStore for one reason: the
// write failing is a branch the loop must be right about, and there is no
// portable way to arrange a genuinely unwritable directory in a test —
// chmod is advisory to a privileged process, and removing the directory just
// makes Save rebuild it, which is the correct behaviour and the wrong test.
// The failure being interesting is the argument for the seam.
type runStore interface {
	Path() string
	Load() ([]crystallize.Run, error)
	Save([]crystallize.Run) error
}

// Learner is the loop.RecoveryCrystallizer that owns the ledger.
//
// It is safe for concurrent use: the ledger counts streaks across
// incidents, and two recovered phases can finish at the same time.
type Learner struct {
	tools  ToolSpecSource
	ledger *crystallize.Ledger
	cfg    Config
	log    *slog.Logger

	// store persists the ledger across a restart. It is nil when
	// Config.StatePath is empty, which is why every use is guarded rather
	// than behind an interface value that would have to be checked anyway.
	store runStore

	mu       sync.Mutex
	lastErr  error
	recorded int
}

// New constructs the learner. tools is required — without a tool registry a
// trial cannot carry the class the emitted declaration is graded on, and a
// guessed class is how a runbook is written at the wrong safety level.
func New(tools ToolSpecSource, cfg Config, log *slog.Logger) (*Learner, error) {
	if tools == nil {
		return nil, errors.New("crystallizehook: a tool registry is required: a trial's class comes from the tool, not from its name")
	}
	if cfg.ToolClass == nil {
		return nil, errors.New("crystallizehook: Config.ToolClass is required: the host must state how a risk level becomes a class")
	}
	if !cfg.BlastRadius.Valid() || cfg.BlastRadius == domain.RadiusNone {
		return nil, fmt.Errorf("crystallizehook: Config.BlastRadius %q is not a reach a human can grant", cfg.BlastRadius)
	}
	if cfg.TTL <= 0 {
		return nil, errors.New("crystallizehook: Config.TTL must be positive")
	}
	if log == nil {
		log = slog.Default()
	}
	learner := &Learner{
		tools:  tools,
		ledger: crystallize.NewLedger(cfg.Policy),
		cfg:    cfg,
		log:    log,
	}
	learner.attachStore(cfg.StatePath, log)
	return learner, nil
}

// attachStore gives the learner a durable ledger, degrading to an in-memory
// one rather than refusing to construct.
//
// The degradation is the same choice federation makes about its ledger, and
// for the same reason: a learner that refuses to build because it cannot
// write a file has switched off a cost saving over a durability detail, and
// will do so on every read-only image and every deployment that forgot to
// mount a volume. A learner that counts in memory is what this package did
// before persistence existed — it works, and a restart forgets. One line at
// boot is strictly better than a feature that is mysteriously absent.
//
// A file that exists and cannot be read is a different case from a path that
// cannot be written, and it is NOT swallowed. "The streaks reset" and "the
// path is unwritable" need different answers, and an operator who is told
// the second while it is the first will go looking for a permission problem
// that does not exist.
func (l *Learner) attachStore(path string, log *slog.Logger) {
	if path == "" {
		return
	}
	store := NewFileStore(path)
	if err := store.Probe(); err != nil {
		log.Warn("crystallize: the state path is not usable, so promotion progress will not survive a restart",
			slog.String("path", store.Path()),
			slog.String("remedy", "make the path writable, or point OPSKEEPER_CRYSTALLIZE_STATE at one that is"),
			slog.Any("err", err),
		)
		return
	}
	runs, err := store.Load()
	if err != nil {
		// Refusing to start on an unreadable state file would be a
		// stronger guarantee than any other here — it is the one failure
		// that can lose a decision — and it is still the wrong trade for
		// the same reason the unwritable path is. The operator is told,
		// loudly, and the alternative is silently starting a counter at
		// zero on a ledger that has already promoted things.
		log.Error("crystallize: the state file could not be read, so this process is counting from zero while an earlier one had not",
			slog.String("path", store.Path()),
			slog.String("remedy", "the file is left in place on purpose; move it aside to start over, or restore a readable one"),
			slog.Any("err", err),
		)
		return
	}
	if err := l.ledger.Restore(runs); err != nil {
		log.Error("crystallize: the persisted runs were refused, so this process is counting from zero",
			slog.String("path", store.Path()),
			slog.String("remedy", "the file is left in place on purpose; move it aside to start over"),
			slog.Any("err", err),
		)
		return
	}
	// The store is attached even when there was nothing to restore, and that
	// ordering is load-bearing. A missing file is what a deployment that has
	// never recorded a trial looks like, which is also the state it is in
	// when it should start writing one — so attaching only on a non-empty
	// load is a loop that never begins: the first boot finds nothing, skips
	// the store, records into memory, and the second boot finds nothing
	// again. The persistence would have looked correct in every test that
	// exercised FileStore on its own.
	l.store = store
	if len(runs) == 0 {
		return
	}
	log.Info("crystallize: promotion progress restored",
		slog.String("path", store.Path()),
		slog.Int("runs", len(runs)),
		slog.Int("promoted", len(l.ledger.Promoted())))
}

// Ledger exposes the underlying ledger for a console that renders promoted
// patterns and their drafts. Nil until New has run.
func (l *Learner) Ledger() *crystallize.Ledger { return l.ledger }

// LastError returns the most recent refusal, for a health surface.
func (l *Learner) LastError() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastErr
}

// Recorded reports how many trials this learner has accepted. It exists so a
// test can assert the ledger was reached without reaching into it.
func (l *Learner) Recorded() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recorded
}

// Learn implements loop.RecoveryCrystallizer.
//
// It returns an error for a run it declined to record, and the caller is
// expected to treat that as advisory — the recovery already verified, and a
// learning failure must not un-verify it. The reason is stored for the
// health surface either way.
func (l *Learner) Learn(_ context.Context, ev loop.RecoveryEvidence) error {
	trial, ok, why := l.trialOf(ev)
	if !ok {
		return l.note(why)
	}
	rep, err := l.ledger.Record(trial)
	if err != nil {
		return l.note(fmt.Errorf("crystallize: record trial for %s: %w", ev.Target, err))
	}
	l.mu.Lock()
	l.recorded++
	l.lastErr = nil
	l.mu.Unlock()
	l.persist()
	l.log.Info("crystallize: recovery recorded",
		slog.String("incident", ev.IncidentID),
		slog.String("target", ev.Target),
		slog.String("tool", ev.Tool),
		slog.String("verdict", string(rep.Verdict)),
		slog.String("reason", rep.Reason))
	return nil
}

// persist writes the ledger out after a trial changed it.
//
// A failure here is reported and not returned, and the asymmetry is the
// point. The trial IS recorded — the counters moved, and a draft may have
// been earned — so returning an error would tell the orchestrator that the
// learning failed when what actually happened is that the copy failed. The
// loop treats Learn's error as advisory and would carry on either way, but
// the health surface reads it, and a health surface that reports "not
// recorded" for a run that was recorded is worse than one that reports the
// narrower truth: the run is in memory, and this restart will lose it.
func (l *Learner) persist() {
	if l.store == nil {
		return
	}
	if err := l.store.Save(l.ledger.Runs()); err != nil {
		l.mu.Lock()
		l.lastErr = err
		l.mu.Unlock()
		l.log.Error("crystallize: the trial was recorded but not written out, so a restart will forget it",
			slog.String("path", l.store.Path()),
			slog.Any("err", err),
		)
	}
}

func (l *Learner) note(err error) error {
	if err == nil {
		return nil
	}
	l.mu.Lock()
	l.lastErr = err
	l.mu.Unlock()
	l.log.Warn("crystallize: recovery not recorded", slog.Any("err", err))
	return err
}

// trialOf assembles the ledger's input, or explains why this run is not one.
//
// The refusal messages are the point of the function: a run the platform
// cannot learn from is a finding about the wiring, and stating which field
// was missing is what makes it fixable rather than mysterious.
func (l *Learner) trialOf(ev loop.RecoveryEvidence) (crystallize.Trial, bool, error) {
	if _, ok := crystallize.OutcomeOf(ev.Verified); !ok {
		return crystallize.Trial{}, false, fmt.Errorf("crystallize: %s verified nothing this run can be read from", ev.IncidentID)
	}
	if len(ev.Argv) == 0 {
		// The action reached its change without an exec (a SQL statement, an
		// API call), or the adapter did not report the vector. Either way
		// there is no program a node could re-run, and inventing one would
		// promote a different action than the one that worked.
		return crystallize.Trial{}, false, fmt.Errorf("crystallize: %s ran %s with no literal argv; an action with nothing to re-run is not a runbook", ev.IncidentID, ev.Tool)
	}
	spec, ok := l.tools.LookupTool(ev.Tool)
	if !ok {
		return crystallize.Trial{}, false, fmt.Errorf("crystallize: %s names tool %q, which is not registered, so its class cannot be read", ev.IncidentID, ev.Tool)
	}
	class, ok := l.cfg.ToolClass(spec.RiskLevel)
	if !ok {
		return crystallize.Trial{}, false, fmt.Errorf("crystallize: tool %q declares risk %q, which this host cannot map to a class", ev.Tool, spec.RiskLevel)
	}
	rem := loop.RemediationOption{Action: ev.Tool, Target: ev.Target}
	trial, ok := crystallize.TrialOf(ev.At, ev.IncidentID, rootCauseFor(ev), ev.Verified, rem, crystallize.Execution{
		Tool:        ev.Tool,
		Class:       class,
		Argv:        ev.Argv,
		Trigger:     ev.Trigger,
		BlastRadius: l.cfg.BlastRadius,
		TTL:         l.cfg.TTL,
	})
	if !ok {
		return crystallize.Trial{}, false, fmt.Errorf("crystallize: %s recovery is not usable evidence (target=%q tool=%q argv=%d)", ev.IncidentID, ev.Target, ev.Tool, len(ev.Argv))
	}
	return trial, true, nil
}

// rootCauseFor rebuilds the minimal contract TrialOf reads a fault kind from.
func rootCauseFor(ev loop.RecoveryEvidence) *loop.RootCauseJSON {
	if ev.FaultKind == "" {
		return nil
	}
	return &loop.RootCauseJSON{RootCauseObject: &loop.RootCauseObject{Kind: ev.FaultKind}}
}
