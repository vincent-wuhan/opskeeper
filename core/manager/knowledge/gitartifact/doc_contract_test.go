package gitartifact

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// 这两条守卫把「文档写的形状」和「代码吐的形状」钉在一起。
//
// 起因是一次真实的漂移：docs/api/git-artifact.md 描述的请求体没有 artifact
// 包裹、响应里有 matched 数组与 needs_human_review、错误码是 4000/4003 这样的
// 业务枚举、还有三个从未实现的端点字段——而代码里一个都没有。文档读起来像一份
// 完整交付，实际上没有任何一行能被 curl 复现。CI 里没有任何东西会发现这件事，
// 因为文档和代码从不互相验证。
//
// 现在文档描述的是代码的实际行为（含未交付清单），而下面两条测试让这件事
// 双向成立：代码多一个键文档必须写，文档写一个键代码必须有。

const docPath = "../../../../docs/api/git-artifact.md"

func readAPIDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	return string(raw)
}

// marshalLink 走真实的 JSON 投影，键集合由结构体标签决定——
// 写一个 map 来断言等于让测试复述实现。
func marshalLink(t *testing.T, r LinkResult) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(linkWire{LinkResult: r, NeedsHumanConfirm: r.NeedsHumanConfirm()})
	if err != nil {
		t.Fatalf("marshal link: %v", err)
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal link: %v", err)
	}
	return out
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestLinkWire_KeySetIsExact(t *testing.T) {
	// 满列：CI 透传了作者与提交信息，低置信度。
	full := marshalLink(t, LinkResult{
		Commit: "c", Repo: "r", FilePath: "f", LineStart: 1, LineEnd: 2,
		Author: "alice", CommitMsg: "fix", Confidence: 0.4,
	})
	want := []string{
		"author", "commit", "commit_msg", "confidence", "file_path",
		"line_end", "line_start", "needs_human_confirm", "repo",
	}
	if got := sortedKeys(full); !equalStrings(got, want) {
		t.Errorf("link key set = %v, want %v", got, want)
	}

	// 缺列：CI 没透传作者。这两列是 optional，缺席的含义是「没透传」，
	// 所以它们不能变成空串键——空串会被读成「这个提交没有作者」。
	bare := marshalLink(t, LinkResult{Commit: "c", Confidence: 0.95})
	for _, k := range []string{"author", "commit_msg", "evidence", "flag"} {
		if _, ok := bare[k]; ok {
			t.Errorf("key %q must be absent when unset, got %s", k, bare[k])
		}
	}
	if needs, _ := bare["needs_human_confirm"]; string(needs) != "false" {
		t.Errorf("needs_human_confirm = %s, want false (confidence 0.95)", needs)
	}
}

func TestAPIDocDocumentsEveryWireKey(t *testing.T) {
	doc := readAPIDoc(t)
	link := marshalLink(t, LinkResult{
		Commit: "c", Repo: "r", FilePath: "f", LineStart: 1, LineEnd: 2,
		Author: "alice", CommitMsg: "fix", Confidence: 0.5,
	})
	for _, key := range sortedKeys(link) {
		// 键在文档里有两种合法写法：正文里的行内代码，以及响应示例里的 JSON 键。
		if !strings.Contains(doc, "`"+key+"`") && !strings.Contains(doc, `"`+key+`"`) {
			t.Errorf("docs/api/git-artifact.md does not document the response key %q", key)
		}
	}
}

// jsonBlocks 抽出文档里所有 ```json 围栏的内容。
//
// 契约的载体是这些示例块——读者照着它们发请求。正文里说「这个键我们没有」
// 是诚实的交付说明，不是把没实现的东西写成交付；所以死键的检查只针对
// 示例块，而示例块里出现一个代码不吐的键，就是文档在教人期待一个不存在的
// 响应。
func jsonBlocks(doc string) string {
	var out strings.Builder
	rest := doc
	for {
		start := strings.Index(rest, "```json")
		if start < 0 {
			return out.String()
		}
		rest = rest[start+len("```json"):]
		end := strings.Index(rest, "```")
		if end < 0 {
			return out.String()
		}
		out.WriteString(rest[:end])
		out.WriteByte('\n')
		rest = rest[end+len("```"):]
	}
}

// 文档的示例块不得复活被删掉的键。flag 尤其危险：它与 needs_human_confirm
// 同源，复活它等于把「同一个判断两处表达、其中一处恒空」这个缺陷请回来。
func TestAPIDocHasNoResurrectedKeys(t *testing.T) {
	blocks := jsonBlocks(readAPIDoc(t))
	for _, dead := range []string{
		"needs_human_review", "match_type", "indexed_symbols",
		"total_symbols", "eta_s", "flag", "evidence",
	} {
		if strings.Contains(blocks, `"`+dead+`"`) {
			t.Errorf("docs/api/git-artifact.md shows %q inside a response example; no code path emits it", dead)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
