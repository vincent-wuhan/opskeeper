//go:build e2e

// Charter validation (tasks.md 1.1): frontmatter closed field set, role
// in the AgentTeams role set, positive budget, catalog_refs resolvable
// to real rows of docs/test/e2e-catalog.md.
//
// The role set is enumerated from AgentTeamsWorkerPermissions() — not
// hand-copied. A hand-copied list is exactly how this validator would go
// stale silently the day an eighth role lands.
package explore

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/auth"
)

// charterFieldSet is the closed set of allowed frontmatter keys.
var charterFieldSet = map[string]bool{
	"id":             true,
	"role":           true,
	"worker":         true,
	"tenant":         true,
	"max_tool_calls": true,
	"max_steps":      true,
	"catalog_refs":   true,
}

var frontmatterRe = regexp.MustCompile(`(?s)^---\n(.*?)\n---\n`)
var fieldRe = regexp.MustCompile(`(?m)^([a-z_]+):[ \t]*(.*)$`)
var catalogRefRe = regexp.MustCompile(`^[A-P][0-9]+$`)

// agentTeamsRoles enumerates the production role set.
func agentTeamsRoles() map[string]bool {
	out := map[string]bool{}
	for _, row := range auth.AgentTeamsWorkerPermissions() {
		out[row.Role] = true
	}
	return out
}

// repoRootOfExplore locates the repo root from this test's file.
func repoRootOfExplore(t *testing.T) string {
	t.Helper()
	// tests/e2e/explore/charter_test.go → repo root is three dirs up.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("charter: getwd: %v", err)
	}
	for i := 0; i < 3; i++ {
		dir = filepath.Dir(dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("charter: cannot locate repo root from %s: %v", dir, err)
	}
	return dir
}

// parseFrontmatter extracts the frontmatter block from a charter file.
func parseFrontmatter(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("charter: read %s: %v", path, err)
	}
	block := frontmatterRe.FindStringSubmatch(string(raw))
	if block == nil {
		t.Fatalf("charter: %s has no frontmatter block", path)
	}
	fields := map[string]string{}
	for _, m := range fieldRe.FindAllStringSubmatch(block[1], -1) {
		fields[m[1]] = strings.TrimSpace(m[2])
	}
	return fields
}

// catalogCaseIDs parses every case ID (A1..P n) from the catalog.
func catalogCaseIDs(t *testing.T, root string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "docs", "test", "e2e-catalog.md"))
	if err != nil {
		t.Fatalf("charter: read e2e-catalog.md: %v", err)
	}
	out := map[string]bool{}
	rowRe := regexp.MustCompile(`(?m)^\|\s*([A-P][0-9]+)\s*\|`)
	for _, m := range rowRe.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = true
	}
	if len(out) == 0 {
		t.Fatalf("charter: e2e-catalog.md parsed zero case rows — parser is broken")
	}
	return out
}

func charterDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRootOfExplore(t), "docs", "test", "explore-charters")
}

func TestChartersValidate(t *testing.T) {
	roles := agentTeamsRoles()
	cases := catalogCaseIDs(t, repoRootOfExplore(t))
	entries, err := os.ReadDir(charterDir(t))
	if err != nil {
		t.Fatalf("charter: read dir: %v", err)
	}
	validated := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".md") || name == "_template.md" {
			continue
		}
		path := filepath.Join(charterDir(t), name)
		fields := parseFrontmatter(t, path)
		// Closed field set: unknown key → reject.
		for key := range fields {
			if !charterFieldSet[key] {
				t.Errorf("%s: unknown frontmatter field %q (closed set: %v)", name, key, keysOf(charterFieldSet))
			}
		}
		for _, required := range []string{"id", "role", "worker", "tenant", "max_tool_calls", "max_steps"} {
			if fields[required] == "" {
				t.Errorf("%s: missing required field %q", name, required)
			}
		}
		if fields["id"] == "" {
			continue
		}
		// Role must be in the production role set.
		role := fields["role"]
		if !roles[role] {
			t.Errorf("%s: role %q is not in the AgentTeams role set %v", name, role, keysOf(roles))
		}
		// Worker must be the role-bound worker name.
		if want := auth.AgentTeamsWorkerForRole(role); fields["worker"] != want {
			t.Errorf("%s: worker %q != role-bound worker %q", name, fields["worker"], want)
		}
		// Two budgets, both positive integers. A charter without a ceiling
		// on tool calls and steps is a runaway bill, so an absent or
		// non-positive value is a charter that cannot be issued.
		for _, budgetField := range []string{"max_tool_calls", "max_steps"} {
			budget, err := strconv.Atoi(fields[budgetField])
			if err != nil || budget <= 0 {
				t.Errorf("%s: %s %q is not a positive integer", name, budgetField, fields[budgetField])
			}
		}
		// catalog_refs resolvable: the field is a YAML list under the key.
		raw, _ := os.ReadFile(path)
		block := frontmatterRe.FindStringSubmatch(string(raw))
		refs := parseCatalogRefs(block[1])
		if len(refs) == 0 {
			t.Errorf("%s: catalog_refs is empty or unparsable", name)
		}
		for _, ref := range refs {
			if !catalogRefRe.MatchString(ref) {
				t.Errorf("%s: catalog ref %q is not a domain-letter + number form", name, ref)
				continue
			}
			if !cases[ref] {
				t.Errorf("%s: catalog ref %q does not resolve to a row in docs/test/e2e-catalog.md", name, ref)
			}
		}
		validated++
	}
	if validated < 2 {
		t.Fatalf("charter: expected at least 2 real charters, validated %d", validated)
	}
}

func TestChartersRejectUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-extra-field.md")
	content := "---\nid: bad\nrole: investigator\nworker: opskeeper-investigator\nbudget: 5\nfavorite_color: blue\ncatalog_refs:\n  - F1\n---\n\n# bad\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("charter: write temp: %v", err)
	}
	fields := parseFrontmatter(t, path)
	for key := range fields {
		if !charterFieldSet[key] {
			return // rejected as required
		}
	}
	t.Fatalf("charter: unknown field favorite_color was not rejected")
}

func TestChartersRejectUnresolvableCatalogRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-ref.md")
	content := "---\nid: bad\nrole: investigator\nworker: opskeeper-investigator\nbudget: 5\ncatalog_refs:\n  - Z99\n---\n\n# bad\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("charter: write temp: %v", err)
	}
	cases := catalogCaseIDs(t, repoRootOfExplore(t))
	raw, _ := os.ReadFile(path)
	block := frontmatterRe.FindStringSubmatch(string(raw))
	refs := parseCatalogRefs(block[1])
	for _, ref := range refs {
		if !cases[ref] {
			return // Z99 must not resolve
		}
	}
	t.Fatalf("charter: unresolvable ref Z99 was accepted")
}

func TestRoleSetEnumeratedFromProduction(t *testing.T) {
	roles := agentTeamsRoles()
	for _, want := range []string{"investigator", "repairer"} {
		if !roles[want] {
			t.Errorf("production role set is missing %s", want)
		}
	}
	if len(roles) != 7 {
		t.Errorf("expected the 7 production roles, enumerated %d — if an 8th role landed, this is the reminder to extend the charters, not to hand-copy", len(roles))
	}
}

// parseCatalogRefs pulls the catalog_refs YAML list items out of a
// frontmatter block.
func parseCatalogRefs(block string) []string {
	var out []string
	lines := strings.Split(block, "\n")
	inList := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "catalog_refs:") {
			inList = true
			continue
		}
		if inList {
			if strings.HasPrefix(trimmed, "- ") {
				out = append(out, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
				continue
			}
			if trimmed != "" {
				inList = false
			}
		}
	}
	return out
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
