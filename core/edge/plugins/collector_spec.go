package plugins

import (
	"log/slog"
	"path/filepath"
)

// CollectorSpec declares a subprocess collector: which binary it runs, how
// its config file is rendered, and how its argv is built.
//
// Before this type, a collector's binary name and config template were
// buried inside its own constructor — logs/plugin.go hardcoded "promtail",
// traces/plugin.go "otelcol-contrib" — so "what collectors does this node
// run" could only be answered by reading those files, and a new collector
// meant editing the node binary.
//
// Now a collector is a declaration. The two first-party collectors are
// built from one of these, and a third-party collector package uses the
// same constructor, so the node's collector set is extensible the same way
// its tool set already is: by shipping a package, not by rebuilding the
// node.
type CollectorSpec struct {
	// Name is the OTel signal name ("logs", "traces"), used as the plugin
	// identifier and as the directory key under <workDir>/plugins/.
	Name string
	// BinaryName is the executable's filename under the node's bin
	// directory, e.g. "promtail" or "otelcol-contrib".
	BinaryName string
	// ConfigFileName is the rendered config's filename under the plugin's
	// work directory. Empty means "<Name>.yaml".
	ConfigFileName string
	// Render returns the config bytes to write at the config file. nil
	// means the collector has no config file and encodes everything into
	// Args.
	Render func(PluginConfig) ([]byte, error)
	// Args builds the subprocess argv from the current PluginConfig and the
	// rendered config path.
	Args func(cfg PluginConfig, configFile string) []string
}

// NewSubprocessCollector builds a supervised subprocess collector from a
// declaration.
//
// It is the one constructor a collector needs: the binary path is derived
// from binDir and the spec's BinaryName, the work directory from workDir and
// the spec's Name, and the config path from the spec's ConfigFileName — so a
// caller supplies the declaration and the two directories, and nothing else.
func NewSubprocessCollector(spec CollectorSpec, binDir, workDir string, log *slog.Logger) Plugin {
	configName := spec.ConfigFileName
	if configName == "" {
		configName = spec.Name + ".yaml"
	}
	pluginWorkDir := filepath.Join(workDir, spec.Name)
	return NewSubprocess(SubprocessOpts{
		Name:         spec.Name,
		Binary:       filepath.Join(binDir, spec.BinaryName),
		WorkDir:      pluginWorkDir,
		ConfigFile:   filepath.Join(pluginWorkDir, configName),
		ConfigRender: spec.Render,
		Args:         spec.Args,
		Log:          log,
	})
}
