// Package traces is the edge-side `traces` plugin.
//
// It wraps an OpenTelemetry Collector (otelcol-contrib) subprocess:
// opskeeper-edge writes an otelcol config derived from the manager-pushed
// PluginConfig, spawns otelcol-contrib, and lets it accept OTLP gRPC/HTTP
// from local applications and push directly to manager nginx /v1/traces.
// opskeeper-edge does not touch the trace byte stream.
//
// Plugin name "traces" is plural — OTel signal naming convention
// and matches the OTLP endpoint path /v1/traces.
package traces

import (
	"log/slog"
	"path/filepath"

	"github.com/vincent-wuhan/opskeeper/internal/edgeagent/plugins"
)

// Name is the OTel signal name used as plugin identifier and as the
// directory key under <workDir>/plugins/.
const Name = "traces"

// New constructs the traces plugin. binDir is where opskeeper-edge looks for
// the bundled otelcol-contrib binary (typically /usr/local/lib/opskeeper-edge);
// workDir is where rendered config + subprocess log live (typically
// /var/lib/opskeeper-edge/plugins).
//
// The returned *plugins.SubprocessPlugin satisfies plugins.Plugin and is
// registered with the Supervisor by opskeeper-edge main.
func New(binDir, workDir string, log *slog.Logger) plugins.Plugin {
	return plugins.NewSubprocess(plugins.SubprocessOpts{
		Name:         Name,
		Binary:       filepath.Join(binDir, "otelcol-contrib"),
		WorkDir:      filepath.Join(workDir, Name),
		ConfigFile:   filepath.Join(workDir, Name, "otelcol.yaml"),
		ConfigRender: render,
		Args: func(_ plugins.PluginConfig, configFile string) []string {
			// otelcol-contrib uses --config=... (also accepts repeated flags
			// for layered configs; we only need a single rendered file).
			return []string{
				"--config=" + configFile,
			}
		},
		Log: log,
	})
}
