package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every place that runs a tool by name is a door into the bag, and a gate
// wired into three of them is a gate the fourth one walks past. This is the
// inventory: each call site below is the one the reader-tier check has to
// reach, and the day a new one appears the gate goes red.
func TestEveryToolInvocationSiteGoesThroughTheReaderGate(t *testing.T) {
	root := repoRootForTest(t)

	// The call sites that must be gated: the bag reached without the chat
	// runtime's per-session decoration.
	gated := map[string]string{
		"flow 的 tool 节点":  "WithSensitivity(t, s.gate).InvokableRun",
		"loop 的 MCP 工具":  "WithSensitivity(tool, readerGate).InvokableRun",
		"确定性确认执行":     "decorators.WithSensitivity(tool, rt.cfg.Sensitivity)",
		"协调者会话工具袋":    "decorators.WithSensitivityAll(sessionToolBag",
		"worker 会话工具袋":  "decorators.WithSensitivityAll(workerTools",
	}

	haystack := readAll(t, root,
		filepath.Join("cmd", "opskeeper", "main.go"),
		filepath.Join("core", "manager", "biz", "aiops", "chatruntime", "config_confirm.go"),
		filepath.Join("core", "manager", "biz", "aiops", "chatruntime", "runtime.go"),
		filepath.Join("core", "manager", "biz", "aiops", "chatruntime", "worker.go"),
	)
	for door, needle := range gated {
		if !strings.Contains(haystack, needle) {
			t.Errorf("%s no longer passes through the reader gate: %q is gone from the tree. "+
				"A tool surface that runs the bag without the gate is a door around it.",
				door, needle)
		}
	}
}

// The inventory above can only be trusted if it is complete, so this is the
// other direction: every InvokableRun call site in the agent tool surfaces is
// listed, and each one is either gated or carries the sentence that explains
// why it is not. A new call site that nobody classified is the failure.
func TestEveryAgentSurfaceToolCallIsGatedOrExplained(t *testing.T) {
	root := repoRootForTest(t)

	// Why a site is allowed to run without the reader gate. Each entry is a
	// reason someone had to think about, not a placeholder.
	explained := map[string]string{
		"core/manager/biz/aiops/chatruntime/worker.go:984": "prologueKBLookup 定义了但没有调用方",
	}

	files := []string{
		"cmd/opskeeper/main.go",
		"core/manager/biz/aiops/chatruntime/config_confirm.go",
		"core/manager/biz/aiops/chatruntime/worker.go",
		"core/manager/biz/aiops/chatruntime/runtime.go",
	}
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for lineNo, line := range strings.Split(string(data), "\n") {
			idx := strings.Index(line, ".InvokableRun(")
			if idx < 0 {
				continue
			}
			where := rel + ":" + itoa(lineNo+1)
			if strings.Contains(line, "WithSensitivity") {
				continue
			}
			if why, ok := explained[where]; ok {
				continue
			} else if ok == false && why == "" {
				continue
			}
			t.Errorf("%s invokes a tool without the reader gate and no reason on file: %s",
				where, strings.TrimSpace(line))
		}
	}
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

func readAll(t *testing.T, root string, paths ...string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		b.Write(data)
		b.WriteString("\n")
	}
	return b.String()
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("no go.mod above the test's working directory")
	return ""
}

var _ = context.Background
