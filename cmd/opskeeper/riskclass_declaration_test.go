package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Dual sign decides what needs a second signature by reading the risk class a
// producer put on the row. A producer that puts nothing gets single-signer —
// which is right for a row nobody classified and wrong for a row where the
// producer simply did not bother.
//
// Those two cases look identical in the source, so this is the inventory that
// tells them apart: every ProposeInput in production either declares a class
// or is on the list below with a reason. A new producer with no class and no
// entry here is the failure this test is for.
func TestEveryProducerDeclaresHowFarItsActionReaches(t *testing.T) {
	root := repoRootForTest(t)

	// Producers that do not classify, and why. Each is a claim somebody has
	// to make deliberately.
	unclassified := map[string]string{
		"mcp_call": "MCP 服务器不声明工具类别，提案点拿不到任何可信的分类。" +
			"**不猜**：把它标成 destructive 会让每一次 MCP 调用都需要两个管理员，" +
			"而那是没有人选过的风险等级。缺口是真的，见 dataguard 登记表 approval.dual-sign 行。",
	}

	files := []string{
		"cmd/opskeeper/main.go",
		"cmd/opskeeper/agentkernel_approval_inbox.go",
	}
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		lines := strings.Split(string(data), "\n")
		for i := 0; i < len(lines); i++ {
			if !strings.Contains(lines[i], "ProposeInput{") {
				continue
			}
			// Read forward to the closing brace of the literal.
			kind, declared := "", false
			for j := i; j < len(lines) && j < i+20; j++ {
				// First one wins: the fields appear once each, and a later
				// line that happens to hold no Kind would otherwise blank
				// the one that did.
				if kind == "" {
					kind = extractField(lines[j], "Kind")
				}
				if extractField(lines[j], "RiskClass") != "" {
					declared = true
				}
				if extractField(lines[j], "BlastRadius") != "" {
					declared = true
				}
				if strings.TrimSpace(lines[j]) == "})" || strings.TrimSpace(lines[j]) == "})" {
					break
				}
			}
			if declared {
				continue
			}
			if why, ok := unclassified[kind]; ok {
				if why == "" {
					t.Errorf("the exemption for %q has no reason written down", kind)
				}
				continue
			}
			t.Errorf("%s:%d proposes a %q with no RiskClass and no BlastRadius. "+
				"A producer that does not classify its own action gets single-signer "+
				"approval by default, which is the opposite of what a proposal is for. "+
				"Either declare the class or add it to the exemption list with a reason.",
				rel, i+1, kind)
		}
	}
}

// extractField pulls `Field: "value"` off one line, tolerating the gofmt
// alignment that puts spaces after the colon.
func extractField(line, field string) string {
	idx := strings.Index(line, field+":")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(line[idx+len(field)+1:])
	rest = strings.TrimPrefix(rest, `"`)
	if q := strings.Index(rest, `"`); q >= 0 {
		return rest[:q]
	}
	return rest
}
