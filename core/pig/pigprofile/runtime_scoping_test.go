//go:build pigscoping

package pigprofile

// The gate that the unit tests in this package structurally cannot be.
//
// TestTheProfileRemovesTheShellFromTheMenu calls piglet.ScopeTools with a
// hand-built tool list, because ScopeTools is the only part of PiG's
// scoping this module can call. That proves the profile is correct *given*
// a runtime that classifies extension tools by extension name — and the
// shipped runtime does not. The real path runs an unexported conversion
// that derives each tool's source from the SourceInfo its own host
// attaches, and nothing in a unit test can reach it.
//
// So this file runs the real binary: a real `pig` built from core/pig the
// way a release builds it, the node's own generated profile, the shipped
// read-only package, and a fake model that records exactly which tools the
// runtime offered.
//
// It stays behind a build tag for cost, not for redness: it builds a real
// 70 MB pig binary inside the test, and `make test` runs on every module on
// every push. It is a decision gate in CI instead (scripts/cigate, see
// `make ci-gate-check`), which is the arrangement that actually keeps it
// honest -- a tag that only a human remembers to type owns nothing, and
// that is how it sat red at 0/18 for months (decision 168).
//
//	make pig-tool-scoping-check
//
// It was red for a reason that was not ours: PiG's tool-provenance
// conversion wrote `source` where it read `name`, so every extension tool
// was misfiled as a built-in and the node profile stripped it (decision
// 129, docs/opskeeper2-architecture.md §4.265). PiG shipped the fix in
// v0.4.0; no profile can work around it, and none tries.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/coding/packagecontent"

	"github.com/vincent-wuhan/opskeeper/core/edge/agentprofile"
)

// readonlyPackageRel is the package a node admits first: the read-only
// diagnosis toolset, which is the set of tools the plan's §3.3 table calls
// "✅ 可插件化" and the only toolset a node may run without a human.
const readonlyPackageRel = "../../../plugins/pig-ops/opskeeper-sre-readonly"

// shippedPackagesRel is the directory a node's packages are installed from.
// The fleet question is asked over whatever is actually shipped, not over a
// list written down next to the question -- a hand-written list is a list
// that stops being true.
const shippedPackagesRel = "../../../plugins/pig-ops"

func TestTheNodeProfileActuallyOffersTheToolsItsPackagesDeclare(t *testing.T) {
	pkg, err := filepath.Abs(readonlyPackageRel)
	if err != nil {
		t.Fatalf("resolve package: %v", err)
	}
	declared := declaredToolNames(t, pkg)
	if len(declared) == 0 {
		t.Fatal("the package declares no tools, so this gate has nothing to assert")
	}

	offered := offeredToolNames(t, pkg)

	var missing []string
	for _, name := range declared {
		if !contains(offered, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the node agent was offered %d of the %d tools its admitted package declares.\n"+
			"missing: %s\n\n"+
			"The model cannot call a tool it was never shown, so a node in this state "+
			"holds a correct profile, a signed package, a gate, an allow-list and an audit "+
			"ledger, and an agent that can do nothing with any of them.\n\n"+
			"Two defects produced this, and each one hid the other, so fixing "+
			"either alone still leaves the node with nothing.\n\n"+
			"1. Upstream, in PiG: sessionToolRegistry.refresh overwrote each tool's "+
			"per-tool source with the registering extension's provenance, and "+
			"coding/piglet's converter has no key for provenance, so every tool "+
			"resolved to \"builtin\" and the profile's `tools: []` — which exists to "+
			"remove the shell — removed the plugins with it. Fixed in PiG by keeping "+
			"a tool's own source and falling back to the extension's only when the "+
			"host set none.\n\n"+
			"2. Here, and the reason this was invisible: the profile's "+
			"`discovery.extensions` did not admit the scope its own packages are "+
			"registered at, so the agent was offered the host's built-ins and none "+
			"of the node's plugins. The unit tests could not see it because they "+
			"read the profile's text, and the text had said what they wanted.\n\n"+
			"Both fixes are in: fix 2 here, fix 1 in PiG v0.4.0. So this gate is "+
			"no longer waiting for anything. A red now means one of three things, "+
			"and they need different answers: the pinned PiG lost fix 1 again, "+
			"the profile stopped admitting the package's own scope, or a package "+
			"shipped tools no extension registers. See §4.104.",
			len(offered), len(declared), strings.Join(missing, ", "))
	}
}

// TestTheManifestReaderKnowsADeclaredToolFromASignedAction is the control on
// the reader the two runtime gates above are built out of. Both of them
// compare a manifest's tool list against a real agent, so a reader that
// returned an empty list, or the wrong list, would make them agree on
// nothing -- and the fleet gate proved that by reporting a signed self-heal
// as an unrunnable tool.
//
// The assertions are on the two shapes the manifests actually contain: the
// read-only package's multi-line `- name:` tool entries, and the autonomy
// package's `autonomy.actions`, which is a list of `name:` entries that is
// not a tool list.
func TestTheManifestReaderKnowsADeclaredToolFromASignedAction(t *testing.T) {
	readonly, err := filepath.Abs(readonlyPackageRel)
	if err != nil {
		t.Fatalf("resolve read-only package: %v", err)
	}
	tools := declaredToolNames(t, readonly)
	if len(tools) < 10 {
		t.Fatalf("the read-only package read as %d tools (%v); the reader is not seeing the "+
			"tool list and every gate built on it would pass on nothing", len(tools), tools)
	}
	if !contains(tools, "get_topology") {
		t.Errorf("the read-only package's tools do not include get_topology (%v)", tools)
	}

	autonomy, err := filepath.Abs(shippedPackagesRel + "/opskeeper-sre-autonomy")
	if err != nil {
		t.Fatalf("resolve autonomy package: %v", err)
	}
	signed := declaredToolNames(t, autonomy)
	if len(signed) != 1 || signed[0] != "host_autonomy_run" {
		t.Fatalf("the autonomy package's tools read as %v; it declares exactly one tool, and "+
			"its autonomy actions are signed commands rather than tools", signed)
	}
	if contains(signed, "restart_orders") {
		t.Error("the reader counted a signed autonomy action as a tool; that is the bug this " +
			"test exists for")
	}
}

// TestEveryShippedPackageIsOfferedItsDeclaredTools widens the question from
// the first package a node admits to all of them.
//
// The gate above answers "can a node use the read-only diagnosis set", which
// is the set the plan's table marks as the only one a node may run without a
// human -- and therefore the one most likely to be healthy while everything
// else is not. A fleet that admits five packages has five profiles, five
// admission decisions and five manifests, and the defect this gate exists
// for was never specific to the read-only one: it lived in the tool
// provenance every package shares.
//
// The number the failure reports is per package rather than a fleet total on
// purpose. A total lets five healthy packages pay for a sixth that offers
// nothing, which is the same mistake the plan's §6 gate list warns about --
// the number quoted often enough has to be the one that moves.
func TestEveryShippedPackageIsOfferedItsDeclaredTools(t *testing.T) {
	root, err := filepath.Abs(shippedPackagesRel)
	if err != nil {
		t.Fatalf("resolve shipped packages: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	var found, declaredTotal int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pkg := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(pkg, "pig-ops.yaml")); err != nil {
			continue
		}
		found++
		t.Run(entry.Name(), func(t *testing.T) {
			declared := declaredToolNames(t, pkg)
			if len(declared) == 0 {
				t.Fatal("the package declares no tools, so this gate has nothing to assert")
			}
			declaredTotal += len(declared)
			offered := offeredToolNames(t, pkg)
			if missing := missingFrom(offered, declared); len(missing) > 0 {
				t.Fatalf("the node agent was offered %d of the %d tools this package declares.\n"+
					"missing: %s\n\n"+
					"The model cannot call a tool it was never shown. Everything the node "+
					"holds -- profile, signature, gate, allow-list, audit ledger -- is real, "+
					"and the agent still cannot use any of it. See the sibling test for the "+
					"two defects that produced this state and §4.104 for what a red means now.",
					len(offered), len(declared), strings.Join(missing, ", "))
			}
			// The converse, for the same reason the sibling test asserts it:
			// a package that became *more* capable is the failure this gate
			// would otherwise report as health.
			for _, name := range offered {
				if !contains(declared, name) {
					t.Errorf("the runtime offered %q, which this package's manifest never "+
						"declared; the manifest is the review surface, so a tool reaching the "+
						"model without being listed means a package gained capability that "+
						"nobody accepted", name)
				}
			}
		})
	}
	if found == 0 {
		t.Fatalf("no package with a manifest under %s; the gate would pass on nothing", root)
	}
	t.Logf("checked %d shipped packages and %d declared tools against a real agent binary",
		found, declaredTotal)
}

// TestAToolRemovedFromTheReviewSurfaceIsRemovedFromTheMenu is the property
// the gate above cannot see, and it is the one that decides whether a fix
// to the upstream defect is safe to take.
//
// Making the tools appear is half the problem. The other half is that a
// package's per-extension tool list is the review surface — `pig-ops.yaml`
// says so in as many words, and the whole point of naming a tool there is
// that a tool nobody listed cannot run. An upstream fix that merely made
// every tool visible would satisfy the first test and quietly delete the
// second: a package that ships a tool its manifest never declared would
// still be offered that tool, and the failure would be invisible because
// the node would look healthy and productive.
//
// So the assertion is deliberately a contrast: the same package, run twice,
// differing only in whether one tool is listed. Offerings must differ.
func TestAToolRemovedFromTheReviewSurfaceIsRemovedFromTheMenu(t *testing.T) {
	pkg, err := filepath.Abs(readonlyPackageRel)
	if err != nil {
		t.Fatalf("resolve package: %v", err)
	}
	declared := declaredToolNames(t, pkg)
	if len(declared) < 2 {
		t.Fatalf("the package declares %d tools; this gate needs at least 2", len(declared))
	}
	dropped := declared[0]

	trimmed := copyPackage(t, pkg, t.TempDir())
	if err := os.WriteFile(filepath.Join(trimmed, "pig-ops.yaml"),
		[]byte(manifestWithout(t, pkg, dropped)), 0o640); err != nil {
		t.Fatalf("write trimmed manifest: %v", err)
	}
	stillDeclared := declaredToolNames(t, trimmed)
	if contains(stillDeclared, dropped) {
		t.Fatalf("the trimmed manifest still declares %q; the gate would prove nothing", dropped)
	}

	offered := offeredToolNames(t, trimmed)

	for _, name := range stillDeclared {
		if !contains(offered, name) {
			t.Fatalf("with %q removed from the manifest the model was offered %d of the "+
				"remaining %d tools; a node whose plugins are all switched off is not a "+
				"node with plugins\nmissing: %s", dropped, len(offered), len(stillDeclared),
				strings.Join(missingFrom(offered, stillDeclared), ", "))
		}
	}
	if contains(offered, dropped) {
		t.Errorf("%q is no longer declared in the package's manifest, but the runtime still "+
			"offered it. The manifest's tool list is the review surface: an undeclared tool "+
			"reaching the model means a package can become more capable without anybody "+
			"accepting the diff. This is what a fix that only makes tools visible — rather "+
			"than attributing each one to the extension that registered it — produces. "+
			"See §4.265.4", dropped)
	}
}

// manifestWithout returns the package's manifest with one tool entry removed.
func manifestWithout(t *testing.T, pkg, tool string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pkg, "pig-ops.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if comment := strings.IndexByte(line, '#'); comment >= 0 {
			// Keep the comment: a trimmed manifest that loses its own
			// annotations is a worse thing to read than a shorter one.
			line = line[:comment] + line[comment:]
		}
		if strings.Contains(line, "name: "+tool+",") || strings.Contains(line, "name: "+tool+" ") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// copyPackage copies a package tree so a gate can vary one file in it
// without touching the repository.
func copyPackage(t *testing.T, src, dst string) string {
	t.Helper()
	if err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rErr := filepath.Rel(src, path)
		if rErr != nil {
			return rErr
		}
		if info.IsDir() && strings.HasPrefix(filepath.Base(path), ".") {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		in, oErr := os.Open(path)
		if oErr != nil {
			return oErr
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		out, cErr := os.Create(target)
		if cErr != nil {
			return cErr
		}
		defer out.Close()
		_, cpErr := io.Copy(out, in)
		return cpErr
	}); err != nil {
		t.Fatalf("copy package: %v", err)
	}
	return dst
}

func missingFrom(offered, want []string) []string {
	var missing []string
	for _, name := range want {
		if !contains(offered, name) {
			missing = append(missing, name)
		}
	}
	return missing
}

// offeredToolNames runs a real agent against a fake model and returns the
// tool names the model was actually offered.
func offeredToolNames(t *testing.T, pkg string) []string {
	t.Helper()

	var mu sync.Mutex
	var offered []string
	seen := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		if !seen {
			seen = true
			for _, tool := range body.Tools {
				offered = append(offered, tool.Function.Name)
			}
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		send := func(payload string) {
			fmt.Fprint(w, payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
		send(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe",` +
			`"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n")
		send(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe",` +
			`"choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n")
		send(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe",` +
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n")
		send("data: [DONE]\n\n")
	}))
	defer server.Close()

	dir := t.TempDir()
	// The agent's own scope, exactly as the node writes it: a model
	// endpoint, the admitted package set, and the generated profile. The
	// key is a literal because the model is a fake in this process.
	write := func(name, body string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("models.json", fmt.Sprintf(`{"providers":{"probe":{"name":"probe","baseUrl":%q,`+
		`"apiKey":"sk-probe","api":"openai-completions","models":[{"id":"probe","name":"probe"}]}}}`,
		server.URL+"/v1"), 0o600)
	write("settings.json", fmt.Sprintf(`{"packages":["file://%s"]}`, pkg), 0o600)

	// The profile is built from the package under test, not from a
	// constant. A fixed profile would make the gate insensitive to the very
	// thing the second test varies — a manifest with one tool removed —
	// and it would pass for a node offering tools no manifest declared,
	// which is the finding.
	profile := filepath.Join(dir, agentprofile.FileName)
	if err := os.WriteFile(profile, []byte(agentprofile.Render(profileFor(t, pkg))), 0o640); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, pigBinary(t),
		"--piglet", profile, "--provider", "probe", "--model", "probe", "-p", "say ok")
	cmd.Env = append(os.Environ(), "PIG_CODING_AGENT_DIR="+dir)
	out, runErr := cmd.CombinedOutput()

	mu.Lock()
	defer mu.Unlock()
	if !seen {
		t.Fatalf("the agent never reached the model, so nothing was offered or withheld\n"+
			"exit: %v\noutput:\n%s", runErr, out)
	}
	sort.Strings(offered)
	return offered
}

// profileFor is the node's profile for one package: every extension the
// package declares, held to the tools its manifest declared.
//
// The grouping is the package's, not the extension's, and that is the same
// choice the edge makes when it writes a real node's profile - the manifest
// reviews a package, and which of its extensions registers a given tool is
// an implementation detail nobody reviewed. The gate makes the identical
// pairing, keying on tool names, so the two agree about what was reviewed.
func profileFor(t *testing.T, pkg string) []agentprofile.Extension {
	t.Helper()
	resources, err := packagecontent.Discover(pkg)
	if err != nil {
		t.Fatalf("discover package resources: %v", err)
	}
	if len(resources.ExtensionEntries) == 0 {
		t.Fatalf("the package at %s declares no extensions; there is nothing for a "+
			"profile to name and the gate would prove nothing", pkg)
	}
	tools := declaredToolNames(t, pkg)
	out := make([]agentprofile.Extension, 0, len(resources.ExtensionEntries))
	for _, entry := range resources.ExtensionEntries {
		name, err := packagecontent.PublicName(packagecontent.Extensions, entry, "")
		if err != nil {
			t.Fatalf("public name for extension %q: %v", entry, err)
		}
		out = append(out, agentprofile.Extension{Name: name, Tools: tools})
	}
	return out
}

// builtOnce makes the agent build once per test binary. A gate that asks
// the same question of six packages must not build a 70 MB binary six
// times, and the second build is not free just because the first was.
var builtOnce sync.Once

// builtPath is where that one binary lives.
var builtPath string

// builtDir is the scratch directory that binary lives in. It is recorded
// rather than removed at the build site because the Once that builds the
// agent and the test that needed it are not the same test: a cleanup
// registered against the test that happened to trigger the build would pull
// the binary out from under the five packages that still have to run against
// it. TestMain is the only point at which none of them can be running.
var builtDir string

// TestMain hands back the scratch directory. Without it every run of this
// gate left a 70 MB agent binary in the system temp directory — invisible on
// a workstation, and the reason a CI runner eventually fails to compile
// anything at all.
func TestMain(m *testing.M) {
	code := m.Run()
	if builtDir != "" {
		_ = os.RemoveAll(builtDir)
	}
	os.Exit(code)
}

// pigBinary builds the agent the way a release builds it, or uses one the
// caller already built.
func pigBinary(t *testing.T) string {
	t.Helper()
	if pre := os.Getenv("OPSKEEPER_PIG_BIN"); pre != "" {
		return pre
	}
	builtOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pig-gate-")
		if err != nil {
			builtPath = "mkdtemp failed: " + err.Error()
			return
		}
		builtDir = dir
		out := filepath.Join(dir, "pig")
		cmd := exec.Command("go", "build", "-o", out, "github.com/MichaelKinsy/PiG/cmd/pig")
		cmd.Dir = ".."
		// The workspace is off on purpose: a release builds the agent from the
		// pinned tag, and a gate that silently built against a developer's
		// local PiG checkout would prove something else.
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if combined, err := cmd.CombinedOutput(); err != nil {
			builtPath = "build failed: " + err.Error() + "\n" + string(combined)
			return
		}
		builtPath = out
	})
	if strings.HasPrefix(builtPath, "mkdtemp failed") || strings.HasPrefix(builtPath, "build failed") {
		t.Fatalf("build pig: %s", builtPath)
	}
	return builtPath
}

// declaredToolNames reads the tool names a package's governance manifest
// declares, which is the node's own allow-list.
//
// It is section-aware, and it has to be: the autonomy package's manifest
// holds a second list of `name:` entries under `autonomy.actions` -- the
// self-heal actions a person signed -- and a reader that counted those as
// tools would ask the agent to be offered a tool that no extension has ever
// registered. That is not a hypothetical: the fleet-wide test below found
// exactly that, on `restart_orders`, and the failure it reported -- "the
// node agent cannot run its own signed self-heal" -- was entirely the
// reader's.
//
// The same reader also builds the profile the gate runs against, so a
// miscount would have put a nonexistent tool into the node's allow-list and
// then complained that the node did not have it. One mistake, two wrong
// answers.
func declaredToolNames(t *testing.T, pkg string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pkg, "pig-ops.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var names []string
	// key is the mapping key a list item belongs to, and keyIndent is that
	// key's own indentation: YAML puts a sequence under the key whose
	// indentation it continues, so the item's own indent is the evidence.
	key, keyIndent := "", -1
	for _, line := range strings.Split(string(raw), "\n") {
		if comment := strings.IndexByte(line, '#'); comment >= 0 {
			line = line[:comment]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- ") && !strings.HasPrefix(trimmed, "- ") {
			// A mapping key at a shallower indentation is the section the
			// following list items belong to. Keys deeper than the current
			// one belong to the item being read, and are ignored on purpose:
			// a list item written across several lines (`- name: x` then
			// `  class: read`) must not retarget the reader.
			if colon := strings.Index(trimmed, ":"); colon > 0 && !strings.HasPrefix(trimmed, "- ") {
				if k := strings.TrimSpace(trimmed[:colon]); k != "" {
					key, keyIndent = k, indent
				}
			}
			continue
		}
		if key != "tools" || indent <= keyIndent {
			continue
		}
		// The manifests write their tool list in both shapes -- `- name: x`
		// and `- { name: x, class: read }` -- and a reader that understood
		// only one of them would report a short list and quietly weaken this
		// gate into a partial one.
		marker := strings.Index(trimmed, "name:")
		if marker < 0 {
			continue
		}
		rest := trimmed[marker+len("name:"):]
		if cut := strings.IndexAny(rest, ",}"); cut >= 0 {
			rest = rest[:cut]
		}
		if name := strings.TrimSpace(rest); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
