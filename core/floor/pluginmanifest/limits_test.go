package pluginmanifest

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
	// The executors have to be registered for skill.Get to find them.
	_ "github.com/vincent-wuhan/opskeeper/core/floor/skill/builtin"
)

// highCardinalityTools are the shipped reads whose reply size the *caller*
// chooses: max_lines, max_matches, which file, which pid.
//
// This list is the assertion that §4.28.4's blocking gap stays closed. The
// gap was not that a mechanism was missing — the spill helper and its 1 MiB
// constant had been written — it was that no tool called it and no manifest
// declared anything, so the highest-volume tools on a node shipped with no
// ceiling at all.
var highCardinalityTools = []string{
	"host_dmesg",
	"host_grep_file",
	"host_lsof",
	"host_mtr",
	"host_read_journal",
	"host_sosreport",
	"host_strace",
	"host_tail_file",
	"host_traceroute",
}

// Every one of them declares a ceiling in its own metadata AND in the
// shipped manifest, and the two agree.
//
// Three assertions rather than one, because they fail for different reasons:
// the first is "the author wrote a number down", the second is "the tool the
// node runs knows about that number", and the third is "the two files have
// not drifted". A single assertion would have passed on any one of them
// alone.
func TestEveryHighCardinalityReadDeclaresACeilingInBothPlacesItExists(t *testing.T) {
	plugins, err := LoadAll(filepath.Join(repoRoot(t), "plugins", "pig-ops"))
	if err != nil {
		t.Fatalf("load the shipped plugin catalog: %v", err)
	}
	declared := map[string]domain.ToolLimits{}
	for _, p := range plugins {
		for _, tool := range p.Manifest.Spec.Tools {
			declared[tool.Name] = tool.Limits
		}
	}
	if len(declared) == 0 {
		t.Fatal("no plugin manifests were loaded; the test would pass on nothing")
	}

	for _, name := range highCardinalityTools {
		exec, ok := skill.Get(name)
		if !ok {
			t.Errorf("%s is not a registered tool; this list and the toolset have drifted", name)
			continue
		}
		fromCode := exec.Metadata().Limits
		if fromCode.OutputBytes <= 0 {
			t.Errorf("%s has caller-controlled reply volume and declares no output ceiling in its own metadata; "+
				"the node would serve it the host default, which nobody reviewed", name)
		}
		if fromCode.TimeoutSeconds <= 0 {
			t.Errorf("%s declares no wall-clock ceiling; a tool that ignores its context would hold its slot for ever", name)
		}

		fromManifest, ok := declared[name]
		if !ok {
			t.Errorf("%s is not declared in any shipped manifest", name)
			continue
		}
		if fromManifest != fromCode {
			t.Errorf("%s declares output_bytes=%d timeout_seconds=%d and its executor declares %d/%d; "+
				"the manifest is what the host enforces, so the two are one number and must be one",
				name, fromManifest.OutputBytes, fromManifest.TimeoutSeconds,
				fromCode.OutputBytes, fromCode.TimeoutSeconds)
		}
	}
}

// 工具配额的**词表是闭合的**，而闭不闭合只有一条判据：这个字段能不能被执行。
//
// 这条守卫存在的理由是一条真实的失效路径：计划里写着「per-tool 内存/输出
// 上限」，看到 output_bytes 落地而 memory 缺席的人，很容易把缺席读成一个
// 还没填的坑，然后补上一个字段——**而那个字段在当前隔离粒度下无法执行**
// （`plugins.SubprocessPlugin.runOnce` 每个插件一个进程，同一扩展里的八个
// 工具共享地址空间）。包作者会照着它调大查询范围，宿主并不执行，
// 于是多出来的是一个兑现不了的承诺，而不是一个控制。
//
// 所以这里不是"提醒别加"，是"加的时候让这条测试把三件事一起说出来"：
// 隔离粒度、执行点、以及新字段的名字是否与它实际能保证的粒度相符。
func TestTheToolLimitVocabularyIsClosed(t *testing.T) {
	want := map[string]bool{"OutputBytes": true, "TimeoutSeconds": true}

	typ := reflect.TypeOf(domain.ToolLimits{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if want[name] {
			delete(want, name)
			continue
		}
		t.Errorf("domain.ToolLimits gained a field %q. Before shipping it, all three of these "+
			"have to be true, and the field name has to match the one that is not:\n"+
			"  1. there is an object the host can apply it to — today the only per-tool object "+
			"is the reply at the tool socket (toolbroker.replyFor) and the call's context "+
			"(ToolBinding.Timeout); a memory ceiling needs a process or a cgroup, and "+
			"plugins.SubprocessPlugin.runOnce gives one process per *extension*, not per tool;\n"+
			"  2. the enforcement is on the host side, reachable by policygate before the "+
			"tool runs — the yaml comment says a declared limit is a limit the host applies, "+
			"not a request the tool may honour;\n"+
			"  3. the name says the granularity it can actually guarantee. A per-extension "+
			"ceiling called Memory on a per-tool struct promises more than it delivers, and a "+
			"package author will size their queries against the promise.\n"+
			"See the ToolLimits doc comment in core/domain/plugin.go for the full argument.",
			name)
	}
	for name := range want {
		t.Errorf("domain.ToolLimits no longer has %q; the manifest schema and every "+
			"pig-ops.yaml in plugins/ still declare it", name)
	}
}
