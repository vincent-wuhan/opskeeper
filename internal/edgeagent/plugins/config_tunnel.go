package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/tunnel"
)

// TunnelConfigFetcher pulls plugin configs from the manager via the
// tunnel RPC `get_plugin_configs`. Auth credentials
// (basic-auth user/pass for the data plane endpoint) are NOT carried on
// the wire — the edge fills them in from its local env (the same
// access_key/secret_key it used to authenticate the tunnel).
//
// Composes with EnvConfigFetcher as a fallback: when the tunnel is
// unreachable (cold start before Dial completes, or transient outage),
// Fetch returns the env-driven snapshot so the supervisor can keep a
// reasonable default running.
type TunnelConfigFetcher struct {
	client       tunnel.Client
	knownPlugins []string
	fallback     *EnvConfigFetcher

	// Auth + edge_id materialised from env once at construction so
	// every Fetch doesn't re-read os.Getenv. ConfigFetcher contract
	// allows mutation of returned PluginConfigs each call so this is
	// safe: we copy values into each PluginConfig per Fetch call.
	authUser string
	authPass string
	edgeID   uint64
}

// NewTunnelConfigFetcher builds a fetcher that fronts a tunnel.Client
// with an EnvConfigFetcher fallback for offline/early-boot cases.
//
// knownPlugins is the same slice passed to NewEnvConfigFetcher — used
// to filter the manager's response (defensive against the manager
// somehow returning configs for unknown plugin names) and to drive the
// fallback path.
func NewTunnelConfigFetcher(client tunnel.Client, knownPlugins []string) *TunnelConfigFetcher {
	return &TunnelConfigFetcher{
		client:       client,
		knownPlugins: append([]string(nil), knownPlugins...),
		fallback:     NewEnvConfigFetcher(knownPlugins),
		authUser:     firstNonEmpty(os.Getenv("OPSKEEPER_EDGE_PLUGIN_DATAPLANE_USER"), os.Getenv("OPSKEEPER_EDGE_ACCESS_KEY")),
		authPass:     firstNonEmpty(os.Getenv("OPSKEEPER_EDGE_PLUGIN_DATAPLANE_PASS"), os.Getenv("OPSKEEPER_EDGE_SECRET_KEY")),
		edgeID:       envUint("OPSKEEPER_EDGE_ID"),
	}
}

// Fetch calls MethodGetPluginConfigs and converts the wire response into
// the supervisor's PluginConfig shape. On any RPC error it falls back to
// EnvConfigFetcher so a partition between edge and manager doesn't kill
// already-configured plugins.
func (t *TunnelConfigFetcher) Fetch(ctx context.Context) (map[string]PluginConfig, error) {
	if t.client == nil {
		return t.fallback.Fetch(ctx)
	}
	var resp tunnel.GetPluginConfigsResponse
	if err := t.client.Call(ctx, tunnel.MethodGetPluginConfigs, struct{}{}, &resp); err != nil {
		// Don't surface the error — supervisor would log "config fetch
		// failed; keeping previous state" and never recover until the
		// next reload. Falling back to env keeps things alive at the
		// cost of a stale snapshot during outages.
		envSnap, _ := t.fallback.Fetch(ctx)
		// Annotate via a sentinel error type if needed; current callers
		// only care about the snapshot.
		_ = err
		return envSnap, nil
	}

	out := make(map[string]PluginConfig, len(t.knownPlugins))
	known := map[string]bool{}
	for _, n := range t.knownPlugins {
		known[n] = true
	}
	// Edge ID source of truth: env > tunnel response. Env wins because
	// the operator may run a single edge against multiple managers in
	// dev — the env's ID is what's baked into Loki labels regardless.
	edgeID := t.edgeID
	if edgeID == 0 {
		edgeID = resp.EdgeID
	}
	for name, entry := range resp.Configs {
		if !known[name] {
			continue
		}
		out[name] = PluginConfig{
			Enabled:  entry.Enabled,
			EdgeID:   edgeID,
			Endpoint: entry.Endpoint,
			AuthUser: t.authUser,
			AuthPass: t.authPass,
			Spec:     entry.Spec,
		}
	}
	// Plugins manager didn't mention default to disabled; supervisor
	// stops them if they were running.
	for _, name := range t.knownPlugins {
		if _, ok := out[name]; !ok {
			out[name] = PluginConfig{Enabled: false, EdgeID: edgeID}
		}
	}
	return out, nil
}

// MarshalJSON kept on the type for diagnostic dumping (currently unused
// but useful for `--dump-config` style flags).
func (t *TunnelConfigFetcher) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		KnownPlugins []string `json:"known_plugins"`
		EdgeID       uint64   `json:"edge_id"`
		HasClient    bool     `json:"has_client"`
	}{t.knownPlugins, t.edgeID, t.client != nil})
}

// AssertKnown is a tiny sanity helper for tests / debug tooling. Returns
// an error listing the unknown names, otherwise nil.
func (t *TunnelConfigFetcher) AssertKnown(names []string) error {
	known := map[string]bool{}
	for _, n := range t.knownPlugins {
		known[n] = true
	}
	var bad []string
	for _, n := range names {
		if !known[n] {
			bad = append(bad, n)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("unknown plugin names: %v", bad)
	}
	return nil
}
