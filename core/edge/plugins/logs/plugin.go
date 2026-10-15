// Package logs is the edge-side `logs` plugin.
//
// It wraps a Promtail subprocess: opskeeper-edge writes a Promtail config
// derived from the manager-pushed PluginConfig, spawns promtail, and
// lets Promtail push directly to manager nginx /loki/api/v1/push.
// opskeeper-edge does not touch the log byte stream.
package logs

import (
	"log/slog"
	"path/filepath"

	"github.com/vincent-wuhan/opskeeper/core/edge/plugins"
)

// Name is the OTel signal name used as plugin identifier and as the
// directory key under <workDir>/plugins/.
const Name = "logs"

// New constructs the logs plugin. binDir is where opskeeper-edge looks for
// the bundled promtail binary (typically /opt/opskeeper-edge/bin); workDir
// is where rendered config + promtail positions + subprocess log live
// (typically /var/lib/opskeeper-edge/plugins).
//
// The returned *plugins.SubprocessPlugin satisfies plugins.Plugin and is
// registered with the Supervisor by opskeeper-edge main.
func New(binDir, workDir string, log *slog.Logger) plugins.Plugin {
	return plugins.NewSubprocessCollector(Spec(), binDir, workDir, log)
}

// Spec declares the logs collector: the promtail binary and the config
// template it is run with.
//
// It is the declaration NewSubprocessCollector consumes. Before it, the
// binary name and template were inline in New, so the node's collector set
// could only be read by reading this constructor; now it is a value, and a
// third-party collector package is built from its own Spec the same way.
func Spec() plugins.CollectorSpec {
	return plugins.CollectorSpec{
		Name:           Name,
		BinaryName:     "promtail",
		ConfigFileName: "promtail.yaml",
		Render:         render,
		Args: func(_ plugins.PluginConfig, configFile string) []string {
			return []string{
				"-config.file=" + configFile,
				// Promtail's positions file lives next to the config so
				// re-creates of the workdir don't lose journald cursor.
				"-positions.file=" + filepath.Join(filepath.Dir(configFile), "positions.yaml"),
			}
		},
	}
}
