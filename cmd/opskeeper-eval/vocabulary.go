package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
	"github.com/vincent-wuhan/opskeeper/core/harness/vocabulary"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
	managerbizloop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/loop/investigatorreal"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/toolset"
)

// opskeeper-eval vocabulary — can this build produce the answers the golden
// cases ask for?
//
// plugin-coverage answers a related question (can a *plugin package* serve
// this expectation) and this one answers the prior question: can the
// *system* serve it at all. Both had to be asked, because when the answer is
// no the score is a zero, and a zero on a leaderboard is read as the agent
// being wrong rather than the case being unpassable.
//
// The two vocabularies are joined here from declarations the production
// packages export — the root-cause enum is derived from the very JSON schema
// the investigated phase prompts the model with, and the action list is
// pinned to the literals the investigator actually passes to
// remediation(...) by a test that scrapes its own AST. Nothing in this
// output is a hand-maintained summary that can quietly go stale.
// defaultPluginsDir mirrors the default the coverage gate uses, so both
// gates describe the same fleet.
const defaultPluginsDir = "plugins/pig-ops"

// loopProviderName is the provider holding the closed loop's own
// remediation vocabulary. The assessment itself lives in the vocabulary
// package under the same name, so the report and the number cannot drift
// apart.
const loopProviderName = vocabulary.LoopProviderName

type vocabularyFlags struct {
	casesDir   string
	pluginsDir string
	filter     string
	kindMap    string
	failOnGap  bool
	jsonOut    bool
}

func cmdVocabulary(_ context.Context, args []string) error {
	var f vocabularyFlags
	fs := flag.NewFlagSet("vocabulary", flag.ExitOnError)
	fs.StringVar(&f.casesDir, "cases-dir", "core/harness/cases", "golden case 目录")
	fs.StringVar(&f.pluginsDir, "plugins-dir", defaultPluginsDir, "插件包目录")
	fs.StringVar(&f.filter, "filter", "", "只检查匹配的 case（如 host/ 或 pg/）")
	fs.StringVar(&f.kindMap, "kind-map", "", "校验 kind → case 词表映射文件（可选）")
	fs.BoolVar(&f.failOnGap, "fail-on-gap", false, "存在结构上不可满足的 case 时以非零码退出")
	fs.BoolVar(&f.jsonOut, "json", false, "输出 JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runVocabulary(f, os.Stdout)
}

// productionCapability reads every capability registry this binary has, as
// the union of the subsystems that own them.
//
// The providers are consulted in the order a reader cares about: the
// control-plane middleware adapters, then each shipped plugin package, then
// the loop's own remediation vocabulary. Order matters because ProviderOf
// credits the first match, and the adapter is the subsystem that would own a
// `<family>.<method>` gap.
//
// An error here is returned, never substituted with an empty capability. An
// empty capability would make every case report as unservable, and a report
// that says "nothing works" when the real problem is "a registry failed to
// load" is worse than no report.
// The second return is the middleware tools' required arguments, which the
// executability check needs and the Capability itself has nowhere to put:
// it describes what the loop can propose, not what each tool needs to run.
func productionCapability(pluginsDir string) (vocabulary.Capability, map[string][]string, error) {
	adapterTools, adapterRequired, err := middlewareAdapterTools()
	if err != nil {
		return vocabulary.Capability{}, nil, err
	}
	plugins, err := pluginmanifest.LoadAll(pluginsDir)
	if err != nil {
		return vocabulary.Capability{}, nil, fmt.Errorf("load plugins from %s: %w", pluginsDir, err)
	}
	providers := []vocabulary.Provider{{Name: "middleware-adapter", Symbols: adapterTools}}
	for _, p := range plugins {
		// A package declares which capability families it covers, not the
		// individual method names, so it is registered as family coverage.
		// Re-deriving per-method claims here would be a second opinion the
		// package never gave, and a false negative here reads as a missing
		// capability that somebody then goes and rebuilds.
		providers = append(providers, vocabulary.Provider{
			Name:     "plugin:" + p.Name(),
			Families: p.Capabilities(),
		})
	}
	// The loop's remediation planner is a fourth namespace: it is what a
	// closed-loop run actually proposes, so a case it cannot satisfy is
	// unscoreable by the loop even when an adapter offers the tool. It
	// speaks the same dotted shape, so it can be consulted alongside the
	// rest.
	providers = append(providers, vocabulary.Provider{
		Name:    "loop-investigator",
		Symbols: append([]string(nil), investigatorreal.RemediationActions...),
	})
	return vocabulary.Capability{Providers: providers}, adapterRequired, nil
}

// loopActionExecutability reports which actions the closed loop can propose
// that nothing in the build can execute.
//
// Only exact symbol coverage counts. A provider that declares a capability
// family is a real answer for "can this platform do host work", and a
// useless one for "can the system run pg.kill_backend right now" — a family
// claim cannot dispatch a specific action, and treating it as executable
// would report a closed loop that can act when it cannot.
//
// The check exists because loop.RemediationOption's own contract comment says
// "Action is the tool method name", and the production investigator has
// never satisfied it. Nothing in the loop path resolves Action to a tool —
// it reaches the chat UI as a display string — so an action nothing
// implements is invisible until a run tries to perform it.
// The second return value is the actions that are registered but cannot be
// dispatched from a loop action: the tool exists, and it needs an argument a
// RemediationOption does not carry. Those are reported apart from the
// unexecutable ones because they are a different kind of gap with a
// different fix — a resolver rather than an implementation — and folding
// them into one list would tell whoever picks this up to write an adapter
// for a tool that already exists.
func loopActionExecutability(cap vocabulary.Capability, adapterRequired map[string][]string) (unexecutable, undispatchable, executable []string) {
	actions, ok := loopActions(cap)
	if !ok {
		return nil, nil, nil
	}
	// The loop is excluded from the providers it is measured against.
	// Leaving it in makes the question "can the loop execute the name the
	// loop itself just wrote?", which is always yes, and the check then
	// reports that every action is executable while executing nothing —
	// a gate that is green because it asks the wrong question is worse than
	// no gate, because it looks like coverage.
	others := vocabulary.Capability{}
	for _, p := range cap.Providers {
		if p.Name == loopProviderName {
			continue
		}
		others.Providers = append(others.Providers, p)
	}
	for _, a := range actions {
		_, cov, found := others.ProviderOf(a)
		if !found || cov != vocabulary.CoverageExact {
			unexecutable = append(unexecutable, a)
			continue
		}
		// A loop action names a tool and a resource locator. It carries
		// no pid, no role, no table, so a tool that requires one is
		// covered by name and undone by argument.
		if req, needsArgs := adapterRequired[a]; needsArgs && len(req) > 0 {
			undispatchable = append(undispatchable, a)
			continue
		}
		executable = append(executable, a)
	}
	sort.Strings(unexecutable)
	sort.Strings(undispatchable)
	sort.Strings(executable)
	return unexecutable, undispatchable, executable
}

func loopActions(cap vocabulary.Capability) ([]string, bool) {
	for _, p := range cap.Providers {
		if p.Name == loopProviderName {
			return append([]string(nil), p.Symbols...), true
		}
	}
	return nil, false
}

// middlewareAdapterTools asks each middleware adapter to register its tools
// into a fresh registry and reports what landed there.
//
// This is the authoritative list, read by running the registration rather
// than by scraping the source for string literals. A scrape is a second
// parser with its own opinions about what counts as a tool name, and it
// would silently miss a tool registered through any path the scrape does
// not model — the exact failure a capability gate exists to prevent.
func middlewareAdapterTools() ([]string, map[string][]string, error) {
	// The list of adapters is toolset.Sources, not a copy of it. The gate
	// and the control plane have to agree about which namespaces exist —
	// that agreement is the whole reason the product-namespaced MQ families
	// are wired at boot — and two handwritten lists agree only until the
	// first adapter is added to one of them.
	reg, err := toolset.Registry()
	if err != nil {
		return nil, nil, err
	}
	tools := reg.ListTools("")
	// The required arguments come from the same live registration, because
	// they are the difference between "a tool by this name exists" and
	// "a loop action naming it can actually be performed". A
	// RemediationOption carries an action name and a resource locator —
	// never a pid or a role — so a tool that requires one cannot be
	// dispatched from a remediation without a resolver, and a gate that
	// only counts names would call that covered.
	required := make(map[string][]string, len(tools))
	for _, name := range tools {
		spec, ok := reg.LookupTool(name)
		if !ok {
			continue
		}
		if len(spec.RequiredArgs) > 0 {
			required[name] = spec.RequiredArgs
		}
	}
	sort.Strings(tools)
	return tools, required, nil
}

func runVocabulary(f vocabularyFlags, out *os.File) error {
	if f.pluginsDir == "" {
		f.pluginsDir = defaultPluginsDir
	}
	cap, adapterRequired, err := productionCapability(f.pluginsDir)
	if err != nil {
		return fmt.Errorf("read the production capability: %w", err)
	}
	cases, err := schema.NewLoader(f.casesDir).LoadAll()
	if err != nil {
		return fmt.Errorf("load cases from %s: %w", f.casesDir, err)
	}
	if f.filter != "" {
		kept := cases[:0]
		for _, c := range cases {
			if strings.Contains(c.ID, f.filter) {
				kept = append(kept, c)
			}
		}
		cases = kept
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })

	kinds, err := loop.RootCauseKinds()
	if err != nil {
		return fmt.Errorf("read the loop root-cause enum: %w", err)
	}
	gaps := vocabulary.CheckAll(cases, cap)
	// The closed loop is scored separately because it answers a different
	// question. A case can be perfectly servable by the platform — an
	// adapter registers the expected tool — and still be unscoreable from a
	// closed-loop run, because the loop's remediation planner proposes a
	// different name. That produces a zero on the judge for a reason no
	// amount of agent quality can fix, and a platform-only report would
	// say nothing about it.
	var kindResolve vocabulary.KindResolver
	if f.kindMap != "" {
		resolve, err := loadKindMap(f.kindMap)
		if err != nil {
			return err
		}
		// The two resolver types have the same shape; the conversion is
		// here so neither package has to know about the other.
		kindResolve = vocabulary.KindResolver(resolve)
	}
	loopCases := vocabulary.AssessLoop(cases, cap, kinds, kindResolve)
	unexecutable, undispatchable, _ := loopActionExecutability(cap, adapterRequired)
	undispatchable = append([]string(nil), undispatchable...)
	resolvableFromEvidence := map[string]struct{}{}
	for _, a := range managerbizloop.ActionsResolvableFromEvidence() {
		resolvableFromEvidence[a] = struct{}{}
	}
	// The kind map is analysed before anything is emitted, because in JSON
	// mode its verdict has to travel inside the single report document
	// rather than behind it.
	var kindMapReport *staleKindMapReport
	if f.kindMap != "" {
		kindMapReport = analyzeStaleKindMap(f.kindMap, cap)
	}
	stale := 0
	if kindMapReport != nil {
		stale = kindMapReport.StaleEntryCount
	}

	if f.jsonOut {
		if err := emitVocabularyJSON(out, cap, gaps, kinds, loopCases, f.kindMap, unexecutable, undispatchable, kindMapReport); err != nil {
			return err
		}
	} else {
		emitVocabularyText(out, cap, gaps, kinds, loopCases, f.kindMap)
		emitActionText(out, unexecutable, undispatchable, adapterRequired, len(loopCases))
		if kindMapReport != nil {
			emitStaleKindMapText(out, kindMapReport)
		}
	}

	if f.failOnGap {
		if len(unexecutable) > 0 {
			fmt.Fprintf(out, "%d of the closed loop's actions have no registered implementation\n", len(unexecutable))
		}
		if n := len(gaps) - vocabulary.ServableCount(gaps); n > 0 {
			return fmt.Errorf("%d of %d golden cases expect symbols this build cannot emit", n, len(gaps))
		}
		if stale > 0 {
			return fmt.Errorf("%d entries in %s map onto symbols this build cannot emit", stale, f.kindMap)
		}
	}
	return nil
}

// reportStaleKindMap checks a contract-kind → case-symbol mapping against
// the build's own capability set and returns the number of stale entries.
//
// The mapping is the one place in the evaluation path where a human states
// that two different vocabularies mean the same thing, so it is also the
// place a rename goes stale fastest: a tool is renamed, the map keeps
// pointing at the old name, and every run through it scores zero on
// remediation_quality with no error anywhere. A mapping file is data, so
// nothing about it breaks when it goes wrong — which is exactly why it needs
// something to check it against.
// staleKindMapReport is the verdict of the kind-map check. It is a field
// of the one report document, not a second document printed after it: a
// --json flag that emits two documents is a flag that breaks every parser
// pointed at it.
type staleKindMapReport struct {
	Path            string       `json:"path"`
	Entries         int          `json:"entries"`
	StaleEntryCount int          `json:"stale_entry_count"`
	Stale           []staleEntry `json:"stale,omitempty"`
	Error           string       `json:"error,omitempty"`
}

type staleEntry struct {
	Kind  string   `json:"kind"`
	Stale []string `json:"stale_symbols"`
}

// analyzeStaleKindMap reads the mapping and returns what it says about this
// build. It writes nothing: a mapping file that cannot be read or parsed is
// itself a finding, and it travels back to the caller as Error so that the
// JSON report can carry it rather than losing it.
func analyzeStaleKindMap(path string, cap vocabulary.Capability) *staleKindMapReport {
	raw, err := os.ReadFile(path)
	if err != nil {
		return &staleKindMapReport{Path: path, Error: fmt.Sprintf("unreadable: %v", err)}
	}
	var m map[string][]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return &staleKindMapReport{Path: path, Error: fmt.Sprintf("unparseable: %v", err)}
	}
	kinds := make([]string, 0, len(m))
	for k := range m {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	rep := &staleKindMapReport{Path: path, Entries: len(m)}
	for _, k := range kinds {
		var bad []string
		for _, sym := range m[k] {
			if _, _, ok := cap.ProviderOf(sym); !ok {
				bad = append(bad, sym)
			}
		}
		if len(bad) > 0 {
			rep.Stale = append(rep.Stale, staleEntry{Kind: k, Stale: bad})
		}
	}
	rep.StaleEntryCount = len(rep.Stale)
	return rep
}

// emitStaleKindMapText prints the verdict for a human reader. The JSON
// report carries the same facts in its kind_map field.
func emitStaleKindMapText(out *os.File, rep *staleKindMapReport) {
	if rep.Error != "" {
		fmt.Fprintf(out, "  kind map: %s\n", rep.Error)
		return
	}
	fmt.Fprintf(out, "\nKind map %s: %d entries, %d stale\n", rep.Path, rep.Entries, rep.StaleEntryCount)
	for _, e := range rep.Stale {
		fmt.Fprintf(out, "  STALE %-20s %s\n", e.Kind, strings.Join(e.Stale, " "))
	}
	if rep.StaleEntryCount == 0 {
		fmt.Fprintf(out, "  every mapped symbol is one this build can emit\n")
	}
}

func emitVocabularyText(out *os.File, cap vocabulary.Capability, gaps []vocabulary.Gap, kinds []string, loopCases []vocabulary.LoopCase, kindMap string) {
	fmt.Fprintf(out, "Capability registries consulted\n")
	for _, p := range cap.Providers {
		// Both counts are shown. A package that covers a family rather
		// than named tools would otherwise print as "0 symbols" and read
		// as contributing nothing at all.
		fmt.Fprintf(out, "  %-34s %d symbols, %d families %s\n",
			p.Name, len(p.Symbols), len(p.Families), p.Families)
	}
	// The root-cause enum is a different namespace from the tool names and
	// is reported separately: a root-cause *kind* is not a tool, so folding
	// it into the union would let a case look servable for the wrong reason.
	fmt.Fprintf(out, "  %-34s %d values: %s\n", "loop-root-cause-kinds", len(kinds), strings.Join(kinds, " "))
	fmt.Fprintln(out)
	for _, g := range gaps {
		if g.Servable() {
			fmt.Fprintf(out, "  ok   %-28s fully servable\n", g.CaseID)
			continue
		}
		fmt.Fprintf(out, "  GAP  %-28s %d root cause, %d remediation\n",
			g.CaseID, len(g.UnservableRootCauses), len(g.UnservableRemediations))
		for _, sym := range g.UnservableRootCauses {
			fmt.Fprintf(out, "         %s (root cause)\n", sym)
		}
		for _, sym := range g.UnservableRemediations {
			fmt.Fprintf(out, "         %s (remediation)\n", sym)
		}
	}
	fmt.Fprintf(out, "\n%d/%d golden cases are fully servable by this build\n",
		vocabulary.ServableCount(gaps), len(gaps))
	emitLoopCoverage(out, loopCases, kindMap)
}

// emitLoopCoverage prints the two axes a closed-loop run has to clear, and
// keeps them apart. The remediation axis is always assessable; the
// root-cause axis needs a kind map, and without one saying "0 of 20" would
// be reporting a missing input as a finding.
func emitLoopCoverage(out *os.File, cases []vocabulary.LoopCase, kindMap string) {
	fmt.Fprintf(out, "\nClosed-loop coverage (the two axes are separate)\n")
	fmt.Fprintf(out, "  remediation axis: %d/%d cases the loop can propose an accepted action for\n",
		vocabulary.LoopRemediationServableCount(cases), len(cases))
	if kindMap == "" {
		fmt.Fprintf(out, "  root-cause axis:  not assessed (pass --kind-map; without one the loop's\n")
		fmt.Fprintf(out, "                   kind enum has no declared route onto case vocabulary)\n")
	} else {
		fmt.Fprintf(out, "  root-cause axis:  %d/%d cases some kind in the enum maps onto\n",
			vocabulary.LoopRootCauseServableCount(cases), len(cases))
		fmt.Fprintf(out, "  both axes:       %d/%d cases a closed-loop run can actually score\n",
			vocabulary.LoopServableCount(cases), len(cases))
	}
	for _, c := range cases {
		if c.Servable() {
			continue
		}
		if len(c.MissingRemediations) > 0 {
			fmt.Fprintf(out, "    blocked %-26s no proposal for %s\n", c.CaseID, strings.Join(c.MissingRemediations, " "))
			continue
		}
		if c.RootCauseAssessed {
			fmt.Fprintf(out, "    blocked %-26s no kind maps onto an expected root cause\n", c.CaseID)
		}
	}
}

type vocabularyReport struct {
	Providers                        []vocabularyProvider `json:"providers"`
	RootCauseKinds                   []string             `json:"loop_root_cause_kinds"`
	Cases                            []vocabularyGap      `json:"cases"`
	Servable                         int                  `json:"servable"`
	Total                            int                  `json:"total"`
	LoopRemediationServable          int                  `json:"loop_remediation_servable"`
	LoopRootCauseAssessed            bool                 `json:"loop_root_cause_assessed"`
	LoopRootCauseServable            int                  `json:"loop_root_cause_servable"`
	LoopServable                     int                  `json:"loop_servable"`
	LoopActionsWithoutImplementation []string             `json:"loop_actions_without_implementation,omitempty"`
	// LoopActionsNeedingArguments are actions whose tool is registered but
	// whose required arguments a RemediationOption cannot carry. They are
	// separate from the list above because the fix is an argument
	// resolver, not an implementation.
	LoopActionsNeedingArguments []string            `json:"loop_actions_needing_arguments,omitempty"`
	LoopBlocked                 []vocabularyGap     `json:"loop_blocked,omitempty"`
	KindMap                     *staleKindMapReport `json:"kind_map,omitempty"`
}

type vocabularyProvider struct {
	Name     string   `json:"name"`
	Symbols  int      `json:"symbols"`
	Families []string `json:"families,omitempty"`
}

type vocabularyGap struct {
	CaseID                 string   `json:"case_id"`
	Servable               bool     `json:"servable"`
	UnservableRootCauses   []string `json:"unservable_root_causes,omitempty"`
	UnservableRemediations []string `json:"unservable_remediations,omitempty"`
	Reason                 string   `json:"reason,omitempty"`
}

func emitVocabularyJSON(out *os.File, cap vocabulary.Capability, gaps []vocabulary.Gap, kinds []string, loopCases []vocabulary.LoopCase, kindMap string, unexecutable, undispatchable []string, kindMapReport *staleKindMapReport) error {
	rep := vocabularyReport{
		RootCauseKinds:          kinds,
		Total:                   len(gaps),
		Servable:                vocabulary.ServableCount(gaps),
		LoopRemediationServable: vocabulary.LoopRemediationServableCount(loopCases),
		LoopRootCauseServable:   vocabulary.LoopRootCauseServableCount(loopCases),
		LoopServable:            vocabulary.LoopServableCount(loopCases),
		// Folded into this one document rather than printed as a second
		// one: a --json flag that emits two documents is a flag that
		// breaks every parser pointed at it.
		LoopActionsWithoutImplementation: unexecutable,
		LoopActionsNeedingArguments:      undispatchable,
		KindMap:                          kindMapReport,
	}
	rep.LoopRootCauseAssessed = kindMap != ""
	for _, lc := range loopCases {
		if lc.Servable() {
			continue
		}
		rep.LoopBlocked = append(rep.LoopBlocked, vocabularyGap{
			CaseID:                 lc.CaseID,
			Servable:               false,
			UnservableRemediations: lc.MissingRemediations,
			Reason:                 lc.Reason(),
		})
	}
	for _, p := range cap.Providers {
		rep.Providers = append(rep.Providers, vocabularyProvider{Name: p.Name, Symbols: len(p.Symbols), Families: p.Families})
	}
	for _, g := range gaps {
		rep.Cases = append(rep.Cases, vocabularyGap{
			CaseID:                 g.CaseID,
			Servable:               g.Servable(),
			UnservableRootCauses:   g.UnservableRootCauses,
			UnservableRemediations: g.UnservableRemediations,
			Reason:                 g.Reason(),
		})
	}
	doc, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", doc)
	return err
}

// emitActionText prints the executability finding, which is a different
// claim from either coverage number: these are actions the loop will write
// into a contract that no subsystem can carry out.
func emitActionText(out *os.File, unexecutable, undispatchable []string, required map[string][]string, caseCount int) {
	// The argument gap is split in two, because one half is "somebody has to
	// write an extractor" and the other is "the evidence does not contain
	// this value at all, so the investigator has to start recording it
	// first". Folding them into one list sends whoever picks this up to
	// write a resolver for a pod name that no tool ever observed.
	resolvableFromEvidence := map[string]struct{}{}
	for _, a := range managerbizloop.ActionsResolvableFromEvidence() {
		resolvableFromEvidence[a] = struct{}{}
	}
	fmt.Fprintf(out, "\nLoop action executability\n")
	// The two findings below are printed independently. The earlier shape
	// returned as soon as the unimplemented list was empty, which hid the
	// argument gap exactly when the build reached full name coverage —
	// the state where the argument question is the only one left to ask.
	if len(unexecutable) == 0 {
		fmt.Fprintf(out, "  every action the loop can propose has a registered implementation\n")
	} else {
		fmt.Fprintf(out, "  %d of the loop's actions have no registered implementation.\n", len(unexecutable))
		// The count above is actions, not cases. Conflating the two would read
		// as "16 of the 20 cases", which is a different and unsupported claim.
		fmt.Fprintf(out, "  Each is written into a RootCauseJSON as the fix to apply, and none resolves\n")
		fmt.Fprintf(out, "  to a tool, so a run that reached the recovery phase would be prescribed a\n")
		fmt.Fprintf(out, "  remedy nothing in this build can carry out (out of %d cases in the corpus):\n", caseCount)
		for _, a := range unexecutable {
			fmt.Fprintf(out, "         %s\n", a)
		}
	}
	if len(undispatchable) == 0 {
		return
	}
	fmt.Fprintf(out, "\n  %d action(s) name a registered tool that a loop action cannot supply\n", len(undispatchable))
	fmt.Fprintf(out, "  arguments to. A RemediationOption carries an action and a resource locator,\n")
	fmt.Fprintf(out, "  never the values the tool acts on, so the dispatch refuses rather than\n")
	fmt.Fprintf(out, "  guessing. `resolvable` means an extractor reads it from the recorded\n")
	fmt.Fprintf(out, "  evidence (it may still refuse when the evidence is ambiguous); the rest\n")
	fmt.Fprintf(out, "  need the investigator to record the value first — no resolver can find a\n")
	fmt.Fprintf(out, "  pod name that was never observed:\n")
	for _, a := range undispatchable {
		source := "no evidence records this value"
		if _, ok := resolvableFromEvidence[a]; ok {
			source = "resolvable"
		}
		fmt.Fprintf(out, "         %-28s needs %-24s %s\n", a, strings.Join(required[a], ", "), source)
	}
}
