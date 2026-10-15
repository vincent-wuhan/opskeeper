package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/agentmodel"
	"github.com/vincent-wuhan/opskeeper/core/edge/agentprofile"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigprofile"
)

// agentSettingsFile is the agent settings file the node writes.
//
// A node does not pass packages on the command line: the agent discovers its
// plugins from a settings list, and the working directory is the single most
// security-relevant thing a node points at. Writing the file here rather than
// letting a package decide where it goes is what makes "which code runs on
// this host" a question
// with one answer.
const agentSettingsFile = "settings.json"

// agentConfigDirName mirrors the agent's own rule for its project config
// directory.
//
// The two must agree or the node writes a file the agent never reads, and
// the node boots with no plugins while its settings say otherwise - the
// kind of mismatch that is only discovered during an incident. Deriving it
// from the same env var the agent uses is the only way to keep them in
// step without importing the agent's paths package, which would put PiG in
// the node's dependency graph for a string.
func agentConfigDirName() string {
	if os.Getenv("PIG_USE_PI_DIRS") == "1" {
		return ".pi"
	}
	return ".pig"
}

// agentSettings is the slice of the agent's settings file this node writes.
//
// Only packages is set, and it is written whole each time rather than
// merged. A merge would preserve keys an operator added by hand, which for
// a file that decides what code runs on a host is a liability: a stale
// entry would keep a removed plugin loaded for ever.
type agentSettings struct {
	Packages []string `json:"packages"`
}

// defaultPluginScopes is what a node grants when nobody has configured
// anything else.
//
// It is exactly what the first-party read-only package declares, and that
// is the point rather than a coincidence: a third-party package needing
// anything more is refused until an operator adds the scope by hand, so
// adding a plugin is a decision somebody makes rather than a consequence
// of installing one. Before this check existed every package got every
// scope implicitly, so this is not a narrowing of what ships — it is the
// same grant, written down.
var defaultPluginScopes = []domain.Scope{
	domain.ScopeHostRead,
	domain.ScopeTopologyRO,
	domain.ScopeAlertRO,
}

// nodePluginPolicy builds the node's review policy from the environment.
//
// Everything is env-driven for the same reason the rest of the node agent
// config is: a node is provisioned on a host OpsKeeper has never seen, and
// the only thing that reaches it is install.sh and the tunnel. The
// defaults are the ones that are safe to run without being told anything.
//
// The two that matter most are the safety ceiling and the scopes, because
// both default to the *lowest* value that lets the shipped read-only
// package work. In particular the ceiling is L1, which means the L2 repair
// package is refused until an operator says this host may change things —
// a capability that has to be enabled is a capability nobody enabled by
// accident.
// edgeProfileEnv names the deployment profile this node belongs to.
//
// A profile sets the ceiling, the radius and the granted scopes together,
// because those three are one decision per deployment rather than three
// per host. A node that sets only one of them is a node whose policy is
// the profile's for the fields it did not set and its own for the field it
// did — a combination no profile describes. That is why the fields below
// are refused when a profile is named and they disagree, rather than
// merged.
const edgeProfileEnv = "OPSKEEPER_EDGE_PROFILE"

func nodePluginPolicy() (pluginmanifest.Policy, error) {
	profileName := strings.TrimSpace(os.Getenv(edgeProfileEnv))
	if profileName != "" {
		profile, err := pluginmanifest.ProfileForName(profileName)
		if err != nil {
			// Refused rather than defaulted, for the same reason a bad
			// safety level is: a node silently running the narrow profile
			// would refuse work its fleet is entitled to, and one silently
			// running the wide profile is the failure this whole mechanism
			// is meant to prevent.
			return pluginmanifest.Policy{}, fmt.Errorf("OPSKEEPER_EDGE_PROFILE: %w", err)
		}
		return nodeProfilePolicy(profile)
	}

	level := domain.SafetyLevel(strings.TrimSpace(os.Getenv("OPSKEEPER_EDGE_MAX_SAFETY_LEVEL")))
	if level == "" {
		level = domain.SafetyL1
	}
	if !level.Valid() {
		// Refused rather than defaulted. A typo in a capability ceiling
		// is a node whose policy is not the one the operator believes it
		// has, and a node that guessed L1 would either refuse everything
		// or - if the guess went the other way - quietly host more than
		// was asked for.
		return pluginmanifest.Policy{}, fmt.Errorf(
			"OPSKEEPER_EDGE_MAX_SAFETY_LEVEL=%q is not one of L0, L1, L2, L3", level)
	}

	radius := domain.BlastRadius(strings.TrimSpace(os.Getenv("OPSKEEPER_EDGE_MAX_BLAST_RADIUS")))
	if !radius.Valid() {
		return pluginmanifest.Policy{}, fmt.Errorf(
			"OPSKEEPER_EDGE_MAX_BLAST_RADIUS=%q is not one of pod, single-ns, namespace, cluster", radius)
	}

	scopes := defaultPluginScopes
	if raw := strings.TrimSpace(os.Getenv("OPSKEEPER_EDGE_PLUGIN_SCOPES")); raw != "" {
		scopes = nil
		for _, s := range splitList(raw) {
			scopes = append(scopes, domain.Scope(s))
		}
	}

	return pluginmanifest.PolicyFor(level, radius, scopes), nil
}

// nodeProfilePolicy applies a named deployment profile, and refuses an
// environment that contradicts it.
//
// The refusal is the point of the function. A profile exists so that "which
// policy is this host running" has one answer, and it is checkable remotely.
// A node that allowed OPSKEEPER_EDGE_MAX_SAFETY_LEVEL to override the
// profile's ceiling would be a node whose answer depends on which of two
// settings somebody edited last — and the setting that widens a fleet's
// blast radius is exactly the one that would get edited during an incident
// and never removed afterwards.
//
// Unset fields are fine: they mean the operator let the profile decide,
// which is the intended way to use it.
func nodeProfilePolicy(p pluginmanifest.Profile) (pluginmanifest.Policy, error) {
	if raw := strings.TrimSpace(os.Getenv("OPSKEEPER_EDGE_MAX_SAFETY_LEVEL")); raw != "" {
		if domain.SafetyLevel(raw) != p.Ceiling {
			return pluginmanifest.Policy{}, fmt.Errorf(
				"OPSKEEPER_EDGE_MAX_SAFETY_LEVEL=%q contradicts profile %q, whose ceiling is %s; "+
					"unset it to let the profile decide, or change the profile",
				raw, p.Name, p.Ceiling)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("OPSKEEPER_EDGE_MAX_BLAST_RADIUS")); raw != "" {
		if domain.BlastRadius(raw) != p.MaxRadius {
			return pluginmanifest.Policy{}, fmt.Errorf(
				"OPSKEEPER_EDGE_MAX_BLAST_RADIUS=%q contradicts profile %q, whose ceiling is %q; "+
					"unset it to let the profile decide, or change the profile",
				raw, p.Name, p.MaxRadius)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("OPSKEEPER_EDGE_PLUGIN_SCOPES")); raw != "" {
		declared := domain.Scopes{}
		for _, scope := range splitList(raw) {
			declared = append(declared, domain.Scope(scope))
		}
		if missing := scopesMissingFrom(declared, p.Granted); len(missing) > 0 {
			// Narrowing is refused even though it is the safe direction.
			// A host that grants less than its profile says is a host
			// where a plugin installs everywhere but here, and the
			// symptom is a fleet that is 90% upgraded with no error
			// anywhere — the release simply never finishes and nobody
			// knows why.
			return pluginmanifest.Policy{}, fmt.Errorf(
				"OPSKEEPER_EDGE_PLUGIN_SCOPES omits %s, which profile %q grants; "+
					"unset it to take the profile's grant, or change the profile",
				strings.Join(missing, ", "), p.Name)
		}
	}
	return p.Policy(), nil
}

// scopesMissingFrom reports which of want are absent from have, in want's
// order, so the message names them the way the profile does.
func scopesMissingFrom(have, want domain.Scopes) []string {
	var missing []string
	for _, s := range want {
		if !have.Has(s) {
			missing = append(missing, string(s))
		}
	}
	return missing
}

// nodeBootPolicy is the policy the boot path reviews its bundle against:
// nodePluginPolicy plus the node's own version.
//
// It is a separate function rather than two lines in startNodeAgent so the
// compatibility rule has exactly one implementation. A boot path that
// built its own policy would be a second place for "which version am I"
// to be answered, and the two would eventually disagree — at which point a
// package would be admissible at boot and refused at runtime, or the
// reverse, and nobody would be able to say which was right.
func nodeBootPolicy(buildVersion string) (pluginmanifest.Policy, error) {
	pol, err := nodePluginPolicy()
	if err != nil {
		return pluginmanifest.Policy{}, err
	}
	pol.NodeVersion = bootSelfVersion(buildVersion)
	// The agent's version is read the same way and with the same
	// fail-closed default. It is not derivable from the edge's build:
	// they are two binaries upgraded on two cadences, and a boot path
	// that reused one number for both would enforce a package's
	// min_pig_version against the wrong thing.
	pol.PigVersion = pigSelfVersion()
	return pol, nil
}

// loadTrustStore reads the node's publisher keys, if it has any.
//
// A node with no trust store has no opinion about who may publish for it,
// and that is a different state from a node whose trust store is empty by
// mistake. The first is a deployment that has not been set up; the second
// is indistinguishable from the first, which is why the absence is logged
// rather than swallowed — see admitPackages.
func loadTrustStore() *pluginmanifest.TrustStore {
	path := strings.TrimSpace(os.Getenv("OPSKEEPER_EDGE_TRUST_STORE"))
	if path == "" {
		return pluginmanifest.NewTrustStore()
	}
	store, err := pluginmanifest.TrustStoreFromConfig(path)
	if err != nil {
		// A configured but unreadable trust store is a hard error, not a
		// fall back to unsigned packages: the operator asked for
		// verification and the node cannot do it, and quietly running
		// unsigned is the one answer that is certainly wrong.
		store = pluginmanifest.NewTrustStore()
		store.SetLoadError(err)
	}
	return store
}

// admitPackages reviews each package and returns the admitted ones.
//
// Admission is a gate, not a formality. A package that fails is not written
// into settings at all, so the agent never starts with it: the alternative
// is a node that boots, loads code whose declared capabilities nobody
// verified, and finds out during an incident.
//
// The review runs in a fixed order — signature, then manifest, then this
// node's policy — and the order is the security property. A package's own
// description of itself is not consulted until the bytes it is written in
// have been authenticated, so a package that lies about its capabilities
// is refused by the signature step rather than by a later check that would
// have believed it.
//
// A package that does not target the edge is refused here even though it
// validates. Manifest targets are a declaration, and a declaration the node
// does not honour is worse than none at all - it would read as "this
// plugin was reviewed for the control plane" while running on a host.
func admitPackages(roots []string, trust *pluginmanifest.TrustStore, pol pluginmanifest.Policy) ([]pluginmanifest.Plugin, error) {
	if trust == nil {
		trust = pluginmanifest.NewTrustStore()
	}
	// A node that has never been told who it trusts runs unsigned packages,
	// and says so, out loud, on every boot.
	//
	// This is a transitional default and it is the wrong default, which is
	// why it is a default at all: OpsKeeper does not ship a release private
	// key — shipping one would make every node trust whoever holds the
	// repository — so "signatures required" cannot be true on a node that
	// has not been given a key. The switch is the trust store's own
	// presence: configure one and every package must verify, and there is
	// no configuration that turns that back off. A node without a key is a
	// node that has not finished being provisioned, and it says so here
	// rather than being quietly permissive.
	pol.AllowUnsigned = len(trust.KeyIDs()) == 0
	if pol.AllowUnsigned {
		if loadErr := trust.LoadError(); loadErr != nil {
			return nil, fmt.Errorf("trust store: %w", loadErr)
		}
	}

	out := make([]pluginmanifest.Plugin, 0, len(roots))
	for _, root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("package %q: %w", root, err)
		}
		d := pluginmanifest.Review(abs, trust, pol)
		if !d.Allowed {
			return nil, fmt.Errorf("%s", d)
		}
		p, err := pluginmanifest.Load(abs)
		if err != nil {
			// Unreachable: Review loaded it a moment ago. Kept anyway
			// because a node that skips this on the strength of a
			// preceding call is a node that depends on nothing changing in
			// between, and the cost is one re-read of a YAML file.
			return nil, fmt.Errorf("package %q was admitted and then failed to load: %w", abs, err)
		}
		if !p.RunsOn("edge") {
			return nil, fmt.Errorf("package %q does not target the edge; it declares targets %v",
				p.Name(), p.Targets())
		}
		out = append(out, p)
	}
	// Sorted by path so the settings file is byte-stable across restarts. A
	// file that rewrites itself on every boot is a diff nobody can review.
	sort.Slice(out, func(i, j int) bool { return out[i].Root < out[j].Root })
	return out, nil
}

// packageRoots projects admitted packages back to the paths the agent's
// settings file carries.
func packageRoots(plugins []pluginmanifest.Plugin) []string {
	out := make([]string, 0, len(plugins))
	for _, p := range plugins {
		out = append(out, p.Root)
	}
	return out
}

// agentExtensions turns the node's admitted packages into the extension
// scopes its agent profile is written from.
//
// The two inputs are the same admission, read twice for two files that must
// not disagree: the settings file names the packages the agent will load,
// and the profile names the tools those packages may have offered. A node
// whose profile and package list were built from different things would
// look correct in both files and behave like neither.
//
// The manifest's tool list is attached to every extension the package
// declares, because the manifest reviews the package and not the extension.
// The gate makes the same pairing - it keys on tool names - so the profile
// and the gate agree about what was reviewed.
//
// A package that declares an extension whose name PiG will not produce is a
// boot error rather than a warning. The alternative is a profile that names
// an extension the agent never loads, which reads as coverage and is not:
// the tools that extension would have offered stay unconstrained.
func agentExtensions(plugins []pluginmanifest.Plugin) ([]agentprofile.Extension, error) {
	out := make([]agentprofile.Extension, 0, len(plugins))
	for _, p := range plugins {
		tools := p.Manifest.Spec.Tools.Names()
		for _, declared := range p.Extensions {
			name, err := pigprofile.ExtensionPublicName(declared)
			if err != nil {
				return nil, fmt.Errorf("package %q declares extension %q: %w", p.Name(), declared, err)
			}
			out = append(out, agentprofile.Extension{Name: name, Tools: tools})
		}
	}
	return agentprofile.Scopes(out)
}

// agentSettingsPath is where the agent actually reads its package list from,
// and getting this path wrong is the single most expensive silent failure in
// this file's history.
//
// PiG reads settings from exactly two places:
//
//	<AgentDir>/settings.json        the AGENT scope  (PIG_CODING_AGENT_DIR)
//	<Cwd>/.pig/settings.json        the PROJECT scope
//
// The node used to write the project one. That made every admitted package a
// PROJECT-scope package, and a project package is loaded only when the
// agent's discovery list admits the "workspace" source -- while the node
// profile deliberately admits only "user". So the agent started, reported
// healthy, was offered PiG's built-ins minus the ones the profile strips
// (codemode and tool_search), and had not one plugin tool from any manifest
// it had admitted. No error, no diagnostic, clean exit: the gate saw a node
// that had loaded nothing and could not say so.
//
// The agent scope also needs no trust, where a project scope needs a trusted
// project -- and a node has no way to answer a trust prompt. So the agent
// scope is both the one that works and the one that can be relied on to.
//
// The fallback is the old project path, for a node with no agent scope
// configured: same behaviour as before, still loadable, still reviewable.
func agentSettingsPath(cwd string) string {
	if dir := strings.TrimSpace(os.Getenv(agentmodel.ConfigDirEnv)); dir != "" {
		return filepath.Join(dir, agentSettingsFile)
	}
	return filepath.Join(cwd, agentConfigDirName(), agentSettingsFile)
}

// writeAgentSettings points the agent at the admitted packages.
//
// It replaces the file rather than editing it, and it is written to a
// temporary name and renamed, so a node that is killed halfway through
// this leaves either the old package set or the new one - never a
// truncated file that would start an agent with no plugins at all and no
// explanation.
func writeAgentSettings(cwd string, packages []string) (string, error) {
	if cwd == "" {
		return "", errors.New("agent: no working directory to write settings into")
	}
	path := agentSettingsPath(cwd)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("agent: create %s: %w", filepath.Dir(path), err)
	}

	settings := agentSettings{Packages: make([]string, 0, len(packages))}
	for _, abs := range packages {
		settings.Packages = append(settings.Packages, fileURL(abs))
	}
	body, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", fmt.Errorf("agent: encode settings: %w", err)
	}
	body = append(body, '\n')
	final := path

	tmp, err := os.CreateTemp(filepath.Dir(path), agentSettingsFile+".*")
	if err != nil {
		return "", fmt.Errorf("agent: stage settings: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("agent: write settings: %w", err)
	}
	// The agent reads this as the user it runs as; 0640 keeps it readable
	// to that user and to root, and off the rest of the host.
	if err := tmp.Chmod(0o640); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("agent: chmod settings: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("agent: close settings: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", fmt.Errorf("agent: install settings: %w", err)
	}
	return final, nil
}

// fileURL renders an absolute path the way the agent's package loader
// expects it.
func fileURL(abs string) string {
	return "file://" + (&url.URL{Path: filepath.ToSlash(abs)}).String()
}

// splitList reads a comma-separated environment list.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
