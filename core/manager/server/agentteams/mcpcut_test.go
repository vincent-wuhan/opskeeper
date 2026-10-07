package agentteams

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	mcpauth "github.com/vincent-wuhan/opskeeper/core/manager/server/mcp/middleware"
)

// The `agentteams -> mcp` edge was two types and six methods, and the live
// code used three symbols: mcpauth.FromContext, mcpauth.TraceFromContext and
// TraceContext.HasTrace.
//
// They were package functions. That is the part worth remembering, because a
// package function cannot be injected — so the dependency was structural
// rather than declared, and there was no port to narrow. What arrived whole
// was a six-field credential struct (APIKeyID, AllowedTools and ResolvedAt
// included) and a trace struct plus a method, for routes that read three
// fields and two.
//
// It was cut by declaring MCPCallerLookup here, projecting to domain.MCPCaller
// and domain.MCPTrace, and letting middleware.ContextIdentity satisfy it
// structurally. The second half of the cut is a fold: `ok && tc.HasTrace()` was
// two signals for one fact, and TraceFrom's bool is now the single one.
//
// This file is the half that can be lost silently. The port compiles, the
// handler compiles, every behavioural test passes, and the edge is back the
// moment one import returns — with no behavioural difference anywhere, because
// the narrowed question is a strict subset of the old one.

// mcpPackages are the packages this one must not import from its production
// files. Named explicitly rather than matched by a prefix, for the same reason
// the alert guard names its four: a prefix rule wide enough to be convenient is
// a rule that will eventually refuse a legitimate import and get deleted.
//
// The two `data/mcp` and `service/mcp` entries are listed even though this
// package imports neither today, so that adding one later is caught by this
// test rather than by whoever notices the price column go up.
var mcpPackages = []string{
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/mcp",
	"github.com/vincent-wuhan/opskeeper/core/manager/model/mcp",
	"github.com/vincent-wuhan/opskeeper/core/manager/service/mcp",
	"github.com/vincent-wuhan/opskeeper/core/manager/data/mcp/store",
	"github.com/vincent-wuhan/opskeeper/core/manager/server/mcp",
	"github.com/vincent-wuhan/opskeeper/core/manager/server/mcp/middleware",
}

func TestNoProductionFileInThisPackageImportsTheMCPDomain(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	sawProduction := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// Test files are exempt, and deliberately so. This package's tests
		// wire middleware.ContextIdentity — the producer's real adapter —
		// so that they feed the consumer the producer's actual output. That
		// is the seam having a witness, and `.go-arch-lint.yml` already
		// excludes test-only cross-domain imports repo-wide. What is not
		// allowed is for that convenience to reach a production file, which
		// is exactly what this loop looks at.
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		sawProduction = true
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			for _, forbidden := range mcpPackages {
				if path == forbidden {
					t.Errorf("%s imports %s. Who is calling, and whether there is a trace to "+
						"correlate with, are two questions this package asks through "+
						"MCPCallerLookup; an import of the mcp domain here is the three "+
						"package functions coming back, and it comes back silently because "+
						"the projection is a strict subset of what it replaced",
						name, path)
				}
			}
		}
	}
	if !sawProduction {
		t.Fatal("no production Go files were examined, so this test is measuring nothing. A " +
			"move that empties this directory of production code is not a way to make it green")
	}
}

// TestTheCallerPortAsksTwoQuestionsAndNoMore pins the port's shape, because a
// port that grows is an edge that widens without a decision.
//
// **Only the method count is enforceable, and the return-type half below is
// documentation.** That was measured, not assumed: a mutation that swapped
// TraceFrom's projection for a different one does not compile, because every
// call site reads TraceID and SpanID off the returned value — so the type
// system already refuses it, and this assertion can only go red in a world the
// compiler has rejected. (This is the second time in two cuts that a
// structural claim turned out to be the compiler's job rather than a test's;
// decision 248 found the same about the store-not-satisfying-the-port
// assertion. Two occurrences is a pattern, and the pattern is worth stating:
// **when a consumer declares the projection type, the port's return type stops
// being negotiable — and stops being testable.**)
//
// It is kept because it is the shortest statement of what the port promises,
// and because the one way to *reach* the failure it describes — declaring a
// second copy of the projection inside this package — requires the adapter to
// import the consumer, which the import guard above catches. Between the two
// assertions the claim is covered; neither covers it alone.
func TestTheCallerPortAsksTwoQuestionsAndNoMore(t *testing.T) {
	port := reflect.TypeOf((*MCPCallerLookup)(nil)).Elem()
	if port.NumMethod() != 2 {
		t.Fatalf("MCPCallerLookup has %d methods (%v); the cut left it exactly two questions, "+
			"and a third is how the six-field credential struct comes back",
			port.NumMethod(), methodNamesOf(port))
	}
	want := map[string]reflect.Type{
		"CallerFrom": reflect.TypeOf((*domain.MCPCaller)(nil)).Elem(),
		"TraceFrom":  reflect.TypeOf((*domain.MCPTrace)(nil)).Elem(),
	}
	for i := 0; i < port.NumMethod(); i++ {
		method := port.Method(i)
		projection, ok := want[method.Name]
		if !ok {
			t.Errorf("MCPCallerLookup has %s, which is not one of the two questions", method.Name)
			continue
		}
		// Out(0) is the projection; the bool after it is the answer. See the
		// note above: this half is unreachable as a failure, and is here to
		// say what the port promises rather than to catch anything.
		if got := method.Type.Out(0); got != projection {
			t.Errorf("%s returns %s, want %s. A projection declared here rather than in "+
				"core/domain is a second declaration of the same two structs, and two "+
				"declarations drift", method.Name, got, projection)
		}
		if got := method.Type.NumOut(); got != 2 {
			t.Errorf("%s returns %d values, want 2 (the projection and whether there is one)",
				method.Name, got)
		}
	}
	if port.NumMethod() == 2 {
		if _, ok := want["CallerFrom"]; !ok {
			t.Fatal("unreachable")
		}
	}
	for name := range want {
		if _, ok := port.MethodByName(name); !ok {
			t.Errorf("the port has no %s, so the routes cannot ask it and the count above is "+
				"measuring an interface nobody calls", name)
		}
	}
}

func methodNamesOf(t reflect.Type) []string {
	out := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		out = append(out, t.Method(i).Name)
	}
	return out
}

// TestTheProjectionsStayThreeAndTwoColumns pins the width of what crosses.
//
// Width is a property of the type, so it is measured on the type: an
// equal-value comparison cannot see a third field added to either struct,
// because the new field is zero on both sides. That is the same trap decision
// 247's projection test walked into, and the reason its comment had to be
// rewritten.
//
// Six fields became three, and three became two. The fields that did not come
// are the ones belonging to the authentication decision that already happened:
// APIKeyID, AllowedTools, ResolvedAt, and the unparsed Raw traceparent.
func TestTheProjectionsStayThreeAndTwoColumns(t *testing.T) {
	caller := reflect.TypeOf(domain.MCPCaller{})
	if got := fieldNames(caller); !equalStrings(got, []string{"Consumer", "Role", "TenantID"}) {
		t.Errorf("domain.MCPCaller has fields %v, want exactly [Consumer Role TenantID]. A "+
			"projection that grows is a boundary that widens without a decision, and the "+
			"fields that come back first are always the credential ones",
			got)
	}
	trace := reflect.TypeOf(domain.MCPTrace{})
	if got := fieldNames(trace); !equalStrings(got, []string{"TraceID", "SpanID"}) {
		t.Errorf("domain.MCPTrace has fields %v, want exactly [TraceID SpanID]", got)
	}
}

func fieldNames(t reflect.Type) []string {
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		out = append(out, t.Field(i).Name)
	}
	return out
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

// TestTheTraceBoolAnswersOneQuestionAndNotTwo is the fold's test, and it is
// the assertion the old two-signal shape could not have had.
//
// A TraceContext is present in the context as soon as the middleware has run,
// and its TraceID may still be empty. The old call sites asked
// `ok && tc.HasTrace()`. If TraceFrom's bool meant only "a TraceContext was
// there", every one of those call sites would have to ask again, and the
// second question would be the one that decides. So the empty-but-present
// context is the case worth pinning, and it is the case that cannot be
// constructed by accident: it needs a TraceContext written with nothing in it.
func TestTheTraceBoolAnswersOneQuestionAndNotTwo(t *testing.T) {
	h := NewHandler(nil, nil, "", mcpauth.ContextIdentity{})

	// Present and empty: the middleware ran, and there is no trace to
	// correlate with. This must read as absent.
	empty := mcpauth.WithTraceContext(context.Background(), mcpauth.TraceContext{})
	if _, ok := h.trace(empty); ok {
		t.Error("a TraceContext with no TraceID in it read as a trace. The bool is supposed " +
			"to answer \"is there a trace\", not \"did something put a struct here\" — and " +
			"the two were separate questions until this cut")
	}

	// Present and real: a trace, with and without a span.
	full := mcpauth.WithTraceContext(context.Background(), mcpauth.TraceContext{
		TraceID: "0123456789abcdef0123456789abcdef",
		SpanID:  "0123456789abcdef",
	})
	got, ok := h.trace(full)
	if !ok {
		t.Fatal("a context with a real TraceID read as no trace")
	}
	if got.TraceID != "0123456789abcdef0123456789abcdef" || got.SpanID != "0123456789abcdef" {
		t.Errorf("got %+v, want the trace and span the context carried", got)
	}

	// The short-form protocol may not carry a span, and the route decides
	// whether an empty one is worth writing. So an absent span is a trace.
	noSpan := mcpauth.WithTraceContext(context.Background(), mcpauth.TraceContext{
		TraceID: "0123456789abcdef0123456789abcdef",
	})
	got, ok = h.trace(noSpan)
	if !ok {
		t.Error("a trace with no span read as no trace. A span is optional in the short-form " +
			"protocol and the caller decides whether to write it")
	}
	if got.SpanID != "" {
		t.Errorf("SpanID is %q, want empty", got.SpanID)
	}

	// Nothing at all: also absent, and the two ways of being absent are now
	// the same answer.
	if _, ok := h.trace(context.Background()); ok {
		t.Error("a context this package never touched read as a trace")
	}
}

// TestAHandlerWithNoCallerWiredAnswersUnauthorized pins the nil case, which
// is the one a port introduces and the one a fake would hide.
//
// Before the cut there was no wiring to forget: the routes called a package
// function. Now there is, and a boot path that forgets SetCallerLookup would
// otherwise have every AgentTeams route answer 401 with a message about a
// missing identity — a confusing way to say "the manager is misconfigured".
// Failing closed is right; failing quietly is not.
func TestAHandlerWithNoCallerWiredAnswersUnauthorized(t *testing.T) {
	h := NewHandler(newMemBackend(), nil, "", nil) // deliberate: see above
	if _, ok := h.caller(context.Background()); ok {
		t.Error("a handler with no caller lookup resolved an identity. The zero value must " +
			"fail closed")
	}
	if _, ok := h.trace(context.Background()); ok {
		t.Error("a handler with no caller lookup produced a trace")
	}

	// A nil *PluginHandler is deliberately NOT asserted here. This file
	// asserted it on the first run and the assertion was wrong: reading
	// h.callers dereferences the receiver, so a nil handler panics rather
	// than failing closed. It is not reachable — NewPluginHandler always
	// returns a value and main.go stores it without a nil check — so the
	// honest thing is to leave it out rather than to add a receiver check
	// for a state nothing can produce.
	//
	// What is reachable is the handler whose port was never set, and that is
	// the case above: both handler types are covered by construction in
	// production, and the PluginHandler's own accessor carries the same
	// nil-port guard.
}

// TestThePortStillHasCallers is the other direction. Deleting the port, the
// field and the two accessors would leave the import guard and the projection
// widths perfectly green — the edge really would be gone — and would also
// leave every route unable to tell who is asking. The two accessors are
// therefore pinned by being the only way the port is read.
func TestThePortStillHasCallers(t *testing.T) {
	fset := token.NewFileSet()
	found := 0
	for _, name := range []string{"http.go", "incident_http.go", "plugin_http.go"} {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "caller", "trace":
				found++
			}
			return true
		})
	}
	if found == 0 {
		t.Fatal("no production file calls h.caller or h.trace, so the port is declared and " +
			"never read — which would leave the import guard green on a package that " +
			"authenticates nobody")
	}
}
