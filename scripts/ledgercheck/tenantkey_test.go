package ledgercheck

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// tenantKeyCountRE reads the definition count out of the progress section's
// multi-tenancy entry. The ledger states "生产代码里 `tenantFromContext` 的定义共
// **2** 处"; this pulls the number back out of that sentence.
//
// It is deliberately anchored on the function name rather than on a bare
// digit: a bare number in a paragraph is not a claim anybody can hold you to,
// while "the count of these definitions is N" is.
var tenantKeyCountRE = regexp.MustCompile("`tenantFromContext` 的定义共 \\*\\*(\\d+)\\*\\* 处")

// tenantKeyDefRE matches a production definition of a tenant-key derivation
// function. Only `func tenantFromContext(` at the start of a line counts — the
// call sites are not derivations, and counting those is the mistake this gate
// was written after.
var tenantKeyDefRE = regexp.MustCompile(`(?m)^func tenantFromContext\(`)

// TestTheTenantKeyDerivationsAreTheOnesTheLedgerRegisters is the eighteenth
// gate, and it exists because decision 198 had to withdraw a conclusion.
//
// Decision 197 found that the same caller identity is turned into a tenant key
// by two different rules — `tenant_bind.go` prefers the AgentTeams tenant string
// and falls back to a bare decimal user id, while `server/loop/http.go` returns
// `user-<id>` or `default` — and it concluded from that the tenant type itself
// was an open question for the user. Both halves of that were wrong, and the
// reason it took a whole round to see is that nothing in the repository counts
// the derivations. A third copy of the rule can appear in any package and
// nothing goes red, because "how the tenant key is derived here" is not a
// property any test claims.
//
// So this gate makes the count a claim: the ledger registers how many
// derivations exist, and this walks core/ and cmd/ to see whether that is
// still true. A third one appearing is not automatically a bug — gitartifact's
// uint64 one is arguably one already — but it has to be written down, because
// the failure this round was reading two minority copies as if they were the
// whole population.
func TestTheTenantKeyDerivationsAreTheOnesTheLedgerRegisters(t *testing.T) {
	progress := progressSection(t)

	match := tenantKeyCountRE.FindStringSubmatch(progress)
	if match == nil {
		t.Fatalf("the progress section never states how many `tenantFromContext` " +
			"definitions exist in production code; decision 198 registered that count as " +
			"the thing this gate measures, and a count nobody wrote down is a count " +
			"nobody has to keep true")
	}
	claimed, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("the registered count %q is not a number: %v", match[1], err)
	}

	found := tenantKeyDerivations(t)
	if len(found) != claimed {
		sort.Strings(found)
		t.Errorf("the ledger registers %d production `tenantFromContext` definition(s), "+
			"but there are %d:\n  %s\n"+
			"either the ledger is stale or a tenant key is being derived somewhere that "+
			"nobody registered — and a third derivation is how `user-<id>`, a bare "+
			"decimal id, and an AgentTeams tenant string all end up being called the "+
			"same thing in the same table",
			claimed, len(found), strings.Join(found, "\n  "))
	}

	// The entry is only useful while it names what the placeholder actually
	// returns. Without both literals the reader cannot tell which rule this
	// gap is about, and the gap goes back to being invisible — which is the
	// state decision 197 found it in.
	for _, literal := range []string{"`user-%d`", "`default`"} {
		if !strings.Contains(progress, literal) {
			t.Errorf("the multi-tenancy entry never mentions %s, so the placeholder's "+
				"two possible values are not written down; %s", literal, literal)
		}
	}
}

// tenantKeyDerivations returns "path:line" for every production definition of
// a tenant-key derivation, across the two trees that make up the running
// system. Tests are skipped: a helper in a _test.go file derives nothing at
// runtime, and including them would have made gitartifact's three test-only
// WithTenant calls look like derivations.
func tenantKeyDerivations(t *testing.T) []string {
	t.Helper()

	var found []string
	for _, root := range []string{"../../core", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			loc := tenantKeyDefRE.FindIndex(raw)
			if loc == nil {
				return nil
			}
			found = append(found, fmt.Sprintf("%s:%d",
				filepath.ToSlash(path), lineOf(raw, loc[0])))
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return found
}

// lineOf returns the 1-based line number of byte offset in raw.
func lineOf(raw []byte, offset int) int {
	line := 1
	for i := 0; i < offset && i < len(raw); i++ {
		if raw[i] == '\n' {
			line++
		}
	}
	return line
}
