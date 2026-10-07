package main

import (
	"regexp"
	"strings"
	"testing"
)

// This is the guard for a defect that was in the tool for as long as the tool
// existed, and that nobody could see because it made the output look
// confident.
//
// `pkgKey` welded the core/manager prefix onto every source's relative
// package directory. For core/manager sources that is correct, because that is
// where those packages live. For core/domains sources it produces a path no
// import in the tree has ever named — "core/manager/biz/setting" for a package
// every consumer imports as "core/domains/biz/setting" — and the join between a
// package's exported surface and the path a consumer imported then missed for
// every one of them. No error, no warning, just a lookup that returns nothing.
//
// Two things followed, and both of them made the report wrong in the direction
// that discourages people from cutting edges:
//
//   - An interface declared in core/domains was read as a value, so a
//     substitutable port was filed under "data, expensive". This tool exists
//     because a hand table made exactly that classification by mistake twice
//     (decisions 228 and 230); here the same error was being made
//     programmatically, over half the tree, on every run, by nobody's hand.
//   - The shape counter added in decision 254 reported nothing at all for any
//     core/domains type, which is why `grafana -> setting` came back with nine
//     string constants and no measurement for the `*settingbiz.Service` struct
//     among them.
//
// The test below builds the smallest tree that separates the two prefixes. It
// is not a regression test for a fix that might be reverted by accident — the
// fix is a two-line change that looks like a simplification to anyone who has
// not read this. It is a statement that a key derived from one module's prefix
// cannot answer questions about a second module.

// seamVerdictsOn runs the seam report over a written tree and returns
// "from -> to" mapped to its verdict.
func seamVerdictsOn(t *testing.T, root string) map[string]string {
	t.Helper()
	sources, _, err := parseControlPlane(root)
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	var sb strings.Builder
	printSeams(&sb, sources, defaultRules())
	out := map[string]string{}
	for _, line := range strings.Split(sb.String(), "\n") {
		m := seamRowRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out[m[3]+" -> "+m[4]] = m[1]
	}
	if len(out) == 0 {
		t.Fatalf("the seam report produced no rows for the written tree:\n%s", sb.String())
	}
	return out
}

// The discriminating case: the producer sits under the second prefix and the
// symbol it declares is an interface, so the verdict has to come from the
// producer's own declaration rather than from the consumer's use of it. With
// the manager prefix welded on, the lookup misses and the interface is filed
// as a data shape.
func TestAnInterfaceInTheSecondModuleIsStillAnInterface(t *testing.T) {
	portPath := controlPlanePrefixes[1] + "biz/prod"
	root := writeTree(t, map[string]string{
		"core/manager/biz/consumer/use.go": `package consumer

import prod "` + portPath + `"

// Door is the only thing this consumer selects, and it is a contract.
type Door interface {
	Open() error
}

var _ prod.Door = (*Local)(nil)

type Local struct{}

func (Local) Open() error { return nil }
`,
		"core/domains/biz/prod/port.go": `package prod

// Door is a substitutable port declared in the second module.
type Door interface {
	Open() error
}
`,
	})

	got := seamVerdictsOn(t, root)["consumer -> prod"]
	if got == "" {
		t.Fatal("the consumer -> prod edge is missing from the report")
	}
	if got == "data" {
		t.Errorf("an edge whose only selected symbol is an interface declared in %s is "+
			"classified %q. That is the decision-228 misclassification, and it can only "+
			"come from a package key that names the wrong module", portPath, got)
	}
}

// The shape half of the same defect, which is what decision 254's counter ran
// into. A struct declared in the second module has to be measured, or the
// report says nothing about the size of the thing being carried — and silence
// reads as "nothing worth carrying" rather than as "the tool did not look".
func TestAStructInTheSecondModuleIsStillMeasured(t *testing.T) {
	portPath := controlPlanePrefixes[1] + "biz/prod"
	root := writeTree(t, map[string]string{
		"core/manager/biz/consumer/use.go": `package consumer

import prod "` + portPath + `"

func Use(p prod.Wide) int { return len(p.Skills) }
`,
		"core/domains/biz/prod/wide.go": `package prod

type Wide struct {
	Pack   *Pack
	Skills []string
	Name   string
}

type Pack struct{ ID string }
`,
	})

	sources, _, err := parseControlPlane(root)
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	var sb strings.Builder
	printSeams(&sb, sources, defaultRules())
	if !strings.Contains(sb.String(), "Wide(3f 2c") {
		t.Errorf("the shape line does not report Wide, so a struct declared in %s is "+
			"invisible to the counter:\n%s", portPath, sb.String())
	}
}

// The other half of the fix has to hold too, and it is the reason this was not
// simply "use the module prefix": for core/manager the key must be exactly what
// it always was. Every other test in this package resolves package keys through
// pkgKey, so a change that broke the manager side would show up there — but
// this says so at the function rather than by implication.
func TestPkgKeyIsTheImportPathForBothModules(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"github.com/vincent-wuhan/opskeeper/core/manager/biz/alert/usecase",
			"github.com/vincent-wuhan/opskeeper/core/manager/biz/alert"},
		{"github.com/vincent-wuhan/opskeeper/core/domains/biz/setting/service",
			"github.com/vincent-wuhan/opskeeper/core/domains/biz/setting"},
		// A package directly under the module prefix is the boundary the old
		// spelling got most wrong: it produced the bare prefix, which names
		// the module rather than the package in it.
		{"github.com/vincent-wuhan/opskeeper/core/domains/root",
			"github.com/vincent-wuhan/opskeeper/core/domains"},
	} {
		if got := pkgKey(source{path: tc.path, pkg: "some/relative/dir"}); got != tc.want {
			t.Errorf("pkgKey(%q) = %q, want %q. The pkg field is deliberately ignored: it "+
				"is relative and carries no module, which is the whole defect",
				tc.path, got, tc.want)
		}
	}
}

var _ = regexp.MustCompile // the row pattern lives in seams_test.go; named here only to document the coupling
