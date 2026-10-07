package main

import (
	"testing"
)

// The first layer of this gate reads the tree as text and asks whether a
// path-shaped string exists in it. That question has a very large set of
// answers that are not routes, and every one of them was found the hard way
// during decision 267: a glob in a plugin loader, a prefix in a group mount,
// a path in this command's own header comment. All of them satisfied claims.
//
// These tests are the second layer's own. Each one writes a tree where the
// path exists as text and is not a route, and requires the claim behind it to
// be reported missing.

func TestAStringConstantIsNotARegistration(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/ghost.md": "```\nGET /api/v1/harness/runs\n```\n",
		"cmd/app/const.go":  "const runsEndpoint = \"/api/v1/harness/runs\"\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing = %v, want a constant not to satisfy a claim", claimsOf(t, report))
	}
	if len(report.Routes) == 0 {
		t.Fatal("the literal scan found nothing at all; this test would pass for the wrong reason")
	}
}

// An outbound client's URL is not an inbound route. The tree has a Grafana
// client, a Tempo client and a RabbitMQ management client, and between them
// they contribute a few dozen paths that look exactly like endpoints.
func TestAnOutboundClientPathIsNotARegistration(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/ghost.md": "```\nGET /api/search\n```\n",
		"cmd/app/client.go": "func search() {\n\tstrings.HasPrefix(u.Path, \"/api/search\")\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing = %v, want an outbound path not to satisfy a claim", claimsOf(t, report))
	}
}

// A glob is the specific phantom that made this gate report 26 of 26 claims
// satisfied while two of the three documents described servers that do not
// exist. It is worth its own test at this layer: it is a string, it is
// path-shaped, and it is passed to a function.
func TestAPlobIsNotARegistration(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/ghost.md": "```\nPOST /api/v1/harness/runs\n```\n",
		"cmd/app/load.go":   "func load() {\n\tfilepath.Glob(root + \"/*/pig-ops.yaml\")\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing = %v, want the glob not to satisfy a claim", claimsOf(t, report))
	}
}

// A test that constructs a router and registers on it is still a test. The
// e2e harness in this repository builds routers to drive them.
func TestARegistrationInsideATestIsNotARegistration(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/ghost.md":    "```\nGET /api/v1/thing\n```\n",
		"cmd/app/main_test.go": "func TestX(t *testing.T) {\n\tr.Get(\"/api/v1/thing\", h)\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing = %v, want a test's own router not to satisfy a claim", claimsOf(t, report))
	}
}

// Sharing a method name with a router is not being one. chi verbs always take
// a handler as well as a path, so the arity is the cheapest honest
// discriminator there is.
func TestARouterNameWithoutAHandlerIsNotARegistration(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/ghost.md": "```\nGET /api/v1/thing\n```\n",
		"cmd/app/one.go":    "func f() {\n\tcache.Get(\"/api/v1/thing\")\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing = %v, want a one-argument call not to count", claimsOf(t, report))
	}
}

// net/http's Go 1.22 ServeMux takes the method in the pattern, so the
// argument does not begin with a slash. It is a real registration and used to
// be invisible to any scan that looks for a leading slash.
func TestAMethodPrefixedPatternIsARegistration(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/thing.md": "```\nGET /v1/thing\n```\n",
		"cmd/app/main.go":   "func mount(mux *http.ServeMux) {\n\tmux.HandleFunc(\"GET /v1/thing\", h)\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("missing = %v, want the ServeMux pattern to count", claimsOf(t, report))
	}
	if _, ok := report.Registered["/v1/thing"]; !ok {
		t.Fatalf("registered = %v, want /v1/thing", report.Registered)
	}
}

// A package-level function that happens to be called Post is not a router
// method. The layer requires a selector, which is what "receiver.Method" means
// in every router this repository uses.
func TestABareFunctionCallIsNotARegistration(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/ghost.md": "```\nPOST /api/v1/thing\n```\n",
		"cmd/app/bare.go":   "func f() {\n\tPost(\"/api/v1/thing\", h)\n}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing = %v, want a bare call not to count", claimsOf(t, report))
	}
}

// A file this command cannot parse must stop the run. The failure mode it
// guards is the expensive one: a swallowed parse error shrinks the registered
// set, and a shrunken set reports delivered endpoints as phantoms — which is
// how a gate that only ever errs in this direction gets ignored.
func TestAParseErrorStopsTheRun(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cmd/app/main.go": "func mount( {\n",
	})
	if _, err := registeredRoutes(root); err == nil {
		t.Fatal("a file that does not parse was skipped silently; the route set would shrink " +
			"and every delivered endpoint would start reading as a phantom")
	}
}

// The whole point of the layer: the claim is matched against registrations, and
// a tree where the path is everywhere except a router must fail.
func TestTheClaimIsMatchedAgainstRegistrationsNotLiterals(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/ghost.md": "```\nGET /api/v1/thing\n```\n",
		"cmd/app/main.go": "func mount(r chi.Router) {\n" +
			"\tr.Get(\"/v1/real\", h)\n" +
			"\t// the router used to serve \"/api/v1/thing\"\n" +
			"\tconst retired = \"/api/v1/thing\"\n" +
			"\t_ = retired\n" +
			"}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 {
		t.Fatalf("missing = %v, want the claim unsatisfied", claimsOf(t, report))
	}
	if _, ok := report.Registered["/v1/real"]; !ok {
		t.Fatalf("registered = %v, want the real route to be found", report.Registered)
	}
	if len(report.Routes) <= len(report.Registered) {
		t.Fatalf("literals %d and registrations %d: the inventory is supposed to be larger, "+
			"otherwise this test is not measuring the gap", len(report.Routes), len(report.Registered))
	}
}

// The inventory and the match set are reported separately on purpose: a
// reader who sees "459 literals, 259 registrations" learns how much of the
// tree a string scan would have called an endpoint.
func TestTheReportCarriesBothNumbers(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/api/thing.md": "```\nGET /v1/thing\n```\n",
		"cmd/app/main.go": "func mount(r chi.Router) {\n" +
			"\tr.Get(\"/v1/thing\", h)\n" +
			"\tconst asset = \"/assets/logo.svg\"\n" +
			"\t_ = asset\n" +
			"}\n",
	})
	report, err := check(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Registered) != 1 {
		t.Fatalf("registered = %v, want exactly the one route", report.Registered)
	}
	if len(report.Routes) != 2 {
		t.Fatalf("literals = %v, want the route and the asset path", report.Routes)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("missing = %v, want the one claim satisfied", claimsOf(t, report))
	}
}
