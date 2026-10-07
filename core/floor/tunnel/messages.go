package tunnel

import (
	"encoding/json"
	"time"
)

// This file hand-mirrors api/tunnel/v1/tunnel.proto message shapes as Go
// structs with JSON tags. the tunnel body wire format is JSON
// in MVP; we deliberately avoid generating protobuf Go types for these
// payloads so core/floor/tunnel/ stays dependency-free (no protobuf
// import, no generated-code directory). When (if) we switch to protobuf
// binary in Phase 2, this file is the seam: swap types here, keep
// callers unchanged.
//
// Field names MUST stay in sync with tunnel.proto's json_name annotations.

// Method names used on the wire. Exposing them as constants keeps
// callers spell-safe.
const (
	MethodRegisterEdge     = "register_edge"
	MethodHeartbeat        = "heartbeat"
	MethodPushHostMetrics  = "push_host_metrics"
	MethodPushChangeEvents = "push_change_events"
	MethodPushPromSamples  = "push_prom_samples"
	MethodGetHostLoad      = "get_host_load"
	MethodGetProcessList   = "get_process_list"
	// MethodExecuteSkill is the single dispatcher RPC for the skill
	// framework. Edge agent registers one handler that looks up the
	// skill key in its local registry — no per-skill wire method.
	MethodExecuteSkill = "execute_skill"
	// MethodGetPluginConfigs is edge → manager: pull this edge's full
	// plugin config snapshot. Edge calls on startup, on receiving a
	// MethodPluginConfigsChanged push, and every 60s as safety net.
	MethodGetPluginConfigs = "get_plugin_configs"
	// MethodPluginConfigsChanged is manager → edge: notification that
	// the configs changed. Body is empty; edge re-fetches via
	// MethodGetPluginConfigs. (real-time push.)
	MethodPluginConfigsChanged = "plugin_configs_changed"
	// MethodWriteDatabaseMetricsSecret is manager → edge: write a
	// databasemetrics credential file on the edge host. The manager sends
	// this only during a user-initiated save; the normal plugin config
	// snapshot still carries only non-secret metadata.
	MethodWriteDatabaseMetricsSecret = "write_database_metrics_secret"

	// WebSSH (manager → edge): edge agent acts as an SSH client into
	// the host's local sshd. Each browser session is identified by a
	// uuid SessionID; multiple concurrent sessions per edge are fine.
	//   shell_open start session (SSH dial + Shell())
	//   shell_input one stdin chunk
	//   shell_resize update pty window size
	//   shell_close terminate session (manager-side close)
	// (edge → manager):
	//   shell_output one stdout/stderr chunk
	//   shell_exit terminal frame with exit code

	// MethodAgentUpgrade (manager → edge): swap the running edge binary
	// to the version at URL after verifying SHA256. Edge stages the new
	// binary in its own writable area, exits cleanly; systemd's
	// ExecStartPre script atomically swaps it into /usr/local/bin/.
	MethodAgentUpgrade = "agent_upgrade"

	// MethodFetchPackage (manager → edge,): fetch the whole edge
	// release bundle (agent + plugins + apply script) as a tarball,
	// verify outer SHA256, extract, verify each file from MANIFEST.txt.
	// Edge returns "staged" without restarting; the manager calls
	// MethodApplyPackage when it's ready to flip the swap.
	MethodFetchPackage = "fetch_package"

	// MethodApplyPackage (manager → edge,): signal the agent to
	// exit so systemd restarts it, at which point the ExecStartPre
	// apply-pending-upgrade.sh script swaps every staged file into its
	// declared dest. Edge ACKs first, then exits — the ACK is what
	// tells the manager "swap is happening now, watch for the new
	// agent_version on next register".
	MethodApplyPackage = "apply_package"
)

// ---------------------------------------------------------------------
// webssh
// ---------------------------------------------------------------------
//
// Decision 346 deleted this whole section — six methods and sixteen
// types. It is worth writing down why, because the shape of it is
// exactly the case a dead-code report cannot catch.
//
// The design these types describe was: **the edge holds the SSH
// password, dials SSH itself, owns a pty and a session map, and pushes
// stdout back to the manager** (ShellOpenRequest carried SSHPass, with
// a comment promising it was "wiped from edge memory after Dial"). It
// was never built on the edge — MethodShellOpen appears in exactly one
// commit, the initial publish — and it was then replaced. SSH now
// lives entirely on the manager (core/manager/server/webshell: ssh.New-
// ClientConn, PTY, Shell) and the edge is a one-screen TCP forwarder
// (core/edge/webshell: "the edge has no SSH client, no pty management,
// no session map"). The credential never crosses to the node, which is
// the whole reason the replacement is better.
//
// A dead-code report sees thirteen of the sixteen types as dead. The
// other three are unreachable *by its definition* — the manager
// registers handlers for `shell_output` and `shell_exit`, so the types
// look used. But **nothing on the edge has ever sent either message**:
// the edge cannot, because the thing that would produce stdout chunks
// (an SSH client) no longer exists there.
//
// That is the shape worth naming: **code that is reachable, has a
// registered receiver, and has no counterparty.** A report asks "can I
// walk to it"; this asks "who on the other end speaks it". Only the
// second question tells you whether it does anything.
//
// It was not harmless to leave registered. `DispatchOutput` writes
// bytes into a live operator terminal, keyed by a SessionID that
// arrives on the wire. A handler with no sender is a write primitive
// into someone's shell waiting for a caller, and the ledger had this
// family escalated to the operator twice as "删还是接线" — the answer
// turned out to be neither: the feature it belonged to shipped by
// another route, and this half was already superseded.

// GetPluginConfigsResponse is the wire snapshot served on
// MethodGetPluginConfigs. Mirrors biz/edge.WireSnapshot — duplicated
// here to keep core/floor/tunnel free of biz imports.
type GetPluginConfigsResponse struct {
	EdgeID  uint64                           `json:"edge_id"`
	Configs map[string]GetPluginConfigsEntry `json:"configs"`
}

// GetPluginConfigsEntry is one plugin's slice of the snapshot.
type GetPluginConfigsEntry struct {
	Enabled  bool                   `json:"enabled"`
	Endpoint string                 `json:"endpoint,omitempty"`
	Spec     map[string]interface{} `json:"spec,omitempty"`
}

// WriteDatabaseMetricsSecretRequest carries one edge-local credential file.
// Content is secret material; do not log it and do not persist it on the
// manager side.
type WriteDatabaseMetricsSecretRequest struct {
	SourceID         string                 `json:"source_id"`
	Path             string                 `json:"path"`
	Content          string                 `json:"content,omitempty"`
	DBType           string                 `json:"db_type,omitempty"`
	Credentials      map[string]interface{} `json:"credentials,omitempty"`
	PreservePassword bool                   `json:"preserve_password,omitempty"`
	Delete           bool                   `json:"delete,omitempty"`
}

// WriteDatabaseMetricsSecretsRequest batches edge-local credential writes so
// the edge can stage every secret before replacing any existing file.
type WriteDatabaseMetricsSecretsRequest struct {
	Secrets []WriteDatabaseMetricsSecretRequest `json:"secrets"`
}

// WriteDatabaseMetricsSecretResponse acknowledges that the edge wrote the
// requested credential file.
type WriteDatabaseMetricsSecretResponse struct {
	OK bool `json:"ok"`
}

// ExecuteSkillRequest is the cloud->edge skill invocation envelope.
// Key identifies the skill in the registry; Params is the JSON-encoded
// param object that the skill's Executor decodes.
type ExecuteSkillRequest struct {
	Key    string          `json:"key"`
	Params json.RawMessage `json:"params,omitempty"`
}

// ExecuteSkillResponse carries either the JSON result blob (on success)
// or an Error string. Manager surfaces Error verbatim to the caller.
type ExecuteSkillResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// ---------------------------------------------------------------------
// register_edge (edge -> cloud)
// ---------------------------------------------------------------------

// HostInfo is the static host description carried in RegisterEdgeRequest.
//
// Fingerprint is the per-host stable id (typically /etc/machine-id on
// Linux, IOPlatformUUID on macOS, MachineGuid on Windows). The cloud
// uses it to dedupe Device rows so an edge agent can be uninstalled
// and reinstalled without losing the host's identity. Empty string is
// allowed (older agents, or platforms where the id is unavailable);
// the cloud falls back to a hashed-hostname fingerprint in that case.
type HostInfo struct {
	Hostname      string `json:"hostname"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	KernelVersion string `json:"kernel_version"`
	CPUCount      int    `json:"cpu_count"`
	MemTotalBytes uint64 `json:"mem_total_bytes"`
	Fingerprint   string `json:"fingerprint,omitempty"`

	// HardwareFingerprint is a clone-resistant hardware identity computed
	// edge-side from physical-NIC MACs + CPU model + disk serials (see
	// core/edge/collector hardwareFingerprint). Unlike Fingerprint
	// (gopsutil HostID), it does NOT collapse cloned Linux VMs, which share a
	// single SMBIOS product_uuid (issue #96): a hypervisor regenerates the NIC
	// MAC per clone. The cloud prefers this when non-empty and falls back to
	// Fingerprint otherwise (older agents / hosts with no physical NIC). Both
	// fields are sent together so the cloud can migrate a device from its old
	// HostID-derived fingerprint to this one in place. Empty is allowed.
	HardwareFingerprint string `json:"hardware_fingerprint,omitempty"`

	// IPAddress is the primary IPv4 address of the host, collected
	// edge-side during register_edge. Used for display in the device
	// list/detail so operators can identify hosts by IP without
	// cross-referencing external tools. Empty string when the agent
	// cannot determine a suitable address (e.g. no non-loopback
	// interface found).
	IPAddress string `json:"ip_address,omitempty"`

	// OSVersion is the platform release below OS: "22.04" on Ubuntu,
	// "14.4.1" on macOS, the build number on Windows. It is a separate
	// column rather than part of OS because that is how the device table
	// stores it and because "linux" alone cannot answer "which kernel
	// bug is this host on".
	//
	// Added after the first agents shipped, so it is omitempty: an older
	// agent simply does not send it and the column stays empty. The
	// cloud must not require it (see the projection guard in
	// core/manager/biz/edge/hostinfo_projection_test.go, which fails if a
	// column is added here without a decision about where it lands).
	OSVersion string `json:"os_version,omitempty"`

	// DiskTotalBytes is the capacity of the root filesystem, as opposed
	// to the free/gauge percentages that arrive on the metric path. The
	// device list shows "used / total" and the total has to come from
	// somewhere: it used to come from here, which is how two device
	// columns ended up permanently zero while the API still returned
	// them.
	DiskTotalBytes uint64 `json:"disk_total_bytes,omitempty"`
}

// RegisterEdgeRequest is the first RPC the edge sends after connecting.
type RegisterEdgeRequest struct {
	AccessKey    string   `json:"access_key"`
	SecretKey    string   `json:"secret_key"`
	HostInfo     HostInfo `json:"host_info"`
	AgentVersion string   `json:"agent_version,omitempty"`
}

// RegisterEdgeResponse is what the cloud answers on successful register.
type RegisterEdgeResponse struct {
	EdgeID     uint64 `json:"edge_id"`
	ServerTime int64  `json:"server_time"` // unix seconds UTC
}

// ---------------------------------------------------------------------
// heartbeat (edge -> cloud)
// ---------------------------------------------------------------------

// HeartbeatRequest carries a client-side timestamp. Server compares its
// own clock to detect major skew. Plugins piggybacks the per-plugin
// runtime health so the manager / UI can see "agent is up but the logs
// plugin crashed (binary missing)" instead of silent empty telemetry.
type HeartbeatRequest struct {
	EdgeID      uint64             `json:"edge_id,omitempty"`
	Ts          int64              `json:"ts"` // unix seconds
	StatusFlags map[string]string  `json:"status_flags,omitempty"`
	Plugins     []PluginHealthWire `json:"plugins,omitempty"`
	// PigVersion is the PiG agent build this node hosts, already reduced
	// to the comparable form (the release line, with PiG's composite
	// "+upstream" suffix stripped). It rides the heartbeat rather than
	// register_edge for the same reason the plugin health does, and the
	// reason is that this value moves after the handshake: a node that
	// upgrades its agent, or that restarts onto a different binary, would
	// otherwise leave the control plane holding the version it had at
	// connect time forever. The heartbeat is already a periodic,
	// best-effort report of "what this node currently is", so the
	// version belongs on it by construction.
	//
	// It is the *node's own* answer — the same value its install-time
	// Review compares a package's min_pig_version against — rather than
	// whatever the running process last printed. That is deliberate: a
	// pre-flight that reported one version and a node that decided on
	// another is the exact disagreement the compatibility matrix exists
	// to make impossible.
	PigVersion string `json:"pig_version,omitempty"`
}

// PluginHealthWire is one plugin's runtime health on the heartbeat wire.
// Mirrors edgeagent/plugins.PluginHealth; the edge maps between the two so
// the plugin runtime stays decoupled from the tunnel protocol. State is one
// of stopped|starting|running|crashed. LastError is set when a plugin can't
// start (e.g. "subprocess binary missing") — that string is the whole point
// of this field: it turns a silent failure into an operator-visible reason.
type PluginHealthWire struct {
	Name         string                   `json:"name"`
	State        string                   `json:"state"`
	LastError    string                   `json:"last_error,omitempty"`
	RestartCount int                      `json:"restart_count,omitempty"`
	PID          int                      `json:"pid,omitempty"`
	StartedAt    int64                    `json:"started_at,omitempty"` // unix sec, 0 if never started
	UpdatedAt    int64                    `json:"updated_at,omitempty"` // unix sec
	Targets      []PluginTargetHealthWire `json:"targets,omitempty"`
}

// PluginTargetHealthWire is the source-level health carried inside a plugin
// heartbeat entry. Multi-target metric plugins use this for individual scrape
// targets / database sources.
type PluginTargetHealthWire struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	Kind          string `json:"kind,omitempty"`
	State         string `json:"state"`
	LastError     string `json:"last_error,omitempty"`
	Samples       int    `json:"samples,omitempty"`
	LastSuccessAt int64  `json:"last_success_at,omitempty"`
	UpdatedAt     int64  `json:"updated_at,omitempty"`
}

// HeartbeatResponse carries what the manager tells a node on each beat.
//
// It is no longer empty. The heartbeat is the one periodic, best-effort
// report a node makes about itself, so it is also the natural place for the
// manager to answer a question the node cannot answer alone — which model
// endpoint this deployment wants that node to use. The plan's 0.2 second
// sentence is exactly this: the node's model endpoint is named by the
// manager and reaches the node over the tunnel, rather than being written
// into every host's env by hand.
//
// What rides here is deliberately *not* a credential. The shape mirrors the
// plugin data plane (see pluginEndpointResolver in the manager's main):
// the manager names the destination, and the node presents its own existing
// tunnel credential pair to it. So the fields below are a URL and a model
// slug, both of which are safe in a log and safe on a wire that is already
// the node's authenticated channel. There is no token field and there will
// not be one: a second credential is a second thing to forget to revoke,
// and the node already has one the manager issues and checks.
//
// Contract for the edge side (mirrored in agentmodel.Adopt):
//   - the manager always sets AgentBaseURL to its own public gateway root,
//     so a node the operator never hand-provisioned still has a model;
//   - the node treats the manager's answer as *lower* priority than its own
//     environment, so an operator who pinned an endpoint by hand keeps it;
//   - the value takes effect without a restart because the agent re-reads
//     models.json per request — see agentmodel for why that is safe.
type HeartbeatResponse struct {
	// AgentBaseURL is the OpenAI-compatible *root* a node's agent should
	// talk to, e.g. https://opskeeper.example.com/v1 — the same string an
	// operator would put in OPSKEEPER_EDGE_AGENT_BASE_URL, suffix
	// included. Sending the fully-formed root rather than a bare origin
	// is what makes the env and the tunnel interchangeable, and that is
	// what lets "env wins over tunnel" be a one-line rule instead of a
	// comparison between two differently-shaped values.
	//
	// Empty means "this manager has no public URL configured" and is a
	// valid answer: the node then writes nothing and keeps whatever it
	// had, rather than being pointed at a relative path.
	AgentBaseURL string `json:"agent_base_url,omitempty"`
	// AgentModel is the model slug the cluster serves by default. Empty
	// lets the endpoint's own default stand, which is the same meaning it
	// has in the node's env contract.
	AgentModel string `json:"agent_model,omitempty"`
}

// ---------------------------------------------------------------------
// push_host_metrics (edge -> cloud)
// ---------------------------------------------------------------------

// HostMetricPoint is one sample in a metrics batch.
type HostMetricPoint struct {
	Ts          int64   `json:"ts"` // unix seconds
	CPUPct      float64 `json:"cpu_pct"`
	MemPct      float64 `json:"mem_pct"`
	Load1       float64 `json:"load1"`
	Load5       float64 `json:"load5"`
	Load15      float64 `json:"load15"`
	NetRxBps    uint64  `json:"net_rx_bps"`
	NetTxBps    uint64  `json:"net_tx_bps"`
	DiskUsedPct float64 `json:"disk_used_pct"`
}

// PushHostMetricsRequest pushes a batch of points (ring-buffered edge side).
type PushHostMetricsRequest struct {
	EdgeID uint64            `json:"edge_id,omitempty"`
	Points []HostMetricPoint `json:"points"`
}

// PushHostMetricsResponse reports how many points were accepted
// (after server-side dedup / rejection).
type PushHostMetricsResponse struct {
	Accepted uint32 `json:"accepted"`
}

// ---------------------------------------------------------------------
// push_prom_samples (edge -> cloud)
// ---------------------------------------------------------------------

// PromSample is one (metric_name, labels, value, ts) tuple, mirroring
// Prometheus's text-format/protobuf model. The cloud-side handler
// forwards these to Prometheus via remote_write.
type PromSample struct {
	Name   string            `json:"name"`             // e.g. "node_cpu_seconds_total"
	Labels map[string]string `json:"labels,omitempty"` // dimension labels (mode=, device=, ...)
	Value  float64           `json:"value"`
	TsMs   int64             `json:"ts_ms"` // unix milliseconds
}

// PushPromSamplesRequest is one push of open-set samples. Source identifies
// the producer: "embedded" for in-process collection, or "scrape:<target_name>"
// for an HTTP-scraped target.
type PushPromSamplesRequest struct {
	EdgeID  uint64       `json:"edge_id,omitempty"`
	Source  string       `json:"source"`
	Samples []PromSample `json:"samples"`
}

// PushPromSamplesResponse reports how many samples the cloud accepted.
type PushPromSamplesResponse struct {
	Accepted int `json:"accepted"`
}

// ---------------------------------------------------------------------
// get_host_load (cloud -> edge)
// ---------------------------------------------------------------------

// GetHostLoadRequest has no fields; kept typed for symmetry.
type GetHostLoadRequest struct{}

// GetHostLoadResponse is a real-time load snapshot.
type GetHostLoadResponse struct {
	CPUPct float64 `json:"cpu_pct"`
	MemPct float64 `json:"mem_pct"`
	// DiskUsedPct is the root-filesystem usage percent (0..100).
	// Sourced from HostMetricPoint.DiskUsedPct (gopsutil disk usage on /
	// for embedded collector; node_filesystem_*_bytes for the scrape-mode
	// collector). Added to plug a real-world LLM mis-read where models
	// answered "disk usage = mem_pct" because no disk field was present
	// here — see session a16dec3d-1a3f-40b6-8fed-553b7b6cb9b9.
	DiskUsedPct float64 `json:"disk_used_pct"`
	Load1       float64 `json:"load1"`
	Load5       float64 `json:"load5"`
	Load15      float64 `json:"load15"`
	SampledAt   int64   `json:"sampled_at"` // unix seconds
}

// ---------------------------------------------------------------------
// get_process_list (cloud -> edge)
// ---------------------------------------------------------------------

// ProcessSortBy mirrors the proto enum names (string on wire).
const (
	ProcessSortByCPU = "cpu"
	ProcessSortByMem = "mem"
)

// ProcessInfo is one row in the top-N processes result.
type ProcessInfo struct {
	PID     int32   `json:"pid"`
	Name    string  `json:"name"`
	Cmdline string  `json:"cmdline"`
	CPUPct  float64 `json:"cpu_pct"`
	MemPct  float64 `json:"mem_pct"`
	User    string  `json:"user"`
}

// GetProcessListRequest asks for the top N processes sorted by cpu/mem.
type GetProcessListRequest struct {
	TopN   uint32 `json:"top_n"`
	SortBy string `json:"sort_by"` // "cpu" | "mem"
}

// GetProcessListResponse is the top-N result.
type GetProcessListResponse struct {
	Processes []ProcessInfo `json:"processes"`
	SampledAt int64         `json:"sampled_at"`
}

// ---------------------------------------------------------------------
// agent upgrade (manager -> edge)
// ---------------------------------------------------------------------

// AgentUpgradeRequest tells the edge agent to swap its own binary to
// the artifact at URL. The agent stream-downloads to a private staging
// area, verifies the artifact's sha256, atomically renames it to
// `/var/lib/opskeeper-edge/.upgrade/pending`, then exits cleanly.
//
// On the next process start (driven by systemd Restart=always), the
// unit's ExecStartPre script (running as root) renames the staged
// binary over `/usr/local/bin/opskeeper-edge`, backs up the previous
// binary to `.upgrade/previous`, and lets ExecStart run the new code.
//
// SHA256 is required and lower-hex (64 chars). URL is fetched without
// authentication today — the artifacts live behind nginx on the same
// manager the agent already trusts; revisit if we ever expose a CDN.
type AgentUpgradeRequest struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// AgentUpgradeResponse acks that the new binary is staged. The edge
// returns this BEFORE exiting so the manager sees a clean response;
// the actual restart is implicit (Restart=always picks the agent back
// up within ~5 s). Bytes is informational, useful for logs.
type AgentUpgradeResponse struct {
	StagedPath string `json:"staged_path"`
	Bytes      int64  `json:"bytes"`
}

// FetchPackageRequest tells the edge to download the full
// release tarball at URL, verify its outer SHA256, extract, then
// per-file sha-check every entry listed in the bundle's MANIFEST.txt.
// On success the edge has the new bundle fully staged under
// /var/lib/opskeeper-edge/.upgrade/incoming/ but the swap hasn't happened
// yet — that's done by MethodApplyPackage so the manager can stagger
// stage and apply (e.g. stage all edges, then apply when the user
// clicks "go").
type FetchPackageRequest struct {
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`            // sha of the tarball
	Version string `json:"version,omitempty"` // optional; used by manifest VERSION file + ack
}

// FetchPackageResponse acks staging. ManifestFiles is the count of
// files the manifest declared; useful for the manager to surface
// "staged 6 files" in the UI without round-tripping again.
type FetchPackageResponse struct {
	StagedPath    string `json:"staged_path"`    // /var/lib/opskeeper-edge/.upgrade/incoming
	Bytes         int64  `json:"bytes"`          // size of the tarball
	ManifestFiles int    `json:"manifest_files"` // entries in MANIFEST.txt
	Version       string `json:"version,omitempty"`
}

// ApplyPackageRequest signals the edge to exit so systemd's
// ExecStartPre apply-pending-upgrade.sh swaps the staged bundle in.
// Empty body — the staged bundle is the implicit target.
type ApplyPackageRequest struct{}

// ApplyPackageResponse acks that the edge has accepted the apply
// signal and will exit shortly. Apply is fire-and-forget from the
// manager's POV; the eventual outcome is observed via the new
// agent_version reported on next register (and the next-tick rollback
// if anything's broken).
type ApplyPackageResponse struct {
	Accepted bool `json:"accepted"`
}

// ---------------------------------------------------------------------
// meta (handshake blob, edge -> cloud only; never an RPC body)
// ---------------------------------------------------------------------

// Meta is what the client serializes into geminio's opaque Meta bytes
// on connect; the server decodes it before calling AuthFunc.
type Meta struct {
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// ---------------------------------------------------------------------
// push_change_events (edge -> cloud)
//
// Edge changewatcher (journald / dockerd / packagemgr) emits normalized
// ChangeEvent values. The TunnelSink on the edge buffers them and
// flushes via this method in batches of <= 100. The manager side
// persists into edge_change_events and surfaces them through
// query_change_events (HLD-013 Phase 3 RCA).
// ---------------------------------------------------------------------

// ChangeEventWire mirrors changewatcher.ChangeEvent. The fields are
// kept as plain strings (not the strong types) so the protocol stays
// stable even if the edge-side enum evolves — the manager validates
// at decode time.
type ChangeEventWire struct {
	Source    string            `json:"source"`  // journald / dockerd / packagemgr
	Kind      string            `json:"kind"`    // ssh_login / sudo_use / service_restart / container_start / package_install / ...
	Subject   string            `json:"subject"` // user / container / package name
	Action    string            `json:"action"`  // login / failed / install / upgrade / start / ...
	Timestamp time.Time         `json:"timestamp"`
	Severity  string            `json:"severity"` // info / notice / warn
	Labels    map[string]string `json:"labels,omitempty"`

	// Seq is the write-ahead log's row number for this event, which is the
	// only thing that can tell a replay from a new arrival. A change event
	// has no natural key: two genuine systemd restarts of the same unit in
	// the same second can agree on every other field, so deduping on
	// content would delete real events rather than duplicates.
	//
	// 0 means "this node sent the event without logging it first" — a node
	// with no WAL, or one whose write failed. The center stores those
	// without deduplicating, because it cannot. It is safe as a sentinel
	// because the log's sequence starts at 1.
	Seq uint64 `json:"seq,omitempty"`
}

// PushChangeEventsRequest is one batched push from the edge.
type PushChangeEventsRequest struct {
	EdgeID uint64            `json:"edge_id,omitempty"`
	Events []ChangeEventWire `json:"events"`
}

// PushChangeEventsResponse reports how many events were accepted.
// Rejected counts events the server refused (malformed / over limit).
type PushChangeEventsResponse struct {
	Accepted uint32 `json:"accepted"`
	Rejected uint32 `json:"rejected"`
}
