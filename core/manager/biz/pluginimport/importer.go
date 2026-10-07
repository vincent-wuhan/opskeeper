// Package pluginimport converts a legacy plugin container into a PiG
// package OpsKeeper can install.
//
// The legacy containers are three shapes the ecosystem already produces:
// the `.claude-plugin/plugin.json` form, the `openclaw.plugin.json` form,
// and the skills.sh drop of bare `skills/<name>/SKILL.md` directories. All
// three are read by the control plane today, as prompt and skill sources
// for the in-process agent. In 2.0 they become packages the node's own
// agent loads, and this is the bridge.
//
// The importer's governing rule is that it **invents nothing**. A legacy
// container carries no statement of what its tools do, no safety level, no
// scopes, and no blast radius — and every one of those is a decision
// somebody has to make deliberately before the code runs with a host's
// privileges. So the generated manifest declares the narrowest thing that
// loads: a read-only profile with no tools, and a report saying what is
// still undecided.
//
// The result is a package that installs, is inert, and is obviously
// incomplete. That is the correct output of a converter asked to translate
// a document that did not contain the information.
package pluginimport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// resourceDirs is the flattened view of domain.PackageResources: every
// legacy spelling, in the order PiG declares the classes, each paired with
// the package directory it is written to.
//
// The list is derived rather than written out, and that is the whole point of
// this converter having a coverage gate. A hand-written list here is a list
// somebody has to remember to update, and the failure is not an error — an
// unrecognised directory is simply not copied, so the package loads, reviews
// clean, and arrives on the node missing that resource class. A derived list
// can only fall behind PiG, and core/pig/pigcontract has the test that says
// so against the upstream constants.
var resourceDirs = func() []struct{ legacy, packaged string } {
	var out []struct{ legacy, packaged string }
	for _, r := range domain.PackageResources {
		for _, legacy := range r.LegacyNames {
			out = append(out, struct{ legacy, packaged string }{legacy: legacy, packaged: r.Kind})
		}
	}
	return out
}()

// The four shapes below moved to core/domain as PluginImport* (decision 241).
// The aliases are the whole change: `core/manager/server/marketplace/import.go`
// is handed this converter as a function by the composition root, and the only
// reason that route named this package is that the function type it holds
// spelled these two types. Naming them here made one HTTP file a cross-domain
// importer of a 726-line converter.
//
// They are renamed as well as moved. `Options` is declared thirteen times in
// this repository, `Report` eleven, `Decision` ten, and core/domain is the one
// namespace every domain shares; a bare `domain.Options` would be the
// fourteenth, and a reader who reached for it would get the wrong two fields.
// Every call site in this package keeps its old spelling through the aliases,
// which is why the diff below is four lines and not four hundred.

// Options configures an import. Declared in core/domain as
// PluginImportRequest; see that file for why the names in the shared namespace
// say which import they belong to.
type Options = domain.PluginImportRequest

// Decision is one thing a human still has to decide before the generated
// package can do anything. Declared in core/domain as PluginImportDecision.
type Decision = domain.PluginImportDecision

// SourceManifest is a read of a container's own package.json. Declared in
// core/domain as PluginImportSourceManifest.
type SourceManifest = domain.PluginImportSourceManifest

// Report is what an import produced. Declared in core/domain as
// PluginImportReport — including its json tags, which are the body of
// POST /v1/marketplace/import and are unchanged by the move.
type Report = domain.PluginImportReport

// Import converts one container into a package.
//
// It writes to a staging directory and moves it into place only after the
// generated package has been validated by the same loader the control plane
// uses. An import that produced a package nothing could load would leave an
// operator with a directory and no explanation.
// Importer is the converter with its one dependency injected.
//
// It used to be the package-level function `Import`, which called into the
// agent runtime's package loader directly. A package function cannot be given
// a dependency, so there was no port to narrow and the cross-domain import
// had nothing to cut except the call itself (decision 252, and the same shape
// decision 249 met on three other functions).
type Importer struct {
	loader domain.ContainerLoader
}

// New builds an importer that reads containers through loader.
//
// A nil loader is refused at use rather than at construction, so that a
// forgotten argument produces an error an operator can read instead of a nil
// dereference on the first upload.
func New(loader domain.ContainerLoader) *Importer {
	return &Importer{loader: loader}
}

// Import converts one container into a package.
//
// It writes to a staging directory and moves it into place only after the
// generated package has been validated by the same loader the control plane
// uses. An import that produced a package nothing could load would leave an
// operator with a directory and no explanation.
func (i *Importer) Import(opts Options) (*Report, error) {
	if i == nil || i.loader == nil {
		return nil, errors.New("pluginimport: no container loader is wired; the composition root must pass one to New")
	}
	if opts.Source == "" {
		return nil, errors.New("pluginimport: Source is required")
	}
	if opts.Dest == "" {
		return nil, errors.New("pluginimport: Dest is required")
	}
	source, err := filepath.Abs(opts.Source)
	if err != nil {
		return nil, fmt.Errorf("pluginimport: resolve source: %w", err)
	}
	dest, err := filepath.Abs(opts.Dest)
	if err != nil {
		return nil, fmt.Errorf("pluginimport: resolve dest: %w", err)
	}
	// Refuse to write inside the source: a converter that copied a
	// directory into itself would recurse, and one that overwrote it would
	// destroy the original before anybody looked at it.
	if within(dest, source) {
		return nil, fmt.Errorf("pluginimport: destination %s is inside the source %s", dest, source)
	}
	if _, err := os.Stat(dest); err == nil {
		return nil, fmt.Errorf("pluginimport: %s already exists; an import replaces nothing, because a merge would leave reviewed-by-nobody files behind", dest)
	}
	targets := opts.Targets
	if len(targets) == 0 {
		targets = domain.Targets{domain.TargetEdge}
	}

	// The loader does the recognising and the parsing. This package does
	// not have a second opinion about what counts as a container, because
	// two opinions would eventually disagree and the converter would be the
	// one that wins by being newer.
	//
	// One call, not two. This used to be LoadPluginContainer followed by
	// DetectContainer, which probed the same directory twice and let the
	// report's `kind` come from a different reading than the pack that the
	// first call had produced. The port returns both together because they
	// are one answer, not two.
	src, err := i.loader.LoadContainer(source)
	if err != nil {
		return nil, fmt.Errorf("pluginimport: read %s: %w", source, err)
	}

	staging, err := os.MkdirTemp(filepath.Dir(dest), ".pluginimport-*")
	if err != nil {
		return nil, fmt.Errorf("pluginimport: stage: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	report := &Report{Kind: src.Kind, Warnings: src.Warnings}
	if m, err := readSourceManifest(source); err != nil {
		return nil, err
	} else {
		report.SourceManifest = m
	}
	report.Name = nameOf(src, source)
	report.Version = versionOf(src)
	report.Description = descriptionOf(src)

	if err := copyResources(source, staging, report); err != nil {
		return nil, err
	}
	if err := writeManifest(staging, report, opts, targets); err != nil {
		return nil, err
	}
	report.Decisions = decisionsFor(src, report)

	// The generated package is proven loadable before it is moved into
	// place, using the same loader that will admit it later. An import
	// that produced something the host could not read would be a
	// directory an operator had to debug by hand.
	if _, err := pluginmanifest.Load(staging); err != nil {
		return nil, fmt.Errorf("pluginimport: the generated package does not load: %w", err)
	}

	if err := os.Rename(staging, dest); err != nil {
		return nil, fmt.Errorf("pluginimport: install at %s: %w", dest, err)
	}
	return report, nil
}

// readSourceManifest reads the container's own package.json, if it has one.
//
// It reads rather than copies, and the shape it returns says why. A `pi`
// block is PiG's "these are the only resources this package has" and a `pig`
// block is the same claim for hooks, MCP servers and agent environments;
// carrying either across would carry the suppression with it, and the
// converted package would load fewer resources than the container did.
//
// A package.json with neither block is npm metadata, and is reported as
// present but not declaring — which is a different fact, because an
// extension that needs its dependencies installed is a decision a reviewer
// has to make and this converter is not going to make it by copying a file
// that may carry a postinstall script.
func readSourceManifest(source string) (SourceManifest, error) {
	out := SourceManifest{}
	raw, err := os.ReadFile(filepath.Join(source, "package.json"))
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("pluginimport: read package.json: %w", err)
	}
	out.Present = true

	var doc struct {
		PI *struct {
			Extensions *[]string `json:"extensions"`
			Skills     *[]string `json:"skills"`
			Prompts    *[]string `json:"prompts"`
			Themes     *[]string `json:"themes"`
		} `json:"pi"`
		Pig *struct {
			Hooks             *[]string `json:"hooks"`
			MCPServers        *[]string `json:"mcpServers"`
			AgentEnvironments *[]string `json:"agentEnvironments"`
		} `json:"pig"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		// A package.json this converter cannot read is reported rather than
		// refused: refusing would block an import of a container whose
		// resources are all discoverable by convention anyway, and the
		// operator is better served by a package plus a question than by an
		// error about a file the package does not need.
		out.Entries = map[string][]string{}
		out.Classes = []string{"(unreadable package.json)"}
		// Declares is true even though nothing was read. It means "this
		// manifest may have declared resources and the converter could not
		// find out", which is the one state in which silence would be
		// worst: the package loads by convention and serves a superset, and
		// only somebody reading this field learns that the superset was
		// never a decision anybody made.
		out.Declares = true
		return out, nil
	}
	if doc.PI == nil && doc.Pig == nil {
		return out, nil
	}
	out.Declares = true
	out.Entries = map[string][]string{}
	add := func(kind string, entries *[]string) {
		if entries == nil {
			return
		}
		out.Classes = append(out.Classes, kind)
		out.Entries[kind] = append([]string(nil), *entries...)
	}
	if doc.PI != nil {
		add("extensions", doc.PI.Extensions)
		add("skills", doc.PI.Skills)
		add("prompts", doc.PI.Prompts)
		add("themes", doc.PI.Themes)
	}
	if doc.Pig != nil {
		add("hooks", doc.Pig.Hooks)
		// PiG spells the class "mcp" and the manifest key "mcpServers".
		// The report uses the class name, because that is what a reader
		// compares against the package directory it will be looking for.
		add("mcp", doc.Pig.MCPServers)
		add("agent-environments", doc.Pig.AgentEnvironments)
	}
	sort.Strings(out.Classes)
	return out, nil
}

// copyResources copies every recognised resource directory across.
//
// The written set is tracked rather than inferred from the report counters,
// because two legacy spellings can name one class (`commands` and `prompts`
// are both PiG's `prompts`) and the second one must not clobber the first.
// It used to be inferred, via `report.Prompts > 0`, which happened to work
// only because `commands` was the sole remap and the sole class whose count
// was written before the check.
//
// A collision is now a reported warning rather than a silent skip. The
// container shipped two directories for one class, the converter carried one
// of them, and the operator is entitled to know which. Silently preferring
// one is the failure this file exists to prevent, just one level up from the
// directory the files came from.
func copyResources(source, staging string, report *Report) error {
	written := map[string]string{}
	for _, dir := range resourceDirs {
		from := filepath.Join(source, dir.legacy)
		if _, err := os.Stat(from); err != nil {
			// A container is not required to have every resource class.
			continue
		}
		if previous, taken := written[dir.packaged]; taken {
			report.Warnings = append(report.Warnings, domain.LoadWarning{
				Path: filepath.Join(source, dir.legacy),
				Code: "resource_directory_collides",
				Reason: fmt.Sprintf(
					"%s and %s both name PiG's %q class, so only %s was copied; "+
						"merge the two by hand and re-import if the second directory held anything",
					previous, dir.legacy, dir.packaged, previous),
			})
			continue
		}
		to := filepath.Join(staging, dir.packaged)
		n, err := copyTree(from, to)
		if err != nil {
			return fmt.Errorf("pluginimport: copy %s: %w", dir.legacy, err)
		}
		written[dir.packaged] = dir.legacy
		switch dir.packaged {
		case "skills":
			report.Skills = relFiles(staging, to)
		case "agents":
			report.Agents = relFiles(staging, to)
		case "prompts":
			report.Prompts = n
		case "mcp":
			report.MCP = n
		case "extensions":
			report.Extension = n
		case "themes":
			report.Themes = n
		case "agent-environments":
			report.AgentEnvironments = n
		}
	}
	return nil
}

// copyTree copies a directory, returning the number of files written.
//
// Symlinks are not followed. A container that links outside its own tree
// would otherwise be copied as a file whose content came from somewhere the
// operator never reviewed, and the review surface for a package is its
// files.
func copyTree(from, to string) (int, error) {
	n := 0
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(to, 0o750)
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o750)
		}
		if d.Type()&os.ModeSymlink != 0 {
			// Recorded as zero copies rather than an error: a container
			// with a symlink is unusual, and refusing the whole import for
			// one would be a worse answer than importing the rest and
			// letting the operator see the count.
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if err := copyFile(path, dst); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}

func copyFile(from, to string) (err error) {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm()|0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(dst, src)
	return err
}

// relFiles lists a copied tree's files relative to the package root.
func relFiles(root, tree string) []string {
	var out []string
	_ = filepath.WalkDir(tree, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if rel, err := filepath.Rel(root, path); err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// within reports whether child is inside parent.
func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// nameOf is the package name, falling back to the directory.
//
// The fallbacks live here rather than in the port, and that is the point of
// the split: a source that states no name still converts, and what this
// repository names it is this repository's decision rather than the loader's.
func nameOf(src domain.ContainerSource, source string) string {
	if n := strings.TrimSpace(src.ID); n != "" {
		return n
	}
	if n := strings.TrimSpace(src.DisplayName); n != "" {
		return n
	}
	return filepath.Base(source)
}

// versionOf is the source version, defaulted rather than left blank.
//
// The manifest refuses a blank version, and a converted package with no
// version would be un-updatable. Zero is the honest reading of "the source
// did not say", and it is comparable enough for a first import to be
// followed by a real one.
func versionOf(src domain.ContainerSource) string {
	if v := strings.TrimSpace(src.Version); v != "" {
		return v
	}
	return "0.0.0"
}

func descriptionOf(src domain.ContainerSource) string {
	return strings.TrimSpace(src.Description)
}

// writeManifest emits the governance sidecar.
//
// The header is part of the output, not decoration. The file a reviewer
// opens is this one, and the most important thing it has to say is that
// everything below the header is a default nobody chose.
func writeManifest(dir string, report *Report, opts Options, targets domain.Targets) error {
	name := report.Name
	if name == "" {
		return errors.New("pluginimport: the source has no usable name")
	}
	version := report.Version
	if version == "" {
		version = "0.0.0"
	}

	var b strings.Builder
	b.WriteString("# pig-ops.yaml — GENERATED by pluginimport. Review before installing.\n")
	b.WriteString("#\n")
	b.WriteString("# Converted from a " + string(report.Kind) + " container. That format carries no\n")
	b.WriteString("# statement of what this package's code does, so nothing below was derived\n")
	b.WriteString("# from it. Every value here is the narrowest one that loads, chosen so the\n")
	b.WriteString("# package installs inert rather than not at all.\n")
	b.WriteString("#\n")
	b.WriteString("# The package currently declares no tools, which means the host's allow-list\n")
	b.WriteString("# is empty and every tool call it makes is refused. That is the intended\n")
	b.WriteString("# state of an unreviewed import. See spec.tools below.\n")
	b.WriteString("apiVersion: opskeeper.io/v1\n")
	b.WriteString("kind: Plugin\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + yamlScalar(name) + "\n")
	b.WriteString("  version: " + yamlScalar(version) + "\n")
	if opts.Vendor != "" {
		b.WriteString("  vendor: " + yamlScalar(opts.Vendor) + "\n")
	}
	b.WriteString("spec:\n")
	b.WriteString("  targets: [" + strings.Join(targetNames(targets), ", ") + "]\n")
	b.WriteString("\n")
	b.WriteString("  # L1: read-only, no approval. The one level a converter may choose,\n")
	b.WriteString("  # because it is the only one that adds no authority. Raising it is a\n")
	b.WriteString("  # review decision, and the level the tools below were reviewed at.\n")
	b.WriteString("  safety_level: L1\n")
	b.WriteString("\n")
	b.WriteString("  # Declared so the manifest loads. It is not a claim that the package is\n")
	b.WriteString("  # read-only — see spec.tools, which is what actually decides that.\n")
	b.WriteString("  capabilities: [read]\n")
	b.WriteString("\n")
	b.WriteString("  # THE ALLOW-LIST. Empty on purpose: no tool has been reviewed, so no tool\n")
	b.WriteString("  # is permitted, and the host refuses everything else. Add one line per tool\n")
	b.WriteString("  # this package is allowed to expose, with the class it actually has:\n")
	b.WriteString("  #\n")
	b.WriteString("  #   tools:\n")
	b.WriteString("  #     - { name: host_lsof, class: read }\n")
	b.WriteString("  #\n")
	b.WriteString("  # A tool listed here is a tool somebody agreed this package may run. A\n")
	b.WriteString("  # tool the agent finds that is not listed is refused on every turn.\n")
	b.WriteString("  tools: []\n")
	b.WriteString("\n")
	b.WriteString("  # Credentials are injected per scope and only per scope, so this list is\n")
	b.WriteString("  # the complete set of secrets this package will ever hold. Empty until\n")
	b.WriteString("  # somebody decides which ones it needs.\n")
	b.WriteString("  required_scopes: []\n")
	b.WriteString("\n")
	b.WriteString("  audit:\n")
	b.WriteString("    # The host derives every ledger entry from its own gate. A converted\n")
	b.WriteString("    # package has no way to write one, and cannot be given one.\n")
	b.WriteString("    emits: true\n")
	b.WriteString("    mutates: false\n")
	b.WriteString("\n")
	b.WriteString("  approval:\n")
	b.WriteString("    # No tool is permitted, so nothing reaches a queue. Both become true the\n")
	b.WriteString("    # moment a write-class or destructive-class tool is added above.\n")
	b.WriteString("    required: false\n")
	b.WriteString("\n")
	b.WriteString("  install:\n")
	b.WriteString("    # pinned, not rolling: a converted package is a starting point for review,\n")
	b.WriteString("    # and a package that auto-upgraded on a node fleet before it had been\n")
	b.WriteString("    # looked at would defeat the review entirely.\n")
	b.WriteString("    strategy: pin\n")

	path := filepath.Join(dir, pluginmanifest.ManifestFile)
	if err := os.WriteFile(path, []byte(b.String()), 0o640); err != nil {
		return fmt.Errorf("pluginimport: write manifest: %w", err)
	}
	return nil
}

func targetNames(targets domain.Targets) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, string(t))
	}
	return out
}

// yamlScalar quotes a value when leaving it bare would change its meaning.
//
// A plugin named `123` or `yes` is a real thing, and an unquoted one
// becomes a number or a boolean on the way back in. The importer's job is
// to produce a manifest that round-trips to the same name.
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '/':
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe && !isYamlBooleanOrNumber(s) {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// isYamlBooleanOrNumber reports whether a bare YAML scalar would come back
// as something other than a string.
func isYamlBooleanOrNumber(s string) bool {
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "null", "~":
		return true
	}
	if s == "" {
		return true
	}
	// Anything that parses as a number comes back as one. The strconv
	// round trip is the test: a name that survives ParseFloat unchanged is
	// a number to YAML even if it looks like a name.
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err == nil {
		if got := fmt.Sprintf("%g", f); got == s {
			return true
		}
	}
	return false
}

// decisionsFor is the list of what a human still has to settle.
//
// Every entry says why the answer is not derivable, because "missing
// value" and "cannot be inferred without a review" are different problems
// and an operator should know which one they are looking at.
func decisionsFor(src domain.ContainerSource, report *Report) []Decision {
	out := []Decision{
		{
			Field:    "spec.tools",
			Question: "Which tools may this package expose, and what class does each have?",
			Why: "The container format has no vocabulary for what a tool does. " +
				"Only reading the extension source answers it, and a class guessed " +
				"from a tool's name is exactly the guess the allow-list exists to prevent.",
		},
		{
			Field:    "spec.required_scopes",
			Question: "Which credentials does this package need?",
			Why: "Scopes are credentials, and a credential granted speculatively is a " +
				"credential in the hands of code nobody has read. " +
				"An empty list grants nothing and is the correct starting point.",
		},
		{
			Field:    "spec.safety_level",
			Question: "Is L1 right, or does the package need more?",
			Why: "L1 was chosen because it adds no authority. It is a floor, not a finding — " +
				"raising it requires reading the code that the tools come from.",
		},
	}
	if report.Extension > 0 {
		out = append(out, Decision{
			Field:    "extensions/",
			Question: fmt.Sprintf("Do the %d extension(s) run in-process, and do they need credentials to do it?", report.Extension),
			Why: "An extension runs with the node's privileges. Whether it is safe as " +
				"third-party code depends on what it does, not on how it was packaged.",
		})
	}
	if report.SourceManifest.Declares {
		out = append(out, Decision{
			Field: "package.json (" + strings.Join(report.SourceManifest.Classes, ", ") + ")",
			Question: fmt.Sprintf(
				"The container declared its %s by manifest. The converted package has none, "+
					"so it discovers them by convention instead — which finds MORE than the "+
					"declaration selected. Are those extra resources intended to be served?",
				strings.Join(report.SourceManifest.Classes, " and ")),
			Why: "PiG loads only what a manifest with a resource block declares, and that " +
				"block suppresses convention discovery for every class it governs. Copying it " +
				"across would carry the suppression; dropping it discovers a superset. Neither " +
				"is the same set the container served, and only a person knows which of the " +
				"two the container meant.",
		})
	}
	if len(src.Warnings) > 0 {
		out = append(out, Decision{
			Field:    "(loader warnings)",
			Question: fmt.Sprintf("%d file(s) in the source did not parse cleanly. What were they?", len(src.Warnings)),
			Why: "A file the loader could not read is a file no review has read either. " +
				"Dropping it silently would leave a package that looks complete.",
		})
	}
	return out
}
