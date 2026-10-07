package main

import (
	"log/slog"
	"strings"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/edge/agentmodel"
	"github.com/vincent-wuhan/opskeeper/core/edge/pigsupervisor"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// modelAdopter is the node's half of the plan's 0.2 second sentence: the
// manager names the model endpoint, and the node acts on that answer when,
// and only when, its own environment was silent.
//
// It exists because the two sources have to be reconciled in exactly one
// place. The environment is read once at boot (agentmodel.ConfigFromEnv) and
// the manager's answer arrives later, on the first successful heartbeat. If
// the reconciliation lived at either end — the agent guessing, or the
// manager pushing — a node with a hand-pinned endpoint could be silently
// repointed by a cluster default. The precedence rule (env wins) is written
// down in agentmodel.Resolve; this type is only the mechanism that applies
// it and restarts the agent so the new scope is in the process's
// environment, where it has to be.
//
// Why a restart rather than a message: PiG reads its provider configuration
// from $PIG_CODING_AGENT_DIR, and that variable is fixed when the process is
// spawned. A running agent cannot be pointed at a scope it was not told
// about, and handing it one mid-turn would be a configuration change the
// conversation never agreed to. So an adopted endpoint takes effect as a
// supervised restart, which is the same mechanism a crash uses and the same
// one the node already trusts to bring the agent back.
//
// Idempotence is not a nicety here: the callback fires every 30 seconds. An
// adoption that rewrote the file and restarted the agent on every beat would
// turn a working node into one that spends its life mid-restart. The applied
// key is therefore compared before any work is done.
type modelAdopter struct {
	mu sync.Mutex

	// env is the agent's environment, overlaid onto whatever static values
	// the factory was built with. The adopter owns only the model keys;
	// the socket paths are written once before the supervisor starts and
	// never touched here.
	env map[string]string

	// credential is the node's own tunnel credential pair, "access:secret".
	// It is what the gateway authenticates, so it is the only secret on
	// this path — the manager's answer carries none, and there is no second
	// token to rotate. Empty disables adoption: a node with no credential
	// has nothing the gateway would accept.
	credential string

	// dir is the node-owned agent scope the answer is written into.
	dir string

	// sup is the supervisor to restart once an answer is applied. Set after
	// construction because the factory needs this adopter and the
	// supervisor needs the factory; the cycle is broken here rather than
	// by moving either.
	sup *pigsupervisor.Supervisor

	log *slog.Logger

	// applied is the last answer that took effect, as url+"\x00"+model. It
	// is the guard that keeps the 30-second heartbeat from becoming a
	// restart loop.
	applied string
}

// envOverlay returns the model keys to merge into the agent's environment.
// It is read by the spawn factory, so it takes the same lock adopt writes
// under — the heartbeat and the supervisor are two goroutines and the map is
// the one thing they share.
func (m *modelAdopter) envOverlay() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.env) == 0 {
		return nil
	}
	out := make(map[string]string, len(m.env))
	for k, v := range m.env {
		out[k] = v
	}
	return out
}

// adopt applies one heartbeat answer.
//
// Silent no-ops are deliberate and each has a reason: an empty URL means the
// manager has no public endpoint (do not point the agent at a relative
// path), a missing credential means the gateway would refuse the node (do
// not write a scope the agent cannot use), and an unchanged answer means the
// work is already done (do not restart the agent every beat).
func (m *modelAdopter) adopt(answer tunnel.HeartbeatResponse) {
	baseURL := strings.TrimSpace(answer.AgentBaseURL)
	if baseURL == "" || m.credential == "" {
		return
	}
	key := baseURL + "\x00" + strings.TrimSpace(answer.AgentModel)

	m.mu.Lock()
	if key == m.applied {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	cfg, ok := agentmodel.Resolve(
		agentmodel.Config{},
		false,
		agentmodel.Answer{BaseURL: baseURL, Model: answer.AgentModel},
		m.credential,
		m.dir,
	)
	if !ok {
		return
	}
	if _, err := agentmodel.Write(cfg); err != nil {
		m.log.Warn("adopting the manager's model endpoint failed; the node keeps its own configuration",
			slog.Any("err", err))
		return
	}

	m.mu.Lock()
	if m.env == nil {
		m.env = make(map[string]string, 2)
	}
	for k, v := range cfg.AgentEnvVars() {
		m.env[k] = v
	}
	m.applied = key
	sup := m.sup
	m.mu.Unlock()

	// The endpoint and the model are safe to log; the token is not, and it
	// is not on disk either — see agentmodel.
	m.log.Info("node agent model endpoint adopted from the manager",
		slog.String("base_url", cfg.BaseURL),
		slog.String("model", cfg.Model),
		slog.String("dir", cfg.Dir))

	// Restart so the new scope reaches the process's environment. A
	// failure here is logged, not fatal: the supervisor's own crash policy
	// will bring the agent back, and the node still collects telemetry.
	if sup != nil {
		if err := sup.Restart(); err != nil {
			m.log.Warn("restarting the agent onto the adopted model endpoint failed; "+
				"the supervisor will retry under its crash policy", slog.Any("err", err))
		}
	}
}
