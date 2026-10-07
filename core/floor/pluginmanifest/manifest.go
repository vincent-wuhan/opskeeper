// Package pluginmanifest is the control plane's entry point for plugin
// governance. It is a thin seam over the sdk module so the rest of the
// manager depends on one OpsKeeper import rather than reaching into the
// plugin-facing module directly, and so a host can add repository-local
// policy (an allow-list, a signing check, a directory layout) without
// changing the published plugin contract.
//
// Every shipped plugin is validated through this package. A manifest that
// fails admission must fail a build or a test, never a production install.
package pluginmanifest

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/sdk"
)

// ManifestFile is the governance manifest's filename.
const ManifestFile = sdk.ManifestFile

// Plugin is a validated plugin ready for admission.
type Plugin struct {
	// Root is the plugin's directory on disk.
	Root string
	// Manifest is the parsed and structurally validated declaration.
	Manifest domain.PluginManifest
	// Skills are the SKILL.md directories the package ships, relative to
	// Root. They are collected for the plugin listing; the agent runtime
	// discovers them itself.
	Skills []string
	// Extensions are the package's `pi.extensions` entries, verbatim and
	// relative to Root — the paths the package declares, not the names the
	// agent will load them under.
	//
	// The distinction is PiG's and it is not ours to collapse here: PiG
	// turns "extensions/opskeeper-gate/index.ts" into the public name
	// "opskeeper-gate", and only the module allowed to import PiG can ask
	// it that question without the answer drifting. So this carries the
	// path, and the caller maps it through core/pig.
	//
	// A node needs it because the node's agent profile has to name every
	// extension whose tools it is willing to have offered, and an
	// extension nobody can name is an extension whose tool list cannot be
	// written down.
	Extensions []string
}

// Name returns the plugin's declared name.
func (p Plugin) Name() string { return p.Manifest.Metadata.Name }

// HighestCapability returns the most dangerous class the plugin declares.
func (p Plugin) HighestCapability() domain.ToolClass { return p.Manifest.HighestCapability() }

// Targets reports where the plugin asked to run.
func (p Plugin) Targets() domain.Targets { return p.Manifest.Spec.Targets }

// RunsOn reports whether the plugin targets t.
func (p Plugin) RunsOn(t domain.DeploymentTarget) bool { return p.Manifest.Spec.Targets.Has(t) }

// Validate checks a manifest without a directory to read it from.
//
// Load is the admission path and it needs a package on disk; a producer needs
// the same judgement before a directory exists, because a draft that will not
// load is not a draft. Exposing the same call rather than describing it is
// what keeps the two from disagreeing about what is admissible.
func Validate(m domain.PluginManifest) error { return sdk.Validate(m) }

// Load reads, parses, and validates one plugin directory.
func Load(root string) (Plugin, error) {
	m, err := sdk.Load(root)
	if err != nil {
		return Plugin{}, err
	}
	skills, err := findSkills(root)
	if err != nil {
		return Plugin{}, err
	}
	extensions, err := findExtensions(root)
	if err != nil {
		return Plugin{}, err
	}
	return Plugin{Root: root, Manifest: m, Skills: skills, Extensions: extensions}, nil
}

// LoadAll walks base and loads every immediate subdirectory that carries a
// governance manifest. A plugin without one is not an error: the directory
// is simply not an OpsKeeper plugin (it may be a Pi package that the
// control plane never installs).
//
// Results are sorted by name so a listing is stable across runs.
func LoadAll(base string) ([]Plugin, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var out []Plugin
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		root := filepath.Join(base, e.Name())
		if !sdk.HasGovernanceManifest(root) {
			continue
		}
		p, err := Load(root)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// findSkills collects SKILL.md files under the plugin's skills directory.
func findSkills(root string) ([]string, error) {
	skillsDir := filepath.Join(root, "skills")
	if _, err := os.Stat(skillsDir); err != nil {
		// A plugin may ship only extensions. That is legal.
		return nil, nil
	}
	var out []string
	err := filepath.WalkDir(skillsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "SKILL.md" {
			return nil
		}
		rel, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			return relErr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// findExtensions reads the `pi.extensions` list a package declares.
//
// It reads the manifest rather than walking the extensions directory,
// because those are two different questions and only the first one has an
// answer the package author wrote down. A directory can hold a Go module
// that was never meant to load; a declared entry is a statement that it
// should.
//
// A package with no package.json is not an error. It is a plugin whose
// author declared resources in some other way, and refusing it here would
// make this a stricter gate than the loader that consumes the result.
func findExtensions(root string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc struct {
		Pi struct {
			Extensions []string `json:"extensions"`
		} `json:"pi"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("read package.json: %w", err)
	}
	out := make([]string, 0, len(doc.Pi.Extensions))
	for _, entry := range doc.Pi.Extensions {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		out = append(out, filepath.ToSlash(entry))
	}
	sort.Strings(out)
	return out, nil
}

// Catalog is the validated set of plugins the control plane knows about.
type Catalog struct {
	Plugins []Plugin
}

// ByName returns the named plugin.
func (c Catalog) ByName(name string) (Plugin, bool) {
	for _, p := range c.Plugins {
		if p.Name() == name {
			return p, true
		}
	}
	return Plugin{}, false
}

// ForTarget returns the plugins that asked to run on t.
func (c Catalog) ForTarget(t domain.DeploymentTarget) []Plugin {
	var out []Plugin
	for _, p := range c.Plugins {
		if p.RunsOn(t) {
			out = append(out, p)
		}
	}
	return out
}

// Names returns every plugin name, for diagnostics.
func (c Catalog) Names() []string {
	out := make([]string, 0, len(c.Plugins))
	for _, p := range c.Plugins {
		out = append(out, p.Name())
	}
	return out
}

// LoadCatalog validates every plugin under base and returns the catalog.
// It refuses the whole set if any one plugin is invalid: a control plane
// that serves half its catalog after a bad manifest is harder to reason
// about than one that refuses to start.
func LoadCatalog(base string) (Catalog, error) {
	plugins, err := LoadAll(base)
	if err != nil {
		return Catalog{}, err
	}
	return Catalog{Plugins: plugins}, nil
}

// Describe renders a one-line summary for CLI output and log lines.
func Describe(p Plugin) string {
	var b strings.Builder
	b.WriteString(p.Name())
	b.WriteString(" v")
	b.WriteString(p.Manifest.Metadata.Version)
	b.WriteString(" [")
	b.WriteString(string(p.Manifest.Spec.SafetyLevel))
	b.WriteString(" ")
	b.WriteString(p.HighestCapability().String())
	b.WriteString("] targets=")
	b.WriteString(strings.Join(targetNames(p.Targets()), ","))
	b.WriteString(" scopes=")
	b.WriteString(strings.Join(scopeNames(p.Manifest.Spec.RequiredScopes), ","))
	return b.String()
}

func targetNames(ts domain.Targets) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, string(t))
	}
	return out
}

func scopeNames(ss domain.Scopes) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, string(s))
	}
	return out
}

// Shutdown is a no-op hook so a caller can treat Catalog as a managed
// resource uniformly with other control-plane dependencies.
func (c Catalog) Shutdown(context.Context) error { return nil }
