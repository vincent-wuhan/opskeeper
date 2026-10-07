package audit

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestTheSlotSurvivesEveryContextRewrap pins the reason the slot is a
// pointer inside the context rather than a value on the request.
//
// An earlier implementation installed the slot with context.WithValue and
// then had middleware further down the chain call r.WithContext, which is
// what auth, tenant and otel all do. The value type decided the outcome:
// a *slot is copied by reference, so the handler's write is still there
// when the audit middleware reads it after the handler returns. An Event
// stored by value would have been written into a context the audit
// middleware never sees again.
func TestTheSlotSurvivesEveryContextRewrap(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/iam/users", nil)
	r = r.WithContext(WithSlot(r.Context()))

	// Three re-wraps, which is fewer than the real chain and still enough
	// to have broken the by-value version.
	for i := 0; i < 3; i++ {
		r = r.WithContext(context.WithValue(r.Context(), struct{ n int }{n: i}, i))
	}

	uid := uint64(42)
	SetAuditEvent(r, Event{
		UserID:       &uid,
		Action:       ActionUserCreate,
		ResourceType: ResourceUser,
		ResourceID:   "u-42",
	})

	got, ok := GetAuditEvent(r.Context())
	if !ok {
		t.Fatal("the event set deep in the chain was lost; the slot did not survive the rewrap")
	}
	if got.Action != ActionUserCreate || got.ResourceType != ResourceUser || got.ResourceID != "u-42" {
		t.Fatalf("the event came back as %+v, not what the handler set", got)
	}
	if got.UserID == nil || *got.UserID != uid {
		t.Fatalf("the actor pointer did not survive: %+v", got.UserID)
	}
}

// TestOutsideAMiddlewareChainNothingIsRemembered covers the two calls a
// handler can make when the audit middleware is not in the chain: they
// must be inert, not a panic and not a row that appears later.
func TestOutsideAMiddlewareChainNothingIsRemembered(t *testing.T) {
	// A nil request is what a caller with no request at all passes. It has
	// to be a no-op rather than a nil dereference.
	SetAuditEvent(nil, Event{Action: ActionUserCreate})

	// A real request that never went through WithSlot.
	bare := httptest.NewRequest(http.MethodGet, "/api/v1/iam/users", nil)
	SetAuditEvent(bare, Event{Action: ActionUserCreate, Status: StatusSuccess})

	if _, ok := GetAuditEvent(bare.Context()); ok {
		t.Error("a request without the slot reported an event; only the middleware may install it")
	}
	if _, ok := GetAuditEvent(context.Background()); ok {
		t.Error("a bare context reported an event")
	}

	// An installed but untouched slot is also "not set": the distinction is
	// what lets the middleware skip Emit for unannotated requests.
	untouched := httptest.NewRequest(http.MethodGet, "/api/v1/iam/users", nil).WithContext(WithSlot(context.Background()))
	if _, ok := GetAuditEvent(untouched.Context()); ok {
		t.Error("an installed but unwritten slot reported an event")
	}
}

// TestThePortCannotReachTheLedger is the port's central claim made
// executable: a caller holding this package can ask to be remembered and
// cannot write a row.
//
// A claim like that is worth nothing if the package quietly imports the
// usecase, so the imports are read out of the source rather than assumed.
// This is the invariant that made iam's handlers safe to point here, and it
// is the one a future "just add the DB call here" change would break.
func TestThePortCannotReachTheLedger(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "port.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse port.go: %v", err)
	}

	// Every one of these is a package that could persist, hash, enrich or
	// otherwise decide what a row means. The port is the shape of a row.
	forbidden := []string{
		"core/manager/biz",
		"core/manager/data",
		"core/manager/model",
		"core/manager/server",
		"core/manager/service",
		"core/manager/iam",
		"core/edge",
		"core/floor",
	}
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		for _, bad := range forbidden {
			if strings.Contains(path, bad) {
				t.Errorf("port.go imports %s: a port that can reach the ledger can forge a row", path)
			}
		}
	}
}

// TestTheVocabularyIsClosedAndWellFormed reads the constant list out of
// this file and holds it to the conventions the rest of the platform
// relies on: an action string ends up in a database column, a filter
// dropdown and an operator's grep, so a typo here is a permanent silent
// split rather than a compile error.
func TestTheVocabularyIsWellFormed(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "port.go", nil, 0)
	if err != nil {
		t.Fatalf("parse port.go: %v", err)
	}

	nameOK := regexp.MustCompile(`^(Action|Resource|Status)[A-Z][A-Za-z0-9]*$`)
	valueOK := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

	values := map[string]string{} // "Action" | "Resource" | "Status" -> value -> name
	count := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Values) != 1 || len(vs.Names) != 1 {
				t.Errorf("const %s is not a single name bound to a single literal; the audit vocabulary has to be greppable", vs.Names)
				continue
			}
			name := vs.Names[0].Name
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("const %s is not a string literal", name)
				continue
			}
			value := strings.Trim(lit.Value, `"`)
			count++

			if !nameOK.MatchString(name) {
				t.Errorf("const %s does not follow the Action/Resource/Status + PascalCase convention", name)
			}
			if !valueOK.MatchString(value) {
				t.Errorf("const %s = %q is not lower_snake_case; it lands in a column and a filter", name, value)
			}
			var bucket string
			switch {
			case strings.HasPrefix(name, "Action"):
				bucket = "Action"
			case strings.HasPrefix(name, "Resource"):
				bucket = "Resource"
			case strings.HasPrefix(name, "Status"):
				bucket = "Status"
			}
			if prev, dup := values[bucket+"|"+value]; dup {
				t.Errorf("%s and %s are both %q; two names for one value means two filters for one action", prev, name, value)
			}
			values[bucket+"|"+value] = name
		}
	}
	if count == 0 {
		t.Fatal("no constants were scraped; the AST walk is broken, not the vocabulary")
	}
	// The three status values are the only ones a bucket is allowed to be
	// complete about, because statusBucket in the middleware can only ever
	// return one of them.
	for _, want := range []string{"success", "failure", "denied"} {
		if _, ok := values["Status|"+want]; !ok {
			t.Errorf("status %q is missing; statusBucket cannot return it", want)
		}
	}
}
