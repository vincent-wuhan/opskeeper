package biz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/vincent-wuhan/opskeeper/core/edge/changewatcher"
	skilldispatch "github.com/vincent-wuhan/opskeeper/core/edge/skill"
	"github.com/vincent-wuhan/opskeeper/core/edge/telemetrywal"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Collector is the contract the edge agent requires of a metric source.
// Implementations live in core/edge/collector — both the
// embedded (gopsutil) and scrape (HTTP /metrics) backends satisfy it.
//
// CollectAll returns one CollectorOutput per logical source on each
// tick. Embedded mode produces one element ("embedded"); scrape mode
// produces one per configured target ("scrape:<name>"). An empty slice
// is valid (e.g. scraper warming up); the agent simply skips the push.
type Collector interface {
	CollectAll(ctx context.Context) ([]CollectorOutput, error)
	HostInfo(ctx context.Context) (tunnel.HostInfo, error)
	GetHostLoad(ctx context.Context) (tunnel.GetHostLoadResponse, error)
	GetProcessList(ctx context.Context, topN int, sortBy string) (tunnel.GetProcessListResponse, error)
}

// CollectorOutput is one logical collection result for one source.
//
// HostPoint is the 8-field fast path consumed by the legacy
// push_host_metrics wire method (best-effort: zero values allowed for
// any field the source did not expose; rate-derived fields return 0
// on the very first call).
//
// Samples is the open-set rich path consumed by the new
// push_prom_samples wire method.
type CollectorOutput struct {
	Source         string
	HostPoint      tunnel.HostMetricPoint
	HostPointValid bool
	Samples        []tunnel.PromSample
}

// MinMetricsInterval is the shortest host-metric sampling period the wire can
// carry.
//
// tunnel.HostMetricPoint.Ts is unix *seconds*, so two samples inside one
// second are the same point as far as the center is concerned — and since
// host_metrics_raw now stores (edge_id, ts) exactly once, the second one is
// dropped rather than stored twice. Sampling faster than this would therefore
// lose data silently, at a rate set by the operator, which is the worst
// possible shape for a config knob to have.
//
// A sub-second interval was already unrepresentable before this floor
// existed; it just failed in the other direction, by storing the same second
// twice and double-counting it in the 5m downsample. NewAgent clamps rather
// than refuses, because refusing would strand a node that has a working link
// over a tunable, and the clamp is logged at WARN so the operator can see
// that the number they configured is not the number in force.
const MinMetricsInterval = time.Second

// Config holds the agent run-loop knobs. Zero values are replaced with
// sensible defaults by NewAgent.
type Config struct {
	// HeartbeatInterval is how often the agent sends a heartbeat RPC.
	HeartbeatInterval time.Duration // default DefaultHeartbeatInterval
	// TunnelStuckThreshold is how many consecutive heartbeat failures the
	// agent tolerates before declaring the tunnel stuck and exiting for
	// systemd to respawn. Default DefaultTunnelStuckThreshold.
	//
	// It is a field rather than a constant because the two numbers above
	// multiply into a single number an operator actually has a reason to
	// change — "how long does this node tolerate losing the center" — and
	// a pair of constants offers no way to express it. The product is
	// printed at boot, because a threshold alone is not the tolerance: 5
	// heartbeats is 150s at 30s and 5s at 1s.
	TunnelStuckThreshold int
	// MetricsInterval is how often the agent samples one metric point.
	// Default 10s; floored at MinMetricsInterval. The floor is a protocol
	// limit, not a preference — see MinMetricsInterval.
	MetricsInterval time.Duration
	// MetricsBatchSize is how many points to buffer before push.
	MetricsBatchSize int // default 30 (5min at 10s)

	// AgentVersion is reported on register_edge (optional).
	AgentVersion string

	// PigVersion is the comparable PiG agent build this node hosts,
	// reported on every heartbeat. Optional: a node that leaves it
	// empty is a node the control plane cannot answer the PiG axis for,
	// and it says so rather than guessing.
	//
	// It is a plain string rather than a callback because it is a build
	// constant, not a reading: the answer changes when the operator
	// upgrades the node, which restarts the process that holds it. What
	// it must not do is default to something, because a defaulted axis
	// turns "cannot tell" into "compatible" on the one comparison that
	// decides whether an agent can load a package's extensions.
	PigVersion string

	// UpgradeStageDir is where agent_upgrade stages downloaded binaries.
	// Default /var/lib/opskeeper-edge/.upgrade. Empty disables the
	// MethodAgentUpgrade handler entirely (useful for dev where systemd
	// isn't available — manager will see "method not found").
	UpgradeStageDir string

	// TelemetryWALDir is where the node's telemetry write-ahead log
	// lives. Empty turns the log off.
	//
	// Off is not a small difference: with no directory the agent samples
	// and pushes in one step, and a push that fails loses that sample,
	// which is what the plan's 1.1 is about. It is a legitimate setting
	// for a development node and a wrong one for a fleet, so the
	// production wiring sets it and the empty case is documented as the
	// old behaviour rather than as a default.
	TelemetryWALDir string
	// TelemetryDrainInterval bounds how often the log is drained.
	// Default telemetrywal.DefaultInterval. Raising it trades recovery
	// latency for a gentler load on the center.
	TelemetryDrainInterval time.Duration

	// ChangeEventWALDir is where the change watcher's durable log lives.
	// Empty turns it off, which means a failed flush loses the batch —
	// the behaviour this node shipped with for years, and the one the
	// plan's 1.1 is about.
	//
	// It is a separate directory from the telemetry log because the two
	// are graded by the same table and stored in different files, and
	// sharing one directory would mean an operator who clears "the log"
	// has to guess which of the two they are clearing.
	ChangeEventWALDir string
}

// Agent is the edge run-loop. It owns the tunnel.Client, periodic
// heartbeat / metric push, and handler registration for cloud-issued
// RPCs.
type Agent struct {
	client    tunnel.Client
	collector Collector
	cfg       Config
	log       *slog.Logger

	// edgeID is assigned by the cloud in the register_edge response.
	edgeID uint64
	mu     sync.RWMutex

	// upgradeRequested is closed by the agent_upgrade handler after a
	// new binary is staged. Run() watches this channel and returns nil
	// when it closes — that triggers a clean process exit, which lets
	// systemd run ExecStartPre and swap the binary on restart. Buffered
	// to size 1 so the handler never blocks on a closed-channel race.
	upgradeRequested chan struct{}

	// telemetryRejected counts rows the center refused outright. A node
	// whose rejected count climbs is a node whose collector and the
	// center's expectations have drifted, and it is the one number in
	// this file that says "the log is working and the data is wrong".
	telemetryRejected atomic.Uint64

	// wal is the telemetry write-ahead log. Optional: a node with no
	// TelemetryWALDir pushes straight at the tunnel, which is the
	// pre-1.1 behaviour and is why this is a pointer and not a zero
	// value.
	wal *telemetrywal.WAL

	// pluginHealthFn, when set, returns the current per-plugin health to
	// piggyback on each heartbeat. Wired post-construction (SetPluginHealthFn)
	// because the plugin supervisor is built after the Agent in main; guarded
	// by mu so the heartbeat goroutine reads it race-free.
	pluginHealthFn func() []tunnel.PluginHealthWire

	// modelAnswerFn, when set, receives the manager's answer from each
	// heartbeat: which model endpoint this deployment wants this node's
	// agent to use. Optional and wired post-construction
	// (SetModelAnswerFn) for the same reason pluginHealthFn is.
	//
	// It is a callback rather than a field the agent writes, because the
	// agent having an opinion about where credentials live is the coupling
	// core/edge/agentmodel exists to prevent. The heartbeat's job is to
	// carry the answer back; deciding what to do with it belongs to the
	// composition root, which is the only place that knows whether the
	// node's environment already said something.
	modelAnswerFn func(tunnel.HeartbeatResponse)

	// agentBridge serves the manager's agent.* commands against this node's
	// PiG process. Optional and wired post-construction (SetAgentBridge)
	// for the same reason as pluginHealthFn: the supervisor is built in main
	// after the Agent. A node with no bridge has no agent, and says so by
	// not registering the methods - which the manager can tell apart from
	// an agent that is registered and refusing.
	agentBridge *AgentBridge

	// pluginInstaller is this node's package store, reached over the
	// ports interface. Optional and wired post-construction for the same
	// reason, plus one more: the review it performs needs the node's trust
	// store and policy ceiling, and those are the operator's configuration
	// in the composition root rather than anything this package should be
	// able to see. Guarded by mu for the same reason pluginHealthFn is.
	pluginInstaller ports.PluginInstaller

	// audit is where this node's own rows go before the center has them
	// (决策 126). Optional and wired post-construction (SetAuditSink) for
	// the same reason as pluginHealthFn: the ledger is built in the
	// composition root, after the Agent, because it needs the tunnel client
	// that the Agent is itself built from.
	//
	// A node with no sink is not a node with nothing to say — the gate is
	// the other writer and it is always there — so nil is tolerated here
	// and means "this build recorded nothing", which the composition root
	// is expected to make impossible rather than this package to enforce.
	// Guarded by mu because the plugin handlers run on the tunnel's read
	// goroutine while the setter runs on the boot goroutine.
	audit ports.AuditSink

	// link tracks whether the control plane is answering.
	//
	// The heartbeat is the witness, and nothing else is: a TCP socket that
	// has not failed yet says the network stack accepted a write, not that
	// the manager is there. It matters because the autonomy arbiter's whole
	// question is "is the center reachable, and if not since when", and an
	// answer of "the socket looks fine" is exactly the answer that would
	// let a node keep waiting for a human who is not coming.
	link linkState

	// latest holds the most recent value of each metric the node scraped.
	//
	// It is here rather than in the arbiter because the node is the only
	// thing that scrapes: an autonomy trigger has to be measured against
	// this host's own readings, and a trigger evaluated against a cached
	// value from the control plane is a trigger that works exactly when
	// the link is fine and not when it is needed.
	latest metricIndex
}

// linkState is the control plane's reachability as the heartbeat sees it.
type linkState struct {
	mu sync.Mutex
	// online is the last thing the heartbeat observed. It starts true
	// because the agent does not get here until Dial returned.
	online bool
	// offlineSince is when the last healthy link ended. Zero while the
	// link is healthy, and also zero when the node has never had one —
	// which the arbiter reads as "no outage has been established", the
	// direction that runs nothing.
	offlineSince time.Time
}

func (l *linkState) observe(ok bool, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case ok && !l.online:
		// The link came back. The offline window is over and the moment it
		// started is no longer interesting to anything.
		l.online = true
		l.offlineSince = time.Time{}
	case ok:
		l.online = true
	case !ok && l.online:
		// The first failure starts the clock. Later failures do not
		// restart it, or a node in a brown-out would keep resetting the
		// window and never reach any threshold.
		l.online = false
		l.offlineSince = now
	}
}

// online is the heartbeat's last word, for callers that only need the
// yes or the no. It is the same witness LinkReach reports, so a node
// cannot tell its autonomy arbiter it is connected while telling its
// telemetry drain it is not.
func (l *linkState) isOnline() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.online
}

func (l *linkState) reach() (bool, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.online, l.offlineSince
}

// metricIndex remembers the newest value of every metric name the node
// scraped.
type metricIndex struct {
	mu     sync.RWMutex
	values map[string]float64
}

// observe records one scrape round.
//
// Where a metric carries labels — one series per mount, per device, per
// core — the highest value wins. That is the reading that means "something
// on this host is in the state the action is declared for", and it is the
// direction that acts. The opposite choice, the mean, would average a full
// disk and an empty one into a number that triggers nothing, and a
// self-heal that silently never fires is worse than one that fires on the
// worst series: the action's reach is a declared argv against one target,
// so the cost of a false positive is bounded by what was signed.
func (m *metricIndex) observe(samples []tunnel.PromSample) {
	if len(samples) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		// Lazily, because an Agent is also built directly in tests and a
		// nil map here is a panic on the first scrape rather than a
		// missing reading.
		m.values = make(map[string]float64, len(samples))
	}
	for _, s := range samples {
		if cur, ok := m.values[s.Name]; !ok || s.Value > cur {
			m.values[s.Name] = s.Value
		}
	}
}

func (m *metricIndex) lookup(name string) (float64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.values[name]
	return v, ok
}

// LinkReach reports whether the control plane is answering, and when it
// stopped. It is the autonomy arbiter's only input about the outside world.
func (a *Agent) LinkReach() (online bool, offlineSince time.Time) { return a.link.reach() }

// MetricValue returns the node's most recent reading of a named metric.
func (a *Agent) MetricValue(name string) (float64, bool) { return a.latest.lookup(name) }

// SetPluginHealthFn wires the plugin-health provider used by the heartbeat
// loop. Safe to call after Run has started — the heartbeat goroutine reads
// the field under mu. nil fn disables plugin reporting (heartbeat omits it).
// SetAuditSink wires the node's own ledger.
//
// It exists because the ledger is built after this Agent — it needs the
// tunnel client this Agent was constructed from — and the plugin handlers
// that write to it are registered against this Agent. A setter is the
// honest shape for that, exactly as it is for the plugin health callback.
func (a *Agent) SetAuditSink(sink ports.AuditSink) {
	a.mu.Lock()
	a.audit = sink
	a.mu.Unlock()
}

// auditSink reads the sink under the lock, so a handler that records a
// plugin install and a boot that is still wiring the ledger cannot race.
func (a *Agent) auditSink() ports.AuditSink {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.audit
}

func (a *Agent) SetPluginHealthFn(fn func() []tunnel.PluginHealthWire) {
	a.mu.Lock()
	a.pluginHealthFn = fn
	a.mu.Unlock()
}

// SetModelAnswerFn wires the callback the heartbeat invokes with the
// manager's endpoint answer.
//
// It is called on every heartbeat, including when the answer is empty, so
// the callback must be cheap and idempotent: the common case is "the manager
// said the same thing as last time", and re-writing a file per beat would be
// the kind of work a 30-second timer should not do. See the edge command's
// implementation for how it avoids that.
func (a *Agent) SetModelAnswerFn(fn func(tunnel.HeartbeatResponse)) {
	a.mu.Lock()
	a.modelAnswerFn = fn
	a.mu.Unlock()
}

// SetAgentBridge wires the node's agent command surface.
//
// Safe to call after Run has started, though calling it before is better:
// handlers are installed by registerHandlers, so a bridge attached after
// that point serves agent.state and agent.health but not agent.prompt
// until the agent reconnects. A nil bridge disables the surface entirely,
// which is how a node opts out of running an agent.
func (a *Agent) SetAgentBridge(b *AgentBridge) {
	a.mu.Lock()
	a.agentBridge = b
	a.mu.Unlock()
}

// agentBridgeLocked returns the bridge, or nil. Callers already holding mu
// use this; the rest take the read lock themselves.
func (a *Agent) bridge() *AgentBridge {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.agentBridge
}

// NewAgent builds an Agent; applies defaults for zero-valued Config
// fields.
func NewAgent(client tunnel.Client, collector Collector, cfg Config, log *slog.Logger) *Agent {
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if cfg.TunnelStuckThreshold <= 0 {
		cfg.TunnelStuckThreshold = DefaultTunnelStuckThreshold
	}
	if cfg.MetricsInterval <= 0 {
		cfg.MetricsInterval = 10 * time.Second
	} else if cfg.MetricsInterval < MinMetricsInterval {
		orig := cfg.MetricsInterval
		cfg.MetricsInterval = MinMetricsInterval
		log.Warn("agent: MetricsInterval below the wire resolution; clamped",
			slog.Duration("configured", orig),
			slog.Duration("in_force", cfg.MetricsInterval),
			slog.String("reason", "host metric timestamps are whole seconds; a faster tick would be dropped as a duplicate"))
	}
	if cfg.MetricsBatchSize <= 0 {
		cfg.MetricsBatchSize = 30
	}
	if log == nil {
		log = slog.Default()
	}
	a := &Agent{
		client:           client,
		collector:        collector,
		cfg:              cfg,
		log:              log,
		upgradeRequested: make(chan struct{}, 1),
	}
	// The link is up: an Agent is only constructed after Dial returned, and
	// a node that believes it is offline before its first heartbeat would
	// start an outage clock nobody has declared.
	a.link.online = true
	if cfg.TelemetryWALDir != "" {
		wal, err := telemetrywal.Open(telemetrywal.Options{
			Dir:      cfg.TelemetryWALDir,
			Interval: cfg.TelemetryDrainInterval,
			Log:      log.With(slog.String("comp", "telemetrywal")),
		})
		if err != nil {
			// Loud, and then carry on. A node that refuses to start
			// because its log directory is not writable would take
			// metrics, the tunnel and every RPC with it, and the WAL
			// exists to stop losing data — not to become the most
			// fragile thing on the node. The consequence is exactly the
			// old behaviour plus this line, which is the trade a
			// misconfigured directory should get.
			log.Error("telemetry write-ahead log unavailable; falling back to direct push, and samples lost while the link is down are lost",
				slog.String("dir", cfg.TelemetryWALDir),
				slog.Any("err", err))
		} else {
			a.wal = wal
		}
	}
	return a
}

// New is retained for backwards compatibility with the Phase 1 wiring
// (no collector). Prefer NewAgent.
func New(client tunnel.Client, cfg Config, log *slog.Logger) *Agent {
	return NewAgent(client, noopCollector{}, cfg, log)
}

// EdgeID returns the cloud-assigned edge ID (0 until register_edge succeeds).
func (a *Agent) EdgeID() uint64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.edgeID
}

// Run drives the agent lifecycle: register handlers, dial, register_edge,
// and the two periodic loops. Returns nil on ctx cancel, a non-nil error
// only if an unrecoverable setup step fails (which there shouldn't be —
// Dial retries forever).
func (a *Agent) Run(ctx context.Context) error {
	// 1. Register cloud->edge handlers BEFORE Dial so they are primed
	//    when the end comes up.
	a.registerHandlers()

	// 2. Tunnel-layer reconnect hook: every time the tunnel rebuilds
	//    itself after a frontier broker route invalidation, re-issue
	//    register_edge so the new manager service-end binds the same
	//    canonical edge_id. The agent doesn't inspect RPC error patterns
	//    — that's the tunnel's job.
	a.client.OnReconnect(func() {
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.registerEdge(rctx); err != nil {
			a.log.Warn("agent: re-register after tunnel reconnect failed",
				slog.Any("err", err))
			return
		}
		a.log.Info("agent: re-registered after tunnel reconnect",
			slog.Uint64("edge_id", a.EdgeID()))
	})

	// 3. Dial (blocks until success or ctx cancel).
	if err := a.client.Dial(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("agent dial: %w", err)
	}

	// 4. register_edge.
	if err := a.registerEdge(ctx); err != nil {
		// Register failure is almost always an auth mismatch. Log and
		// continue — the periodic loops will keep trying because
		// tunnel-level reconnect is transparent.
		a.log.Warn("agent: register_edge failed; will keep running",
			slog.Any("err", err),
		)
	} else {
		// health marker — apply-pending-upgrade.sh reads this
		// on the NEXT boot to decide whether to roll back. Writing it
		// after a successful register_edge proves the new binary booted
		// AND the manager accepted us, which is the strongest signal we
		// have that the swap was healthy. Best-effort: if stage dir
		// isn't writable (dev/no-systemd boot), skip.
		a.writeHealthMarker()
	}

	// 4. Spawn ticker goroutines.
	eg, egCtx := errgroup.WithContext(ctx)
	eg.Go(func() error { return a.heartbeatLoop(egCtx) })
	eg.Go(func() error { return a.metricsLoop(egCtx) })

	// The telemetry drain. It is a sibling of the sampling loop rather
	// than part of it, because the two have opposite shapes: sampling is
	// paced by the collector and must not be delayed by the network,
	// while draining is paced by what the center can absorb. Folding them
	// together would mean every slow push also delayed the next sample,
	// which is how a node ends up sampling less often because the center
	// is busy.
	//
	// The link predicate is the same witness the autonomy arbiter reads,
	// so a node cannot believe it is disconnected for one subsystem and
	// connected for another.
	if a.wal != nil {
		eg.Go(func() error { return a.wal.Run(egCtx, a.link.isOnline, a.drainBatches) })
		// The log is closed on the way out rather than left to the
		// process: a deferred close is what makes the last rows durable
		// when a systemd stop lands mid-tick.
		defer func() { _ = a.wal.Close() }()
	}

	// Relay the agent's output to the manager. This returns immediately;
	// the relay is on its own goroutine inside the bridge. A node whose
	// agent is not up yet still serves agent.state and agent.health, so
	// there is nothing to report as an error here.
	//
	// Load-bearing: everything below this line — the changewatcher, the
	// upgrade sentinel, eg.Wait and therefore the graceful-shutdown
	// sequence — is unreachable if this call ever blocks. The node would
	// keep heartbeating and look healthy while never upgrading and never
	// shutting down cleanly. See AgentBridge.StartEvents, and the
	// regression test that pins the non-blocking contract.
	if b := a.bridge(); b != nil {
		b.StartEvents(egCtx)
	}

	// 5. 边缘 changewatcher (journald / dockerd / packagemgr) → TunnelSink → manager.
	// 启动失败仅 log warn, 不阻塞 agent (fail-soft: changewatcher 是 opt-in 能力).
	tunSink := changewatcher.NewTunnelSink(a.client, a.log, changewatcher.TunnelSinkSinkConfig{
		WALDir: a.cfg.ChangeEventWALDir,
	})
	watcher := changewatcher.New(tunSink, a.log)
	watcherStop := watcher.Start(egCtx)
	eg.Go(func() error { tunSink.Run(egCtx); return nil })
	// 进程退出前 Close 强制 flush 残余事件.
	defer func() {
		_ = tunSink.Close()
		// The event log's file handle is released on the same path, so a
		// systemd stop does not leave a node that has written its last
		// row and cannot read it back.
		_ = tunSink.CloseLog()
		watcherStop()
	}()
	// One extra goroutine watches the upgrade-staged signal — return a
	// sentinel error (NOT nil) so errgroup.WithContext cancels egCtx and
	// the heartbeat / metrics loops unwind. Returning nil leaves the
	// other goroutines running forever (their tickers never stop) and
	// eg.Wait() blocks indefinitely — discovered during E2E
	// where systemd never got the EXIT it needs to swap in the staged
	// bundle. We filter the sentinel back to nil in the Run return.
	eg.Go(func() error {
		select {
		case <-egCtx.Done():
			return nil
		case <-a.upgradeRequested:
			a.log.Info("agent: exiting cleanly for upgrade swap")
			return errUpgradeRequested
		}
	})

	err := eg.Wait()
	_ = a.client.Close()
	if errors.Is(err, errUpgradeRequested) {
		return nil
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// errUpgradeRequested is the sentinel returned from the upgrade-watch
// goroutine to cancel siblings via errgroup.WithContext. Run filters
// it back to nil so systemd treats the exit as clean and restarts.
var errUpgradeRequested = errors.New("upgrade requested")

// writeHealthMarker drops <stage>/healthy_marker with the agent version
// after a successful register_edge. apply-pending-upgrade.sh reads it
// on the next boot — if it matches last_upgrade_ver the upgrade is
// considered healthy; if missing OR mismatched the script rolls back
// to the .previous side. Best-effort: empty stage dir / unwritable
// path is non-fatal (dev runs without systemd).
func (a *Agent) writeHealthMarker() {
	dir := strings.TrimSpace(a.cfg.UpgradeStageDir)
	if dir == "" {
		return
	}
	ver := strings.TrimSpace(a.cfg.AgentVersion)
	if ver == "" {
		return
	}
	path := filepath.Join(dir, "healthy_marker")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		a.log.Debug("health marker: mkdir stage", slog.Any("err", err))
		return
	}
	if err := os.WriteFile(path, []byte(ver+"\n"), 0o640); err != nil {
		a.log.Debug("health marker: write", slog.Any("err", err))
		return
	}
	a.log.Info("health marker written", slog.String("path", path), slog.String("version", ver))
}

// registerHandlers installs the cloud->edge tool handlers backed by
// the collector. Delegates to service.Register via a thin adapter so
// we avoid a cyclic import (service imports tunnel, not biz).
//
// The legacy per-method handlers (get_host_load, get_process_list) are
// kept for backward compat; new capabilities go through the unified
// MethodExecuteSkill dispatcher (skill framework) — adding a new skill
// only requires writing one Executor file and registering it in init().
func (a *Agent) registerHandlers() {
	if b := a.bridge(); b != nil {
		b.Register(a.client)
	}
	// Plugin distribution. Registered before the rest of the node's own
	// handlers so a manager's first call after a node comes up finds the
	// plugin methods rather than a tunnel that has not heard of them —
	// which a caller cannot tell apart from a node that is refusing.
	a.registerPluginHandlers()
	a.client.RegisterHandler(tunnel.MethodGetHostLoad,
		func(ctx context.Context, _ tunnel.Session, _ string, _ []byte) ([]byte, error) {
			return jsonEncode(a.collector.GetHostLoad(ctx))
		})
	a.client.RegisterHandler(tunnel.MethodGetProcessList,
		func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
			var req tunnel.GetProcessListRequest
			if len(body) > 0 {
				if err := jsonDecode(body, &req); err != nil {
					return nil, err
				}
			}
			if req.TopN == 0 {
				req.TopN = 20
			}
			if req.SortBy == "" {
				req.SortBy = tunnel.ProcessSortByCPU
			}
			return jsonEncode(a.collector.GetProcessList(ctx, int(req.TopN), req.SortBy))
		})
	// Skill dispatcher: one handler routes every execute_skill RPC by
	// the skill key in the request body. The skill registry is populated
	// by init() blocks in core/floor/skill/builtin/* packages — the agent
	// just imports them transitively to trigger registration.
	a.client.RegisterHandler(tunnel.MethodExecuteSkill,
		func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
			return skilldispatch.Dispatch(ctx, body)
		})
	// Remote agent upgrade. Disabled when no stage dir is configured
	// (dev / non-systemd hosts) so the manager sees "method not found"
	// and the UI can render the button as disabled with a tooltip.
	if a.cfg.UpgradeStageDir != "" {
		a.client.RegisterHandler(tunnel.MethodAgentUpgrade,
			func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
				var req tunnel.AgentUpgradeRequest
				if err := jsonDecode(body, &req); err != nil {
					return nil, err
				}
				resp, err := a.handleAgentUpgrade(ctx, req)
				if err != nil {
					return nil, err
				}
				return jsonEncode(resp, nil)
			})
		// fetch_package: download + stage the full edge bundle.
		// No restart triggered here; manager calls apply_package below
		// when it's ready to flip the swap. Stage-and-apply are split so
		// the manager can stage all targets in a batch before applying.
		a.client.RegisterHandler(tunnel.MethodFetchPackage,
			func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
				var req tunnel.FetchPackageRequest
				if err := jsonDecode(body, &req); err != nil {
					return nil, err
				}
				resp, err := a.handleFetchPackage(ctx, req)
				if err != nil {
					return nil, err
				}
				return jsonEncode(resp, nil)
			})
		// apply_package: ack first, then signal Run() to exit
		// so systemd respawns and apply-pending-upgrade.sh swaps the
		// staged bundle in.
		a.client.RegisterHandler(tunnel.MethodApplyPackage,
			func(ctx context.Context, _ tunnel.Session, _ string, body []byte) ([]byte, error) {
				var req tunnel.ApplyPackageRequest
				if err := jsonDecode(body, &req); err != nil {
					return nil, err
				}
				resp, err := a.handleApplyPackage(ctx, req)
				if err != nil {
					return nil, err
				}
				return jsonEncode(resp, nil)
			})
	}
}

// registerEdge performs the initial handshake RPC and stores the EdgeID.
func (a *Agent) registerEdge(ctx context.Context) error {
	info, err := a.collector.HostInfo(ctx)
	if err != nil {
		a.log.Warn("agent: HostInfo collection failed", slog.Any("err", err))
	}
	req := tunnel.RegisterEdgeRequest{
		AccessKey:    "", // server-side AuthFunc matches by Meta, not body
		SecretKey:    "",
		HostInfo:     info,
		AgentVersion: a.cfg.AgentVersion,
	}
	var resp tunnel.RegisterEdgeResponse
	if err := a.client.Call(ctx, tunnel.MethodRegisterEdge, req, &resp); err != nil {
		return err
	}
	a.mu.Lock()
	a.edgeID = resp.EdgeID
	bridge := a.agentBridge
	a.mu.Unlock()
	// The manager assigns the id, and a reconnect can assign a different
	// one. Frames stamped with the previous id would land on a conversation
	// the manager believes belongs to some other node.
	if bridge != nil {
		bridge.SetEdgeID(resp.EdgeID)
	}
	a.log.Info("agent: registered with cloud",
		slog.Uint64("edge_id", resp.EdgeID),
		slog.Int64("server_time", resp.ServerTime),
	)
	return nil
}

// heartbeatLoop sends one heartbeat every HeartbeatInterval until ctx
// cancels. Errors are logged; transient ones (TCP/RPC blips) are
// recovered by the tunnel layer transparently. When heartbeats fail
// continuously for TunnelStuckThreshold ticks we treat the tunnel as
// stuck (geminio RetryEnd silently giving up on TLS handshake / frontier
// route never re-validating) and return errTunnelStuck so Agent.Run
// unwinds and systemd respawns the process with a clean dial.
func (a *Agent) heartbeatLoop(ctx context.Context) error {
	t := time.NewTicker(a.cfg.HeartbeatInterval)
	defer t.Stop()
	var consecutiveFail int
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			a.mu.RLock()
			healthFn := a.pluginHealthFn
			a.mu.RUnlock()
			var plugins []tunnel.PluginHealthWire
			if healthFn != nil {
				plugins = healthFn()
			}
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			var answer tunnel.HeartbeatResponse
			err := a.client.Call(rctx, tunnel.MethodHeartbeat,
				tunnel.HeartbeatRequest{
					EdgeID:  a.EdgeID(),
					Ts:      time.Now().Unix(),
					Plugins: plugins,
					// Read under the same lock the plugin snapshot is
					// read under, and for the same reason: cfg is
					// immutable after NewAgent in practice, but the
					// heartbeat goroutine must not depend on that.
					PigVersion: strings.TrimSpace(a.cfg.PigVersion),
				}, &answer)
			cancel()
			if err != nil {
				a.link.observe(false, time.Now())
				consecutiveFail++
				a.log.Warn("agent: heartbeat failed",
					slog.Int("consecutive_fail", consecutiveFail),
					slog.Any("err", err))
				if consecutiveFail >= a.cfg.TunnelStuckThreshold {
					a.log.Error("agent: tunnel stuck; exiting for systemd respawn",
						slog.Int("consecutive_fail", consecutiveFail))
					return errTunnelStuck
				}
				continue
			}
			a.link.observe(true, time.Now())
			consecutiveFail = 0
			// Read under the lock and only after the link is known good:
			// an answer from a failed call is the zero value, and acting
			// on it would clear a node's endpoint because of a dropped
			// packet. The callback decides precedence (env wins); this
			// loop only guarantees it sees a *successful* answer.
			a.mu.RLock()
			answerFn := a.modelAnswerFn
			a.mu.RUnlock()
			if answerFn != nil {
				answerFn(answer)
			}
		}
	}
}

// DefaultHeartbeatInterval is how often a node proves the link is alive.
const DefaultHeartbeatInterval = 30 * time.Second

// DefaultTunnelStuckThreshold = consecutive heartbeat failures before we
// declare the tunnel stuck and exit. With DefaultHeartbeatInterval=30s and
// threshold=5, the edge tolerates ~2.5min of network/manager wobble (TCP
// timeouts + 2 normal retries) before bailing. Tuned for "manager restart
// cycle completes within ~90s" vs "transient packet loss never lasts >60s".
const DefaultTunnelStuckThreshold = 5

// MinHeartbeatInterval is the floor on how fast a node may heartbeat.
//
// It exists for the same reason MinMetricsInterval does, and the two are
// deliberately not the same number: metrics are the node talking to a
// Prometheus at its own pace, while a heartbeat is the node asking the
// control plane to confirm it exists. Below a second that stops being a
// liveness check and becomes a load generator pointed at the manager, from
// every node in the fleet at once. Clamped rather than refused, for the
// reason MinMetricsInterval is: refusing would strand a node with a working
// link over a tuning knob, and the clamp is logged so the operator can see
// that the number they configured is not the number in force.
const MinHeartbeatInterval = time.Second

// ErrTunnelStuck is returned from heartbeatLoop when the configured number
// of consecutive heartbeats has failed. It is exported because a caller
// wrapping the node — a supervisor deciding between respawn and alert, or a
// test outside this package — has to be able to tell "the link never came
// back" from "the node was told to stop", and a string comparison is not a
// way to tell two failure modes that both arrive as a non-nil error.
var ErrTunnelStuck = errTunnelStuck

// errTunnelStuck is the sentinel returned from heartbeatLoop when N
// consecutive heartbeats failed. errgroup cancels siblings and Run
// returns this error so systemd (Restart=always) respawns the process.
var errTunnelStuck = errors.New("tunnel stuck: heartbeat failed N times")

// metricsLoop samples the collector every MetricsInterval and hands each
// result to the write-ahead log, which is what sends it.
//
// The order is the whole of the plan's 1.1. This used to push straight at
// the tunnel, and a push that failed lost the sample: the loop logged it
// and the next tick produced a fresh one. For a dashboard that is
// invisible — a missing point is indistinguishable from a quiet host. For
// an investigation it is not, because the question an operator asks at
// 09:00 is "what did this node look like at 03:00", and the machine that
// knew was the machine that could not reach anybody.
//
// The old comment here said that open-set samples are deliberately not
// buffered because "Prometheus remote_write expects timely delivery and
// stale samples are useless". Both halves of that are true and the
// conclusion drawn from them was wrong. Staleness is real, and the answer
// to it is a horizon — a row that is half an hour old is dropped because
// the store would refuse it anyway, not because every row should be
// dropped whenever the link is down. A node that loses a whole outage
// because it resolved staleness by discarding data is not being careful
// about staleness, it is discarding data.
//
// Sampling never blocks on the log. If the log cannot take a row, the row
// is lost and the loss is logged loudly: turning a telemetry problem into
// an availability problem is not a trade this loop is allowed to make.
func (a *Agent) metricsLoop(ctx context.Context) error {
	t := time.NewTicker(a.cfg.MetricsInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			outs, err := a.collector.CollectAll(ctx)
			if err != nil {
				a.log.Warn("agent: collect failed", slog.Any("err", err))
				// CollectAll may still return a partial slice on error.
			}
			for _, out := range outs {
				a.latest.observe(out.Samples)
				if a.wal == nil {
					// No log configured: the pre-1.1 path, push and
					// lose it on failure. Kept because a development
					// node with no data directory should still produce
					// metrics, not refuse to.
					if err := a.pushBatch(ctx, toBatch(out)); err != nil {
						a.log.Warn("agent: push failed; this sample is gone",
							slog.String("source", out.Source),
							slog.Any("err", err))
					}
					continue
				}
				if err := a.wal.Record(ctx, toBatch(out)); err != nil {
					a.log.Error("agent: telemetry log write failed; this sample is gone",
						slog.String("source", out.Source),
						slog.Any("err", err))
				}
			}
			if a.wal != nil {
				// The nudge is what keeps a healthy node's latency at one
				// loop turn rather than one drain interval. The interval
				// still bounds the rate; this only removes the wait.
				a.wal.Nudge()
			}
		}
	}
}

// toBatch converts a collection result into what the log stores.
//
// The stored shape is the shape the two push methods already take, because
// the drain has to re-issue the same calls the live path issues. A log that
// introduced its own envelope on the way back would need a new wire
// method, and a new wire method is something the manager has to be able to
// roll back.
func toBatch(out CollectorOutput) telemetrywal.Batch {
	b := telemetrywal.Batch{Source: out.Source, Samples: out.Samples}
	if out.HostPointValid {
		point := out.HostPoint
		b.HostPoint = &point
	}
	return b
}

// drainBatches is the write-ahead log's sender: it takes a batch of stored
// rows and puts them on the wire, reporting how many the center now has.
//
// The stop condition is what makes this safe to run forever. A transport
// failure stops the drain and leaves everything in place, because the next
// attempt will probably work. A *rejection* does not: the center has
// already said it will not take those samples, and retrying them at the
// drain interval would be a node asking the same question forever. So a
// rejected batch is counted, passed over, and the drain moves on — the
// alternative is a single permanently-refusable row wedging the queue and
// the node going quiet for good, which is the exact failure the log was
// installed to prevent.
func (a *Agent) drainBatches(ctx context.Context, batches []telemetrywal.Batch) (int, error) {
	for i, b := range batches {
		if err := a.pushBatch(ctx, b); err != nil {
			return i, err
		}
	}
	return len(batches), nil
}

// pushBatch emits one batch's two halves and reports how much of it the
// center now has for good.
//
// The report is an error when nothing landed, whatever the reason. A
// transport failure and a center that accepted none of the rows are the
// same instruction to the caller — leave the batch in the log and try
// again — and collapsing them here means drainBatches has one rule to
// apply instead of a taxonomy it would eventually get wrong. The
// distinction that *does* matter is handled below: a partial acceptance is
// not a reason to retry, because the center has already said it will not
// take the rest.
func (a *Agent) pushBatch(ctx context.Context, b telemetrywal.Batch) error {
	// 1) legacy fast path: push_host_metrics with one point, but only
	// for the selected host source. Component scrape targets should not
	// populate dashboard/alert fast-path rows.
	var (
		pointSent, pointAccepted   int
		sampleSent, sampleAccepted int
	)
	if b.HostPoint != nil {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		var resp tunnel.PushHostMetricsResponse
		err := a.client.Call(rctx, tunnel.MethodPushHostMetrics,
			tunnel.PushHostMetricsRequest{
				EdgeID: a.EdgeID(),
				Points: []tunnel.HostMetricPoint{*b.HostPoint},
			}, &resp)
		cancel()
		if err != nil {
			return err
		}
		pointSent, pointAccepted = 1, int(resp.Accepted)
	}

	// 2) open-set rich path: push_prom_samples
	if len(b.Samples) > 0 {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		var resp tunnel.PushPromSamplesResponse
		err := a.client.Call(rctx, tunnel.MethodPushPromSamples,
			tunnel.PushPromSamplesRequest{
				EdgeID:  a.EdgeID(),
				Source:  b.Source,
				Samples: b.Samples,
			}, &resp)
		cancel()
		if err != nil {
			return err
		}
		sampleSent, sampleAccepted = len(b.Samples), int(resp.Accepted)
	}

	// A batch is durable only when the center says it took all of it.
	//
	// "Accepted < sent" and "Accepted == 0" are two different sentences,
	// and the difference is whether a retry is worth anything:
	//
	//   - Accepted == 0 means the center placed none of it and is still
	//     waiting to be able to — register_edge has not landed, the host
	//     junction is missing, the prom ingester is not wired yet. Every
	//     one of those is a state a later attempt can find changed, so the
	//     honest report is "nothing landed", which this returns as an
	//     error. The pump then leaves the batch in the log and the next
	//     drain tries again. Without this branch the batch is acked and
	//     gone — which is exactly the silent loss the write-ahead log was
	//     installed to prevent, and it would be reintroduced by the very
	//     node that went to the trouble of logging first.
	//   - 0 < Accepted < sent means the center took part of it and will
	//     never take the rest: retrying is a node asking the same question
	//     forever. The un-taken part is counted as rejected (a metric, not
	//     a queue wedge) and the batch is allowed to move on.
	//
	// The center decides which sentence it is saying by what it returns;
	// this side only refuses to guess. It is deliberately all-or-nothing
	// on a partial ack for the same reason the audit log is: a batch that
	// was half stored has no clean retry, and guessing which half is worse
	// than either losing one sample or asking once more.
	if pointAccepted+sampleAccepted == 0 && pointSent+sampleSent > 0 {
		return fmt.Errorf("agent: the center accepted none of %d telemetry rows (point %d/%d, samples %d/%d); the batch stays in the log",
			pointSent+sampleSent, pointAccepted, pointSent, sampleAccepted, sampleSent)
	}
	if pointAccepted < pointSent {
		a.telemetryRejected.Add(uint64(pointSent - pointAccepted))
		a.log.Warn("agent: host metric point refused by the center; it will not be retried",
			slog.String("source", b.Source))
	}
	if sampleAccepted < sampleSent {
		a.telemetryRejected.Add(uint64(sampleSent - sampleAccepted))
		a.log.Warn("agent: samples refused by the center; the batch will not be retried",
			slog.String("source", b.Source),
			slog.Int("sent", sampleSent),
			slog.Int("accepted", sampleAccepted))
	}
	return nil
}

// noopCollector is used when the Phase 1 New() constructor is still in
// flight (cmd/opskeeper-edge hasn't been re-wired yet). It returns zero
// values and never errors.
type noopCollector struct{}

func (noopCollector) CollectAll(context.Context) ([]CollectorOutput, error) {
	return []CollectorOutput{{
		Source:         "noop",
		HostPoint:      tunnel.HostMetricPoint{Ts: time.Now().Unix()},
		HostPointValid: true,
	}}, nil
}
func (noopCollector) HostInfo(context.Context) (tunnel.HostInfo, error) {
	return tunnel.HostInfo{}, nil
}
func (noopCollector) GetHostLoad(context.Context) (tunnel.GetHostLoadResponse, error) {
	return tunnel.GetHostLoadResponse{SampledAt: time.Now().Unix()}, nil
}
func (noopCollector) GetProcessList(context.Context, int, string) (tunnel.GetProcessListResponse, error) {
	return tunnel.GetProcessListResponse{SampledAt: time.Now().Unix()}, nil
}

// Recovery path moved to the tunnel layer: tunnel.Client.Call detects
// broker route invalidation, redials transparently, and fires
// OnReconnect callbacks. Run() registers a callback that calls
// registerEdge so the agent stays out of the error-pattern matching
// business.
