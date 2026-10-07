package toolset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/reporoot"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	middlewareregistry "github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// The node's middleware read toolset, generated from the live adapters.
//
// Sixty-odd tools, most of them with a description long enough that
// transcribing it by hand is not an option and rewriting a range bound is
// not a mistake anybody should make on purpose. So the file the agent reads
// is produced from the registration the host actually dispatches through,
// and the test below fails the moment the two drift apart.
//
// The direction of the copy matters. The adapter is the authority — it is
// what executes the call, so its description and its argument map are the
// ones that have to be right — and the extension is the copy. A
// hand-maintained copy would drift the moment a tool gained a parameter,
// and it would drift silently, because the model would be calling a schema
// the executor no longer parses and the resulting error reads like bad
// input rather than like staleness.
//
// Regenerate with:
//
//	OPSKEEPER_UPDATE_TOOLSET=1 go test ./core/manager/middleware/toolset/ -run Toolset
//
// then run scripts/sync-pig-ops.sh to copy the extension into the package.

// generatedPath is the canonical extension the package copies are made
// from. The packaged copies are written by scripts/sync-pig-ops.sh, so
// there is exactly one file to regenerate and one place to look.
const generatedPath = "core/pig/extensions/opskeeper-sre-middleware/tools.go"

// packageName must equal the package clause of the file this generator
// writes. It is asserted rather than assumed: a mismatch produces a file
// that does not compile, and the failure would name the generated file
// rather than the generator.
const packageName = "opskeepermiddleware"

func TestToolsetMatchesTheAdapters(t *testing.T) {
	reg, err := Registry()
	if err != nil {
		t.Fatalf("build the adapter registry: %v", err)
	}
	want, err := renderToolset(PackagedReadTools(reg))
	if err != nil {
		t.Fatalf("render the toolset: %v", err)
	}

	path := filepath.Join(repoRoot(t), generatedPath)
	if os.Getenv("OPSKEEPER_UPDATE_TOOLSET") == "1" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(want))
		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\n\nregenerate it with OPSKEEPER_UPDATE_TOOLSET=1 go test ./core/manager/middleware/toolset/ -run Toolset", path, err)
	}
	switch classifyDrift(got, want) {
	case driftNone:
		return
	case driftBanner:
		// 这一支的存在理由见 classifyDrift：**重新生成会把坏头部固化**，
		// 而原来那句"regenerate it"会正好把人引到那一步。
		t.Fatalf("%s disagrees with the generator, and the difference is the banner "+
			"alone — every tool in it is exactly what the adapters register.\n\n"+
			"Do NOT regenerate: regenerating writes this banner into the file and the "+
			"gate goes green on a corrupted header. Look at generatedHeader at the bottom "+
			"of this file instead — it is the source of the banner, and something moved "+
			"into it. This is how a scripted edit anchored on a comment line that appears "+
			"both in generatedHeader and above a test function put a test's doc comment "+
			"inside the shipped banner.", path)
	default:
		t.Fatalf("%s is stale: the adapters and the packaged toolset disagree.\n\n"+
			"Regenerate it with OPSKEEPER_UPDATE_TOOLSET=1 go test ./core/manager/middleware/toolset/ -run Toolset,\n"+
			"then run scripts/sync-pig-ops.sh.", path)
	}
}

// generationDrift is how the committed file differs from what the generator
// writes today. Two shapes, and the difference is the whole point: they need
// opposite remedies.
type generationDrift int

const (
	driftNone generationDrift = iota
	// driftTools means the tool table moved: regenerate.
	driftTools
	// driftBanner means only the header moved: do not regenerate.
	driftBanner
)

// bannerBoundary ends the header. Everything up to and including it is the
// banner; everything after it is the generated table.
const bannerBoundary = "\npackage opskeepermiddleware\n"

// classifyDrift separates "the adapters moved" from "the banner moved",
// because the second one must not be answered by regenerating.
//
// 这不是假想的一条分支。本轮真的撞上过：脚本化编辑按注释行做锚点，
// 而那一行注释**同时**出现在 `generatedHeader`（它是被写进生成文件的头部）
// 和某个测试函数上方，于是编辑把测试的文档注释注入了 shipped banner。
// 逐字节闸门抓住了它——但它给出的补救是"重新生成"，
// 而重新生成恰好会把那个坏 banner 写进文件并让闸门转绿。
//
// **一条闸门把自己引到错误 remedies 上，比闸门本身红更坏**：
// 红会让人去查，绿会让人把它提交下去。所以两种漂移必须分开，
// 而且分开之后要有一条测试证明这个区分是真的（TestClassifyDrift）。
//
// 边界取 `package opskeepermiddleware` 这一行：它同时是包名断言的对象
// （packageName 常量）和 banner 的末尾，所以两边都能切在这一行，
// 不会出现"切出来的头部里混着表"的情况。
func classifyDrift(got, want []byte) generationDrift {
	if bytes.Equal(got, want) {
		return driftNone
	}
	gotBanner, gotRest, gotOK := bytes.Cut(got, []byte(bannerBoundary))
	wantBanner, wantRest, wantOK := bytes.Cut(want, []byte(bannerBoundary))
	if gotOK && wantOK && bytes.Equal(gotBanner, wantBanner) {
		// 头部一致、表不一致：适配器动了，该重新生成。
		_ = gotRest
		_ = wantRest
		return driftTools
	}
	if gotOK && wantOK && bytes.Equal(gotRest, wantRest) {
		// 表一致、头部不一致：有人挪了 generatedHeader，重新生成只会固化它。
		return driftBanner
	}
	return driftTools
}

// TestTheNotPackagedLedgerIsCurrent keeps the exclusion list honest in both
// directions. An entry that no longer names a registered read tool is
// stale and must be deleted; a read tool that is neither packaged nor
// listed is a tool that vanished from the node without anybody deciding
// that it should.
func TestTheNotPackagedLedgerIsCurrent(t *testing.T) {
	reg, err := Registry()
	if err != nil {
		t.Fatalf("build the adapter registry: %v", err)
	}
	packaged := map[string]bool{}
	for _, tool := range PackagedReadTools(reg) {
		packaged[tool.Name] = true
	}
	for _, tool := range ReadTools(reg) {
		if packaged[tool.Name] {
			continue
		}
		if _, ok := NotPackaged[tool.Name]; !ok {
			if _, ok := NotPackagedFamilies[ParseFamily(tool.Name)]; !ok {
				t.Fatalf("%s is a read tool that is neither packaged nor excluded with a reason: "+
					"either ship it or say why not", tool.Name)
			}
		}
	}
	for fam, reason := range NotPackagedFamilies {
		if strings.TrimSpace(reason) == "" {
			t.Fatalf("family %q is excluded with no reason", fam)
		}
		registered := 0
		for _, tool := range ReadTools(reg) {
			if ParseFamily(tool.Name) == fam {
				registered++
			}
		}
		if registered == 0 {
			t.Fatalf("family %q is excluded but the adapters register no read tool under it: "+
				"the exclusion is obsolete, delete it", fam)
		}
	}
	for name, reason := range NotPackaged {
		if strings.TrimSpace(reason) == "" {
			t.Fatalf("%s is excluded with no reason", name)
		}
		found := false
		for _, tool := range ReadTools(reg) {
			if tool.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s is in NotPackaged but the adapters no longer register a read tool by that "+
				"name: the exclusion is obsolete, delete it", name)
		}
	}
}

// TestClassifyDrift proves the two drift shapes really are told apart,
// because the whole point of splitting them is that one of them must not
// be answered by regenerating.
//
// 每一条都写成"照着 remedy 做一遍，看结果是不是它承诺的样子"：
// 一个只断言枚举值的测试，说的是这个 switch 有几个分支；
// 一个断言"重新生成之后闸门会绿"的测试，说的是**这个陷阱是真的**。
func TestClassifyDrift(t *testing.T) {
	const banner = "// GENERATED FILE — do not edit.\n" + bannerBoundary
	table := banner + "var tools = []toolSpec{{Name: \"pg.lock_waits\"}}\n"

	t.Run("in sync", func(t *testing.T) {
		if got := classifyDrift([]byte(table), []byte(table)); got != driftNone {
			t.Errorf("classifyDrift on two identical files = %v, want driftNone", got)
		}
	})

	t.Run("the adapters moved", func(t *testing.T) {
		changed := banner + "var tools = []toolSpec{{Name: \"pg.lock_waits\"}, {Name: \"redis.big_keys\"}}\n"
		got := classifyDrift([]byte(table), []byte(changed))
		if got != driftTools {
			t.Errorf("classifyDrift after an adapter gained a tool = %v, want driftTools", got)
		}
		if drift := classifyDrift([]byte(changed), []byte(changed)); drift != driftNone {
			t.Errorf("regenerating after driftTools does not clear it: %v", drift)
		}
	})

	// 这一条是本测试存在的理由。
	t.Run("only the banner moved", func(t *testing.T) {
		mangled := "// GENERATED FILE — do not edit.\n// (a test's doc comment)\n" +
			bannerBoundary + "var tools = []toolSpec{{Name: \"pg.lock_waits\"}}\n"
		got := classifyDrift([]byte(mangled), []byte(table))
		if got != driftBanner {
			t.Errorf("classifyDrift with an identical tool table and a mangled banner = %v, "+
				"want driftBanner — told driftTools, the failure message would tell the "+
				"reader to regenerate", got)
		}
		// 陷阱本身，而且它正是这句话要成立的原因：
		//
		// 照着原来那句"重新生成"做，文件被写成生成器现在会产出的样子——
		// 也就是那个坏 banner——于是逐字节比对**转绿**，而 shipped banner 是坏的。
		// 闸门此后会一直为一个损坏的头部背书，直到有人再改一次 generatedHeader。
		//
		// 所以"头部漂移"必须被单独认出来：它要的不是重新生成，是去看
		// generatedHeader 里混进了什么。
		if drift := classifyDrift([]byte(mangled), []byte(mangled)); drift != driftNone {
			t.Errorf("after regenerating, the gate reports %v rather than passing; "+
				"the synthetic case no longer describes what regeneration would produce", drift)
		}
		if drift := classifyDrift([]byte(table), []byte(mangled)); drift != driftBanner {
			t.Errorf("a regeneration is exactly the banner-only case, got %v", drift)
		}
	})

	// 一个两边都切不出边界的文件不该被当成"只是 banner 变了"。
	t.Run("neither side has the boundary", func(t *testing.T) {
		if got := classifyDrift([]byte("package other\n"), []byte("package other\nmore\n")); got != driftTools {
			t.Errorf("classifyDrift on unparseable files = %v, want driftTools "+
				"(an unknown shape is never a banner-only drift)", got)
		}
	})
}

// TestTheMiddlewareToolsetIsReadOnly is the negative control for the claim
// the package doc makes.
//
// The generator copies whatever the adapters register, filtered by class.
// If an adapter reclassified a write down to L1 — by accident, or by
// deciding a particular kill was "really a diagnostic" — the tool would
// appear in the node's manifest on the next regeneration, and a reviewer
// reading a large generated diff is exactly the reviewer who will not
// notice one line. So the class is asserted here, per tool, against the
// adapters' own registration: the check does not trust the generated file,
// it re-derives the answer.
func TestTheMiddlewareToolsetIsReadOnly(t *testing.T) {
	reg, err := Registry()
	if err != nil {
		t.Fatalf("build the adapter registry: %v", err)
	}
	all := Tools(reg)
	if len(all) == 0 {
		t.Fatal("the adapter registry is empty, so this test would pass for the wrong reason")
	}
	var writes int
	for _, tool := range all {
		if IsRead(tool.Risk) {
			continue
		}
		writes++
		// The tool must not be in the generated manifest, whatever its
		// class says today.
		for _, read := range ReadTools(reg) {
			if read.Name == tool.Name {
				t.Fatalf("%s is %s but was generated into the read-only toolset", tool.Name, tool.Risk)
			}
		}
	}
	if writes == 0 {
		t.Fatalf("the adapters registered no write tool at all, so this test proves nothing about the filter")
	}
}

// TestMiddlewareFamiliesComeFromTheAdapters asserts the family list against
// the names the adapters actually register.
//
// FamilyNames is consumed by the manager's upcall routing, so a family that
// no adapter uses would be a routing rule that can never fire, and an
// adapter registering under a prefix that is not listed would be a tool the
// node can never reach — a silent, total failure of the feature with no
// error anywhere.
func TestMiddlewareFamiliesComeFromTheAdapters(t *testing.T) {
	reg, err := Registry()
	if err != nil {
		t.Fatalf("build the adapter registry: %v", err)
	}
	declared := map[string]bool{}
	for _, f := range FamilyNames() {
		declared[f] = true
	}
	seen := map[string]int{}
	for _, tool := range Tools(reg) {
		fam := ParseFamily(tool.Name)
		if fam == "" {
			t.Fatalf("%s has no middleware family: add its prefix to families and to FamilyNames", tool.Name)
		}
		if !declared[string(fam)] {
			t.Fatalf("%s parses to family %q, which FamilyNames does not list", tool.Name, fam)
		}
		seen[string(fam)]++
	}
	for _, f := range FamilyNames() {
		if seen[f] == 0 {
			t.Fatalf("family %q is listed but no adapter registers a tool under it", f)
		}
	}
}

// renderToolset writes the Go source for the given tools.
func renderToolset(tools []Tool) ([]byte, error) {
	var b strings.Builder
	b.WriteString("package " + packageName + "\n\n")
	b.WriteString(generatedHeader)

	b.WriteString("// toolSpec is one tool this package contributes to the agent.\n")
	b.WriteString("//\n")
	b.WriteString("// The table is data, not code: every entry is the same shape and every\n")
	b.WriteString("// entry does the same thing, which is hand the call to the host. Nothing\n")
	b.WriteString("// here interprets an argument or touches a system, because everything that\n")
	b.WriteString("// does lives in the control plane, where it is permissioned, covered and\n")
	b.WriteString("// audited.\n")
	b.WriteString("type toolSpec struct {\n")
	b.WriteString("\t// Name is what the model calls. It is also the name the host looks\n")
	b.WriteString("\t// up, so it is one string end to end.\n")
	b.WriteString("\tName string\n")
	b.WriteString("\t// Label is the human-readable name the console shows.\n")
	b.WriteString("\tLabel string\n")
	b.WriteString("\t// Description is what the model reads to decide whether to call this.\n")
	b.WriteString("\tDescription string\n")
	b.WriteString("\t// Parameters is the tool's JSON Schema, derived from the adapter's own\n")
	b.WriteString("\t// argument map. The adapter is what parses the call, so its idea of which\n")
	b.WriteString("\t// argument is required is the one that decides whether a call works.\n")
	b.WriteString("\tParameters string\n")
	b.WriteString("}\n\n")
	b.WriteString("// tools is the whole inventory, sorted by name.\n")
	b.WriteString("var tools = []toolSpec{\n")

	sorted := append([]Tool(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, tool := range sorted {
		schema, err := schemaFor(tool)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", tool.Name, err)
		}
		b.WriteString("\n\t{\n")
		b.WriteString("\t\tName:        " + strconv.Quote(tool.Name) + ",\n")
		b.WriteString("\t\tLabel:       " + strconv.Quote(tool.Name) + ",\n")
		b.WriteString("\t\tDescription: " + strconv.Quote(tool.Description) + ",\n")
		if strings.Contains(schema, "`") {
			return nil, fmt.Errorf("%s: the schema contains a backtick and cannot be a raw literal", tool.Name)
		}
		b.WriteString("\t\tParameters: `" + schema + "`,\n")
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n")
	b.WriteString("\n// ToolNames returns the inventory in order, for the host-side drift\n")
	b.WriteString("// check and for diagnostics.\n")
	b.WriteString("func ToolNames() []string {\n")
	b.WriteString("\tout := make([]string, 0, len(tools))\n")
	b.WriteString("\tfor _, t := range tools {\n")
	b.WriteString("\t\tout = append(out, t.Name)\n")
	b.WriteString("\t}\n")
	b.WriteString("\treturn out\n")
	b.WriteString("}\n")

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("the generated source does not compile: %w", err)
	}
	return src, nil
}

// schemaFor picks the schema a tool ships: the adapter's own complete one
// when it has one, otherwise one derived from its flat argument map.
//
// Two sources rather than one because the flat map cannot express a nested
// object at all, and the alternative — describing the shape in prose beside
// an untyped argument — is what git.find_runtime_link used to do. When an
// adapter takes the trouble to write a schema, it is the authority:
// re-deriving one from a lossy flat map would silently throw away the
// branches.
func schemaFor(tool Tool) (string, error) {
	if strings.TrimSpace(tool.Schema) != "" {
		// Re-encoded rather than passed through: the generated file
		// embeds the schema as a raw literal, and a caller-supplied
		// literal that happened to contain a backtick would break the
		// generated source. Round-tripping through the JSON encoder also
		// proves it parses, which is the property the agent needs.
		var decoded any
		if err := json.Unmarshal([]byte(tool.Schema), &decoded); err != nil {
			return "", fmt.Errorf("the adapter's ParamsSchema is not valid JSON: %w", err)
		}
		out, err := json.MarshalIndent(decoded, "", "  ")
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
	return schemaOf(tool.Args)
}

// schemaOf renders an adapter argument map as a JSON Schema object.
//
// The adapter's map is a flat name → type table with a trailing "!" marking
// an argument required — a convention that predates JSON Schema in this
// codebase. The conversion is mechanical, and an unknown type is an error
// rather than a default: a tool whose new argument type silently rendered
// as a string would be a tool the model calls with the wrong shape, and the
// failure would look like the model's mistake.
func schemaOf(args map[string]string) (string, error) {
	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	sort.Strings(names)

	props := map[string]map[string]string{}
	required := make([]string, 0, len(args))
	for _, name := range names {
		raw := args[name]
		typ := raw
		if strings.HasSuffix(typ, middlewareregistry.RequiredMarker) {
			typ = strings.TrimSuffix(typ, middlewareregistry.RequiredMarker)
			required = append(required, name)
		}
		jsonType, err := jsonTypeOf(typ)
		if err != nil {
			return "", fmt.Errorf("argument %q: %w", name, err)
		}
		props[name] = map[string]string{"type": jsonType}
	}

	// properties is written even when it is empty. A tool that takes no
	// arguments still has to present as an object with a parameter list,
	// because the alternative — a schema with no properties key at all —
	// is what a model reads as "this tool's input shape is unknown", and
	// it responds by inventing one.
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		sort.Strings(required)
		schema["required"] = required
	}
	// Marshalled through a struct-like map with sorted keys: encoding/json
	// sorts map keys, so the output is stable across runs and a
	// regeneration that changed nothing produces an identical file.
	out, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func jsonTypeOf(typ string) (string, error) {
	switch typ {
	case "int":
		return "integer", nil
	case "string":
		return "string", nil
	case "bool":
		return "boolean", nil
	default:
		return "", fmt.Errorf("unknown argument type %q: add it to jsonTypeOf rather than letting it default", typ)
	}
}

// TestSchemaRenderingIsDeterministic guards the drift test's premise: the
// generator is compared byte for byte, so a rendering that varied between
// runs would fail the build at random.
func TestSchemaRenderingIsDeterministic(t *testing.T) {
	args := map[string]string{"z": "int!", "a": "string", "m": "bool"}
	first, err := schemaOf(args)
	if err != nil {
		t.Fatalf("schemaOf: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := schemaOf(args)
		if err != nil {
			t.Fatalf("schemaOf: %v", err)
		}
		if again != first {
			t.Fatalf("schemaOf is not deterministic:\n%s\n%s", first, again)
		}
	}
	if err := json.Unmarshal([]byte(first), &map[string]any{}); err != nil {
		t.Fatalf("the rendered schema is not JSON: %v", err)
	}
}

func TestParseFamily(t *testing.T) {
	cases := []struct {
		in   string
		want Family
	}{
		{"pg.lock_waits", FamilyPostgres},
		{"redis.info", FamilyRedis},
		{"k8s.pod_list", FamilyK8s},
		{"kafka.consumer_lag", FamilyKafka},
		{"rabbitmq.queue_list", FamilyRabbitMQ},
		{"mq.drain_queue", FamilyMQ},
		// A name with no dot belongs to nothing, and neither does a
		// prefix that merely contains a family name.
		{"host_cpu_spike", ""},
		{"",
			""},
		{"pgsql.info", ""},
		{"pg", ""},
	}
	for _, c := range cases {
		if got := ParseFamily(c.in); got != c.want {
			t.Errorf("ParseFamily(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsReadCoversOnlyL0AndL1(t *testing.T) {
	reads := []adapter.RiskLevel{adapter.RiskL0ReadOnly, adapter.RiskL1Diagnostic}
	writes := []adapter.RiskLevel{adapter.RiskL2SoftWrite, adapter.RiskL3HardWrite, adapter.RiskL4Destructive}
	for _, rl := range reads {
		if !IsRead(rl) {
			t.Errorf("IsRead(%s) = false, want true", rl)
		}
	}
	for _, rl := range writes {
		if IsRead(rl) {
			t.Errorf("IsRead(%s) = true, want false", rl)
		}
	}
}

// repoRoot walks up from this file until it finds the workspace root.
//
// It looks for go.work rather than go.mod: this package now lives in the
// manager module, whose go.mod is at core/manager, and the generated file
// it compares against lives in another module entirely. Stopping at the
// first go.mod would resolve generatedPath against core/manager and look
// for a file that is not there.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// The shared tracked markers, not go.work: a clean clone has no
	// go.work, and the old go.mod fallback would have stopped at
	// core/manager -- a module directory, not the repository root -- and
	// looked for the generated file under it, which is the "read a stale
	// copy from the wrong tree" failure the comment above warns about.
	root, ok := reporoot.Find(dir, 10)
	if !ok {
		t.Fatalf("no repository root above %s", dir)
	}
	return root
}

const generatedHeader = `// GENERATED FILE — do not edit.
//
// Produced from the live registration of the tools in
// core/manager/middleware/adapter by core/manager/middleware/toolset's tests. The
// adapter is the authority: it is what executes the call and parses the
// arguments, so its description and its argument map are the ones that have
// to be correct, and this file is the copy the node's agent reads.
//
// Regenerate with:
//
//	OPSKEEPER_UPDATE_TOOLSET=1 go test ./core/manager/middleware/toolset/ -run Toolset
//
// then run scripts/sync-pig-ops.sh to copy the extension into the package.
// TestToolsetMatchesTheAdapters fails if this file and the adapters
// disagree, so editing it by hand fails a test rather than shipping a menu
// the executor cannot parse.
//
// Only L0 and L1 tools appear here. The writes the same adapters offer are
// reachable through the control plane's approval path and deliberately not
// through this one.

`
