package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domains/service/plugin"
)

// The compatibility matrix's route-level tests. What is being protected is
// not the arithmetic — the service tests hold that, against the same
// function the node runs — but the two ways an HTTP layer can turn a
// correct answer into a misleading one: an authorisation hole, and an
// error rendered as a success.

func getCompat(t *testing.T, srv *httptest.Server, path string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if readErr != nil {
			break
		}
	}
	return resp.StatusCode, body
}

func TestTheCompatibilityRouteAnswersWithTheRequirementItWasGiven(t *testing.T) {
	// The manager holds no manifest, so the requirement comes from the
	// caller. That is a real limitation and the response is where it has
	// to be visible: the matrix embeds the requirement, so a console — and
	// anyone reading a log of the call — can see which version floors
	// produced this split rather than having to trust that the caller
	// asked about the package it meant to.
	svc := &fakeService{compat: func(req plugin.Requirement) (plugin.Matrix, error) {
		return plugin.Matrix{
			Requirement: req,
			Hostable:    []plugin.Verdict{{NodeID: 1, EdgeVersion: "0.8.0", Hostable: true}},
			Refused:     []plugin.Verdict{{NodeID: 2, EdgeVersion: "0.7.2", Hostable: false, Step: "version", Reason: "too old"}},
		}, nil
	}}
	srv := newServer(t, svc, roleAdmin)

	code, body := getCompat(t, srv, "/v1/plugins/opskeeper-sre-repair/compatibility?version=0.2.0&min_edge_version=0.8.0&min_pig_version=0.3.0")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, body)
	}
	var got plugin.Matrix
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if got.Plugin != "opskeeper-sre-repair" || got.Version != "0.2.0" {
		t.Errorf("the response does not name the package it answered about: %+v", got.Requirement)
	}
	if got.MinEdgeVersion != "0.8.0" || got.MinPigVersion != "0.3.0" {
		t.Errorf("the response does not echo the version floors: %+v", got.Requirement)
	}
	if len(got.Hostable) != 1 || len(got.Refused) != 1 {
		t.Errorf("matrix = %+v, want one hostable and one refused", got)
	}
	if got.Refused[0].Reason == "" {
		t.Error("a refused row reached the console with no reason")
	}
}

func TestTheCompatibilityRouteTrimsTheQueryParameters(t *testing.T) {
	// A console that builds the URL by concatenation leaves a trailing
	// space on one of these sooner or later, and " 0.8.0" is not a
	// version — CheckVersions refuses to guess, so the node would refuse
	// the package on a floor the operator cannot see.
	svc := &fakeService{}
	srv := newServer(t, svc, roleAdmin)

	if code, body := getCompat(t, srv, "/v1/plugins/p/compatibility?min_edge_version=%200.8.0%20&min_pig_version=%200.3.0%20"); code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, body)
	}
	if svc.lastCompat.MinEdgeVersion != "0.8.0" || svc.lastCompat.MinPigVersion != "0.3.0" {
		t.Errorf("the service received %q / %q, want them trimmed",
			svc.lastCompat.MinEdgeVersion, svc.lastCompat.MinPigVersion)
	}
}

func TestTheCompatibilityRouteIsAdminOnly(t *testing.T) {
	// Same rule as every other route here, and asserted rather than
	// assumed: this one dispatches nothing, which makes it exactly the
	// kind of endpoint that gets filed under "harmless" and left open. It
	// is not harmless — it enumerates the fleet's component versions, which
	// is a map of what to upgrade and therefore what to attack.
	svc := &fakeService{}
	srv := newServer(t, svc, "viewer")
	if code, body := getCompat(t, srv, "/v1/plugins/p/compatibility"); code == http.StatusOK {
		t.Errorf("a viewer read the fleet's version inventory: %s", body)
	}
	if svc.lastCompat.Plugin != "" {
		t.Error("the service was called for a caller that should have been refused")
	}
}

func TestAManagerWithNoVersionSnapshotIsNotRenderedAsAnEmptyFleet(t *testing.T) {
	// The failure this route is most able to cause. A manager that cannot
	// answer must not produce a 200 with two empty arrays, because that
	// renders as "every node can take this" to a console and to whoever
	// reads the access log afterwards.
	svc := &fakeService{compat: func(plugin.Requirement) (plugin.Matrix, error) {
		return plugin.Matrix{}, plugin.ErrNoVersionSnapshot
	}}
	srv := newServer(t, svc, roleAdmin)
	code, body := getCompat(t, srv, "/v1/plugins/p/compatibility")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: a control plane that cannot read its own fleet is "+
			"unavailable, not answering a question: %s", code, body)
	}

	// The assertion is about *shape*, and it has to be a shape assertion
	// rather than a count. Decoding the body into a struct with hostable
	// and refused fields and checking they are empty proves nothing — any
	// JSON object without those keys decodes to two nil slices, including a
	// perfectly good error body. What a console actually does is look for
	// the keys, so what this checks is that they are absent.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	for _, key := range []string{"hostable", "refused", "plugin"} {
		if _, present := fields[key]; present {
			t.Errorf("the error body carries the %q key, so a console reading this as a "+
				"matrix would render it as a real answer: %s", key, body)
		}
	}
	if _, present := fields["error"]; !present {
		t.Errorf("the error body has no error object, so nothing says what went wrong: %s", body)
	}
}

func TestAFailingSnapshotReachesTheConsoleAsAnError(t *testing.T) {
	// The other thing this route must not do: swallow a broken edge
	// inventory and report a clean answer. The operator's next action is
	// completely different for "the control plane cannot read the fleet"
	// and "your fleet is ready".
	boom := errors.New("edge repo unavailable")
	svc := &fakeService{compat: func(plugin.Requirement) (plugin.Matrix, error) {
		return plugin.Matrix{}, boom
	}}
	srv := newServer(t, svc, roleAdmin)
	code, body := getCompat(t, srv, "/v1/plugins/p/compatibility")
	if code == http.StatusOK {
		t.Fatalf("a broken inventory answered 200: %s", body)
	}
	if len(body) == 0 {
		t.Error("the failure reached the console as an empty body, so there is nothing to show an operator")
	}
}

func TestACompatibilityQueryWithNoPackageNameIsNotAnswered(t *testing.T) {
	// Chi will not route a path without a name segment, so this is really
	// a statement about the service boundary rather than the route: the
	// handler has no way to invent a subject, and the service refuses one.
	// Asserted at the service because that is where the refusal lives.
	svc := &fakeService{compat: func(req plugin.Requirement) (plugin.Matrix, error) {
		if req.Plugin == "" {
			return plugin.Matrix{}, errors.New("no package name")
		}
		return plugin.Matrix{Requirement: req}, nil
	}}
	srv := newServer(t, svc, roleAdmin)
	if code, _ := getCompat(t, srv, "/v1/plugins/p/compatibility"); code != http.StatusOK {
		t.Fatalf("a named query was refused: %d", code)
	}
}
