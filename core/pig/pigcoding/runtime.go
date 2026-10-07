// Package pigcoding embeds PiG's coding SDK as OpsKeeper's agent runtime.
//
// # Why this package exists at all
//
// OpsKeeper spent its first year hand-rolling an agent: a message struct, a
// request struct, a ReAct loop, a tool adapter, a provider client, an event
// mapper, and a transcript writer. Every one of those exists in PiG, and
// every one of them was maintained twice — once here, once upstream — with
// the same bugs fixed on whichever side the author happened to be reading.
// The cost was not the lines. It was that a PiG release could not improve
// OpsKeeper's agent without a porting project, because OpsKeeper's agent
// was not PiG's agent.
//
// So the runtime is not wrapped, re-implemented or "adapted". It is
// constructed: coding.NewServices, coding.NewRuntime, rt.New. What a turn
// returns is what PiG's agent loop produced — []agent.AgentMessage, and the
// agent.AgentEvent stream — and OpsKeeper stores it, shows it, and audits
// it without re-deriving a parallel shape.
//
// # What this package is allowed to do
//
// Three things, and no more:
//
//  1. Decide where PiG keeps its state. A server must not write into an
//     operator's ~/.pig, and must not leave .pig/sessions behind in a
//     working directory it was handed. In-memory settings and an in-memory
//     session log make the whole runtime stateless on disk, which is the
//     only way OpsKeeper's own database stays the system of record.
//  2. Decide the lifecycle. One Services and one Runtime per process; one
//     Session per turn; a strict close order. Sessions own extension
//     subprocesses, so leaking one leaks a process tree.
//  3. Re-export the handful of PiG types callers must name, so a consumer
//     that needs coding.ScopedModel does not have to also know that
//     pigcoding exists.
//
// It does not translate. There is no OpsKeeper-shaped request or response
// type in this file, and adding one would undo the entire point.
package pigcoding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/MichaelKinsy/PiG/coding"
)

// ErrNoSuchModel is returned for a provider/model pair the deployment has
// not configured.
//
// It is separate from PiG's own resolution failure because the two mean
// different things to an operator. PiG is deliberately permissive: an
// unlisted slug is treated as "this provider probably serves it", which is
// the right default for a CLI whose model catalog is whatever the vendor
// published this morning. It is the wrong default for a platform where the
// model list is a cost-control and compliance decision an administrator made
// on purpose. OpsKeeper therefore keeps its own allowlist and refuses
// anything outside it.
var ErrNoSuchModel = errors.New("pigcoding: model is not configured for this deployment")

// ErrClosed is returned by a Runtime that has already been closed.
//
// It is a named error because the two failures a caller sees after shutdown
// — "the runtime is gone" and "the provider returned nothing" — demand
// opposite responses: the first is a programming error in the shutdown
// order, the second is a transient to retry.
var ErrClosed = errors.New("pigcoding: runtime is closed")

// RuntimeOptions configures NewRuntime.
//
// The zero value is not usable: an OpsKeeper deployment always has a state
// directory and a cancellation parent, and defaulting them would mean
// picking a path on an operator's behalf.
type RuntimeOptions struct {
	// AgentDir is the PiG configuration directory. OpsKeeper points it
	// inside its own state directory so a node's agent configuration is
	// data the deployment owns, not a developer's home directory. PiG
	// writes auth.json here on a login it is asked to perform; OpsKeeper
	// never asks, because it injects credentials instead.
	AgentDir string

	// CWD anchors session storage. With an in-memory session log it is
	// only a label, but it is also what PiG resolves project settings
	// against, so it must be stable for the process's lifetime.
	CWD string

	// AbortContext parents the runtime's own cancellation. Cancelling it
	// aborts every Session this Runtime created. Nil uses
	// context.Background(), which means only Runtime.Close stops the
	// agent.
	AbortContext context.Context

	// Extensions are pre-loaded into every Session this Runtime creates.
	// OpsKeeper passes its policy extensions here rather than per session,
	// because a tool_call gate that can be forgotten on one code path is
	// not a gate.
	Extensions []Extension

	// Settings replaces PiG's file-backed settings. OpsKeeper supplies an
	// in-memory manager built from its own settings table, so a model edit
	// in the admin UI lands without a restart and without a settings file
	// on disk.
	Settings *coding.SettingsManager

	// NewSessionLog, when set, is called for every Session this Runtime
	// opens, so the caller can hand that session its own transcript log.
	// A nil value means every session shares the runtime's in-memory log,
	// which is correct only when sessions never need to outlive a turn.
	//
	// This is the seam that keeps OpsKeeper's own session table the system
	// of record: the PiG log is a per-turn detail, not the archive.
	NewSessionLog func(cwd string) (*coding.SessionManager, error)
}

// Runtime owns the process-wide PiG services and session factory.
//
// One Runtime per process. It is safe for concurrent use; the Sessions it
// produces are not, exactly as PiG documents.
type Runtime struct {
	svcs *coding.Services
	rt   *coding.Runtime
	log  *coding.SessionManager

	newSessionLog func(string) (*coding.SessionManager, error)

	// mu guards catalogue and closed. BuildModel runs on request handlers
	// while a settings edit re-registers the catalogue, so the read side is
	// genuinely concurrent with the write side.
	mu sync.RWMutex
	// catalogue is the set of "provider/model" pairs the deployment has
	// configured. It is the allowlist ErrNoSuchModel is enforced against.
	catalogue map[string]struct{}

	closed bool
}

// NewRuntime builds the container.
//
// It fails rather than falling back to a default directory. A silent
// fallback here means an agent that works on a laptop and cannot find its
// own configuration on a server, which is the hardest class of bug to
// diagnose from a log.
func NewRuntime(opts RuntimeOptions) (*Runtime, error) {
	if opts.AgentDir == "" {
		return nil, errors.New("pigcoding: AgentDir is required; a server must not default to the operator's home directory")
	}
	if opts.CWD == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("pigcoding: resolve cwd: %w", err)
		}
		opts.CWD = cwd
	}
	// PiG creates the agent directory lazily but expects to be able to
	// write there. Making it here turns a permission problem into a boot
	// error rather than a first-turn error.
	if err := os.MkdirAll(opts.AgentDir, 0o750); err != nil {
		return nil, fmt.Errorf("pigcoding: create agent dir %s: %w", opts.AgentDir, err)
	}

	// In-memory by design. PiG's default session log is a JSONL file under
	// the working directory; OpsKeeper's transcript of record is its own
	// session table, and a second copy on the node's disk is a second
	// thing to secure, back up, and reconcile.
	log, err := coding.NewInMemorySessionManager(opts.CWD)
	if err != nil {
		return nil, fmt.Errorf("pigcoding: in-memory session log: %w", err)
	}

	svcs, err := coding.NewServices(coding.ServicesOptions{
		CWD:             opts.CWD,
		AgentDir:        opts.AgentDir,
		SessionManager:  log,
		SettingsManager: opts.Settings,
		// OpsKeeper is a server: a project directory is never trusted to
		// carry settings, because the project directory is wherever the
		// operator pointed a session, and a node's working directory is
		// frequently an operator-supplied string.
		ProjectTrusted: boolPtr(false),
	})
	if err != nil {
		return nil, fmt.Errorf("pigcoding: NewServices: %w", err)
	}

	rt, err := coding.NewRuntime(coding.RuntimeOptions{
		Services:     svcs,
		AbortContext: opts.AbortContext,
		// Extensions are registered on the Runtime, not per session,
		// because PiG gives every Session its own extension runner and a
		// gate that can be omitted on one construction path is not a gate.
		// The cost is that an extension here sees every session; OpsKeeper's
		// policy extensions are written to be session-agnostic for that
		// reason.
		NewExtensions: opts.Extensions,
	})
	if err != nil {
		// Services is caller-owned once constructed, and this is the last
		// moment there is a caller to close it.
		svcs.Close()
		return nil, fmt.Errorf("pigcoding: NewRuntime: %w", err)
	}

	return &Runtime{
		svcs:          svcs,
		rt:            rt,
		log:           log,
		newSessionLog: opts.NewSessionLog,
		catalogue:     make(map[string]struct{}),
	}, nil
}

// Services exposes the PiG dependency container for the model registry and
// auth storage. It is the seam pigmodel uses to resolve a selection into a
// live *ai.Model.
func (r *Runtime) Services() *coding.Services {
	if r == nil {
		return nil
	}
	return r.svcs
}

// SessionLog returns the runtime's default in-memory transcript log.
func (r *Runtime) SessionLog() *coding.SessionManager {
	if r == nil {
		return nil
	}
	return r.log
}

// BuildModel resolves "provider/model" through PiG's registry, which is how
// a provider's compatibility table, credential resolution and cost rates
// are applied. OpsKeeper does not construct providers itself.
//
// The pair is checked against the deployment's catalogue first. See
// ErrNoSuchModel for why the check is here rather than left to PiG.
func (r *Runtime) BuildModel(spec string) (*AIModel, error) {
	if r == nil || r.svcs == nil {
		return nil, ErrClosed
	}
	// A bare slug is OpenAI by PiG's own back-compat rule. Normalising it
	// here rather than at the call site keeps one spelling per model, so a
	// settings row, a console selection and a prompt override cannot drift
	// into three keys for the same model.
	spec = NormalizeModelSpec(spec)

	r.mu.RLock()
	closed := r.closed
	_, allowed := r.catalogue[spec]
	r.mu.RUnlock()
	if closed {
		return nil, ErrClosed
	}
	if !allowed {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchModel, spec)
	}
	return coding.BuildModel(spec, r.svcs)
}

// NormalizeModelSpec gives a model reference its canonical "provider/model"
// spelling.
func NormalizeModelSpec(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.Contains(spec, "/") {
		return spec
	}
	return "openai/" + spec
}

// Catalogue returns the configured "provider/model" keys, sorted.
//
// It is the model picker's data source, and it is derived from the same map
// BuildModel enforces against — so the list an operator sees and the list
// the server accepts cannot disagree, which is the failure mode that shows
// an operator a model and then rejects every request for it.
func (r *Runtime) Catalogue() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.catalogue))
	for key := range r.catalogue {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// Close releases the runtime and its services.
//
// The order matters and is PiG's: the runtime owns the current Session and
// any extension processes it started, and Services owns the auth storage
// and model registry underneath. Closing Services first would leave a
// running turn holding a closed credential store, and the failure surfaces
// as a provider 401 rather than as a shutdown bug.
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	if r.rt != nil {
		if err := r.rt.Close(); err != nil {
			return fmt.Errorf("pigcoding: close runtime: %w", err)
		}
	}
	if r.svcs != nil {
		r.svcs.Close()
	}
	return nil
}

func boolPtr(v bool) *bool { return &v }
