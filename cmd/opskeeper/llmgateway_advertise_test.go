package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/domains/server/llmgw"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

// This file is the boot-path guard for the plan's first P0.
//
// The plan's P0-1 is that a node cannot reach a model: the credential chain
// is broken, so "one agent per machine" is not deliverable. The fix has two
// halves that live in different files and were never in the same test.
//
//   - the gateway registers its routes, at llmgw.Register: /v1/chat/completions
//     and /v1/models, relative to whatever router main hands it.
//   - the manager tells every node which URL to use, at
//     modelEndpointResolver.AgentEndpoint: publicURL + "/v1".
//
// The two are a contract with no witness. A node is given one string and
// spends it against the other, and **the failure is a 404 per model call** —
// a node that boots, authenticates, dials home, reports its metrics, loads
// its plugins, and then answers every question with an error from a route
// that does not exist. That is precisely the symptom agentmodel.go's boot
// comment says it exists to distinguish from "the model had an opinion".
//
// Measured this round, before this file existed: changing AgentEndpoint to
// return the gateway root without the "/v1" suffix still compiles, and
// `go test ./...` (737 cases) and `go test ./tests/...` (12 cases) both stay
// green. The advertised URL and the registered route can drift by a path
// segment and nothing in the repository notices. This is the tenth hole of
// this shape and the first with no gate, no comment, and no test that even
// mentions the invariant — the crystallisation one (decision 244) at least
// had a make target named after the feature.
//
// So the test does not compare two strings. It builds the real router, mounts
// the real handler on it, asks the real resolver what it would tell a node,
// and then **sends a node's request to exactly that address**. The assertion
// is that the request reaches the model call, which is the whole of P0-1.

// advertisedEdgeCredential is the credential pair a node would present. The
// gateway reuses the tunnel credential, so this is the one secret a node
// already holds (see decision 106's reconciliation with the plan's llm.token).
const (
	advertisedAccessKey = "edge-access"
	advertisedSecretKey = "edge-secret"
)

// edgeAuthAcceptsOneNode is the gateway's credential check, reduced to the
// one pair above. It is the same stub shape llmgw's own tests use; it is
// repeated here rather than exported because the two files are in different
// packages and a shared test helper for two callers is a second place to
// forget to update.
type edgeAuthAcceptsOneNode struct{}

func (edgeAuthAcceptsOneNode) Authenticate(_ context.Context, accessKey, secretKey string) (tunnel.Session, error) {
	if accessKey != advertisedAccessKey || secretKey != advertisedSecretKey {
		return tunnel.Session{}, errs.ErrUnauthorized
	}
	return tunnel.Session{EdgeID: 1}, nil
}

// countingCompleter records that the model was actually reached. The
// distinction matters: a 200 from the gateway proves routing, authentication
// and the model call, whereas a 401 would prove only that *some* route
// matched — which is a weaker claim that a stray middleware could satisfy.
type countingCompleter struct {
	calls atomic.Int64
}

func (c *countingCompleter) Complete(_ context.Context, _ pigmodel.Request) (*pigai.AssistantMessage, error) {
	c.calls.Add(1)
	msg := &pigai.AssistantMessage{}
	msg.Content = append(msg.Content, pigai.TextContent{Text: "acknowledged"})
	return msg, nil
}

// TestTheURLANodeIsToldReachesTheGatewayTheManagerMounts is the closed loop
// for P0-1: manager → advertised URL → registered route → model call.
func TestTheURLANodeIsToldReachesTheGatewayTheManagerMounts(t *testing.T) {
	completer := &countingCompleter{}
	handler, err := llmgw.NewHandler(llmgw.Options{
		Auth:      edgeAuthAcceptsOneNode{},
		Completer: completer,
	})
	if err != nil {
		t.Fatalf("build the gateway: %v", err)
	}

	// main.go mounts the gateway on the root mux, so the test does the same
	// rather than on a subrouter: mounting on a subrouter here would let a
	// prefix drift between the two halves of main.go pass this test.
	router := chi.NewRouter()
	handler.Register(router)
	server := httptest.NewServer(router)
	defer server.Close()

	advertised, _ := modelEndpointResolver{publicURL: server.URL}.AgentEndpoint(context.Background())
	if advertised == "" {
		t.Fatalf("the manager told nodes nothing to reach. A node with no endpoint keeps " +
			"whatever it had, which is a working deployment — so this is not a failure of " +
			"the test, it is the resolver declining. Assert on the route instead")
	}

	// A node's agent is an OpenAI client, so it appends the operation to the
	// root it was given. Spelling the join out rather than hardcoding
	// "/v1/chat/completions" is the point: the test must fail if either side
	// moves, which is the drift this exists to catch.
	target := strings.TrimRight(advertised, "/") + "/chat/completions"

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target,
		strings.NewReader(`{"model":"opskeeper-test","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatalf("build the request a node would send: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+advertisedAccessKey+":"+advertisedSecretKey)

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("a node's request to the URL the manager advertised (%s) did not complete: %v", target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// The whole chain held.
	case http.StatusNotFound:
		t.Fatalf("the manager tells every node to reach %s, and the gateway serves no route "+
			"there. Every model call from every node is a 404, and the symptom is a node that "+
			"boots, authenticates, reports its metrics and then answers every question with an "+
			"error about a route that does not exist. Two places own this string: the routes in "+
			"llmgw.Register, and the suffix in modelEndpointResolver.AgentEndpoint",
			target)
	case http.StatusUnauthorized:
		t.Fatalf("the request to %s found a route and was refused at the credential check. "+
			"The routing half of P0-1 holds; the stub credential in this test is what the "+
			"gateway rejected", target)
	default:
		t.Fatalf("a node's request to %s got %d; the route resolved but the gateway answered "+
			"something other than a model call", target, resp.StatusCode)
	}

	if got := completer.calls.Load(); got != 1 {
		t.Errorf("the model was called %d times, want 1. A 200 without the call means the "+
			"gateway answered from somewhere other than the model, which is the same class "+
			"of drift this test exists to catch", got)
	}
}
