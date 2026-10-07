package frontierbound

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// PromwriteIngester is the narrow surface the push_prom_samples handler
// needs from core/manager/biz/promwrite. Declared here as an interface
// so this package does not import the biz package directly (MetricIngester
// below does the same thing one level down, by holding a port rather than a
// domain type). A nil value means Prom is disabled - the
// handler still installs but silently 200s so edges back off cleanly.
//
// Post-split (May 2026): the deviceID arg is the host device id resolved
// from the tunnel-side edge_id via the edge_devices(type=host) junction.
// The pre-launch backfill keeps the values numerically equal so naive
// callers that pass edge_id continue to work; new code should resolve
// through DeviceResolver below for correctness.
type PromwriteIngester interface {
	Push(ctx context.Context, deviceID uint64, source string, samples []tunnel.PromSample) error
}

// DeviceResolver resolves a tunnel-side edge_id to its host device_id
// via the edge_devices(type=host) junction. Optional in Wiring; nil
// falls back to the legacy "edge_id == device_id" assumption (true for
// pre-launch data thanks to the migration's integer reuse).
type DeviceResolver interface {
	LookupHostDevice(ctx context.Context, edgeID uint64) (uint64, error)
}

// Wiring is the set of biz dependencies the manager-side handlers need.
// It is supplied by cmd/opskeeper/main.go and consumed by Install.
type Wiring struct {
	EdgeAuthn      EdgeAuthenticator
	EdgeUC         EdgeLifecycle
	MetricIngester tunnel.HostMetricIngest
	// PromIngester is optional - nil means Prom is disabled. When nil the
	// push_prom_samples handler still installs but silently accepts and
	// drops every batch so edges (which don't know the cloud's Prom state)
	// don't churn on errors.
	PromIngester PromwriteIngester
	// PluginConfigUC is optional. When non-nil, Install registers
	// MethodGetPluginConfigs so edges can pull their plugin config
	// snapshot via tunnel.
	PluginConfigUC PluginConfigFetcher
	// ChangeEventUC receives batches of edge change events
	// (journald / dockerd / packagemgr). Optional - when nil, the
	// push_change_events handler does not install (A.3 disabled).
	ChangeEventUC ChangeEventIngestor
	// DeviceResolver, when non-nil, is consulted on every push to map
	// the tunnel session's edge_id to the host device_id used as the
	// metric/log/trace label. nil falls back to edge_id == device_id
	// (correct for pre-launch data; explicitly resolving here is the
	// future-proof path for multi-agent hosts).
	DeviceResolver DeviceResolver
	// AgentEvents routes the frames a node's agent pushes back to the
	// console that asked for them. Optional - when nil, agent.event does
	// not install and node agents run, they simply have no way to deliver
	// a turn. Their commands (prompt/steer/abort) still work, so this is
	// a degraded conversation rather than a broken node.
	AgentEvents AgentEventRouter
	// AgentTools runs a control-plane tool on behalf of a node's agent.
	// Optional - when nil, agent.tool does not install and the node's
	// topology and alert queries fail with a plain "not available", while
	// its host-local probes keep working. A node whose agent cannot reach
	// the control plane is degraded, not broken, and the half of the
	// toolset that needs no round trip should not go down with the half
	// that does.
	AgentTools AgentToolRunner
	// AutonomyReplay receives the audit rows a node wrote while it was
	// disconnected and re-ran on its own, so the control plane's chain
	// records what the fleet did during the outage.
	//
	// Optional, and nil is the state of every fleet that runs today's
	// packages: a node whose manifests declare no autonomy never produces
	// a row. When it is nil the handler does not install and the node's
	// pump keeps the rows on disk, which loses nothing and is visible on
	// the node's health line.
	AutonomyReplay AutonomyReplayRecorder
	// NodeLedger receives the rows a node wrote about its own tool calls,
	// its agent turns and its plugin installs. Unlike AutonomyReplay this
	// is not optional in practice: every node that runs a gated tool has
	// something to say, and a fleet whose nodes all record nothing is the
	// state decision 126 was written about.
	//
	// When it is nil the handler does not install, and a node's pump keeps
	// its rows and counts the method as unanswered — visible on the
	// health line rather than silent.
	NodeLedger NodeLedgerRecorder
	// ModelEndpoint names the model endpoint a node's agent should use. It
	// is the manager's half of the plan's 0.2: the destination and the
	// model slug are named here rather than written into every host's env
	// by hand, and they reach the node on the heartbeat the node already
	// sends.
	//
	// Optional. When nil the heartbeat carries no endpoint, which is the
	// correct answer for a manager with no public URL configured: a node
	// keeps whatever it had rather than being pointed at a relative path.
	ModelEndpoint ModelEndpointResolver
	// ClusterLink answers a child cluster's cluster.hello, which is how a
	// cluster this root has provisioned gets a binding to the caller that
	// proved it holds the provisioning token.
	//
	// Optional. When nil the method does not install and a child cluster's
	// hello is never answered, which it reads as a root that will not
	// speak to it — the same thing it would read from a root that was
	// never told about it.
	ClusterLink ClusterHelloHandler
	Log         *slog.Logger
}

// ClusterHelloHandler answers tunnel.MethodClusterHello.
//
// It is declared here, in the package that dispatches, rather than beside the
// implementation, so that the set of methods a dialer may reach is decided in
// one place — and so that adding a second answer to the cluster channel shows
// up as a change to this file rather than as something hidden in a domain
// three directories away.
type ClusterHelloHandler interface {
	// HandleHello is handed the authenticated caller id and the raw body.
	// That id is the transport's opaque number, already authenticated by
	// GetEdgeID; it is not an edge id, and nothing downstream should
	// treat it as one.
	HandleHello(ctx context.Context, edgeID uint64, body []byte) ([]byte, error)
}

// ModelEndpointResolver answers "which model endpoint should a node's agent
// use" once per heartbeat.
//
// It returns a base URL and a model slug and nothing else. There is
// deliberately no credential in the return: the gateway authenticates a
// node's existing tunnel credential pair, so the only secret on this path is
// the one the node already holds. A token field here would be a second
// credential to rotate, and the plan's own acceptance criterion — no cloud
// provider key on the node — is satisfied without it.
//
// The error-free signature is deliberate. A resolver that cannot name an
// endpoint should answer with empty strings, exactly as a manager with no
// public URL does: a heartbeat is a best-effort liveness report, and
// failing the whole beat over a model-catalogue read would turn a
// configuration gap into a fleet-wide "node offline".
type ModelEndpointResolver interface {
	// AgentEndpoint returns the OpenAI-compatible root (suffix included,
	// e.g. https://host/v1) and the default model slug. Either may be
	// empty.
	AgentEndpoint(ctx context.Context) (baseURL, model string)
}

// AutonomyReplayRecorder turns a batch of a node's self-heal rows into
// entries in the control plane's audit chain.
//
// It returns how many rows it took and how many it refused, plus an error
// for the case the node must retry. The three outcomes map onto three
// different things the node should do, and collapsing them is how a
// backlog is lost:
//
//   - err != nil: the chain could not be written (database down, chain
//     contention). The node keeps the whole batch and asks again.
//   - rejected > 0: rows the chain will never take because their *shape*
//     is wrong — a missing action, an unknown phase. Retrying is a node
//     asking the same question forever, so these are counted and passed.
//   - accepted: rows now in the chain for good. The node acks these.
type AutonomyReplayRecorder interface {
	RecordAutonomyReplay(ctx context.Context, edgeID uint64, rows []tunnel.AutonomyAuditRow) (accepted, rejected int, err error)
}

// NodeLedgerRecorder appends a node's own ledger rows to the chain. Same
// three-answer contract as AutonomyReplayRecorder, and for the same reason:
// both are writes to one ordered chain with no dedupe key, so "took none of
// it" and "refused all of it" must stay distinguishable all the way to the
// node, which is the only place that can act on the difference.
type NodeLedgerRecorder interface {
	RecordNodeEntries(ctx context.Context, edgeID uint64, rows []tunnel.AuditEntry) (accepted, rejected int, err error)
}

// AgentToolRunner runs one control-plane tool for a node's agent.
//
// The session travels with the call so the runner can check it, but the
// runner does not have to trust it: edgeID comes from the authenticated
// transport, and a session that does not belong to that edge is not that
// edge's to act for.
type AgentToolRunner interface {
	// RunAgentTool returns the tool's JSON output, or an error written for
	// the model that asked. A refusal and a failure are deliberately the
	// same return, because the agent turns either into text and a model
	// that can tell them apart retries the refusal.
	RunAgentTool(ctx context.Context, edgeID uint64, sessionID, tool string, args json.RawMessage) (json.RawMessage, error)
}

// AgentEventRouter is the narrow surface the agent.event push needs.
// Declared here so frontierbound does not import the fleet: the transport
// layer hands a frame to whatever owns conversation routing, and the two
// are free to move apart without a cycle.
type AgentEventRouter interface {
	// DeliverInbound routes one pushed frame to the conversation it names
	// and reports whether a conversation took it. A false is the ordinary
	// outcome of a frame for a conversation that has since closed, and is
	// never an error - see nodefleet.Fleet.DeliverInbound.
	DeliverInbound(tunnel.AgentEventFrame) bool
}

// the edge biz PluginConfigUC. *edgebiz.PluginConfigUC satisfies it.
type PluginConfigFetcher interface {
	FetchForEdge(ctx context.Context, edgeID uint64) (*domain.PluginConfigSnapshot, error)
}

// Install registers all manager-side reverse-call handlers and the three
// lifecycle callbacks (GetEdgeID, EdgeOnline, EdgeOffline) on the client.
//
// Method names match the constants in core/floor/tunnel/messages.go;
// edges send those exact strings on the wire. Payloads are JSON in the
// shapes declared in that same file.
func Install(ctx context.Context, c *Client, w Wiring) error {
	log := w.Log
	if log == nil {
		log = slog.Default()
	}

	// Disabled client (NewDisabled): nothing to register against; report
	// success so main.go's bring-up sequence can continue to the HTTP
	// server. Edge-facing reverse calls won't ever fire, but that is the
	// whole point of the e2e harness path.
	if c.svc == nil {
		log.Info("frontierbound: Install skipped - client is disabled")
		return nil
	}

	if w.EdgeAuthn == nil {
		return fmt.Errorf("frontierbound: Install: EdgeAuthn is required")
	}
	if w.EdgeUC == nil {
		return fmt.Errorf("frontierbound: Install: EdgeUC is required")
	}
	if w.MetricIngester == nil {
		return fmt.Errorf("frontierbound: Install: MetricIngester is required")
	}

	resolveEdgeID := func(meta []byte) (uint64, error) {
		var m tunnel.Meta
		if err := json.Unmarshal(meta, &m); err != nil {
			log.Warn("frontierbound: GetEdgeID: bad meta", slog.Any("err", err))
			return 0, fmt.Errorf("bad meta: %w", err)
		}
		sess, err := w.EdgeAuthn.Authenticate(ctx, m.AccessKey, m.SecretKey)
		if err != nil {
			// AccessKeyAuthenticator already collapses all failure paths
			// to errs.ErrUnauthorized so we don't leak enumeration here.
			log.Debug("frontierbound: GetEdgeID: authn failed",
				slog.String("access_key", m.AccessKey),
				slog.Any("err", err),
			)
			return 0, err
		}
		return sess.EdgeID, nil
	}

	// Lifecycle: GetEdgeID parses the edge's Meta JSON, runs access-key
	// authentication, and returns the resolved EdgeID. Any failure path
	// returns 0 + error so frontier rejects the dial - the manager never
	// allocates anonymous IDs.
	if err := c.RegisterGetEdgeID(ctx, resolveEdgeID); err != nil {
		return fmt.Errorf("frontierbound: register GetEdgeID: %w", err)
	}

	if err := c.RegisterEdgeOnline(ctx, func(edgeID uint64, meta []byte, addr net.Addr) error {
		canonicalEdgeID, err := resolveEdgeID(meta)
		if err == nil {
			c.bindEdgeTransport(edgeID, canonicalEdgeID)
		}
		log.Info("frontierbound: edge online",
			slog.Uint64("edge_id", canonicalEdgeID),
			slog.Uint64("transport_edge_id", edgeID),
			slog.String("addr", safeAddr(addr)),
		)
		if err != nil {
			return err
		}
		// Real-time edge_offline alerting was removed in
		// The metric_raw rule on edge_last_seen_seconds_ago auto-resolves
		// once PipelineEvaluator's next tick refreshes the gauge to 0.
		return nil
	}); err != nil {
		return fmt.Errorf("frontierbound: register EdgeOnline: %w", err)
	}

	if err := c.RegisterEdgeOffline(ctx, func(edgeID uint64, _ []byte, addr net.Addr) error {
		// Translate the frontier transport id to the canonical Edge.ID
		// before unbinding - we need the canonical id for the alert
		// notifier and the unbind clears the mapping.
		canonicalEdgeID := c.canonicalizeEdgeID(edgeID)
		c.unbindTransport(edgeID)
		// Subscribers get the transport id, not the canonical one. A
		// cluster binding is keyed by the caller that proved its token,
		// and that is the number the broker recycles; handing them a
		// canonical edge id would make Forget miss every time the broker
		// had not yet canonicalised the dial.
		c.notifyEdgeOffline(edgeID)
		log.Info("frontierbound: edge offline",
			slog.Uint64("edge_id", canonicalEdgeID),
			slog.Uint64("transport_edge_id", edgeID),
			slog.String("addr", safeAddr(addr)),
		)
		// Persist status=offline so the UI / list endpoints stop showing
		// this edge as online once the tunnel closes. Without this the
		// edges row sticks at status=online with a stale last_seen_at
		// until the next ticker / re-handshake cleans it up.
		if w.EdgeUC != nil && canonicalEdgeID != 0 {
			if err := w.EdgeUC.HandleOffline(ctx, canonicalEdgeID, time.Now().UTC()); err != nil {
				log.Warn("frontierbound: handle offline failed",
					slog.Uint64("edge_id", canonicalEdgeID),
					slog.Any("err", err))
			}
		}
		// Real-time edge_offline alerting was removed in
		// PipelineEvaluator's metric_raw rule on edge_last_seen_seconds_ago
		// fires within one ticker interval (default 30s).
		return nil
	}); err != nil {
		return fmt.Errorf("frontierbound: register EdgeOffline: %w", err)
	}

	// register_edge: persist HostInfo + flip status=online.
	// cluster.hello: a child cluster introducing itself. Registered here
	// rather than in the link's own package so that the reachable method
	// set is decided in one place.
	if w.ClusterLink != nil {
		if err := c.Register(ctx, tunnel.MethodClusterHello, w.ClusterLink.HandleHello); err != nil {
			return fmt.Errorf("frontierbound: register %q: %w", tunnel.MethodClusterHello, err)
		}
	}

	if err := c.Register(ctx, tunnel.MethodRegisterEdge, func(rpcCtx context.Context, edgeID uint64, body []byte) ([]byte, error) {
		var in tunnel.RegisterEdgeRequest
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("register_edge: decode: %w", err)
		}
		canonicalEdgeID := c.canonicalizeEdgeID(edgeID)
		if canonicalEdgeID == 0 {
			// Transport→canonical binding not established yet (EdgeOnline's
			// resolveEdgeID must land first). Registering with 0 makes
			// HandleRegister(0) fail AND leaves the edge_devices host
			// junction uncreated - after which every metric falls back to
			// edge_id (issue #96 root cause). Fail loudly so the edge
			// retries once the binding is ready, instead of silently
			// mis-registering and poisoning the device_id labels.
			log.Error("frontierbound: register_edge - no canonical edge id (transport binding not ready, retry)",
				slog.Uint64("transport_edge_id", edgeID))
			return nil, fmt.Errorf("register_edge: edge binding not ready")
		}
		if err := w.EdgeUC.HandleRegister(rpcCtx, canonicalEdgeID, in.HostInfo, in.AgentVersion); err != nil {
			log.Error("frontierbound: HandleRegister",
				slog.Uint64("edge_id", canonicalEdgeID),
				slog.Uint64("transport_edge_id", edgeID),
				slog.Any("err", err),
			)
			return nil, fmt.Errorf("register_edge: %w", err)
		}
		c.bindEdgeTransport(edgeID, canonicalEdgeID)
		out := tunnel.RegisterEdgeResponse{
			EdgeID:     canonicalEdgeID,
			ServerTime: time.Now().UTC().Unix(),
		}
		return json.Marshal(out)
	}); err != nil {
		return fmt.Errorf("frontierbound: register %q: %w", tunnel.MethodRegisterEdge, err)
	}

	// heartbeat: bump last_seen_at.
	if err := c.Register(ctx, tunnel.MethodHeartbeat, func(rpcCtx context.Context, edgeID uint64, body []byte) ([]byte, error) {
		var in tunnel.HeartbeatRequest
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("heartbeat: decode: %w", err)
		}
		canonicalEdgeID := c.canonicalizeEdgeID(edgeID)
		if in.EdgeID != 0 {
			canonicalEdgeID = in.EdgeID
			c.bindEdgeTransport(edgeID, canonicalEdgeID)
		}
		ts := time.Unix(in.Ts, 0).UTC()
		if in.Ts == 0 {
			ts = time.Now().UTC()
		}
		if err := w.EdgeUC.HandleHeartbeat(rpcCtx, canonicalEdgeID, ts, in.PigVersion); err != nil {
			log.Warn("frontierbound: HandleHeartbeat",
				slog.Uint64("edge_id", canonicalEdgeID),
				slog.Uint64("transport_edge_id", edgeID),
				slog.Any("err", err),
			)
			return nil, fmt.Errorf("heartbeat: %w", err)
		}
		// Piggybacked plugin health (best-effort, in-memory only). Lets the
		// UI show "logs: crashed - binary missing" instead of silent empty
		// telemetry. Never fail the heartbeat on this.
		if len(in.Plugins) > 0 {
			items := make([]domain.PluginHealth, 0, len(in.Plugins))
			for _, p := range in.Plugins {
				targets := make([]domain.PluginTargetHealth, 0, len(p.Targets))
				for _, t := range p.Targets {
					targets = append(targets, domain.PluginTargetHealth{
						ID:            t.ID,
						Name:          t.Name,
						Kind:          t.Kind,
						State:         t.State,
						LastError:     t.LastError,
						Samples:       t.Samples,
						LastSuccessAt: unixOrZero(t.LastSuccessAt),
						UpdatedAt:     unixOrZero(t.UpdatedAt),
					})
				}
				items = append(items, domain.PluginHealth{
					Name:         p.Name,
					State:        p.State,
					LastError:    p.LastError,
					RestartCount: p.RestartCount,
					PID:          p.PID,
					StartedAt:    unixOrZero(p.StartedAt),
					UpdatedAt:    unixOrZero(p.UpdatedAt),
					Targets:      targets,
				})
			}
			w.EdgeUC.RecordPluginHealth(canonicalEdgeID, items)
		}
		// The manager's answer, named here so the node does not have to
		// be hand-provisioned per host. Non-secret by construction — a
		// URL and a model slug — because the node sends its own tunnel
		// credential to the gateway. See ModelEndpointResolver.
		var out tunnel.HeartbeatResponse
		if w.ModelEndpoint != nil {
			out.AgentBaseURL, out.AgentModel = w.ModelEndpoint.AgentEndpoint(rpcCtx)
		}
		return json.Marshal(out)
	}); err != nil {
		return fmt.Errorf("frontierbound: register %q: %w", tunnel.MethodHeartbeat, err)
	}

	// push_host_metrics: forward batches to the ingester.
	if err := c.Register(ctx, tunnel.MethodPushHostMetrics, func(rpcCtx context.Context, edgeID uint64, body []byte) ([]byte, error) {
		var in tunnel.PushHostMetricsRequest
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("push_host_metrics: decode: %w", err)
		}
		canonicalEdgeID := c.canonicalizeEdgeID(edgeID)
		if in.EdgeID != 0 {
			canonicalEdgeID = in.EdgeID
			c.bindEdgeTransport(edgeID, canonicalEdgeID)
		}
		if canonicalEdgeID == 0 {
			// Edge hasn't completed register_edge yet (race on first
			// connect). Drop for now; Accepted=0 is what tells the edge
			// this batch is still its responsibility. Letting transport
			// ID through would create ghost edge_id labels in Prom
			// (v0.7.39 fix).
			return json.Marshal(tunnel.PushHostMetricsResponse{Accepted: 0})
		}
		deviceID := resolveDeviceID(rpcCtx, w.DeviceResolver, canonicalEdgeID)
		if deviceID == 0 {
			// Host junction missing - drop rather than write edge_id as a
			// bogus device_id label (issue #96). Accepted=0 is a *retry
			// signal*, not a discard: a node that cached this point in its
			// write-ahead log keeps it until register_edge lands. This is
			// the difference between "the center refused it" and "the
			// center could not place it yet".
			log.Warn("frontierbound: push_host_metrics deferred - device_id unresolved (edge_devices host junction missing; edge needs to (re)register)",
				slog.Uint64("edge_id", canonicalEdgeID),
				slog.Uint64("transport_edge_id", edgeID),
				slog.Int("n", len(in.Points)),
			)
			return json.Marshal(tunnel.PushHostMetricsResponse{Accepted: 0})
		}
		if err := w.MetricIngester.Push(rpcCtx, deviceID, in.Points); err != nil {
			log.Warn("frontierbound: ingest push",
				slog.Uint64("edge_id", canonicalEdgeID),
				slog.Uint64("transport_edge_id", edgeID),
				slog.Int("n", len(in.Points)),
				slog.Any("err", err),
			)
			return nil, fmt.Errorf("push_host_metrics: %w", err)
		}
		out := tunnel.PushHostMetricsResponse{Accepted: uint32(len(in.Points))}
		return json.Marshal(out)
	}); err != nil {
		return fmt.Errorf("frontierbound: register %q: %w", tunnel.MethodPushHostMetrics, err)
	}

	// push_change_events: persist batches of edge changewatcher events
	// (journald / dockerd / packagemgr) into edge_change_events. Optional
	// - when ChangeEventUC is nil, do not install (A.3 change events
	// disabled in this deployment). Rejected counts events the server
	// refused (malformed / over batch limit).
	if w.ChangeEventUC != nil {
		if err := c.Register(ctx, tunnel.MethodPushChangeEvents, func(rpcCtx context.Context, edgeID uint64, body []byte) ([]byte, error) {
			var in tunnel.PushChangeEventsRequest
			if err := json.Unmarshal(body, &in); err != nil {
				return nil, fmt.Errorf("push_change_events: decode: %w", err)
			}
			// The transport's binding wins whenever it exists, which on a
			// real connection it always does: EdgeOnline resolved it from
			// the access key before the node could push anything. A body
			// that names a different edge is therefore not a re-binding
			// request, it is a claim the node cannot make — and this is the
			// one handler where believing it would write one host's
			// self-heal history under another host's name, into a ledger
			// that is supposed to be evidence.
			//
			// A transport with *no* binding yet is the first-connect race,
			// and there the body's id is accepted as the binding, exactly
			// as push_host_metrics accepts it. The alternative — deferring
			// forever because the handshake has not landed — is what
			// Accepted=0 below already covers.
			canonicalEdgeID := c.canonicalizeEdgeID(edgeID)
			switch {
			case canonicalEdgeID == 0 && in.EdgeID != 0:
				canonicalEdgeID = in.EdgeID
				c.bindEdgeTransport(edgeID, canonicalEdgeID)
			case canonicalEdgeID != 0 && in.EdgeID != 0 && in.EdgeID != canonicalEdgeID:
				log.Warn("frontierbound: autonomy replay named an edge the transport did not authenticate as; using the transport's binding",
					slog.Uint64("edge_id", canonicalEdgeID),
					slog.Uint64("transport_edge_id", edgeID),
					slog.Uint64("claimed_edge_id", in.EdgeID))
			}
			if canonicalEdgeID == 0 {
				// Edge hasn't completed register_edge yet; accept but record 0,
				// edge retries once binding is set up.
				return json.Marshal(tunnel.PushChangeEventsResponse{Accepted: 0})
			}
			events := make([]domain.ChangeEventInput, 0, len(in.Events))
			var rejected uint32
			for _, e := range in.Events {
				if e.Source == "" || e.Kind == "" || e.Timestamp.IsZero() {
					rejected++
					continue
				}
				// seq 0 is "this node never logged it", and the edge domain
				// stores NULL for that rather than 0 — see
				// changeevent.Usecase.Ingest. The handler only has to say
				// "absent", and the pointer is how it says it.
				ev := domain.ChangeEventInput{
					EdgeID:    canonicalEdgeID,
					Source:    e.Source,
					Kind:      e.Kind,
					Subject:   e.Subject,
					Action:    e.Action,
					Timestamp: e.Timestamp,
					Severity:  e.Severity,
					Labels:    e.Labels,
				}
				if e.Seq != 0 {
					seq := e.Seq
					ev.Seq = &seq
				}
				events = append(events, ev)
			}
			accepted, err := w.ChangeEventUC.Ingest(rpcCtx, events)
			if err != nil {
				return nil, fmt.Errorf("push_change_events: insert: %w", err)
			}
			return json.Marshal(tunnel.PushChangeEventsResponse{
				Accepted: uint32(accepted),
				Rejected: rejected,
			})
		}); err != nil {
			return fmt.Errorf("frontierbound: register %q: %w", tunnel.MethodPushChangeEvents, err)
		}
	}

	// push_prom_samples: forward open-set samples to Prometheus via the
	// promwrite ingester. When the ingester is nil (Prom disabled), accept
	// silently - the edge has no business knowing the cloud's Prom state.
	if err := c.Register(ctx, tunnel.MethodPushPromSamples, func(rpcCtx context.Context, edgeID uint64, body []byte) ([]byte, error) {
		var in tunnel.PushPromSamplesRequest
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("push_prom_samples: decode: %w", err)
		}
		canonicalEdgeID := c.canonicalizeEdgeID(edgeID)
		if in.EdgeID != 0 {
			canonicalEdgeID = in.EdgeID
			c.bindEdgeTransport(edgeID, canonicalEdgeID)
		}
		n := len(in.Samples)
		if canonicalEdgeID == 0 {
			// Edge hasn't completed register_edge yet. Drop for now to
			// avoid leaking the raw transport ID as edge_id label
			// (v0.7.39 fix). Accepted=0: a node with a write-ahead log
			// keeps the batch and tries again once the binding lands.
			log.Debug("frontierbound: push_prom_samples deferred (no canonical edge yet)",
				slog.Uint64("transport_edge_id", edgeID),
				slog.String("source", in.Source),
				slog.Int("n", n),
			)
			return json.Marshal(tunnel.PushPromSamplesResponse{Accepted: 0})
		}
		if w.PromIngester == nil {
			// Prom disabled / not wired. The node must not keep retrying
			// data this deployment has no store for, so it is refused:
			// Accepted=0 with a reason the edge counts as a rejection.
			log.Debug("frontierbound: push_prom_samples refused (prom disabled)",
				slog.Uint64("edge_id", canonicalEdgeID),
				slog.Uint64("transport_edge_id", edgeID),
				slog.String("source", in.Source),
				slog.Int("n", n),
			)
			return json.Marshal(tunnel.PushPromSamplesResponse{Accepted: 0})
		}
		deviceID := resolveDeviceID(rpcCtx, w.DeviceResolver, canonicalEdgeID)
		if deviceID == 0 {
			// Host junction missing - drop rather than pollute the TSDB
			// with edge_id-as-device_id (issue #96). Accepted=0 is a retry
			// signal: the link lands on register_edge and the node's log
			// still holds the batch until then.
			log.Warn("frontierbound: push_prom_samples deferred - device_id unresolved (edge_devices host junction missing; edge needs to (re)register)",
				slog.Uint64("edge_id", canonicalEdgeID),
				slog.Uint64("transport_edge_id", edgeID),
				slog.String("source", in.Source),
				slog.Int("n", n),
			)
			return json.Marshal(tunnel.PushPromSamplesResponse{Accepted: 0})
		}
		if err := w.PromIngester.Push(rpcCtx, deviceID, in.Source, in.Samples); err != nil {
			log.Warn("frontierbound: prom ingest push",
				slog.Uint64("edge_id", canonicalEdgeID),
				slog.Uint64("transport_edge_id", edgeID),
				slog.String("source", in.Source),
				slog.Int("n", n),
				slog.Any("err", err),
			)
			return nil, fmt.Errorf("push_prom_samples: %w", err)
		}
		return json.Marshal(tunnel.PushPromSamplesResponse{Accepted: n})
	}); err != nil {
		return fmt.Errorf("frontierbound: register %q: %w", tunnel.MethodPushPromSamples, err)
	}

	// get_plugin_configs: serve the edge its own plugin config snapshot
	//Optional - only registered when PluginConfigUC is wired
	// (lets opskeeper run without the plugin runtime when no plugins are
	// in use).
	if w.PluginConfigUC != nil {
		if err := c.Register(ctx, tunnel.MethodGetPluginConfigs, func(rpcCtx context.Context, edgeID uint64, _ []byte) ([]byte, error) {
			canonicalEdgeID := c.canonicalizeEdgeID(edgeID)
			snap, err := w.PluginConfigUC.FetchForEdge(rpcCtx, canonicalEdgeID)
			if err != nil {
				return nil, fmt.Errorf("get_plugin_configs: %w", err)
			}
			// Convert biz snapshot to wire snapshot (same shape, separate
			// types so core/floor/tunnel stays biz-free).
			out := tunnel.GetPluginConfigsResponse{
				EdgeID:  snap.EdgeID,
				Configs: make(map[string]tunnel.GetPluginConfigsEntry, len(snap.Configs)),
			}
			for name, cfg := range snap.Configs {
				out.Configs[name] = tunnel.GetPluginConfigsEntry{
					Enabled:  cfg.Enabled,
					Endpoint: cfg.Endpoint,
					Spec:     cfg.Spec,
				}
			}
			return json.Marshal(out)
		}); err != nil {
			return fmt.Errorf("frontierbound: register %q: %w", tunnel.MethodGetPluginConfigs, err)
		}
	}

	// agent.event: a node's agent pushing a turn's output back to the
	// console that asked for it. The reply is always success. The node
	// relays on a bounded deadline and counts a failed push as a drop, so
	// a refusal here costs nothing on its side - but an error would make
	// the edge treat a normal end-of-conversation as a transport fault.
	if w.AgentEvents != nil {
		if err := c.Register(ctx, tunnel.MethodAgentEvent, func(rpcCtx context.Context, edgeID uint64, body []byte) ([]byte, error) {
			var in tunnel.AgentEventFrame
			if err := json.Unmarshal(body, &in); err != nil {
				// A frame the manager cannot parse is the node's bug, not
				// this conversation's. Answering with an error makes the
				// edge log it and carry on; there is nothing to retry.
				log.Warn("frontierbound: agent.event decode failed",
					slog.Uint64("edge_id", edgeID), slog.Any("err", err))
				return []byte(`{}`), nil
			}
			// The transport's edge id is authoritative over the node's own
			// stamp: it is what the connection authenticated as, and a
			// frame claiming a different node would otherwise let one node
			// write into another node's conversation.
			in.EdgeID = edgeID
			w.AgentEvents.DeliverInbound(in)
			return []byte(`{}`), nil
		}); err != nil {
			return fmt.Errorf("frontierbound: register %q: %w", tunnel.MethodAgentEvent, err)
		}
	}

	// agent.audit.replay: a node handing over the self-heal decisions it
	// made while the control plane was away, so the center's chain can
	// record them.
	//
	// The transport's edge id wins over the body's, exactly as it does on
	// agent.event: the connection authenticated as one node, and a batch
	// that claimed another node's id would otherwise let one host write
	// self-heal history onto another host's ledger entry.
	//
	// A response with Accepted=0 and no error is not a failure it can act
	// on — the node treats a partial accept as "the chain took this much"
	// and keeps the rest — so the handler only returns an error when the
	// whole store is unreachable, which is the node's signal to hold
	// everything and retry.
	if w.AutonomyReplay != nil {
		if err := c.Register(ctx, tunnel.MethodAgentAuditReplay, func(rpcCtx context.Context, edgeID uint64, body []byte) ([]byte, error) {
			var in tunnel.AutonomyAuditReplayRequest
			if err := json.Unmarshal(body, &in); err != nil {
				// A body this manager cannot parse is the node's bug, and
				// it will be the same body next time. Answer with a
				// refusal count rather than an RPC error so the node does
				// not wedge its backlog on one malformed row.
				log.Warn("frontierbound: agent.audit.replay decode failed",
					slog.Uint64("edge_id", edgeID), slog.Any("err", err))
				return json.Marshal(tunnel.AutonomyAuditReplayResponse{
					Accepted: 0, Rejected: 0, Reason: "malformed replay body",
				})
			}
			// The transport's binding wins whenever it exists, which on a
			// real connection it always does: EdgeOnline resolved it from
			// the access key before the node could push anything. A body
			// that names a different edge is therefore not a re-binding
			// request, it is a claim the node cannot make — and this is the
			// one handler where believing it would write one host's
			// self-heal history under another host's name, into a ledger
			// that is supposed to be evidence.
			//
			// A transport with *no* binding yet is the first-connect race,
			// and there the body's id is accepted as the binding, exactly
			// as push_host_metrics accepts it. The alternative — deferring
			// forever because the handshake has not landed — is what
			// Accepted=0 below already covers.
			canonicalEdgeID := c.canonicalizeEdgeID(edgeID)
			switch {
			case canonicalEdgeID == 0 && in.EdgeID != 0:
				canonicalEdgeID = in.EdgeID
				c.bindEdgeTransport(edgeID, canonicalEdgeID)
			case canonicalEdgeID != 0 && in.EdgeID != 0 && in.EdgeID != canonicalEdgeID:
				log.Warn("frontierbound: autonomy replay named an edge the transport did not authenticate as; using the transport's binding",
					slog.Uint64("edge_id", canonicalEdgeID),
					slog.Uint64("transport_edge_id", edgeID),
					slog.Uint64("claimed_edge_id", in.EdgeID))
			}
			if canonicalEdgeID == 0 {
				// Not registered yet. Accepted=0 makes the node keep the
				// rows and try again once the binding lands, which is the
				// same handshake the metrics path uses.
				log.Debug("frontierbound: agent.audit.replay deferred (no canonical edge yet)",
					slog.Uint64("transport_edge_id", edgeID),
					slog.Int("rows", len(in.Rows)))
				return json.Marshal(tunnel.AutonomyAuditReplayResponse{Accepted: 0})
			}
			accepted, rejected, err := w.AutonomyReplay.RecordAutonomyReplay(rpcCtx, canonicalEdgeID, in.Rows)
			if err != nil {
				log.Warn("frontierbound: autonomy replay failed; the node will retry",
					slog.Uint64("edge_id", canonicalEdgeID),
					slog.Int("rows", len(in.Rows)),
					slog.Any("err", err))
				return nil, fmt.Errorf("agent.audit.replay: %w", err)
			}
			return json.Marshal(tunnel.AutonomyAuditReplayResponse{
				Accepted: accepted,
				Rejected: rejected,
			})
		}); err != nil {
			return fmt.Errorf("frontierbound: register %q: %w", tunnel.MethodAgentAuditReplay, err)
		}
	}

	// agent.audit.entries: a node's own ledger rows, on their way into the
	// chain (决策 126).
	//
	// This is the last hop for every tool call, block, agent turn and
	// plugin install that happens on a node. Until it existed the manager
	// recorded what the *console* did and nothing about what the fleet did,
	// which for an agent fleet is the half that matters: the console
	// approved a release, the node then ran whatever it liked with it.
	//
	// The identity rule is agent.audit.replay's, and it is not optional
	// here: the rows are the node's testimony about a host, and a node
	// that could file another host's testimony would make the chain say
	// something false. The transport's binding wins; the body's EdgeID is
	// only consulted when nothing has bound the transport yet, which is
	// the first-connect race the metrics path already handles.
	//
	// A body this manager cannot parse answers with a refusal count rather
	// than an error, so one malformed batch does not wedge a backlog that
	// is otherwise fine — the node counts those rows and passes them, and
	// the count is on its health line.
	if w.NodeLedger != nil {
		if err := c.Register(ctx, tunnel.MethodAgentAuditEntries, func(rpcCtx context.Context, edgeID uint64, body []byte) ([]byte, error) {
			var in tunnel.AuditEntriesRequest
			if err := json.Unmarshal(body, &in); err != nil {
				log.Warn("frontierbound: agent.audit.entries decode failed",
					slog.Uint64("edge_id", edgeID), slog.Any("err", err))
				return json.Marshal(tunnel.AuditEntriesResponse{
					Accepted: 0, Rejected: 0, Reason: "malformed node ledger body",
				})
			}
			canonicalEdgeID := c.canonicalizeEdgeID(edgeID)
			switch {
			case canonicalEdgeID == 0 && in.EdgeID != 0:
				canonicalEdgeID = in.EdgeID
				c.bindEdgeTransport(edgeID, canonicalEdgeID)
			case canonicalEdgeID != 0 && in.EdgeID != 0 && in.EdgeID != canonicalEdgeID:
				log.Warn("frontierbound: a node's ledger named an edge the transport did not authenticate as; using the transport's binding",
					slog.Uint64("edge_id", canonicalEdgeID),
					slog.Uint64("transport_edge_id", edgeID),
					slog.Uint64("claimed_edge_id", in.EdgeID))
			}
			if canonicalEdgeID == 0 {
				// Not registered yet. Accepted=0 with no error is the
				// signal the node keeps its batch, which is the same
				// handshake the metrics and autonomy paths use.
				log.Debug("frontierbound: agent.audit.entries deferred (no canonical edge yet)",
					slog.Uint64("transport_edge_id", edgeID),
					slog.Int("rows", len(in.Entries)))
				return json.Marshal(tunnel.AuditEntriesResponse{Accepted: 0})
			}
			accepted, rejected, err := w.NodeLedger.RecordNodeEntries(rpcCtx, canonicalEdgeID, in.Entries)
			if err != nil {
				log.Warn("frontierbound: a node's ledger rows failed; the node will retry",
					slog.Uint64("edge_id", canonicalEdgeID),
					slog.Int("rows", len(in.Entries)),
					slog.Any("err", err))
				return nil, fmt.Errorf("agent.audit.entries: %w", err)
			}
			return json.Marshal(tunnel.AuditEntriesResponse{
				Accepted: accepted,
				Rejected: rejected,
			})
		}); err != nil {
			return fmt.Errorf("frontierbound: register %q: %w", tunnel.MethodAgentAuditEntries, err)
		}
	}

	// agent.tool: a node's agent asking the control plane to run a tool it
	// does not implement. Topology and alert queries live here, so the
	// node has to ask; the alternative — a second route into the control
	// plane from inside the agent process — would be unaudited.
	//
	// The edge id is the transport's, never the node's own stamp, for the
	// same reason agent.event does: it is what the connection authenticated
	// as, so one node cannot spend another node's authority.
	if w.AgentTools != nil {
		if err := c.Register(ctx, tunnel.MethodAgentTool, func(rpcCtx context.Context, edgeID uint64, body []byte) ([]byte, error) {
			var in tunnel.AgentToolRequest
			if err := json.Unmarshal(body, &in); err != nil {
				return nil, fmt.Errorf("agent.tool: decode: %w", err)
			}
			if in.Tool == "" {
				return nil, fmt.Errorf("agent.tool: a call with no tool name cannot be run")
			}
			out, err := w.AgentTools.RunAgentTool(rpcCtx, edgeID, in.SessionID, in.Tool, in.Arguments)
			if err != nil {
				// Answered, not failed: the RPC succeeded and the tool did
				// not. That distinction matters to the node, which turns an
				// RPC error into "the control plane is unreachable" and an
				// error string into something the model can read and act
				// on.
				return json.Marshal(tunnel.AgentToolResponse{Error: err.Error()})
			}
			return json.Marshal(tunnel.AgentToolResponse{Result: out})
		}); err != nil {
			return fmt.Errorf("frontierbound: register %q: %w", tunnel.MethodAgentTool, err)
		}
	}

	log.Info("frontierbound: handlers installed")
	return nil
}

// NotifyPluginConfigsChanged pushes a reload notification to one edge.
// Cloud → edge RPC; the edge handler simply triggers Supervisor.Reload.
// Body is empty by design - edge re-fetches via MethodGetPluginConfigs
// to avoid wire-format coupling between push payload and pull response.
//
// Failure modes are caller's responsibility to log; in particular this
// is fire-and-forget for the biz layer because the edge's 60s
// safety-net poll catches missed pushes anyway.
// Implements edgebiz.EdgeReloadNotifier.
func (c *Client) NotifyPluginConfigsChanged(ctx context.Context, edgeID uint64) error {
	_, err := c.Call(ctx, edgeID, tunnel.MethodPluginConfigsChanged, []byte("{}"))
	return err
}

// safeAddr renders a net.Addr without panicking on nil.
func safeAddr(a net.Addr) string {
	if a == nil {
		return ""
	}
	return a.String()
}

// resolveDeviceID maps a tunnel-side edge_id to the host device_id
// labelled into the push pipeline. Returns the resolved device_id, or 0
// when it cannot be resolved.
//
// It MUST NOT fall back to edge_id. After the edge/device entity split
// (May 2026) edge_id and device.ID are independent auto-increment
// sequences, so a fallback writes a WRONG device_id label into the
// immutable Prometheus TSDB (issue #96 - Monitor showed edge_ids like
// 10/11/12 that don't exist on the Devices page). Callers MUST drop the
// batch when this returns 0 rather than persist a bogus label.
func resolveDeviceID(ctx context.Context, dr DeviceResolver, edgeID uint64) uint64 {
	if dr == nil || edgeID == 0 {
		return 0
	}
	id, err := dr.LookupHostDevice(ctx, edgeID)
	if err != nil || id == 0 {
		// Host junction not resolvable (edge not yet linked to a device).
		// Return 0 so the caller drops this batch instead of polluting
		// history with edge_id-as-device_id.
		return 0
	}
	return id
}

// unixOrZero converts a unix-seconds wire value to a UTC time, returning the
// zero time for 0 (the edge sends 0 for "never started" / unset).
func unixOrZero(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}
