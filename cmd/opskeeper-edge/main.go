// Command opskeeper-edge is the edge-side binary. It opens a tunnel to cloud,
// pushes host metrics, and serves tool RPC handlers (get_host_load / ...).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/errgroup"

	"github.com/vincent-wuhan/opskeeper/core/floor/config"
	"github.com/vincent-wuhan/opskeeper/core/floor/httpserver"
	"github.com/vincent-wuhan/opskeeper/core/floor/logger"
	"github.com/vincent-wuhan/opskeeper/core/floor/prom"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"

	edgebash "github.com/vincent-wuhan/opskeeper/core/edge/bash"
	edgebiz "github.com/vincent-wuhan/opskeeper/core/edge/biz"
	edgecollector "github.com/vincent-wuhan/opskeeper/core/edge/collector"
	edgehostfiles "github.com/vincent-wuhan/opskeeper/core/edge/host_files"
	edgeplugins "github.com/vincent-wuhan/opskeeper/core/edge/plugins"
	edgeplugincustommetrics "github.com/vincent-wuhan/opskeeper/core/edge/plugins/custommetrics"
	edgeplugindatabasemetrics "github.com/vincent-wuhan/opskeeper/core/edge/plugins/databasemetrics"
	edgepluginhostmetrics "github.com/vincent-wuhan/opskeeper/core/edge/plugins/hostmetrics"
	edgepluginlogs "github.com/vincent-wuhan/opskeeper/core/edge/plugins/logs"
	edgepluginmetrics "github.com/vincent-wuhan/opskeeper/core/edge/plugins/metrics"
	edgepluginprocmetrics "github.com/vincent-wuhan/opskeeper/core/edge/plugins/procmetrics"
	edgeplugintraces "github.com/vincent-wuhan/opskeeper/core/edge/plugins/traces"
	edgerestartservice "github.com/vincent-wuhan/opskeeper/core/edge/restart_service"
	edgesvc "github.com/vincent-wuhan/opskeeper/core/edge/service"
	edgewebshell "github.com/vincent-wuhan/opskeeper/core/edge/webshell"

	// Builtin skill init() blocks register Executors with the shared
	// core/floor/skill registry. The edge-side dispatcher
	// (core/edge/skill) routes execute_skill RPCs by key —
	// without this import the registry is empty and every skill call
	// returns "unknown skill".
	_ "github.com/vincent-wuhan/opskeeper/core/floor/skill/builtin"
)

// version is overwritten at build time via -ldflags.
var version = "dev"

// edgeMetricsAddr is the local debug /metrics port for edge. Kept separate
// from cloud metrics (:9100) so both can run on the same dev host.
const edgeMetricsAddr = ":9101"

func main() {
	// Print-and-exit flags before anything that can fail (config load,
	// env access). install.sh and operators rely on `opskeeper-edge --version`
	// printing the build tag without starting the agent.
	for _, a := range os.Args[1:] {
		switch a {
		case "--version", "-v":
			fmt.Fprintf(os.Stdout, "opskeeper-edge %s\n", version)
			return
		case "--help", "-h":
			fmt.Fprintf(os.Stdout, "opskeeper-edge %s\n", version)
			fmt.Fprintln(os.Stdout, "Run as a systemd service. See /etc/opskeeper-edge/opskeeper-edge.env for config.")
			return
		}
	}

	fmt.Fprintf(os.Stderr, "opskeeper-edge %s starting\n", version)

	// The node reads its own configuration and nothing else. config.Load
	// would also read every model-vendor API key, the admin password, the
	// JWT secret and the database DSN into this process, and this process
	// runs restart_service and a bash sandbox on a customer host. The
	// loader's own comment says why; the short version is that a node which
	// cannot represent a vendor key cannot be handed one.
	cfg := config.LoadEdge()

	log := logger.WithService(logger.New(slog.LevelInfo), "opskeeper-edge")
	log.Info("configuration loaded",
		slog.String("cloud_addr", cfg.CloudAddr),
		slog.String("collector_mode", cfg.CollectorMode),
		slog.String("version", version),
	)

	reg := prom.NewRegistry()

	// Tunnel client.
	client := tunnel.NewClient(tunnel.ClientConfig{
		CloudAddr: cfg.CloudAddr,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
		Log:       log,
	})

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	eg, egCtx := errgroup.WithContext(rootCtx)

	// Build the collector based on configured mode.
	collector, scraperRunner, err := buildCollector(egCtx, cfg, log, eg)
	if err != nil {
		log.Error("collector init failed", slog.Any("err", err))
		os.Exit(1)
	}
	_ = scraperRunner // scraper goroutine already wired into eg below

	edgesvc.RegisterWithCollector(client, collector, log)

	// host_files plugin (PR-8 + PR-N of): register the three
	// filesystem inspection handlers (find_large_files / du_summary /
	// stat_file). Real shell-out gated by SandboxConfig — failure to
	// validate the sandbox (no allowed paths, missing find/du in PATH)
	// is non-fatal: the edge boots without the host_files capability
	// and operators see the warning in the journal.
	if err := edgehostfiles.Register(client, log); err != nil {
		log.Warn("host_files register failed; capability disabled", slog.Any("err", err))
	}

	// restart_service plugin — the first and only MUTATING skill on a node.
	//
	// The posture is the operator's: Mocked defaults to true, and a mocked
	// node answers successfully without touching a service, saying so in
	// its response. Setting it false makes this node really run systemctl,
	// against an allow-list that is still enforced here at the edge rather
	// than trusted from the cloud. A sandbox that fails validation costs
	// the capability, not the boot: the edge can still scrape metrics and
	// read files without it.
	restartSandbox := &edgerestartservice.SandboxConfig{
		Mocked:        cfg.RestartService.Mocked,
		SystemctlPath: cfg.RestartService.SystemctlPath,
	}
	if len(cfg.RestartService.AllowedUnits) > 0 {
		restartSandbox.AllowedUnits = cfg.RestartService.AllowedUnits
	} else {
		restartSandbox.AllowedUnits = edgerestartservice.DefaultAllowedUnits()
	}
	if err := edgerestartservice.RegisterWith(client, restartSandbox, log); err != nil {
		log.Warn("restart_service register failed; capability disabled", slog.Any("err", err))
	}

	// bash skill: generic read-only shell-execution gated by
	// core/edge/cmdpolicy. The cmdpolicy package owns the
	// rules (binary classes / arg matchers / path + network
	// allowlists); this Register call wires the cmdpolicy.Sandbox to
	// the host_files path validator and installs the handler. Boot
	// continues on any soft failure (operator yaml override parse
	// error, missing binaries) — cmdpolicy.Sandbox.Decide just
	// rejects calls cleanly with a Reason the LLM can read.
	//
	// The sandbox is built once and used twice: by the bash handler below,
	// and by the autonomy runner that a signed package may install. One
	// sandbox on one host is one policy; two would be two answers to
	// "what may this node run", and the day they disagree the signed argv
	// is running under whichever rule nobody reviewed.
	sandbox, err := edgebash.NewSandbox(log)
	if err != nil {
		log.Warn("bash sandbox unavailable; capability disabled", slog.Any("err", err))
	} else if err := edgebash.RegisterWithSandbox(client, sandbox, log); err != nil {
		log.Warn("bash register failed; capability disabled", slog.Any("err", err))
	}

	// WebSSH: edge is a stream port-forwarder. Manager opens a
	// frontier stream with Meta describing the target (sshd at
	// 127.0.0.1:22), edge io.Copy's bytes both ways. SSH client
	// lives entirely on the manager — see core/manager/server/
	// webshell. The edge has no SSH lib, no PTY, no session map.
	edgewebshell.Register(client, log.With(slog.String("comp", "webshell")))

	// UpgradeStageDir defaults to the systemd-install layout
	// /var/lib/opskeeper-edge/.upgrade. Empty disables agent_upgrade entirely
	// (dev / non-systemd); set OVERRIDE via env to relocate for tests.
	stageDir := os.Getenv("OPSKEEPER_EDGE_UPGRADE_STAGE_DIR")
	if stageDir == "" {
		stageDir = "/var/lib/opskeeper-edge/.upgrade"
	}
	// Telemetry write-ahead log. Same systemd-install layout as the
	// upgrade stage and the plugin work dir, with the same escape hatch,
	// and the same reason: a node that cannot write here still starts and
	// still heartbeats — it just loses samples while the link is down,
	// which the agent logs rather than hides.
	telemetryWALDir := os.Getenv("OPSKEEPER_EDGE_TELEMETRY_WAL_DIR")
	if telemetryWALDir == "" {
		telemetryWALDir = "/var/lib/opskeeper-edge/telemetry"
	}

	// The change watcher's durable log gets its own directory, because the
	// two logs are graded by one policy table and stored in two files, and
	// an operator who clears "the log" should not have to guess which.
	changeEventWALDir := os.Getenv("OPSKEEPER_EDGE_CHANGE_EVENT_WAL_DIR")
	if changeEventWALDir == "" {
		changeEventWALDir = "/var/lib/opskeeper-edge/changes"
	}
	// How long this node tolerates losing the control plane. Read before the
	// agent is built so a typo is a boot error rather than a node running
	// on numbers its operator did not choose, and logged with the product
	// because the two settings multiply into the one an operator is
	// actually trying to control.
	tunables, err := loadTunables(log)
	if err != nil {
		log.Error("edge: refusing to boot on an unparseable link tolerance", slog.Any("err", err))
		os.Exit(1)
	}
	log.Info("edge: link tolerance",
		slog.Duration("heartbeat", tunables.heartbeat),
		slog.Int("stuck_after", tunables.stuck),
		slog.Duration("gives_up_after", tunables.tolerance()))

	agentCfg := edgebiz.Config{
		MetricsInterval:   cfg.CollectorInterval,
		AgentVersion:      version,
		TelemetryWALDir:   telemetryWALDir,
		ChangeEventWALDir: changeEventWALDir,
		// The same value install-time admission compares
		// min_pig_version against, so the control plane's pre-flight and
		// this node's own verdict can never be looking at two different
		// numbers for the same node. pigSelfVersion is not free: it reads
		// the operator's override first and only then falls back to the
		// linked release line, which is the same order the installer uses.
		PigVersion:      pigSelfVersion(),
		UpgradeStageDir: stageDir,
	}
	tunables.apply(&agentCfg)
	agent := edgebiz.NewAgent(client, collector, agentCfg, log)

	// Node agent: a PiG process under this node's supervision, serving the
	// manager's agent.* commands. A failure here is logged and the edge
	// boots without it - the node still collects metrics and still serves
	// its own skill RPCs. An edge that refused to start because the AI
	// was down would take the telemetry with it, and telemetry is the one
	// thing a node cannot be redeployed to get back quickly.
	agentLog := log.With(slog.String("comp", "node-agent"))
	nodeCfg := loadNodeAgentConfig()
	agentBridge, stopNodeAgent, err := startNodeAgent(egCtx, client, nodeCfg, version, agent,
		autonomyRunner{sandbox: sandbox}, agentLog)
	if err != nil {
		log.Warn("node agent unavailable; the node runs without an AI agent", slog.Any("err", err))
	} else {
		agent.SetAgentBridge(agentBridge)
		// The bridge's events are relayed on goroutines that outlive the
		// agent supervisor's own loop, so the process is stopped on the
		// way out rather than left orphaned holding node credentials.
		defer stopNodeAgent()
	}

	// Plugin distribution. Wired outside the node-agent block above
	// because it is independent of it: a node with no AI agent can still
	// be handed a package, and a node whose agent failed to start should
	// not silently become a node that cannot be updated.
	//
	// It shares nodeCfg on purpose. The store republishes the boot
	// bundle alongside anything it installs, and the bundle it republishes
	// has to be the one the node actually booted with.
	if store, storeErr := newPluginStore(nodeCfg, log.With(slog.String("comp", "plugin-store"))); storeErr != nil {
		log.Warn("plugin distribution is unavailable; the node runs its boot packages only",
			slog.Any("err", storeErr))
	} else {
		agent.SetPluginInstaller(store)
	}

	// Local /metrics listener for debugging.
	metricsMux := chi.NewRouter()
	metricsMux.Handle("/metrics", prom.Handler(reg))
	metricsMux.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	metricsServer := httpserver.New(edgeMetricsAddr, metricsMux, log.With(slog.String("listener", "metrics")))

	eg.Go(func() error { return metricsServer.Start(egCtx) })
	// When agent.Run returns (clean ctx cancel OR upgrade
	// swap), cancel rootCtx so every other goroutine — plugin
	// supervisor, scraper, metrics http server — unwinds and the
	// process exits. Without this, returning nil from agent.Run
	// leaves siblings on tickers and eg.Wait() blocks forever;
	// systemd never gets the EXIT it needs to swap the staged bundle.
	eg.Go(func() error {
		defer cancel()
		return agent.Run(egCtx)
	})

	// Plugin runtime: logs / traces / future plugins. Each
	// plugin is a goroutine (in-process) or supervised subprocess. The
	// supervisor reconciles current state against config from a
	// ConfigFetcher; PR-C1 ships the env-based fetcher (manager-driven
	// fetcher follows in PR-C2).
	pluginBinDir := envOr("OPSKEEPER_EDGE_PLUGIN_BIN_DIR", "/usr/local/lib/opskeeper-edge")
	pluginWorkDir := envOr("OPSKEEPER_EDGE_PLUGIN_WORK_DIR", "/var/lib/opskeeper-edge/plugins")
	pluginLog := log.With(slog.String("comp", "plugins"))
	edgeplugindatabasemetrics.RegisterSecretHandler(client, pluginLog)

	registered := []edgeplugins.Plugin{
		edgepluginlogs.New(pluginBinDir, pluginWorkDir, pluginLog),
		// traces plugin: subprocess otelcol-contrib. Stays
		// disabled until manager pushes a PluginConfig with enabled=true
		// + Endpoint set to the manager public /v1/traces URL.
		edgeplugintraces.New(pluginBinDir, pluginWorkDir, pluginLog),
		// metrics plugin: in-process scraper that polls a
		// local /metrics endpoint (default node_exporter on
		// 127.0.0.1:9100) and pushes via the existing push_prom_samples
		// tunnel RPC. No subprocess, no remote_write — the pre-existing
		// manager-side ingester injects the canonical device_id label,
		// which is the join key the Monitor / alert rule preview /
		// correlate_incident all rely on.
		edgepluginmetrics.New(client, agent.EdgeID, pluginLog),
		// custommetrics: in-process scraper for arbitrary operator-provided
		// Prometheus /metrics endpoints.
		edgeplugincustommetrics.New(client, agent.EdgeID, pluginLog),
		// databasemetrics: edge-side managed database exporters. The manager
		// sends source specs; the edge reads local secret files and starts the
		// exporter subprocesses.
		edgeplugindatabasemetrics.New(pluginBinDir, pluginWorkDir, client, agent.EdgeID, pluginLog),
		// hostmetrics plugin: subprocess node_exporter. Exposes node_*
		// at :9102 (configurable via spec.listen_address) for the
		// manager-side Prometheus to scrape through the docker bridge.
		// Default-disabled; manager toggles enabled=true via the
		// Plugins UI to bring it up.
		edgepluginhostmetrics.New(pluginBinDir, pluginWorkDir, pluginLog),
		// procmetrics plugin: subprocess process-exporter. Exposes
		// namedprocess_namegroup_* at :9256 grouped by comm (default)
		// or operator-tuned regex. Backs the Monitor "Top N processes"
		// timeline panel via PromQL.
		edgepluginprocmetrics.New(pluginBinDir, pluginWorkDir, pluginLog),
	}
	pluginNames := make([]string, 0, len(registered))
	for _, p := range registered {
		pluginNames = append(pluginNames, p.Name())
	}
	// Tunnel-driven fetcher (real-time push from manager via
	// MethodGetPluginConfigs;). Falls back to env
	// when the tunnel is down so plugins keep running during outages.
	tunnelFetcher := edgeplugins.NewTunnelConfigFetcher(client, pluginNames)
	supervisor := edgeplugins.NewSupervisor(edgeplugins.SupervisorOpts{
		Fetcher: tunnelFetcher,
		Log:     pluginLog,
	})
	for _, p := range registered {
		supervisor.Register(p)
	}
	// Ship per-plugin health on every heartbeat so the manager / UI can
	// see which plugins are running vs. crashed (and why) instead of
	// discovering empty telemetry days later. Set after the supervisor
	// exists; the agent reads this under a lock so the post-Run wiring is
	// race-free.
	agent.SetPluginHealthFn(func() []tunnel.PluginHealthWire {
		snaps := supervisor.HealthSnapshots()
		out := make([]tunnel.PluginHealthWire, 0, len(snaps))
		for _, s := range snaps {
			targets := make([]tunnel.PluginTargetHealthWire, 0, len(s.Targets))
			for _, th := range s.Targets {
				wth := tunnel.PluginTargetHealthWire{
					ID:        th.ID,
					Name:      th.Name,
					Kind:      th.Kind,
					State:     th.State,
					LastError: th.LastError,
					Samples:   th.Samples,
				}
				if !th.LastSuccessAt.IsZero() {
					wth.LastSuccessAt = th.LastSuccessAt.Unix()
				}
				if !th.UpdatedAt.IsZero() {
					wth.UpdatedAt = th.UpdatedAt.Unix()
				}
				targets = append(targets, wth)
			}
			w := tunnel.PluginHealthWire{
				Name:         s.Name,
				State:        string(s.State),
				LastError:    s.LastError,
				RestartCount: s.RestartCount,
				PID:          s.PID,
				Targets:      targets,
			}
			if !s.StartedAt.IsZero() {
				w.StartedAt = s.StartedAt.Unix()
			}
			if !s.UpdatedAt.IsZero() {
				w.UpdatedAt = s.UpdatedAt.Unix()
			}
			out = append(out, w)
		}
		return out
	})
	// Manager → edge reload push: when manager mutates plugin config it
	// calls MethodPluginConfigsChanged on this edge. We just nudge the
	// supervisor to re-fetch — body is empty by design.
	client.RegisterHandler(tunnel.MethodPluginConfigsChanged, func(_ context.Context, _ tunnel.Session, _ string, _ []byte) ([]byte, error) {
		supervisor.TriggerReload()
		return []byte(`{}`), nil
	})
	eg.Go(func() error { return supervisor.Run(egCtx) })

	err = eg.Wait()

	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("shutdown with error", slog.Any("err", err))
		os.Exit(1)
	}
	log.Info("opskeeper-edge shutdown complete")
}

// buildCollector constructs the collector matching cfg.CollectorMode.
// For scrape mode the per-target scrape goroutines are added to eg so
// they share the agent's lifecycle.
//
// Modes:
//
//	off / "" — preferred for installs running the hostmetrics +
//	  procmetrics plugins. CollectAll is a no-op so no node_* samples
//	  are pushed via the tunnel; manager-side Prom scrapes the
//	  node_exporter / process-exporter subprocesses directly through
//	  the docker bridge. On-demand RPCs (host_info / get_host_load /
//	  get_host_processes) still work via the embedded gopsutil
//	  snapshot path.
//	auto — legacy: embedded (gopsutil push) + scraper.
//	embedded — embedded push only.
//	scrape — scraper only.
func buildCollector(ctx context.Context, cfg *config.EdgeConfig, log *slog.Logger, eg *errgroup.Group) (edgebiz.Collector, *edgecollector.Scraper, error) {
	switch cfg.CollectorMode {
	case "off", "none", "":
		// Default for fresh installs: don't push anything periodically.
		// On-demand RPCs still hit gopsutil via the wrapped embedded
		// collector so AIOps tools and EdgeDetail cards keep working.
		em, err := edgecollector.NewEmbedded(log)
		if err != nil {
			return nil, nil, fmt.Errorf("embedded collector: %w", err)
		}
		return collectorAdapter{c: edgecollector.NewNoopPush(em)}, nil, nil

	case "auto":
		em, err := edgecollector.NewEmbedded(log)
		if err != nil {
			return nil, nil, fmt.Errorf("embedded collector: %w", err)
		}
		sc, err := edgecollector.LoadScrapeConfig(cfg.ScrapeConfigFile)
		if err != nil {
			log.Warn("scrape config unavailable; using embedded baseline only", slog.Any("err", err))
			return collectorAdapter{c: em}, nil, nil
		}
		scraper := edgecollector.NewScraper(sc, log)
		eg.Go(func() error { return scraper.Run(ctx) })
		return collectorAdapter{c: edgecollector.NewComposite(em, scraper, log)}, scraper, nil

	case "scrape":
		sc, err := edgecollector.LoadScrapeConfig(cfg.ScrapeConfigFile)
		if err != nil {
			return nil, nil, fmt.Errorf("scrape config: %w", err)
		}
		scraper := edgecollector.NewScraper(sc, log)
		eg.Go(func() error { return scraper.Run(ctx) })
		return collectorAdapter{c: scraper}, scraper, nil

	case "embedded":
		em, err := edgecollector.NewEmbedded(log)
		if err != nil {
			return nil, nil, fmt.Errorf("embedded collector: %w", err)
		}
		return collectorAdapter{c: em}, nil, nil
	default:
		return nil, nil, fmt.Errorf("unknown collector mode %q", cfg.CollectorMode)
	}
}

// envOr reads an env var, returning def when unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// collectorAdapter bridges the collector package's Collector interface to
// the biz package's identical-shaped interface. Two interfaces, one
// implementation — the seam exists so biz/agent.go does not import
// core/edge/collector (avoids cycles when the collector package
// in turn depends on tunnel types).
type collectorAdapter struct {
	c edgecollector.Collector
}

func (a collectorAdapter) CollectAll(ctx context.Context) ([]edgebiz.CollectorOutput, error) {
	outs, err := a.c.CollectAll(ctx)
	if err != nil {
		return nil, err
	}
	bizOuts := make([]edgebiz.CollectorOutput, 0, len(outs))
	for _, o := range outs {
		bizOuts = append(bizOuts, edgebiz.CollectorOutput{
			Source:         o.Source,
			HostPoint:      o.HostPoint,
			HostPointValid: o.HostPointValid,
			Samples:        o.Samples,
		})
	}
	return bizOuts, nil
}

func (a collectorAdapter) HostInfo(ctx context.Context) (tunnel.HostInfo, error) {
	return a.c.HostInfo(ctx)
}

func (a collectorAdapter) GetHostLoad(ctx context.Context) (tunnel.GetHostLoadResponse, error) {
	return a.c.GetHostLoad(ctx)
}

func (a collectorAdapter) GetProcessList(ctx context.Context, topN int, sortBy string) (tunnel.GetProcessListResponse, error) {
	return a.c.GetProcessList(ctx, topN, sortBy)
}
