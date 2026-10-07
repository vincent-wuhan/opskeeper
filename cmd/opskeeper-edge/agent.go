package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/edge/agentmodel"
	"github.com/vincent-wuhan/opskeeper/core/edge/agentprofile"
	"github.com/vincent-wuhan/opskeeper/core/edge/gatesocket"
	"github.com/vincent-wuhan/opskeeper/core/edge/pigsupervisor"
	"github.com/vincent-wuhan/opskeeper/core/edge/policygate"
	"github.com/vincent-wuhan/opskeeper/core/edge/toolbroker"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigrpc"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigwire"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"

	edgebiz "github.com/vincent-wuhan/opskeeper/core/edge/biz"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// nodeAgentConfig is how a node is told which agent to run and what it may
// load.
//
// Everything is env-driven because a node is provisioned by install.sh on
// hosts OpsKeeper has never seen, and the manager reaches them only through
// the tunnel. The defaults point at a read-only profile in a plugin bundle
// the operator has to place deliberately: an agent that finds no bundle
// starts with no tools at all rather than with whatever happens to be in the
// working directory.
type nodeAgentConfig struct {
	// Binary is the agent executable. Empty uses "pig" off PATH.
	Binary string
	// Cwd is the agent's working directory and, because the agent
	// discovers its extensions and skills relative to where it was
	// launched, the package root of the plugin bundle it is allowed to
	// load. This is the single most security-relevant setting on the
	// node: it decides which code runs with the node's privileges.
	Cwd string
	// Packages are the plugin bundles the agent may load, as absolute
	// directories. Each is admitted against its governance manifest before
	// any of them is written into the agent's settings; one that fails is
	// a boot error, not a warning.
	Packages []string
	// Provider and Model pin the agent's model. Empty lets the manager
	// pin per conversation.
	Provider string
	Model    string
	// MaxCrashAttempts bounds crash-looping. Zero restarts forever, which
	// is right for a node whose agent is known-good and wrong for one
	// that is simply broken.
	MaxCrashAttempts int
	// RestartBackoff is the first restart delay.
	RestartBackoff time.Duration
	// AccessKey and SecretKey are the node's tunnel credential pair, which
	// is also the gateway's credential — one secret, one thing to rotate.
	// They are set from the node's own configuration rather than read again
	// here, so "which credential does this node present" has one source.
	//
	// They are used only to build the model endpoint an adopted answer
	// resolves to; the credential itself is still never written to disk
	// (see agentmodel). Empty disables adopting the manager's answer.
	AccessKey string
	SecretKey string
}

// credential renders the pair as the gateway's Authorization value wants it.
// It is not logged anywhere and never reaches a file.
func (c nodeAgentConfig) credential() string {
	if c.AccessKey == "" || c.SecretKey == "" {
		return ""
	}
	return c.AccessKey + ":" + c.SecretKey
}

// defaultAgentPackageDir is the read-only profile every node starts with.
//
// It is a default, not a constant that always applies: an operator who
// points OPSKEEPER_EDGE_AGENT_PACKAGES somewhere else gets exactly what
// they asked for. What is not negotiable is that nothing outside the
// configured list is ever loaded.
const defaultAgentPackageDir = "/var/lib/opskeeper-edge/agent/packages"

// loadNodeAgentConfig reads the node agent's settings from the environment.
func loadNodeAgentConfig() nodeAgentConfig {
	packages := splitList(os.Getenv("OPSKEEPER_EDGE_AGENT_PACKAGES"))
	if len(packages) == 0 {
		packages = []string{defaultAgentPackageDir}
	}
	return nodeAgentConfig{
		Binary:           envOr("OPSKEEPER_EDGE_AGENT_BIN", "pig"),
		Cwd:              envOr(agentmodel.WorkingDirEnv, agentmodel.DefaultWorkingDir),
		Packages:         packages,
		Provider:         os.Getenv("OPSKEEPER_EDGE_AGENT_PROVIDER"),
		Model:            os.Getenv("OPSKEEPER_EDGE_AGENT_MODEL"),
		MaxCrashAttempts: 5,
		RestartBackoff:   pigsupervisor.DefaultRestartBackoff,
		// The tunnel pair, which the gateway also authenticates. Read here
		// rather than threaded in as a parameter because loadNodeAgentConfig
		// is already the single reader of this node's agent environment,
		// and a second reader would be the place the two drift apart.
		AccessKey: os.Getenv("OPSKEEPER_EDGE_ACCESS_KEY"),
		SecretKey: os.Getenv("OPSKEEPER_EDGE_SECRET_KEY"),
	}
}

// startNodeAgent brings up this node's PiG agent and returns the bridge that
// serves the manager's agent.* commands against it.
//
// It is a composition point, not a layer: this file is the one place that
// knows both the PiG adapter and the node supervisor exist. The supervisor
// is handed a client, not a PiG type, so the node module never names PiG
// and a PiG upgrade changes this file and core/pig and nothing else on the
// node plane.
//
// A node agent that fails to start is not fatal. The edge still collects
// metrics, still serves its own skill RPCs, and still answers agent.state
// and agent.health with an explanation. An edge that refuses to boot
// because the AI is down would take the telemetry with it.
func startNodeAgent(
	ctx context.Context,
	client tunnel.Client,
	cfg nodeAgentConfig,
	version string,
	// agent is the tunnel-side agent. It is passed in rather than built
	// here because the heartbeat and the collector that live on it are two
	// of the three inputs the autonomy arbiter needs, and a second agent
	// built in this function would be a second heartbeat and a second
	// opinion about whether the control plane is there.
	agent *edgebiz.Agent,
	// runner executes a declared action when the arbiter allows it. It is
	// built by main so the autonomy path runs through the same sandbox the
	// bash tool does.
	runner autonomyRunner,
	log *slog.Logger,
) (bridge *edgebiz.AgentBridge, stop func(), err error) {
	// Admit the packages before anything starts. A package that fails
	// validation is a boot error the operator has to fix, not a warning
	// that scrolls past: starting the agent without it would produce a
	// node that answers confidently and cannot see half the host.
	trust := loadTrustStore()
	// The boot bundle is reviewed against the same rule the runtime path
	// uses, including the version check: a node whose own bundle declares
	// a min_edge_version it does not meet should say so at boot — loudly,
	// once — rather than confidently serving a package it cannot host.
	policy, err := nodeBootPolicy(version)
	if err != nil {
		return nil, nil, fmt.Errorf("edge agent policy: %w", err)
	}
	admitted, err := admitPackages(cfg.Packages, trust, policy)
	if err != nil {
		return nil, nil, fmt.Errorf("edge agent packages: %w", err)
	}
	if trust.LoadError() != nil {
		// A trust store that was configured and could not be read is
		// already a hard error from admitPackages. Reaching here means
		// there is no configured store, which is worth saying once per
		// boot: this node is running packages nobody signed.
		log.Warn("node agent trust store is not configured; packages are not being signature-checked",
			slog.String("set", "OPSKEEPER_EDGE_TRUST_STORE"))
	}
	settingsPath, err := writeAgentSettings(cfg.Cwd, packageRoots(admitted))
	if err != nil {
		return nil, nil, err
	}

	// The profile, written next to the package list and for the same
	// reason: it is the other half of what this agent may do, and it is
	// written before the process starts so the two can never describe
	// different nodes. A failure here stops the node. The profile is not
	// a convenience - a node that booted without it would hand the model
	// a shell on a production host, and the gate would then refuse every
	// call to it, which is a safe but unreadable place to be during an
	// incident.
	extensions, err := agentExtensions(admitted)
	if err != nil {
		return nil, nil, fmt.Errorf("edge agent profile: %w", err)
	}
	profilePath, err := agentprofile.Write(filepath.Join(cfg.Cwd, agentConfigDirName()), extensions)
	if err != nil {
		return nil, nil, fmt.Errorf("edge agent profile: %w", err)
	}
	log.Info("node agent package set installed",
		slog.Int("packages", len(admitted)),
		slog.String("settings", settingsPath),
		slog.String("profile", profilePath))

	// The model endpoint, resolved before anything is started.
	//
	// This is a separate step from the package set above and it fails
	// differently on purpose. A package that does not review is a boot
	// error. A model endpoint that is half-configured is also a boot error,
	// for a reason that has nothing to do with review: without it the node
	// starts, loads its plugins, authenticates to the tunnel and then
	// answers every question with no model behind it. The symptom is a node
	// that reports healthy, and the cause is visible in exactly one place —
	// a boot log nobody reads after the rollout is finished.
	//
	// A node with no endpoint at all is not an error. That is a deployment
	// that has not been given a model yet, and the agent's own configuration
	// scope is left entirely alone so an operator who provisioned one by
	// hand keeps it. See agentmodel.go for why the default scope is not
	// something to rely on.
	modelCfg, modelConfigured, err := agentmodel.ConfigFromEnv()
	if err != nil {
		return nil, nil, fmt.Errorf("edge agent model configuration: %w", err)
	}
	// socketPath is read by the factory when it spawns the agent, which is
	// after the socket exists. A closure over it rather than a value,
	// because the socket cannot be created until the gate is.
	var socketPath string

	// toolSocketPath is read the same late way: the broker cannot exist
	// until the invoker and the authoriser do, and those cannot be built
	// until the registry is.
	var toolSocketPath string

	// The agent's whole environment, assembled here and completed below,
	// then read by the factory when it spawns.
	//
	// A Go map holds copies, not references, so the two socket entries are
	// written again where the paths are actually assigned rather than being
	// captured now with their empty values. That redundancy is deliberate
	// and it is the whole reason the factory takes a map instead of being
	// handed each value: "what the agent process is told" is then one
	// object, and every key in it was written by code that had the value in
	// hand. An agent told an empty gate socket path fails to load its
	// plugins, and it does so by refusing every tool call — a node that
	// looks configured and answers nothing.
	agentEnv := map[string]string{
		wire.GateSocketEnv: socketPath,
		wire.ToolSocketEnv: toolSocketPath,
	}
	if modelConfigured {
		modelsPath, err := agentmodel.Write(modelCfg)
		if err != nil {
			return nil, nil, err
		}
		for key, value := range modelCfg.AgentEnvVars() {
			agentEnv[key] = value
		}
		// The endpoint and the model are safe to log; the token is not, and
		// it is not in models.json either — see the agentmodel package.
		log.Info("node agent model endpoint installed",
			slog.String("models", modelsPath),
			slog.String("provider", agentmodel.ProviderID),
			slog.String("base_url", modelCfg.BaseURL),
			slog.String("model", modelCfg.Model))
	} else {
		log.Info("node agent has no model endpoint configured; the agent will use whatever "+
			"provider its own configuration scope resolves",
			slog.String("set", agentmodel.BaseURLEnv))
	}

	// The adoption path: when the environment was silent, the manager's
	// heartbeat answer fills the same scope later. It writes into the same
	// agentEnv the factory reads, so an adopted endpoint reaches the agent
	// through exactly the mechanism a boot-time one does — there is no
	// second way for the process to learn where its model lives.
	//
	// The credential is the node's own tunnel pair, and that is the whole
	// point of the design: the gateway authenticates this pair, so the
	// answer from the manager carries no secret and the node has no second
	// token to rotate. Empty disables adoption; see modelAdopter.
	adopter := &modelAdopter{
		env:        agentEnv,
		credential: cfg.credential(),
		dir:        agentmodel.DirFromEnv(),
		log:        log,
	}

	// The allow-list is built from the same manifests that were just
	// admitted, and is built before the agent starts. Nothing reaches the
	// gate later than this: a tool the host did not bind at boot is
	// refused on every turn, so a package that ships an undeclared tool
	// cannot become reachable by being clever about it at run time.
	registry, err := policygate.RegistryFromManifests(manifestsOf(admitted))
	if err != nil {
		// Two packages claiming one tool is a packaging problem the
		// operator has to resolve, and resolving it by picking a winner
		// would make the effective permission of a tool depend on install
		// order. Refusing the node is the honest outcome.
		return nil, nil, fmt.Errorf("edge agent tool allow-list: %w", err)
	}
	log.Info("node agent tool allow-list built",
		slog.Int("tools", registry.Len()),
		slog.Any("names", registry.Names()))

	// Autonomy is built from the same admitted manifests and in the same
	// place, so a package cannot be in the allow-list and missing from the
	// self-heal declarations, or the other way round. It returns nil when
	// nothing asked for autonomy, which is the state of every package that
	// ships today, and nil is not a failure.
	autonomyStack, err := buildAutonomy(ctx, client, admitted, agent, runner, cfg.Cwd, log)
	if err != nil {
		// A package that asked for autonomy on a node that cannot hold the
		// audit spool is refused, not downgraded. Autonomy without a record
		// is a self-heal nobody can audit, which is the whole thing the
		// control plane signs for.
		return nil, nil, fmt.Errorf("edge agent autonomy: %w", err)
	}
	if autonomyStack != nil {
		// The counters are in the boot log because the first question
		// anybody asks a node that did not self-heal is "did it think it
		// was allowed to", and an answer that is only in memory is an
		// answer nobody has.
		log.Info("node autonomy health", slog.Any("health", autonomyStack.Health()))
		// The replay loop shares the node's context, so it stops when the
		// node stops and not a moment later. It drains once immediately:
		// a node that has just come back from an outage has rows the
		// operator is waiting to read, and the pump's own interval governs
		// the *rate* of the backlog rather than the first row of it.
		go func() {
			if err := autonomyStack.pump.Run(ctx); err != nil && ctx.Err() == nil {
				log.Warn("autonomy replay pump stopped early", slog.Any("err", err))
			}
		}()
		priorStop := stop
		stop = func() {
			// Closing the spool first flushes and releases the file the
			// arbiter writes through. A node that is restarted mid-outage
			// has to leave its decisions readable by the process that comes
			// after it.
			if err := autonomyStack.spool.Close(); err != nil {
				log.Warn("autonomy audit spool did not close cleanly", slog.Any("err", err))
			}
			priorStop()
		}
	}

	// The node's own ledger (决策 126), built here for the same reason
	// autonomy is: it has to exist before the gate, because the gate is
	// its first writer and a gate constructed without a sink is a gate
	// that records nothing — which is the state this node was in until
	// this line existed.
	audit, err := buildAuditLedger(client, agent, cfg.Cwd, log)
	if err != nil {
		return nil, nil, fmt.Errorf("edge agent audit ledger: %w", err)
	}
	log.Info("node audit ledger health", slog.Any("health", audit.Health()))
	// The same sink for the Agent's own writers — the plugin installer
	// today. It is a setter and not a constructor argument because the
	// Agent was built before this file ran, and rebuilding it to hand it
	// a field would mean duplicating the collector, the config and the
	// link state it owns.
	agent.SetAuditSink(audit.sink)
	go func() {
		if err := audit.pump.Run(ctx); err != nil && ctx.Err() == nil {
			log.Warn("node audit pump stopped early", slog.Any("err", err))
		}
	}()
	{
		priorStop := stop
		stop = func() {
			// Close before the gate socket goes away: a node shutting down
			// mid-turn still has rows in flight, and the file they are in
			// has to be flushed for the process that comes after this one.
			if err := audit.sink.Close(); err != nil {
				log.Warn("node audit ledger did not close cleanly", slog.Any("err", err))
			}
			priorStop()
		}
	}

	// The gate, the socket, the bridge and the supervisor reference each
	// other, and the cycle is broken in one place rather than spread across
	// the file: the supervisor is built first with a factory that reads the
	// socket path at the moment it spawns, the bridge is built next, then
	// the gate, then the socket, and only then is the supervisor started.
	// Every reference below is to something already constructed, and the
	// one forward reference is a variable the factory reads late.

	// --mode rpc is the headless protocol this node speaks. --piglet points
	// at the profile written above, and it is not optional: without it the
	// agent starts with PiG's stock built-ins, which include a shell, and
	// the model on a production node would be offered it. The order is the
	// agent's own; both flags are read before the session starts, so
	// neither has to precede the other.
	args := []string{"--mode", "rpc", "--piglet", profilePath}
	// Every process is a new one: a restart is a new agent, not a resume.
	// The agent holds no transcript across its own death, and pretending
	// otherwise would drop the turn's output without saying so.
	factory := func() ports.AgentProcess {
		// The overlay is read here, at spawn time, rather than captured
		// above: an endpoint adopted from the manager must reach the next
		// process, and the supervisor respawns through this same closure.
		// Reading it here is what makes adoption survive the restart it
		// triggers — a captured copy would restart the agent onto the old
		// configuration and the loop would never converge.
		spawnEnv := make(map[string]string, len(agentEnv)+2)
		for key, value := range agentEnv {
			spawnEnv[key] = value
		}
		for key, value := range adopter.envOverlay() {
			spawnEnv[key] = value
		}
		return pigrpc.New(pigrpc.Options{
			Binary:   cfg.Binary,
			Cwd:      cfg.Cwd,
			Args:     args,
			Provider: cfg.Provider,
			Model:    cfg.Model,
			Version:  version,
			// Two sockets, and they are the only things the agent is told:
			// not the role, not the session, not what is permitted. An
			// agent handed those would be handed the ability to assert
			// them, and an assertion is not a lookup.
			//
			// The gate socket is how a tool call inside the agent reaches
			// the host that is allowed to say no. The tool socket is how
			// the call, once permitted, reaches the host that actually
			// performs it — because the agent process holds no
			// implementation of any of them.
			//
			// The model endpoint and its credential are in here too, and
			// for the same reason: the agent is told where to send a
			// request, never how to answer one. It holds no provider key
			// of its own and no way to reach one that was not named here.
			Env: spawnEnv,
		})
	}
	sup, err := pigsupervisor.New(pigsupervisor.Config{
		Factory:          factory,
		Log:              log,
		MaxCrashAttempts: cfg.MaxCrashAttempts,
		RestartBackoff:   cfg.RestartBackoff,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("edge agent supervisor: %w", err)
	}
	// The adopter restarts through this supervisor once the manager names
	// an endpoint. The reference is set after construction because the
	// factory needs the adopter and the supervisor needs the factory; the
	// cycle is broken by this one late assignment.
	adopter.sup = sup
	// Subscribe the adopter to the manager's heartbeat answer. It is
	// registered unconditionally: even a node that configured its endpoint
	// by hand wants the answer recorded for the next time the environment
	// is not there, and the precedence rule lives in agentmodel.Resolve
	// rather than in this wiring.
	agent.SetModelAnswerFn(adopter.adopt)
	// The supervisor's own loop ends with ctx, but the agent process it
	// supervises is a child of this binary and does not. Without an
	// explicit stop, an edge shutting down for an upgrade would leave a
	// `pig` holding the node's credentials with nobody to supervise it.
	stop = func() {
		if err := sup.Stop(); err != nil {
			log.Warn("edge agent did not stop cleanly", slog.Any("err", err))
		}
	}

	// One translator per conversation, held by the node. The agent
	// multiplexes conversations over a single process, and a shared
	// counter would interleave two operators' turns into one sequence.
	translators := pigwire.NewSet(nil, 0)

	bridge, err = edgebiz.NewAgentBridge(edgebiz.AgentBridgeOptions{
		Source:    sup,
		Client:    client,
		Log:       log,
		Translate: translators.Translate,
	})
	if err != nil {
		stop()
		return nil, nil, err
	}

	// The gate is the node's only path to a tool call. It relays approval
	// frames through the bridge and the bridge applies the operator's
	// answers back to it, so the bridge is built first and given the gate
	// afterwards.
	//
	// The base policy is read-only and every real decision goes through
	// ByActor, so the only way a mutating call runs is by a role the
	// manager authenticated and the node resolved. A caller the node
	// cannot place in that ladder gets the base policy, which is the
	// bottom of it.
	gate, err := policygate.New(policygate.Options{
		Policy:  registry.Policy(roleCeiling("")),
		ByActor: func(actor string) policygate.Policy { return registry.Policy(roleCeiling(actor)) },
		Emit:    bridge.EmitApproval,
		// The gate records every call it allows, blocks and defers. It
		// was handed nothing before decision 126, and every row it wrote
		// went to a nil check at the top of record() — the check is still
		// there, because a gate with no sink must not panic, but on this
		// node it is no longer the path taken.
		Audit: audit.sink,
	})
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("edge agent approval gate: %w", err)
	}
	bridge.SetDecider(gate)

	// The socket is the enforcement point. It is created before the agent
	// is started so there is no window in which a running agent has no way
	// to ask, and it is the last thing torn down so a tool call in flight
	// during shutdown still gets an answer.
	socket, err := gatesocket.Listen(gatesocket.Options{
		Admit: gate,
		Actor: bridge.ActorFor,
		Log:   log,
	})
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("edge agent gate socket: %w", err)
	}
	socketPath = socket.Path()
	agentEnv[wire.GateSocketEnv] = socketPath
	priorStop := stop
	stop = func() {
		if err := socket.Close(); err != nil {
			log.Warn("edge agent gate socket did not close cleanly", slog.Any("err", err))
		}
		priorStop()
	}

	// The broker is the other half of the same arrangement, and it is
	// built after the gate because it authorises against the same registry
	// with the same role ladder. It re-checks rather than trusting the
	// gate's answer because the gate is reached through an extension in
	// the agent process: a package that replaced that extension would
	// silence the check, and this is the one it cannot silence.
	broker, err := toolbroker.Listen(toolbroker.Options{
		// The gate is passed in so the broker can demand the receipt for
		// any call that needed a human. Without it a package that replaced
		// the courier would find its mutating tools running unapproved.
		Authorize: toolAuthorizer(registry, gate, agent),
		Invoke:    &agentToolInvoker{client: client, log: log, obs: agent},
		Actor:     bridge.ActorFor,
		// The declared limits, read from the same registry the class is
		// read from. This is the last point at which a tool's answer is
		// still in host hands, so it is where the ceiling is applied: the
		// tools that can return a gigabyte are not the ones an author
		// remembers to bound, and a limit enforced by the tool is a limit
		// the tool can decline to honour.
		BudgetFor: func(toolName string) toolbroker.Budget {
			binding, ok := registry.Lookup(toolName)
			if !ok {
				// The authoriser refuses an unbound tool one line earlier;
				// reaching here with an unknown name would mean the two
				// disagree, and the default is what keeps the disagreement
				// from also being an unbounded reply.
				return toolbroker.Budget{}
			}
			return toolbroker.Budget{
				MaxOutputBytes: binding.Budget(),
				Timeout:        binding.Timeout(),
			}
		},
		Log: log,
	})
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("edge agent tool broker: %w", err)
	}
	toolSocketPath = broker.Path()
	agentEnv[wire.ToolSocketEnv] = toolSocketPath
	priorStop = stop
	stop = func() {
		if err := broker.Close(); err != nil {
			log.Warn("edge agent tool broker did not close cleanly", slog.Any("err", err))
		}
		priorStop()
	}

	if err := sup.Start(ctx); err != nil {
		// Report it and keep the supervisor. It is already in the
		// crash-loop policy, so a binary that is missing now may be
		// present after the next package push, and a node that gave up on
		// its agent at boot would need the edge restarted to pick it up.
		log.Error("edge agent did not start; the node will keep retrying under the crash policy",
			slog.String("binary", cfg.Binary),
			slog.String("dir", cfg.Cwd),
			slog.Any("err", err))
	}

	// The console learns that the agent restarted through agent.state and
	// agent.health, not through a resumed sequence. Carrying the old
	// counters across the swap would make a fresh process answer a
	// conversation the operator believes is further along than it is.
	sup.OnRestart(translators.ForgetAll)
	return bridge, stop, nil
}
