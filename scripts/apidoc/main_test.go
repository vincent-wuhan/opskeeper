package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree materialises a miniature repository: a doc under docs/api and the
// Go files that do or do not register what the doc claims.
//
// The fixtures are tiny on purpose. This command's whole job is comparing a
// claim against a route literal, and a fixture large enough to be realistic
// would mostly be testing the filesystem.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, body := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		// A Go file with no package clause is not Go, and the registration
		// layer reads the tree with go/parser — so every Go fixture is given
		// one. Doing it here rather than in each fixture keeps the bodies
		// below about the one thing they are testing, and keeps the
		// deliberately-unparseable fixture the only one that has to opt out.
		// The extension is checked too: a document is not Go and must not be
		// given a package clause, since a finding's line number is read off it.
		if strings.HasSuffix(path, ".go") && !strings.HasPrefix(strings.TrimSpace(body), "package ") {
			body = "package fixture\n\n" + body
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func claimsOf(t *testing.T, report Report) []string {
	t.Helper()
	out := make([]string, 0, len(report.Missing))
	for _, m := range report.Missing {
		out = append(out, m.Claim)
	}
	return out
}

func TestADocumentedEndpointServedByAGroupMountedRoute(t *testing.T) {
	// The shape this repository actually uses: the handler registers
	// "/v1/thing" and the router mounts the group at "/api".
	root := writeTree(t, map[string]string{
		"docs/api/thing.md": "```\nGET /api/v1/thing\n```\n",
		"cmd/app/main.go":   "func mount(r chi.Router) {\n\tr.Handle(\"/v1/thing\", h)\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("missing = %v, want none", claimsOf(t, report))
	}
	if report.Endpoints != 1 {
		t.Errorf("endpoints claimed = %d, want 1", report.Endpoints)
	}
}

func TestAParameterInTheDocumentedPathMatchesAWildcard(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/thing.md": "```\nGET /api/v1/thing/{id}\n```\n",
		"cmd/app/main.go":   "func mount(r chi.Router) {\n\tr.Get(\"/thing/{id}\", h)\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("missing = %v, want none: {id} is a documented parameter", claimsOf(t, report))
	}
}

// The case this whole command was written for: a document that reads like a
// delivered contract and describes a server that was never built.
func TestAnEndpointNobodyRegisteredIsReported(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/ghost.md": "```\nPOST /api/v1/harness/runs\n```\n",
		"cmd/app/main.go":   "func mount(r chi.Router) {\n\tr.Handle(\"/v1/other\", h)\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 || report.Missing[0].Claim != "endpoint /api/v1/harness/runs" {
		t.Fatalf("missing = %v, want the one ghost endpoint", claimsOf(t, report))
	}
	if report.Missing[0].Line != 2 {
		t.Errorf("line = %d, want 2: a finding a reader cannot jump to is a finding nobody reads", report.Missing[0].Line)
	}
}

// A glob pattern is not a route. The plugin loader carries "/*/pig-ops.yaml",
// and treating that "*" as a subtree mount made this gate call every
// documented endpoint in the repository served.
func TestAGlobPatternIsNotASubtreeMount(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/ghost.md": "```\nPOST /api/v1/harness/runs\n```\n",
		"cmd/app/glob.go":   "var pattern = \"/*/pig-ops.yaml\"\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing = %v, want the glob not to count as a route", claimsOf(t, report))
	}
}

// A subtree mount is a route. chi's "/v1/thing/*" serves everything under it,
// and refusing that would make the gate report a delivered endpoint as a
// phantom — the failure mode that teaches people to add exemptions.
func TestASubtreeMountServesWhatIsUnderIt(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/thing.md": "```\nGET /api/v1/thing/{id}\n```\n",
		"cmd/app/main.go":   "func mount(r chi.Router) {\n\tr.Handle(\"/v1/thing/*\", h)\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("missing = %v, want none", claimsOf(t, report))
	}
}

// Commenting a route out must unregister it. This is the hole a phantom is
// easiest conjured through: write the path in a comment and every check that
// reads strings goes green.
func TestARouteMentionedOnlyInACommentIsNotARoute(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/ghost.md": "```\nPOST /api/v1/harness/runs\n```\n",
		"cmd/app/note.go":   "// the router used to mount \"/api/v1/harness/runs\" here\n/* and \"/api/v1/harness/runs/{id}\" too */\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing = %v, want the commented path not to count", claimsOf(t, report))
	}
}

// A test asserting a request is not a registration. Otherwise a single
// httptest.NewRequest line keeps a deleted endpoint documented for ever.
func TestARouteOnlyAssertedInATestIsNotARoute(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/ghost.md":    "```\nPOST /api/v1/harness/runs\n```\n",
		"cmd/app/main_test.go": "func TestGone(t *testing.T) {\n\thttp.NewRequest(\"POST\", \"/api/v1/harness/runs\", nil)\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing = %v, want the test-only path not to count", claimsOf(t, report))
	}
}

func TestASubcommandClaimMustMatchTheDispatch(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/cli.md":            "```\nopskeeper-eval judge --input a.json\n```\n",
		"cmd/opskeeper-eval/main.go": "func main() {\n\tswitch sub {\n\tcase \"judge\":\n\t\tfs := flag.NewFlagSet(\"judge\", flag.ExitOnError)\n\t\tfs.StringVar(&in, \"input\", \"\", \"\")\n\t\tfs.Parse(nil)\n\t}\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 0 || report.Subcommands != 1 {
		t.Fatalf("missing = %v (claimed %d), want none claimed once", claimsOf(t, report), report.Subcommands)
	}
}

func TestAnUnimplementedSubcommandIsReported(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/cli.md":            "```\nopskeeper-eval leaderboard-check\n```\n",
		"cmd/opskeeper-eval/main.go": "func main() {\n\tswitch sub {\n\tcase \"judge\":\n\t}\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 || !strings.Contains(report.Missing[0].Claim, "leaderboard-check") {
		t.Fatalf("missing = %v, want the unimplemented subcommand", claimsOf(t, report))
	}
}

// The 未交付 sections of these docs name paths on purpose. Reading those as
// claims would force a doc to lie in order to be true about what is missing.
func TestProseAboutAMissingEndpointIsNotAClaim(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/thing.md": "这一节曾经描述 `GET /api/v1/harness/runs`，但它从未实现。\n\n" +
			"```\nGET /api/v1/thing\n```\n",
		"cmd/app/main.go": "func mount(r chi.Router) {\n\tr.Handle(\"/v1/thing\", h)\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("missing = %v, want only the fenced block to count", claimsOf(t, report))
	}
	if report.Endpoints != 1 {
		t.Errorf("endpoints claimed = %d, want 1", report.Endpoints)
	}
}

// A documented {param} is a statement about the shape of the path. Letting it
// match any literal means "/x/{id}" is "served" by a route registered as
// "/x/cases", which is how "/api/v1/middleware" came to be reported as served
// by some unrelated "/{incident_id}".
func TestADocumentedParameterIsNotSatisfiedByALiteralSegment(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/thing.md": "```\nGET /api/v1/thing/{id}\n```\n",
		"cmd/app/main.go":   "func mount(r chi.Router) {\n\tr.Get(\"/v1/thing/cases\", h)\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing = %v, want the literal segment not to satisfy {id}", claimsOf(t, report))
	}
}

// The command line is a claim like any other: a flag written in a document that
// the binary does not define exits with "flag provided but not defined", and a
// subcommand it does not dispatch exits 2. These fixtures pin the four shapes
// that decide whether that finding is real or invented.

func cliTree(doc string) map[string]string {
	return map[string]string{
		"docs/guide.md": doc,
		"cmd/tool/main.go": `import "flag"

func main() {
	switch os.Args[1] {
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		fs.StringVar(&target, "target", "", "")
		dir := fs.String("dir", ".", "")
		_ = dir
		fs.Parse(os.Args[2:])
	case "board":
		fs := flag.NewFlagSet("board", flag.ExitOnError)
		fs.Float64Var(&threshold, "threshold", 0.5, "")
		fs.Parse(os.Args[2:])
	default:
		fmt.Println("unknown subcommand:", os.Args[1])
		os.Exit(2)
	}
}
`,
	}
}

func TestADocumentedFlagIsCheckedAgainstTheBinaryThatDefinesIt(t *testing.T) {
	root := writeTree(t, cliTree("```\ntool run --target x --dir y\n```\n"))
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("missing = %v, want the two documented flags to be accepted", claimsOf(t, report))
	}
	if report.CLIFlags != 2 {
		t.Fatalf("CLIFlags = %d, want 2", report.CLIFlags)
	}
}

func TestADocumentedFlagTheBinaryDoesNotDefineIsRed(t *testing.T) {
	root := writeTree(t, cliTree("```\ntool run --targte x\n```\n"))
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing = %v, want the misspelled flag reported", claimsOf(t, report))
	}
	if !strings.Contains(report.Missing[0].Claim, "--targte") {
		t.Fatalf("claim = %q, want it to name the flag that is not defined", report.Missing[0].Claim)
	}
}

// A longer word that merely starts with a binary name is not an invocation of
// it. "opskeeper-llm-credentials" is a kubectl secret name, and reading it as
// "opskeeper llm-credentials" would invent a subcommand out of a string that was
// never a command.
func TestABinaryNameInsideALongerWordIsNotAnInvocation(t *testing.T) {
	root := writeTree(t, cliTree("```\ntool-run-credentials --target x\nkubectl -n opskeeper get secret tool-llm-credentials\n```\n"))
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.CLIFlags != 0 {
		t.Fatalf("CLIFlags = %d, want 0: neither line invokes the binary", report.CLIFlags)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("missing = %v, want nothing reported", claimsOf(t, report))
	}
}

// A binary with no subcommand dispatcher has no command table to judge a word
// against, so the finding is recorded and skipped rather than invented. This is
// the "opskeeper helm upgrade" shape: helm is somebody else's binary.
func TestAnUnjudgeableWordIsSkippedRatherThanCalledWrong(t *testing.T) {
	tree := cliTree("```\ntool helm upgrade -f x.yaml\n```\n")
	tree["cmd/tool/main.go"] = "package x\n\nfunc main() {}\n"
	root := writeTree(t, tree)
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("missing = %v, want the word skipped, not called wrong", claimsOf(t, report))
	}
	if report.UnknownSubs["tool helm"] != 1 {
		t.Fatalf("UnknownSubs = %v, want it to record tool helm once", report.UnknownSubs)
	}
}

// The same word IS judgeable when the binary does dispatch subcommands: this
// one prints "unknown subcommand" and exits 2, so the document is describing a
// command that cannot run.
func TestASubcommandOutsideAKnownTableIsRed(t *testing.T) {
	root := writeTree(t, cliTree("```\ntool lock --until 2026-08-01\n```\n"))
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing = %v, want the undocumented subcommand reported", claimsOf(t, report))
	}
	if !strings.Contains(report.Missing[0].Claim, "tool lock") {
		t.Fatalf("claim = %q, want it to name the subcommand", report.Missing[0].Claim)
	}
}

// A flag defined with the non-Var form (fs.String) is a real flag. Reading only
// the Var form would report --dir and --threshold as undefined, which is the
// gate disagreeing with the compiler.
func TestANonVarFlagIsStillAFlag(t *testing.T) {
	root := writeTree(t, cliTree("```\ntool board --threshold 0.6\n```\n"))
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("missing = %v, want --threshold accepted", claimsOf(t, report))
	}
}

// A string that merely looks like a flag name to some other function is not a
// flag. errors.New("boom") must not put "boom" in the vocabulary, or a document
// writing --boom would pass this gate.
func TestAStringArgumentToAnotherFunctionIsNotAFlag(t *testing.T) {
	tree := cliTree("```\ntool run --boom\n```\n")
	tree["cmd/tool/main.go"] = `package x

func main() {
	switch os.Args[1] {
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		fs.Parse(nil)
		boom("boom")
	default:
		fmt.Println("unknown subcommand:", os.Args[1])
		os.Exit(2)
	}
}

func boom(msg string) error { return nil }
`
	root := writeTree(t, tree)
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing = %v, want --boom reported as undefined", claimsOf(t, report))
	}
}

// A claim only counts inside a fence. A document that says "there is no such
// flag" in prose is describing an absence, and this gate must not read it as
// asserting the thing exists.
func TestACommandLineInProseIsNotAClaim(t *testing.T) {
	root := writeTree(t, cliTree("there used to be a `tool run --targte x` command; it is gone.\n"))
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.CLIFlags != 0 {
		t.Fatalf("CLIFlags = %d, want 0 for a line outside a fence", report.CLIFlags)
	}
}
