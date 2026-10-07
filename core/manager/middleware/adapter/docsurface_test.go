package adapter_test

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/toolset"
)

// docs/api/middleware.md 的族表与总数必须等于活注册表。
//
// 这条守卫的存在是因为那份文档曾经是** fiction**：它列了九个 REST 端点
// （POST /api/v1/middleware、{id}/diagnose/pg、approvals/{ticket_id}/decide …），
// 而中间件从来不是 HTTP 服务——它是 Agent 的工具表。文档读起来像一份完整交付，
// 没有一行能被 curl 复现，而 CI 里没有任何东西会红。
//
// 守卫只钉两件事：族名与每族的工具数、以及总数。**不钉 101 行工具清单**——
// 那等于抄第二份清单，而第二份清单唯一的下场是漂；抄不动的部分（具体工具名）
// 应该去问 `opskeeper-eval vocabulary`，它读的是同一张活表。
const middlewareDocPath = "../../../../docs/api/middleware.md"

// docFamilyRE 匹配族表的一行：| `pg` | 23 | 说明… |
var docFamilyRE = regexp.MustCompile("(?m)^\\|\\s*`([a-z0-9]+)`\\s*\\|\\s*([0-9]+)\\s*\\|")

// docTotalRE 匹配合计行：| **合计** | **101** | … |
var docTotalRE = regexp.MustCompile("(?m)^\\|\\s*\\*\\*合计\\*\\*\\s*\\|\\s*\\*\\*([0-9]+)\\*\\*")

func readMiddlewareDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(middlewareDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", middlewareDocPath, err)
	}
	return string(raw)
}

// liveToolCounts builds the family → count map from the registry, so the
// expected values come from a live registration rather than a second list.
func liveToolCounts(t *testing.T) (map[string]int, int) {
	t.Helper()
	reg, err := toolset.Registry()
	if err != nil {
		t.Fatalf("build the live tool registry: %v", err)
	}
	counts := map[string]int{}
	for _, tool := range toolset.Tools(reg) {
		family := string(toolset.ParseFamily(tool.Name))
		if family == "" {
			t.Fatalf("tool %q belongs to no family: the doc counts by family and this one is in none", tool.Name)
		}
		counts[family]++
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	return counts, total
}

func TestTheDocumentedFamiliesAreExactlyTheRegisteredOnes(t *testing.T) {
	doc := readMiddlewareDoc(t)
	live, _ := liveToolCounts(t)

	documented := map[string]int{}
	for _, match := range docFamilyRE.FindAllStringSubmatch(doc, -1) {
		count, err := strconv.Atoi(match[2])
		if err != nil {
			t.Fatalf("family %s has an unreadable count %q", match[1], match[2])
		}
		if _, dup := documented[match[1]]; dup {
			t.Errorf("family %q is documented twice", match[1])
		}
		documented[match[1]] = count
	}
	if len(documented) == 0 {
		t.Fatalf("no family rows found in %s: the table changed shape and this guard is looking at nothing", middlewareDocPath)
	}

	names := make([]string, 0, len(live))
	for family := range live {
		names = append(names, family)
	}
	sort.Strings(names)
	for _, family := range names {
		got, ok := documented[family]
		if !ok {
			t.Errorf("family %q registers %d tools but the doc does not document it", family, live[family])
			continue
		}
		if got != live[family] {
			t.Errorf("family %q: doc says %d tools, the live registry has %d — "+
				"the doc is a copy of a registration that moved", family, got, live[family])
		}
	}
	for family := range documented {
		if _, ok := live[family]; !ok {
			t.Errorf("the doc documents family %q, which no adapter registers", family)
		}
	}
}

func TestTheDocumentedTotalIsTheLiveTotal(t *testing.T) {
	doc := readMiddlewareDoc(t)
	_, liveTotal := liveToolCounts(t)
	match := docTotalRE.FindStringSubmatch(doc)
	if match == nil {
		t.Fatalf("no 合计 row in %s: the doc's total is the number a reader trusts first", middlewareDocPath)
	}
	documented, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("unreadable total %q", match[1])
	}
	if documented != liveTotal {
		t.Errorf("doc total = %d, live registry has %d tools", documented, liveTotal)
	}
}

// 文档不得把中间件说成 HTTP 服务。
//
// 曾经的整份文档都是 REST。这条守卫读的是那句话本身：只要「没有 REST」这句话被删掉
// 而端点表没回来，中间件的面就又变成了一个不存在的服务。
func TestTheDocSaysThereIsNoRESTAPI(t *testing.T) {
	doc := readMiddlewareDoc(t)
	if !strings.Contains(doc, "这里没有 REST API") {
		t.Errorf("%s no longer states that the middleware surface is not REST; "+
			"the risk is not that a REST API exists, it is that a document describes one", middlewareDocPath)
	}
	// 端点行只能出现在未交付一节的散文里，不能出现在围栏里。
	for _, block := range fencedBlocks(doc) {
		for _, line := range strings.Split(block, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "GET /") || strings.HasPrefix(trimmed, "POST /") ||
				strings.HasPrefix(trimmed, "PUT /") || strings.HasPrefix(trimmed, "DELETE /") {
				t.Errorf("the doc presents an endpoint as live: %s", trimmed)
			}
		}
	}
}

// fencedBlocks returns the contents of every ``` fenced block.
func fencedBlocks(text string) []string {
	var out []string
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			continue
		}
		i++
		start := i
		for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			i++
		}
		out = append(out, strings.Join(lines[start:i], "\n"))
	}
	return out
}
