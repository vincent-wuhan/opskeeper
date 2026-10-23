// Package opskeeperdatasourcevictoriametrics is OpsKeeper's first
// third-party datasource package: a VictoriaMetrics backend that answers the
// `prom` signal.
//
// The package is preinstalled in the binary rather than loaded from a plugin
// directory. That is a deployment fact, not a design preference: the disk
// channel (OPSKEEPER_MANAGER_PLUGINS_DIR plus the host's activator) exists in
// the code, but nothing deploys into that directory yet, so shipping the disk
// channel first would deliver a switch that is configured, enabled, and dead.
// What "preinstalled" does not mean is "unadmitted": activation still runs
// the manifest through the host's admission gate on every boot, so the
// third-party path is exercised in production rather than only in a test.
//
// Nothing here imports the control plane. The manifest is decoded through the
// sdk, the querier returns the core/base result type, and the registration
// goes through the kernel's closed extension-point set — three stable surfaces
// a third-party package is allowed to depend on.
package opskeeperdatasourcevictoriametrics

import (
	_ "embed"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/sdk"

	"github.com/vincent-wuhan/opskeeper/plugins/opskeeper-datasource-victoriametrics/internal/contributor"
	"github.com/vincent-wuhan/opskeeper/plugins/opskeeper-datasource-victoriametrics/internal/vmclient"
	"github.com/vincent-wuhan/opskeeper/plugins/opskeeper-datasource-victoriametrics/internal/vmquery"
)

// pigOpsYAML is the package's governance manifest, embedded from the file in
// this directory. The test in this package compares it against the bytes on
// disk — a manifest embedded into a binary and a manifest edited on disk are
// two things that drift, and the only way to notice is to compare them.
//
//go:embed pig-ops.yaml
var pigOpsYAML []byte

// PluginName is the package name the manifest declares. The contribution and
// the manifest must agree: the host refuses a pair that does not.
const PluginName = contributor.PluginName

// Manifest decodes and validates the embedded pig-ops.yaml.
//
// The file on disk is the only declaration of this package's governance: there
// is no Go struct that restates it. A second declaration would be a second
// source of truth, and the two would drift the first time somebody edited the
// YAML — which is the change an operator actually makes.
//
// The returned Plugin carries only its Manifest, with no Root and no Origin:
// those two fields describe where a manifest was loaded from on disk, and this
// one was not.
func Manifest() (pluginmanifest.Plugin, error) {
	m, err := sdk.Decode(pigOpsYAML)
	if err != nil {
		return pluginmanifest.Plugin{}, fmt.Errorf("victoriametrics: %w", err)
	}
	if m.Metadata.Name != PluginName {
		// Not reachable from the file this package ships; reachable the
		// moment somebody edits pig-ops.yaml. The host would catch it too,
		// but only at activation, and naming the mismatch here points at the
		// file rather than at the host.
		return pluginmanifest.Plugin{}, fmt.Errorf(
			"victoriametrics: manifest names %q but this package is %q",
			m.Metadata.Name, PluginName)
	}
	return pluginmanifest.Plugin{Manifest: m}, nil
}

// QueryPromqlCallTimeout is how long one PromQL dispatch may take.
//
// It re-exports vmquery's own constant rather than being it: vmquery is
// internal, and the drift check that keeps the two ceilings equal lives in the
// root module (cmd/opskeeper), which can import this package but not that one.
// It is read by that check and by nothing else, which is why it is a function
// rather than a constant — a second const declaration would be a second place
// for the value to be edited.
func QueryPromqlCallTimeout() time.Duration { return vmquery.QueryPromqlCallTimeout }

// Config is how a caller describes the VictoriaMetrics deployment to reach.
//
// It is an alias rather than a second declaration on purpose: the control
// plane must be able to write pkg.Config{BaseURL: ...} without importing
// internal/vmclient, and a copy of the struct here would be a second place for
// a field to be added and forgotten.
type Config = vmclient.Config

// New builds the VictoriaMetrics querier from cfg.
func New(cfg Config) *vmclient.Client { return vmclient.New(cfg) }

// NewWithResolverAndHTTPClient is the hot-reloading form: the base URL is
// resolved per call, so a deployment can move the backend from an admin UI
// without restarting the manager. r must resolve a Prometheus-compatible API
// root, and hc owns the TLS material and injects credentials per request.
//
// This is the constructor the control plane uses. New exists for callers that
// know their URL at build time — the plugin's own tests, and an assembly layer
// with no settings store behind it.
func NewWithResolverAndHTTPClient(
	r vmclient.BaseURLResolver,
	hc *http.Client,
	log *slog.Logger,
) *vmclient.Client {
	return vmclient.NewWithResolverAndHTTPClient(r, hc, log)
}

// Contribution returns the registration for the querier, ready to hand to
// kernel.Host.Activate alongside Manifest().
func Contribution(q *vmclient.Client) contributor.VictoriaMetrics {
	return contributor.VictoriaMetrics{Q: q}
}
