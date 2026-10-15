// Command dcell is the measuring stick for one cell of the OpsKeeper
// progress table: plugin ecosystem.
//
// The cell used to read 95% with the residual written as "more plugins to
// migrate". That residual has no list, no acceptance line and no decidable
// end state, which is why two consecutive deliveries — the tool count going
// 91 -> 92 and the diagnosis axis going 17/20 -> 18/20 — moved every other
// reading in the ledger and left this one untouched. A cell that cannot
// measure its own progress trains everyone to ignore it.
//
// So this command restates the cell as a list. Each item is one deliverable
// the plan named for the plugin ecosystem, and each item carries a predicate
// that is evaluated against the working tree right now. The score is the
// fraction of items whose predicate holds. Adding a package, a route or a
// guard moves it; nothing else does.
//
// What this is NOT, stated here so the number is not over-read:
//
//   - The predicates read repository state. They do not execute a node, so
//     they cannot tell whether the review pipeline really blocks an unsigned
//     package. Behavioural claims stay with the behavioural gates:
//     make eval-gates (the diagnosis axis), make pig-tool-scoping-check
//     (the shipped tool set against a real pig binary) and the package
//     tests. This cell is a census, not a proof.
//
//   - An item whose predicate cannot be evaluated is reported as a failure on
//     stderr, never as closed. A checker that guesses is worse than no
//     checker, because the number it prints then means nothing.
//
// A predicate returns a shortfall sentence or nothing at all; it has no third
// "pass with a note" channel. An earlier draft of this file returned a note
// on success and the caller read it as a shortfall, so a green item printed
// as OPEN. Readouts belong in the census line, where they cannot be
// mistaken for a verdict.
//
// Usage:
//
//	go run ./scripts/dcell [repo-root]
//
// Exit status is 0 when every predicate that is evaluable agrees with the
// claim, and non-zero when a declared item contradicts the tree. Items that
// are honestly unfinished are printed as open and do not fail the command —
// they are the score.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// manifest is the slice of a pig-ops.yaml this command reads. Everything
// else in the file is somebody else's concern; the point here is a census,
// and a census that failed to parse is not a census with a zero.
type manifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name    string `yaml:"name"`
		Version string `yaml:"version"`
		Vendor  string `yaml:"vendor"`
	} `yaml:"metadata"`
	Spec struct {
		Targets     []string `yaml:"targets"`
		SafetyLevel string   `yaml:"safety_level"`
		Tools       []struct {
			Name  string `yaml:"name"`
			Class string `yaml:"class"`
		} `yaml:"tools"`
		Approval struct {
			Required bool `yaml:"required"`
		} `yaml:"approval"`
	} `yaml:"spec"`
}

// item is one declared deliverable of the cell.
type item struct {
	id      string
	subject string
	// check returns "", nil when the item holds, and a non-empty sentence
	// describing the shortfall when it does not.
	check func(root string) (string, error)
}

var (
	semverRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	// The five node packages the ecosystem cell claims. A sixth directory
	// under plugins/pig-ops is a new package and must be added here on
	// purpose, which is the behaviour we want: the roster is the claim.
	roster = []string{
		"opskeeper-sre-readonly",
		"opskeeper-sre-observability",
		"opskeeper-sre-middleware",
		"opskeeper-sre-repair",
		"opskeeper-sre-autonomy",
	}
)

func loadManifest(path string) (manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, err
	}
	var m manifest
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(false)
	if err := dec.Decode(&m); err != nil {
		return manifest{}, fmt.Errorf("%s: %v", filepath.Base(path), err)
	}
	return m, nil
}

// loadFleet parses every manifest on the roster.
func loadFleet(root string) (map[string]manifest, error) {
	fleet := map[string]manifest{}
	for _, name := range roster {
		m, err := loadManifest(filepath.Join(root, "plugins", "pig-ops", name, "pig-ops.yaml"))
		if err != nil {
			return nil, err
		}
		fleet[name] = m
	}
	return fleet, nil
}

// itemManifests checks that every rostered package is present with a header
// a node can trust: a known apiVersion, the right kind, a name that matches
// its directory, and a semantic version.
func itemManifests(root string) (string, error) {
	fleet, err := loadFleet(root)
	if err != nil {
		return "", err
	}
	var bad []string
	for _, name := range roster {
		m := fleet[name]
		switch {
		case m.APIVersion != "opskeeper.io/v1":
			bad = append(bad, name+": apiVersion is "+strconv(m.APIVersion))
		case m.Kind != "Plugin":
			bad = append(bad, name+": kind is "+strconv(m.Kind))
		case m.Metadata.Name != name:
			bad = append(bad, name+": metadata.name is "+strconv(m.Metadata.Name))
		case !semverRe.MatchString(m.Metadata.Version):
			bad = append(bad, name+": version "+strconv(m.Metadata.Version)+" is not semver")
		}
	}
	if len(bad) > 0 {
		return "manifest headers: " + strings.Join(bad, "; "), nil
	}
	return "", nil
}

// itemReadPackagesPure checks that the three read packages ship nothing but
// reads.
//
// Two packages are excluded, and the first exclusion is a correction of an
// earlier version of this file. The repair package was swept in here and the
// check duly reported apply_config_change as "a write tool where no write
// tool belongs". That is the predicate being wrong, not the package: repair
// is B3, it exists to write, and it carries approval.required for exactly
// that reason. A census that cannot tell a write package from a broken one is
// a census that will be argued with instead of believed.
//
// The autonomy package is excluded because it is the one package whose tool
// may run with nobody present to approve it; D3 holds it to its own promise.
func itemReadPackagesPure(root string) (string, error) {
	fleet, err := loadFleet(root)
	if err != nil {
		return "", err
	}
	readOnly := []string{
		"opskeeper-sre-readonly",
		"opskeeper-sre-observability",
		"opskeeper-sre-middleware",
	}
	var bad []string
	total := 0
	for _, name := range readOnly {
		for _, tool := range fleet[name].Spec.Tools {
			total++
			if tool.Class != "read" {
				bad = append(bad, name+"/"+tool.Name+" is "+strconv(tool.Class))
			}
		}
	}
	if len(bad) > 0 {
		return "non-read tools in the read packages: " + strings.Join(bad, ", "), nil
	}
	_ = total // the census line below already prints the per-package totals
	return "", nil
}

// itemAutonomyDeclared checks that the autonomy package states its tier and
// carries an approval block. An autonomy package without a declared radius is
// the one shape that must never ship: the host has nothing to compare a call
// against.
func itemAutonomyDeclared(root string) (string, error) {
	fleet, err := loadFleet(root)
	if err != nil {
		return "", err
	}
	m := fleet["opskeeper-sre-autonomy"]
	if m.Spec.SafetyLevel != "L3" {
		return "autonomy safety_level is " + strconv(m.Spec.SafetyLevel) + ", want L3", nil
	}
	if len(m.Spec.Tools) == 0 {
		return "autonomy package declares no tool, so its L3 tier buys nothing", nil
	}
	if !m.Spec.Approval.Required {
		return "autonomy package has approval.required=false", nil
	}
	return "", nil
}

// itemRepairNeedsApproval checks the B3 promise in one predicate: the package
// that writes carries a human.
func itemRepairNeedsApproval(root string) (string, error) {
	fleet, err := loadFleet(root)
	if err != nil {
		return "", err
	}
	m := fleet["opskeeper-sre-repair"]
	if len(m.Spec.Tools) == 0 {
		return "repair package declares no tool", nil
	}
	if !m.Spec.Approval.Required {
		return "repair package has approval.required=false", nil
	}
	return "", nil
}

// itemReleaseRoutes counts the release transport routes on the control
// plane. Six is the set the ledger claims; a route deleted from the handler
// without updating the ledger turns this red rather than quietly shrinking
// the transport.
func itemReleaseRoutes(root string) (string, error) {
	path := filepath.Join(root, "core", "domains", "server", "plugin", "http.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(`"/v1/plugins/releases`)
	n := len(re.FindAllString(string(raw), -1))
	if n != 6 {
		return fmt.Sprintf("%d release routes registered, want 6", n), nil
	}
	return "", nil
}

// itemSignature checks that package signing is a control-plane function and
// not a comment. This is the first of the three review stages.
func itemSignature(root string) (string, error) {
	return symbolPresent(root, "core/manager/biz/marketplace/signature.go", "func VerifySignature(")
}

// itemEdgeAdmit checks the node-side admission path exists. This is the last
// of the three review stages, and the only one that runs where a bad package
// would actually land.
func itemEdgeAdmit(root string) (string, error) {
	return symbolPresent(root, "core/edge/policygate/gate.go", "func (g *Gate) Admit(")
}

// itemImporter checks the container importer is wired, not just present as a
// file: the control plane route and the implementation both.
func itemImporter(root string) (string, error) {
	if short, err := symbolPresent(root, "core/manager/server/marketplace/import.go", "func (h *Handler) importContainer("); err != nil || short != "" {
		return short, err
	}
	return symbolPresent(root, "core/manager/biz/pluginimport/importer.go", "func (i *Importer) Import(")
}

// itemMarketIndex checks the catalog index is reachable from production.
//
// Two halves, because either one alone is a class in a library: the usecase
// method that reads the install root, and the route that serves it. This
// item was an open one until 决策 455, and the thing it found is the reason
// it is worth a cell — LoadCatalog had existed the whole time with no
// production caller, so the index was something only tests could see.
func itemMarketIndex(root string) (string, error) {
	if short, err := symbolPresent(root, "core/manager/biz/marketplace/usecase.go", "func (uc *Usecase) Catalog("); err != nil || short != "" {
		return short, err
	}
	return symbolPresent(root, "core/manager/server/marketplace/http.go", "/v1/marketplace/catalog")
}

// itemMultiRootIndex checks that the marketplace index reads every root a
// tenant can install from, not just its own.
//
// The single-root reader was correct about what it did and wrong about what
// it claimed: the route is "the packages this tenant can install", and in a
// multi-tenant deployment the cluster-wide root is installable by every
// tenant. A package sitting there was invisible to every one of them while
// the route's own name said otherwise.
//
// All three origin labels are checked, not just the function. A
// multi-root loader with two of the three roots is a loader whose
// precedence has nothing to order, and a census that only looked for the
// function name would report that as done.
func itemMultiRootIndex(root string) (string, error) {
	// Two files, and that is not tidiness: the loader sits beside LoadCatalog
	// in manifest.go and the origin vocabulary beside Entry in catalog.go,
	// because they answer different questions. Checking the function in the
	// file that holds the labels would report a green for a loader that is
	// not there.
	if short, err := symbolPresent(root, "core/floor/pluginmanifest/manifest.go",
		"func LoadCatalogSources("); err != nil || short != "" {
		return short, err
	}
	for _, needle := range []string{
		`OriginTenant = "tenant"`,
		`OriginSystem = "system"`,
		`OriginBuiltin = "builtin"`,
	} {
		if short, err := symbolPresent(root, "core/floor/pluginmanifest/catalog.go", needle); err != nil || short != "" {
			return short, err
		}
	}
	// The wiring, not just the loader: a loader with three roots that
	// production still calls with one is the same bug one layer down.
	return symbolPresent(root, "core/manager/biz/marketplace/usecase.go",
		"func (uc *Usecase) catalogRoots(")
}

// itemCompatMatrix checks that a compatibility matrix exists AND that the
// fleet's own packages are held to declaring both host floors.
//
// The matrix itself was already here — core/domains/service/plugin projects
// the verdict across the real fleet — so half of this item is deliberately
// not "build a matrix". The half that was missing is the gate: five
// packages declared a node floor and nothing declared a PiG floor, so that
// projection had an axis that could never go red, and an axis that is
// always green is indistinguishable from no matrix at all.
//
// This item was written once against the wrong file. The first version
// looked for a grid over hypothetical (edge, pig) pairs that this knife had
// also written, and deleting that duplicate is why the predicate names the
// service projection instead.
func itemCompatMatrix(root string) (string, error) {
	if short, err := symbolPresent(root, "core/domains/service/plugin/compatibility.go",
		"func (m *Manager) Compatibility("); err != nil || short != "" {
		return short, err
	}
	return symbolPresent(root, "core/floor/pluginmanifest/catalog_test.go",
		"func TestEveryShippedPackageDeclaresBothHostFloors(")
}

// itemSDK checks the third-party authoring surface ships its three parts.
// A plugin author needs a manifest type, a registration call and a version
// negotiation; two of three is a half-built SDK that still looks finished.
func itemSDK(root string) (string, error) {
	for _, f := range []string{"manifest.go", "register.go", "negotiate.go"} {
		if _, err := os.Stat(filepath.Join(root, "sdk", f)); err != nil {
			return "sdk/" + f + " is missing", nil
		}
	}
	return "", nil
}

// itemGapReasoning checks that a recorded diagnosis gap still has to say
// where it looked — and, since the table is empty, that the RULE is still
// checked rather than only written down.
//
// The predicate changed when 决策 453 closed the last gap. It used to look
// for a Searched entry in coverage.go, which is a check on one entry; with
// the table empty there is no entry to look at, and a predicate pointed at a
// map that is meant to stay empty would be a check that fails the moment the
// repository succeeds. So the claim became the rule plus its proof: the
// reason type still carries the field, the failure function is still there,
// and the mutations are still asserted.
func itemGapReasoning(root string) (string, error) {
	body, err := readFileOrFail(root, "core/floor/pluginmanifest/coverage.go")
	if err != nil {
		return "", err
	}
	if !strings.Contains(body, "Searched []string") {
		return "GapReason no longer carries the Searched list", nil
	}
	test, err := readFileOrFail(root, "core/floor/pluginmanifest/coverage_test.go")
	if err != nil {
		return "", err
	}
	for _, want := range []string{"func gapReasonFailures(", "TestAGapReasonRulesRejectEachHistoricalError"} {
		if !strings.Contains(test, want) {
			return "the gap-reason rules are no longer proven by " + strconv(want), nil
		}
	}
	return "", nil
}

func readFileOrFail(root, rel string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// itemGatesOnRecord checks the two behavioural gates that back this cell are
// reachable from the makefile. If either target disappears, the cell's
// behavioural half silently stops running while this one keeps printing a
// number.
func itemGatesOnRecord(root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return "", err
	}
	for _, target := range []string{"eval-gates:", "pig-tool-scoping-check:"} {
		if !strings.Contains(string(raw), target) {
			return "makefile no longer has a " + target + " target", nil
		}
	}
	return "", nil
}

// itemPackageSync checks the packaging copy of the manifests is kept in step
// by a script that exists, since the bundle ships the copy and not the
// source tree.
func itemPackageSync(root string) (string, error) {
	path := filepath.Join(root, "scripts", "sync-pig-ops.sh")
	if _, err := os.Stat(path); err != nil {
		return "scripts/sync-pig-ops.sh is missing", nil
	}
	return "", nil
}

func items() []item {
	return []item{
		{"D1", "五份节点包清单齐备且头部可信", itemManifests},
		{"D2", "三个只读包只带只读工具", itemReadPackagesPure},
		{"D3", "自治包声明 L3 与审批半径", itemAutonomyDeclared},
		{"D4", "修复包的写操作挂着人", itemRepairNeedsApproval},
		{"D5", "发布运输通道六条路由在册", itemReleaseRoutes},
		{"D6", "审核第一段：包签名校验", itemSignature},
		{"D7", "审核第三段：节点侧准入", itemEdgeAdmit},
		{"D8", "存量容器导入器接线", itemImporter},
		{"D9", "第三方 sdk 三个发布物", itemSDK},
		{"D10", "GAP 理由的规则仍被变异验证", itemGapReasoning},
		{"D11", "本格的行为闸门仍在册", itemGatesOnRecord},
		{"D12", "打包副本同步脚本在册", itemPackageSync},
		{"D13", "插件市场索引有生产接线", itemMarketIndex},
		{"D14", "兼容矩阵存在且两轴都被声明", itemCompatMatrix},
		{"D15", "索引覆盖本租户安装根之外的根", itemMultiRootIndex},
		{"D16", "远端注册表索引：产出端与消费端都在册", itemRemoteRegistryIndex},
		{"D17", "索引里的包可以真的装上，且装之前验摘要与清单", itemRegistryInstall},
	}
}

// open items are declared, not discovered.
//
// An open cell carries a closer for the same reason the audit's external
// items do: a cell that says only what is missing gives its reader nothing
// to do, and a cell whose reason has gone stale is worse than no cell —
// the reader acts on it. Both happened to this one.
//
// X1 originally read "the index covers only this tenant's install root".
// 决策 459 made that half false — the index now reads the tenant root, the
// cluster-wide root and the image-baked ones, and D15 is the cell that
// measures it. What remained was the other half: a remote registry's own
// listable index.
//
// 决策 466 then made the reason itself wrong, which is the second time this
// cell's prose has been the defect. The reason said "this repository does not
// have a remote to call", and the repository can now both emit the document
// (scripts/registryindex) and read one back (marketplace's RegistryIndexes).
// A reason that describes a limitation of the reader when the limitation is
// of the reader's imagination is worse than an admission, because it is acted
// on: it says stop, and it says stop while the work is in fact possible.
//
// So the cell split. D16 measures the in-repo half, which is now decidable and
// measured. X1 keeps only what genuinely needs another system: a registry
// serving an index over a network to a control plane that reads it.
var planned = []struct{ id, subject, why, closer string }{
	{
		"X1", "远端注册表的索引在真实部署中被读",
		"仓内的两端都齐了（产出见 D16 的 scripts/registryindex，消费见 core/manager " +
			"的 RegistryIndexes）。剩下的不是本仓能关的：要有一次跨网络的读取，" +
			"即一份由部署侧服务出去的索引，被另一个部署读回来并据此安装",
		"同 audit 的 E4 的剩余部分：需要一个真的把索引服务出来的 registry，和一个" +
			"真的从网络读它的控制面。仓内已经能产出和消费，所以部署时只要把 " +
			"OPSKEEPER_MARKETPLACE_REGISTRIES 指过去即可",
	},
}

// itemRemoteRegistryIndex checks that a registry index is both produced and
// consumed by this tree.
//
// Why this is a cell rather than another line of X1's reason. X1 said the
// missing half was "a remote registry's own listable index, and reading it
// needs a remote system this repository does not have". That reason was true
// about the network and false about the repository: the repository can now
// emit the document (scripts/registryindex) and read one back
// (marketplace's RegistryIndexes), so the part that is genuinely external is
// only the deployment that serves one across a network. Splitting the cell is
// what stops the number from hiding behind a reason that used to be
// convenient.
//
// Both ends are checked because either alone is the failure this cell exists
// to catch. A reader with no producer on this tree means the only index it
// has ever seen was written by a test, which is exactly the state that let
// the previous reason go stale. A producer with no reader means the
// repository publishes something nothing on it consumes.
//
// The reader is checked for its parser and its wiring, not for its tests: the
// behaviour is pinned in core/floor/pluginmanifest and core/manager/biz/marketplace,
// and a census that re-ran those tests would be a second, slower, less
// informative copy of them.
func itemRemoteRegistryIndex(root string) (string, error) {
	// The document itself, and the rule that a listing may not contradict
	// the manifest behind it.
	if short, err := symbolPresent(root, "core/floor/pluginmanifest/index.go",
		"func ParseIndex("); err != nil || short != "" {
		return short, err
	}
	// The producer.
	if short, err := symbolPresent(root, "scripts/registryindex/main.go",
		"func build("); err != nil || short != "" {
		return short, err
	}
	// The consumer, and the precedence rule that keeps a remote claim from
	// displacing a copy this control plane holds.
	if short, err := symbolPresent(root, "core/floor/pluginmanifest/manifest.go",
		"func LoadCatalogSources("); err != nil || short != "" {
		return short, err
	}
	if short, err := symbolPresent(root, "core/floor/pluginmanifest/catalog.go",
		`OriginRegistry = "registry"`); err != nil || short != "" {
		return short, err
	}
	// The production wiring: a consumer nothing configures is a consumer that
	// never runs, and this cell has already caught one of those.
	return symbolPresent(root, "cmd/opskeeper/main.go", "OPSKEEPER_MARKETPLACE_REGISTRIES")
}

// itemRegistryInstall checks that a row in a registry index can be installed,
// and that the two checks which make installing from an index mean anything
// are on the path rather than in a comment.
//
// The distinction from D16 is the whole point. D16 measures that the repository
// can produce an index and read one back — a catalogue. This measures that
// the catalogue is not the end of the road: a row naming a url and a digest
// leads to a package on disk, and the bytes that land are the bytes the row
// described.
//
// Both checks are named individually because either one alone leaves the path
// open. The digest alone catches a substituted archive but not a registry
// that publishes a manifest which is not in the package it points at; the
// manifest comparison alone catches nothing, because a substituted archive
// carries whatever manifest the substitutor chose. So a census that checked
// only the first would report a green for a path where the governance
// manifest an operator reviewed is not the one the node installs.
func itemRegistryInstall(root string) (string, error) {
	// The resolution: label, pack and version in, one row out.
	if short, err := symbolPresent(root, "core/manager/biz/marketplace/usecase.go",
		"func (uc *Usecase) resolveRegistryItem("); err != nil || short != "" {
		return short, err
	}
	// The two verifications, by their own names. A single function holding
	// only the digest would leave the second unreachable, and the census
	// would still be green.
	if short, err := symbolPresent(root, "core/manager/biz/marketplace/usecase.go",
		"func (uc *Usecase) verifyRegistryPackage("); err != nil || short != "" {
		return short, err
	}
	for _, needle := range []string{"pluginmanifest.TreeDigest(dir)", "pig-ops.yaml", "item.ManifestYAML"} {
		if short, err := symbolPresent(root, "core/manager/biz/marketplace/usecase.go", needle); err != nil || short != "" {
			return short, err
		}
	}
	// The path itself, not just the helpers beside it.
	return symbolPresent(root, "core/manager/biz/marketplace/usecase.go",
		"item, err := uc.resolveRegistryItem(ctx, src)")
}

// symbolPresent reports whether a file exists and still contains a symbol.
//
// The needle is passed separately from the path on purpose. When the two were
// one variadic argument the checker read "func VerifySignature(" as a
// directory, which is the shape of bug that makes a census quietly report
// "cannot evaluate" on every wiring item and teaches the reader to ignore it.
func symbolPresent(root, rel, needle string) (string, error) {
	file := filepath.Join(root, rel)
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	if !strings.Contains(string(raw), needle) {
		return rel + " no longer contains " + strconv(needle), nil
	}
	return "", nil
}

func strconv(s string) string { return "\"" + s + "\"" }

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	list := items()
	open := 0
	var notes []string
	var failures []string
	for _, it := range list {
		shortfall, err := it.check(root)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s %s: cannot evaluate: %v", it.id, it.subject, err))
			continue
		}
		if shortfall == "" {
			fmt.Printf("  ok   %-3s %s\n", it.id, it.subject)
			continue
		}
		open++
		fmt.Printf("  OPEN %-3s %s\n         %s\n", it.id, it.subject, shortfall)
	}
	for _, p := range planned {
		fmt.Printf("  OPEN %-3s %s\n         %s\n         closer: %s\n", p.id, p.subject, p.why, p.closer)
		open++
	}
	total := len(list) + len(planned)
	notes = append(notes, fmt.Sprintf("%d/%d closed", total-open, total))

	fleet, err := loadFleet(root)
	if err == nil {
		var names []string
		tools := 0
		for name, m := range fleet {
			names = append(names, fmt.Sprintf("%s@%s", name, m.Metadata.Version))
			tools += len(m.Spec.Tools)
		}
		sort.Strings(names)
		notes = append(notes, fmt.Sprintf("%d tools declared across %d packages", tools, len(fleet)))
		notes = append(notes, strings.Join(names, " "))
	} else {
		failures = append(failures, "manifest census: "+err.Error())
	}

	fmt.Printf("\ndcell: %s\n", strings.Join(notes, "\n       "))
	fmt.Printf("       score %d/%d = %.1f%%  (census of repository state, not a behavioural proof)\n",
		total-open, total, 100*float64(total-open)/float64(total))

	if len(failures) > 0 {
		fmt.Fprintln(os.Stderr)
		for _, f := range failures {
			fmt.Fprintln(os.Stderr, "dcell: "+f)
		}
		os.Exit(1)
	}
}
