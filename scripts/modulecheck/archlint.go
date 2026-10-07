package main

// The second gate, and the hole between the two gates.
//
// .go-arch-lint.yml is the only place in this repository where a
// *component* — a whole directory tree — is granted permission to import
// another whole directory tree. Every other boundary statement is either
// the Go module system, which is exact, or a rule in this file, which names
// exact import paths. The yml is different, and the difference is not
// stylistic: `manager_biz: mayDependOn: [manager_service]` means every file
// under core/manager/biz/ may import every package under
// core/manager/service/, today and after any change to either tree.
//
// That is a legitimate way to state an architecture — "biz may see service"
// is how most Go codebases are drawn. It stops being legitimate the moment a
// *specific* edge needs it, because the grant is then carrying one import
// while authorising all the ones nobody asked for. And nothing noticed when
// that happened here, because no check reads the yml at all. So:
//
//   - a grant added for one import survives the import's removal, silently,
//     and the hole it leaves is invisible to every gate in the repository;
//   - a grant added for one import also covers the *next* import added under
//     it — the case the doc names, where a new manager_biz → manager_data
//     store is red nowhere.
//
// The second case is not hypothetical, and it is the reason this file is
// written in the past tense. core/manager/biz/imbridge/adapter.go reached up
// into core/manager/service/aiops and nothing objected: modulecheck's layer
// rule only covers service→data and biz→data, and the yml's
// component-granular grant waved the imbridge edge through. That edge was a
// real inversion — a use case holding a concrete HTTP-layer service — and
// until this file existed it was debt nobody had named.
//
// Decision 280 deleted the file (the wiring moved to the composition root)
// and the entry in the file-granular ledger outlived it by two decisions,
// because the test that reports a stale entry had not been run since. So
// this paragraph said "today" about a file that no longer existed — a
// narration that reads as a live claim and is indistinguishable from one.
// The tense here is load-bearing: a reader has to be able to tell, without
// running anything, whether the example is still in the tree.
//
// So this file reads the yml and checks it, and it holds a file-granular
// ledger for the edges that exist to pay for a known inversion rather than
// to state the architecture. The ledger is the same bargain layerDebt makes
// on the other side of the boundary, and for the same reason: an entry that
// is no longer a debt is worse than no entry, because the next reader cannot
// tell it apart from a live one.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// archLintConfig is the part of .go-arch-lint.yml this checker reads.
//
// It is decoded rather than scanned. A line-oriented scan of a YAML file is
// the kind of shortcut that produces a gate reporting "all clear" because
// it stopped understanding the file — and a boundary checker that silently
// stops checking is strictly worse than one that is absent, because it is
// still trusted. Decoding also means an `in:` glob or a `mayDependOn` list
// that grows a shape this struct does not model becomes a loud error rather
// than a silently ignored line.
type archLintConfig struct {
	Version      int                          `yaml:"version"`
	Workdir      string                       `yaml:"workdir"`
	Components   map[string]archLintComponent `yaml:"components"`
	Deps         map[string]archLintDeps      `yaml:"deps"`
	ExcludeFiles []string                     `yaml:"excludeFiles"`
}

// archLintComponent is one declared component.
//
// `in:` is written two ways in the yml and both are in use: a scalar for a
// component that owns one tree, and a sequence for one that owns several
// (oxedge_runtime lists the ten core/edge directories that are not
// service/, biz/ or model/). Decoding only the scalar form is not a
// simplification — it is how a checker ends up believing those ten
// directories belong to no component at all, and therefore that everything
// they import is outside the boundary.
type archLintComponent struct {
	In archLintGlobs `yaml:"in"`
}

// archLintGlobs is a glob list that accepts the scalar or the sequence
// spelling, because the yml uses both and neither is a mistake on the
// writer's part.
type archLintGlobs []string

func (g *archLintGlobs) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var one string
		if err := value.Decode(&one); err != nil {
			return err
		}
		*g = archLintGlobs{one}
		return nil
	case yaml.SequenceNode:
		var many []string
		if err := value.Decode(&many); err != nil {
			return err
		}
		*g = archLintGlobs(many)
		return nil
	default:
		return fmt.Errorf("component `in:` must be a path or a list of paths, got %v", value.Tag)
	}
}

type archLintDeps struct {
	MayDependOn []string `yaml:"mayDependOn"`
}

// errNoArchLintConfig is returned when the tree declares no component
// boundary at all.
//
// It is a distinct error rather than a nil config so that "this tree has no
// yml" and "this tree has an empty yml" stay different answers. The second
// is a broken config and is reported; the first is a legitimate state for a
// tree that is not this repository, which is what every fixture in this
// package's tests is.
//
// The real repository losing its yml is not silently covered by this
// sentinel, and that is deliberate rather than an oversight: `make
// arch-lint` prints a warning naming the file as documentation-only when it
// is absent, and `make arch-lint-run` fails on it outright. The gate that
// knows how to complain about a missing config is the one whose schema
// requires it.
var errNoArchLintConfig = errors.New("modulecheck: this tree declares no .go-arch-lint.yml")

// loadArchLint reads and decodes the yml. A missing config is a distinct
// error from an empty one: "the file is not there" and "the file permits
// nothing" would otherwise produce the same silent all-clear, and only one
// of them is true.
func loadArchLint(root string) (*archLintConfig, error) {
	path := filepath.Join(root, ".go-arch-lint.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errNoArchLintConfig
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg archLintConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(cfg.Components) == 0 {
		return nil, fmt.Errorf("%s declares no components; the boundary it states would be vacuous", path)
	}
	if len(cfg.Deps) == 0 {
		return nil, fmt.Errorf("%s declares no deps; the file is inert and every grant is a guess", path)
	}
	return &cfg, nil
}

// archLintSortedComponents returns component names in a stable order, so
// that componentOf's first-match-wins is deterministic. A checker whose
// answer changes between two runs of an unchanged tree is one nobody can
// act on, and Go's map iteration is exactly that.
func archLintSortedComponents(cfg *archLintConfig) []string {
	names := make([]string, 0, len(cfg.Components))
	for name := range cfg.Components {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// archLintExcludes compiles the yml's exclude regexes.
//
// A regex Go cannot compile is substituted with one that matches nothing,
// which turns a broken exclusion into a silent inclusion. That is the wrong
// direction for a gate, but failing the whole run on a malformed regex would
// take down every other check with it, so the failure is made loud in the
// message instead and the pattern is treated as absent.
func archLintExcludes(cfg *archLintConfig) ([]*regexp.Regexp, []string) {
	out := make([]*regexp.Regexp, 0, len(cfg.ExcludeFiles))
	var broken []string
	for _, expr := range cfg.ExcludeFiles {
		re, err := regexp.Compile(expr)
		if err != nil {
			broken = append(broken, expr)
			continue
		}
		out = append(out, re)
	}
	return out, broken
}

func archLintExcluded(res []*regexp.Regexp, rel string) bool {
	// The yml matches against FileRelativePath, which go-arch-lint renders
	// with a leading separator. A path from filepath.Rel has none, so both
	// forms are tried: matching only one would silently un-exclude whichever
	// form the yml did not anticipate.
	slash := filepath.ToSlash(rel)
	for _, re := range res {
		if re.MatchString(slash) || re.MatchString("/"+slash) {
			return true
		}
	}
	return false
}

// archLintComponentOf returns the component that claims rel, or "".
func archLintComponentOf(cfg *archLintConfig, names []string, rel string) string {
	rel = filepath.ToSlash(rel)
	for _, name := range names {
		for _, glob := range cfg.Components[name].In {
			if glob == "" {
				continue
			}
			if archLintGlobMatch(glob, rel) {
				return name
			}
		}
	}
	return ""
}

// archLintGlobMatch implements the `in:` glob dialect: `**` spans directory
// separators, `*` does not.
//
// It is translated to a regexp rather than handed to filepath.Match, which
// has no `**` at all and would reject every pattern in the file. The result
// is compiled per call because these run a few hundred times per run and
// the alternative is a package-level cache whose invalidation nobody would
// remember to write.
func archLintGlobMatch(glob, path string) bool {
	var rx strings.Builder
	rx.WriteByte('^')
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				rx.WriteString(".*")
				i++
			} else {
				rx.WriteString("[^/]*")
			}
		case '?':
			rx.WriteString("[^/]")
		default:
			rx.WriteString(regexp.QuoteMeta(string(glob[i])))
		}
	}
	rx.WriteByte('$')
	return regexp.MustCompile(rx.String()).MatchString(path)
}

// archLintImportComponent maps an import path onto the component that owns
// it, by reducing it to the repository-relative directory the yml's globs
// are written against. An import outside this repository names no component,
// which is correct: the yml governs the project-component graph, not the
// vendor graph, and says so in its own `allow:` block.
func archLintImportComponent(cfg *archLintConfig, names []string, imp string) string {
	if !strings.HasPrefix(imp, repoModule+"/") {
		return ""
	}
	rel := strings.TrimPrefix(imp, repoModule+"/")
	// Both spellings are tried, and the reason is not tidiness. A component
	// is declared as a directory tree — `core/edge/service/**` — while an
	// import names a package, so the import of the directory's own root
	// package is the bare `core/edge/service` with no trailing separator.
	// Matching only the tree form makes that package belong to no
	// component, which in turn makes every grant into it look dead.
	//
	// That is not a hypothetical rounding error. It reported 33 of cmd's
	// grants as unused, including ones cmd demonstrably uses, and acting on
	// that answer would have deleted a third of the architecture file on the
	// strength of a missing slash. A checker that can be wrong in the
	// destructive direction has to be proven against a known import before
	// anyone is allowed to act on what it says.
	for _, candidate := range []string{rel, rel + "/"} {
		if name := archLintComponentOf(cfg, names, candidate); name != "" {
			return name
		}
	}
	return ""
}

// splitComponent separates a component name into its bounded context and
// its layer. "manager_biz" is ("manager", "biz"); "oxfloor" has no layer and
// yields ""; a vendor component like "oxpig_model" is ("oxpig", "model") and
// simply never pairs with a manager rule below.
func splitComponent(name string) (bc, layer string) {
	i := strings.LastIndex(name, "_")
	if i < 0 {
		return name, ""
	}
	return name[:i], name[i+1:]
}

// upwardEdges are the same-context imports that point at a layer above the
// importer.
//
// The list is written out rather than computed from a rank, because the
// layering is not a total order and pretending otherwise is how a rule ends
// up flagging the one direction that is correct. data sits *beside* biz
// rather than above or below it: biz declares the interface and data
// implements it, so data → biz is the intended direction and only its
// reverse is debt — and that reverse is already guarded by layerDebt, which
// is why it is absent here rather than duplicated.
//
// Everything on this list is a use case (or a model, or a store) holding a
// type from a layer meant to be downstream of it. The consequence is not a
// style complaint: the dependency arrow points the wrong way, so the layer
// above can no longer be changed, tested or replaced without dragging the
// layer below with it, and the cycle that eventually forms cannot be
// untangled by moving a package.
var upwardEdges = [][2]string{
	{"biz", "server"},
	{"biz", "service"},
	{"data", "server"},
	{"data", "service"},
	{"model", "server"},
	{"model", "service"},
	{"model", "biz"},
	{"model", "data"},
}

// isUpwardEdge reports whether an edge points from a layer at one layer
// above the one it names.
//
// It is a function rather than an inline loop because two checks ask the
// same question and their answers have to agree. When they disagreed — one
// treating a component with no layer suffix as never-upward, say — the
// unauthorized-edge check would have started reporting edges that the
// ledger check already reports with a better message, and a gate that says
// the same thing twice is a gate people learn to skim.
func isUpwardEdge(from, to string) bool {
	fromBC, fromLayer := splitComponent(from)
	toBC, toLayer := splitComponent(to)
	if fromBC == "" || fromBC != toBC {
		return false
	}
	for _, pair := range upwardEdges {
		if pair[0] == fromLayer && pair[1] == toLayer {
			return true
		}
	}
	return false
}

// archLintEdge is one real import between two components, kept at file
// granularity because a component-granular record cannot answer the only
// question that matters here, which is *which file* to name in the ledger.
type archLintEdge struct {
	From string // component
	To   string // component
	File string // repo-relative source path
	Imp  string // the import path
}

// checkArchLint reports every way the yml and the tree have drifted apart.
//
// It returns all of them rather than the first, for the reason the rest of
// this tool does: a person fixing boundaries wants the whole list in one
// pass, and a checker that stops at the first is one that gets run once and
// then worked around.
func checkArchLint(root string) ([]string, error) {
	cfg, err := loadArchLint(root)
	if err != nil {
		return nil, err
	}
	names := archLintSortedComponents(cfg)
	excludes, broken := archLintExcludes(cfg)

	var violations []string
	// unattached collects Go files no component in the yml claims. They are
	// gathered during the walk and reported after it, so the message order
	// does not depend on where in the tree the file sits.
	var unattached []string
	for _, expr := range broken {
		violations = append(violations, fmt.Sprintf(
			".go-arch-lint.yml: excludeFiles entry %q is not a valid regexp; it is being treated as "+
				"absent, so those files are counted", expr))
	}

	// observed records, per component, the set of components it actually
	// imports. A permission nothing uses is a hole with no floor under it;
	// a permission something unlisted uses is a hole already walked through.
	observed := map[string]map[string]bool{}
	seen := map[string]bool{}
	var edges []archLintEdge

	// The whole repository, not the module list. The module rules exist to
	// answer "may this module import that module", and the root module —
	// the one holding cmd/ and api/ — is not among them, so walking rules()
	// would have examined neither cmd/ nor api/ and would then reported
	// every grant the yml gives them as dead. The yml is a different
	// question: it covers every tree in the repository, including the ones
	// that are not a module of their own, and the answer has to be the
	// answer for the tree as it is rather than for the subset the module
	// checker happens to own.
	//
	// Vendored and fixture trees are left to the yml's own excludeFiles,
	// which are honoured above. Re-deriving the exclusion list here would
	// be a second place to forget an entry, and the one that gets updated
	// when a new vendored tree appears is the file the tool already reads.
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// The walk root is exempt. When the tool is invoked as
			// `modulecheck .` — which is how the Makefile calls it, and
			// how every developer types it — the root's own base name is
			// "." and the dot-directory rule below would classify the
			// repository as a hidden directory and skip the entire tree.
			//
			// That failure is silent and it is the worst shape a gate can
			// fail in: the walk completes without error, every component
			// looks like it imports nothing, and every grant in the yml is
			// reported as dead. It was found because the "no file imports
			// anything" answer is checkable against a file known to import
			// two hundred things, which is the only reason it was caught
			// before this reached a Makefile target anyone trusted.
			if path == root {
				return nil
			}
			base := info.Name()
			if base == "testdata" || strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		// A test is where a boundary gets exercised, not where it is
		// crossed — the same exemption the rest of this tool applies,
		// and for the same stated reason. Without it every integration
		// test in the control plane reads as an architecture violation,
		// and a gate that fires on its own tests gets switched off.
		if strings.HasSuffix(rel, "_test.go") || archLintExcluded(excludes, rel) {
			return nil
		}
		from := archLintComponentOf(cfg, names, rel)
		if from == "" {
			// A file no component claims is a file no mayDependOn rule
			// constrains, and go-arch-lint refuses to run when it finds
			// one ("not attached to any component"). This checker used to
			// read past it, so the one gate that runs without installing
			// a binary was blind to exactly the mistake the other gate
			// names: core/pig/pigmcp landed with no component, and nothing
			// said so until somebody ran the linter by hand. The two gates
			// answer the same question or the softer one is decoration.
			unattached = append(unattached, fmt.Sprintf(
				".go-arch-lint.yml: %s is not attached to any component, so no "+
					"mayDependOn rule constrains it; add it to a component in that file", rel))
			return nil
		}
		for _, imp := range importsOf(path) {
			to := archLintImportComponent(cfg, names, imp)
			if to == "" || to == from {
				continue
			}
			if observed[from] == nil {
				observed[from] = map[string]bool{}
			}
			observed[from][to] = true
			key := rel + "\x00" + imp
			if !seen[key] {
				seen[key] = true
				edges = append(edges, archLintEdge{From: from, To: to, File: rel, Imp: imp})
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk the tree for arch-lint edges: %w", walkErr)
	}

	sort.Slice(edges, func(i, j int) bool {
		if edges[i].File != edges[j].File {
			return edges[i].File < edges[j].File
		}
		return edges[i].Imp < edges[j].Imp
	})

	// 1. Every upward edge must be named, file by file.
	for _, e := range edges {
		if !isUpwardEdge(e.From, e.To) {
			continue
		}
		if _, ok := layerInversion[e.File]; ok {
			continue
		}
		violations = append(violations, fmt.Sprintf(
			"%s: %s imports %s (%s), a layer above itself; an upward edge needs a "+
				"file-granular entry in layerInversion naming this file and why, or the "+
				"%s mayDependOn %s grant in .go-arch-lint.yml covers it for free",
			e.File, e.From, e.Imp, e.To, e.From, e.To))
	}

	// 2. Every grant must be paying for something.
	//
	// A grant with no matching import is a permission nobody needed. It
	// cannot fail a build, cannot be noticed in review, and will still be
	// there in two years granting the next import for free — which is how a
	// boundary stops being a boundary without anyone deciding it should.
	depNames := make([]string, 0, len(cfg.Deps))
	for name := range cfg.Deps {
		depNames = append(depNames, name)
	}
	sort.Strings(depNames)
	for _, name := range depNames {
		allowed := append([]string(nil), cfg.Deps[name].MayDependOn...)
		sort.Strings(allowed)
		for _, target := range allowed {
			if target == name {
				continue // a self-edge is a statement about itself, not a grant
			}
			if observed[name][target] {
				continue
			}
			violations = append(violations, fmt.Sprintf(
				".go-arch-lint.yml: %s mayDependOn %s, but no file in %s imports anything in %s; "+
					"the grant is dead — delete it, or it keeps authorising the next import for free",
				name, target, archLintDirOf(cfg, name), archLintDirOf(cfg, target)))
		}
	}

	// 3. Every edge the tree actually has must be authorised, and the two
	// ways to authorise it are both file-granular on purpose.
	//
	// This is the symmetric half of check 2, and it is the half that was
	// missing. Check 2 walks the grants and asks whether each one is used;
	// nothing walked the imports and asked whether each one is allowed. An
	// import into a component that declares no grant produced no violation
	// at all, which is the exact shape of the hole this file exists to
	// close: a component whose rule was emptied to `anyVendorDeps` — the
	// expression of "may depend on nothing", used by five components after
	// the dead grants were deleted — could take a new cross-component
	// import and the reader would say nothing.
	//
	// go-arch-lint does police this, and that is not a reason to skip it
	// here. Two gates that both have to be run to catch one mistake is a
	// gate that is missed whenever either one is, and this one is wired
	// into `make module-check` while the arch-lint binary is a separate
	// target. The ledger is also an authorisation, not a permission: a
	// file named in layerInversion has been looked at by a person, so the
	// edge is known rather than merely tolerated.
	granted := map[string]map[string]bool{}
	for name, deps := range cfg.Deps {
		if granted[name] == nil {
			granted[name] = map[string]bool{}
		}
		for _, target := range deps.MayDependOn {
			granted[name][target] = true
		}
	}
	for _, e := range edges {
		// An upward edge is already check 1's business, and it arrives
		// there with the better remedy attached: a ledger entry naming this
		// file, not a grant naming a component. Reporting it a second time
		// would mean the same missing thing produces two violations with
		// two different suggested fixes.
		if isUpwardEdge(e.From, e.To) {
			continue
		}
		if granted[e.From][e.To] {
			continue
		}
		if _, ok := layerInversion[e.File]; ok {
			continue
		}
		violations = append(violations, fmt.Sprintf(
			"%s: %s imports %s (%s), which no rule in .go-arch-lint.yml permits; "+
				"add %s to %s mayDependOn, or a file-granular entry in layerInversion "+
				"saying why this one file needs it",
			e.File, e.From, e.Imp, e.To, e.To, e.From))
	}
	// 4. Every Go file must belong to a component.
	//
	// This is the check that makes the two gates answer the same question.
	// A file outside every component is invisible to checks 1-3 — they all
	// start from a component — so a package can arrive, take an import that
	// no rule permits, and be reported by nothing until someone remembers
	// to run go-arch-lint, which is a binary this environment does not
	// install. Reading the yml here means the enforced target says what the
	// optional one would have said.
	sort.Strings(unattached)
	violations = append(violations, unattached...)

	return violations, nil
}

// archLintDirOf renders a component's territory for a message. A component
// with one tree prints that tree; one with several prints them joined, which
// is the same information the yml states.
func archLintDirOf(cfg *archLintConfig, name string) string {
	if c, ok := cfg.Components[name]; ok {
		return strings.Join(c.In, ", ")
	}
	return name
}
