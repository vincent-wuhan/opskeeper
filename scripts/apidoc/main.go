// Command apidoc checks that every endpoint and every subcommand the API docs
// claim actually exists in the source tree.
//
// Why this exists. docs/api/harness.md described thirteen HTTP endpoints and
// docs/api/middleware.md described nine, and not one of them had a route
// registration anywhere in this repository: the harness surface is a CLI
// (cmd/opskeeper-eval) and the middleware surface is a tool registry
// (core/manager/middleware). Both documents read like delivered contracts —
// request bodies, response shapes, error codes — and a reader who trusted one
// would have written a client against a server that does not exist.
//
// Nothing caught it, because a document is not compiled and a route is not
// required to be documented. This command makes the two disagree loudly.
//
// What it checks, and what it deliberately does not:
//
//   - Every "GET /path" / "POST /path" line inside a ``` fence of
//     docs/api/*.md must be served by a route the tree actually registers,
//     comparing segment by segment with {param} as a wildcard.
//   - Every `opskeeper-eval <subcommand>` invocation must match a `case
//     "<subcommand>"` in cmd/opskeeper-eval/main.go.
//
// "Actually registers" is the second layer, and it is the whole point of this
// revision. The first version matched a claim against any path-shaped string
// in the tree, and that set contains a great deal that is not a route: a glob
// in a plugin loader, a constant, a path in a comment, an outbound client's
// URL. On this repository it holds 459 such strings and 259 registrations, and
// the difference is 200 ways to write a document about a server that does not
// exist and have this command agree with you. A check a phantom can satisfy
// is not a weaker check — it is a check that teaches the reader to add
// phantoms. See registration.go.
//
// It does not check response shapes. A response body is a claim about
// structured data, and the honest way to pin one is a test on the handler plus
// a doc example — which is what knowledge/gitartifact/doc_contract_test.go
// does for the one API whose responses are pinned. This command answers the
// coarser question that gate cannot: is this endpoint real at all.
//
// And "registered" is still not "reachable": a route can be registered on a
// router that no server ever serves. That is a further question, it needs the
// wiring rather than the syntax, and it is not answered here.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func main() {
	root := "."
	verbose := false
	for _, arg := range os.Args[1:] {
		if arg == "-v" {
			verbose = true
			continue
		}
		root = arg
	}
	report, err := check(root, verbose)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apidoc: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("docs checked: %d   endpoints claimed: %d   subcommands claimed: %d\n",
		report.Docs, report.Endpoints, report.Subcommands)
	fmt.Printf("route literals in source: %d   of those registered on a router: %d   "+
		"eval subcommands found: %d\n", len(report.Routes), len(report.Registered), len(report.Subcommands2))
	fmt.Printf("command-line flags checked: %d   of those not defined by the binary: %d\n",
		report.CLIFlags, countFlagFindings(report))
	if len(report.UnknownSubs) > 0 {
		names := make([]string, 0, len(report.UnknownSubs))
		for name := range report.UnknownSubs {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Printf("subcommands this command did not recognise (skipped, not failures): %d\n", len(names))
		for _, name := range names {
			fmt.Printf("  %s  x%d\n", name, report.UnknownSubs[name])
		}
	}
	if len(report.Missing) > 0 {
		fmt.Fprintln(os.Stderr, "\nthese claims have nothing behind them:")
		for _, m := range report.Missing {
			fmt.Fprintf(os.Stderr, "  %s:%d  %s\n", m.Doc, m.Line, m.Claim)
		}
		os.Exit(1)
	}
	fmt.Println("\nevery endpoint and subcommand the docs claim exists in the source.")
}

// Missing is one documented claim with no counterpart in the tree.
type Missing struct {
	Doc   string
	Line  int
	Claim string
}

// Report is what one run found.
//
// Routes and Registered are both here and they are not the same set. Routes
// is every path-shaped string literal in the tree; Registered is the subset
// handed to something that registers routes. Only Registered can satisfy a
// documented claim — the other number is printed because a reader who sees
// "459 literals, 268 registrations" learns how much of the tree a string
// scan would have called an endpoint.
type Report struct {
	Docs         int
	Endpoints    int
	Subcommands  int
	Routes       map[string]bool
	Registered   map[string]bool
	Subcommands2 map[string]bool
	CLIFlags     int
	UnknownSubs  map[string]int
	Missing      []Missing
}

var (
	// An endpoint line, in either of the two ways a document writes one:
	// the bare "GET /api/v1/..." form, and the curl form
	// "curl -X POST https://host/api/v1/...". The second is not optional: the
	// only REST API harness-guide documents is written entirely as curl
	// invocations, and a pattern that required the line to begin with the verb
	// would have read that section as containing no claims at all.
	endpointRE = regexp.MustCompile(`(?m)(?:^|\s)(?:GET|POST|PUT|DELETE|PATCH)\s+(?:https?://[^\s/]+)?(/[A-Za-z0-9_\-/{}.:]*)`)
	// A CLI invocation: "opskeeper-eval judge --flags".
	cliRE = regexp.MustCompile(`opskeeper-eval\s+([a-z][a-z0-9-]*)`)
	// A route literal: any string starting with "/" that looks like a path.
	routeRE = regexp.MustCompile(`"(/[A-Za-z0-9_\-/{}.:*]*)"`)
	// A subcommand dispatch: case "judge":
	caseRE = regexp.MustCompile(`(?m)^\s*case\s+"([a-z][a-z0-9-]*)"\s*:`)
)

func check(root string, verbose bool) (Report, error) {
	report := Report{
		Routes:       map[string]bool{},
		Registered:   map[string]bool{},
		Subcommands2: map[string]bool{},
		UnknownSubs:  map[string]int{},
	}
	// The source side first: every route literal and every subcommand.
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// These trees are large, vendored, or generated; a route literal
			// in them is not this repository's contract. The list is
			// skipDir's, shared with the registration pass — two lists would
			// be two chances to scan a tree the other one skips.
			if skipDir(filepath.Base(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := stripComments(string(body))
		if strings.HasSuffix(path, "_test.go") {
			// A route asserted in a test is a claim about a handler, not a
			// registration; counting it would let a test keep a phantom alive.
			text = stripTestFuncs(text)
		}
		for _, m := range routeRE.FindAllStringSubmatch(text, -1) {
			report.Routes[m[1]] = true
		}
		if strings.Contains(path, "cmd/opskeeper-eval/main.go") {
			for _, m := range caseRE.FindAllStringSubmatch(text, -1) {
				report.Subcommands2[m[1]] = true
			}
		}
		return nil
	})
	if err != nil {
		return report, err
	}

	// The second layer, and the one a claim is actually matched against.
	registered, err := registeredRoutes(root)
	if err != nil {
		return report, err
	}
	report.Registered = registered

	docs, err := filepath.Glob(filepath.Join(root, "docs", "api", "*.md"))
	if err != nil {
		return report, err
	}
	sort.Strings(docs)
	for _, doc := range docs {
		body, err := os.ReadFile(doc)
		if err != nil {
			return report, err
		}
		report.Docs++
		rel, _ := filepath.Rel(root, doc)
		for _, block := range fencedBlocks(string(body), string(body)) {
			for _, match := range endpointRE.FindAllStringSubmatchIndex(block.body, -1) {
				report.Endpoints++
				path := block.body[match[2]:match[3]]
				if verbose {
					fmt.Printf("claim %-44s served by %q\n", path, matchingRoute(report.Registered, path))
				}
				if !anyRouteMatches(report.Registered, path) {
					report.Missing = append(report.Missing, Missing{
						Doc:   rel,
						Line:  lineOf(string(body), block.offset+match[0]),
						Claim: "endpoint " + path,
					})
				}
			}
			for _, match := range cliRE.FindAllStringSubmatchIndex(block.body, -1) {
				report.Subcommands++
				sub := block.body[match[2]:match[3]]
				if !report.Subcommands2[sub] {
					report.Missing = append(report.Missing, Missing{
						Doc:   rel,
						Line:  lineOf(string(body), block.offset+match[0]),
						Claim: "subcommand opskeeper-eval " + sub,
					})
				}
			}
		}
	}

	// The command line, over a wider set of documents than the endpoint pass
	// above. Endpoints are declared in docs/api/ and nowhere else; command
	// lines are written all over docs/ (integration-guide, harness-guide,
	// operations-manual), which is why they needed their own sweep.
	allDocs, err := allDocFiles(root)
	if err != nil {
		return report, err
	}
	missingFlags, checkedFlags, unknownSubs, err := checkCLIFlags(root, allDocs, os.ReadFile)
	if err != nil {
		return report, err
	}
	// The endpoint sweep, over the same wider set. Endpoint claims are written
	// in docs/api/ and also all over docs/ — harness-guide 7.2 posts to
	// /api/v1/harness/runs, a path no route in this tree serves, and calls it
	// "REST API（CI 集成）" with a response shape. Same class of claim, same
	// cost when it is wrong, so it gets the same question.
	apiDocs := map[string]bool{}
	for _, d := range docs {
		apiDocs[d] = true
	}
	for _, doc := range allDocs {
		if apiDocs[doc] {
			continue
		}
		body, rerr := os.ReadFile(doc)
		if rerr != nil {
			return report, rerr
		}
		report.Docs++
		rel, _ := filepath.Rel(root, doc)
		for _, block := range fencedBlocks(string(body), string(body)) {
			for _, match := range endpointRE.FindAllStringSubmatchIndex(block.body, -1) {
				report.Endpoints++
				path := block.body[match[2]:match[3]]
				if !anyRouteMatches(report.Registered, path) {
					report.Missing = append(report.Missing, Missing{
						Doc:   rel,
						Line:  lineOf(string(body), block.offset+match[0]),
						Claim: "endpoint " + path,
					})
				}
			}
		}
	}

	report.CLIFlags = checkedFlags
	report.Missing = append(report.Missing, dedupe(missingFlags)...)
	for name, count := range unknownSubs {
		report.UnknownSubs[name] = count
	}
	return report, nil
}

// allDocFiles is every markdown file under docs/.
func allDocFiles(root string) ([]string, error) {
	var out []string
	err := filepath.Walk(filepath.Join(root, "docs"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// docs/superpowers/{plans,specs} is a tree of proposals, not of
			// delivered contracts: a plan document says what someone intends to
			// build, and a spec says what they decided. Judging them by the same
			// question as an operations manual would report every unfinished
			// plan as a defect, and the fix would be to delete the plans.
			if skipDir(filepath.Base(path)) || proposalDir(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".md") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// anyRouteMatches reports whether one registered route serves a documented
// path.
//
// The comparison allows a documented path to carry up to two leading segments
// that the router mounts rather than the handler: this repository nests its
// routes under an /api group and, inside it, a /v1 group, so a handler
// registered as "/v1/git-artifacts" is what serves the documented
// "/api/v1/git-artifacts". That allowance is deliberately capped at two
// segments. The first version of this rule had no cap at all and accepted a
// bare "/api" as a match for every documented path in the repository — which
// made the gate report 26 of 26 claims satisfied while two of the three
// documents described servers that do not exist.
// matchingRoute returns the route that serves a documented path, for -v.
func matchingRoute(routes map[string]bool, documented string) string {
	docSegments := splitPath(documented)
	if len(docSegments) == 0 {
		return ""
	}
	for skip := 0; skip <= 2 && skip < len(docSegments)-1; skip++ {
		for route := range routes {
			if segmentsServe(splitPath(route), docSegments[skip:]) {
				return route
			}
		}
	}
	return ""
}

func anyRouteMatches(routes map[string]bool, documented string) bool {
	docSegments := splitPath(documented)
	if len(docSegments) == 0 {
		return false
	}
	// The remaining part must still be a path. Without this floor, skipping two
	// segments off "/api/v1/middleware" left a single segment, and any
	// one-segment route in the tree — there are several "/{incident_id}" — was
	// read as serving it.
	for skip := 0; skip <= 2 && skip < len(docSegments)-1; skip++ {
		for route := range routes {
			if segmentsServe(splitPath(route), docSegments[skip:]) {
				return true
			}
		}
	}
	return false
}

func splitPath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// segmentsServe reports whether a registered route serves a documented path.
// The two must be the same length, with {param} matching any single segment
// and a trailing "*" (chi's subtree mount) matching the rest.
func segmentsServe(registered, documented []string) bool {
	if len(registered) == 0 || len(documented) == 0 {
		return false
	}
	// A registered route longer than the documented path is not serving it.
	if len(registered) > len(documented) {
		return false
	}
	for i, segment := range registered {
		if segment == "*" {
			// A subtree mount serves everything under it, including nothing.
			//
			// Only in the LAST position. A "*" earlier in the path is a glob
			// pattern (the plugin loader carries "/*/pig-ops.yaml"), and
			// treating that as a mount made this gate report every documented
			// endpoint in the repository as served — including the twenty-three
			// that no handler serves at all.
			if i == len(registered)-1 {
				return i <= len(documented)
			}
			return false
		}
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			continue
		}
		// A documented {param} is a promise about the shape of the path, not a
		// wildcard the registered route may satisfy with any literal: "/x/{id}"
		// is not served by a route registered as "/x/cases".
		if strings.HasPrefix(documented[i], "{") && strings.HasSuffix(documented[i], "}") {
			return false
		}
		if segment != documented[i] {
			return false
		}
	}
	return len(registered) == len(documented)
}

var fenceRE = regexp.MustCompile("(?s)```[a-zA-Z0-9]*\n(.*?)```")

// fencedBlocks returns the contents of every fenced block. Endpoint and
// command claims only count inside a fence: prose that mentions a path while
// explaining that it does not exist is exactly what the 未交付 sections do, and
// this command must not read that as a claim.
// fencedBlock is one fenced region together with where it starts, so a
// finding can name a line a reader can jump to.
type fencedBlock struct {
	body   string
	offset int
}

func fencedBlocks(text, _ string) []fencedBlock {
	var out []fencedBlock
	for _, match := range fenceRE.FindAllStringSubmatchIndex(text, -1) {
		out = append(out, fencedBlock{
			body: text[match[2]:match[3]],
			// The offset of the CONTENT, not of the fence: a finding's line is
			// where the claim is written, and the opening ``` is a line above it.
			offset: match[2],
		})
	}
	return out
}

var (
	lineCommentRE  = regexp.MustCompile(`(?m)//.*$`)
	blockCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// stripComments removes comments before route literals are read out of a file.
//
// Without this the gate has a hole shaped exactly like the thing it exists to
// catch: a path written inside a comment is not a registration, and this
// command's own header comment contains three of them. A phantom route is most
// easily conjured by talking about it.
func stripComments(text string) string {
	return lineCommentRE.ReplaceAllString(blockCommentRE.ReplaceAllString(text, " "), " ")
}

func stripTestFuncs(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inTest := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "func Test") ||
			strings.HasPrefix(strings.TrimSpace(line), "func Benchmark") ||
			strings.HasPrefix(strings.TrimSpace(line), "func Fuzz") {
			inTest = true
		}
		if inTest {
			if strings.HasPrefix(line, "}") {
				inTest = false
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func lineOf(text string, index int) int {
	return strings.Count(text[:index], "\n") + 1
}

// countFlagFindings is how many of the missing claims are flags rather than
// endpoints or subcommands, for the one number the summary prints.
func countFlagFindings(report Report) int {
	n := 0
	for _, m := range report.Missing {
		if strings.Contains(m.Claim, "that flag is not defined") {
			n++
		}
	}
	return n
}

// dedupe removes findings that name the same flag on the same line.
//
// Closing a shell line continuation can put one flag on the joined line twice if
// the document wrote it twice, and a reader told the same thing is not told it
// twice. Distinct flags on one line are all kept: they are distinct defects.
func dedupe(in []Missing) []Missing {
	seen := map[string]bool{}
	out := make([]Missing, 0, len(in))
	for _, m := range in {
		key := m.Doc + ":" + itoa(m.Line) + ":" + m.Claim
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, m)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// proposalDir is true for the two directories under docs/superpowers that hold
// proposals rather than documentation of what exists.
func proposalDir(path string) bool {
	for _, part := range []string{"docs/superpowers/plans", "docs/superpowers/specs"} {
		if strings.Contains(filepath.ToSlash(path), part) {
			return true
		}
	}
	return false
}
