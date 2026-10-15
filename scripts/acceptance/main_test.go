package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// An acceptance command that cannot fail is a ceremony. Every offline check
// here is broken in a fixture and asserted to go red, and every needs-input
// check is asserted to be reported MISSING rather than silently passed.

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, "deploy/install/edge/build-edge-bundle.sh",
		"STAGED=(\n"+
			"  \"promtail                 0755 /usr/local/lib/opskeeper-edge/promtail                 promtail-${ARCH}                      optional\"\n"+
			"  \"pig                      0755 /usr/local/lib/opskeeper-edge/pig                     pig-${ARCH}                           required\"\n"+
			")\n")
	write(t, root, "deploy/Dockerfile.opskeeper-edge",
		"RUN go build -o /out/pig cmd/pig\nCOPY --from=builder /out/pig /opskeeper-edge/pig\nENV OPSKEEPER_EDGE_AGENT_BIN=/opskeeper-edge/pig\n")
	write(t, root, "deploy/install/edge/install-edge.sh",
		"# Spawning `pig --version` here costs milliseconds.\n"+
			"if ! v=\"$(\"$AGENT_BIN\" --version 2>&1)\"; then\n"+
			"    log_error \"installed $AGENT_BIN but it does not run: $v\"\n"+
			"    exit 1\n"+
			"fi\n")
	write(t, root, "deploy/install/edge/opskeeper-edge.env.example",
		"OPSKEEPER_EDGE_MODEL_API_KEY=${OPSKEEPER_PROVIDER_KEY}\n")
	write(t, root, "core/floor/delivery/edge_agent_assets.go", `const modelsTemplate = "{\"apiKey\": \"$VAR\"}"`)
	write(t, root, "Makefile", "edge-credential-check:\n")
	// The two machine-dependent checks get nothing: no pig binary anywhere
	// under the fixture, no OPSKEEPER_ACCEPTANCE_PROVIDER_KEY.
	return root
}

func byID(t *testing.T, id string) check {
	t.Helper()
	for _, c := range stage0Checks {
		if c.id == id {
			return c
		}
	}
	t.Fatalf("no check %s", id)
	return check{}
}

func statusOf(t *testing.T, root, id string) status {
	t.Helper()
	ok, reason := byID(t, id).run(root)
	switch {
	case ok:
		return statusPass
	case reason != "":
		return statusFail
	case byID(t, id).needs != "":
		return statusMissing
	default:
		return statusFail
	}
}

func TestTheFixturePassesEverythingItCan(t *testing.T) {
	root := fixture(t)
	for _, id := range []string{"A1", "A2", "A3", "A4", "A5"} {
		if got := statusOf(t, root, id); got != statusPass {
			t.Errorf("%s = %s on the fixture", id, got)
		}
	}
	// The machine-dependent ones must report MISSING, never pass: a chain
	// that skipped them silently and exited zero would be reporting success
	// it did not verify.
	for _, id := range []string{"A6", "A8"} {
		if got := statusOf(t, root, id); got != statusMissing {
			t.Errorf("%s = %s, want MISSING when its input is absent", id, got)
		}
	}
}

// credentialShaped returns a string that looks like a provider key.
//
// It is assembled at run time on purpose. The open-source auditor flags any
// `sk-` followed by thirty characters or more anywhere in the tracked tree,
// because that is the shape a leaked key has. Writing such a literal into a
// test fixture is therefore not free: it makes the release gate red for a
// file that contains no credential, and the tempting fix — a shorter string
// that happens to dodge the regex — hides the cause instead of removing it.
//
// The template this writes really does contain a key-shaped value, which is
// the whole point of the A4 check. The repository's own source does not.
func credentialShaped() string { return "sk-" + strings.Repeat("x", 40) }

// Each offline check, broken in exactly one way.
func TestEveryOfflineCheckCanGoRed(t *testing.T) {
	cases := []struct {
		id, rel, body string
	}{
		{"A1", "deploy/install/edge/build-edge-bundle.sh",
			"STAGED=(\n  \"pig  0755 /usr/local/lib/opskeeper-edge/pig  pig-${ARCH}  optional\"\n)\n"},
		{"A2", "deploy/Dockerfile.opskeeper-edge", "FROM scratch\n"},
		{"A3", "deploy/install/edge/install-edge.sh",
			"# Spawning `pig --version` here costs milliseconds.\necho installing\n"},
		{"A4", "deploy/install/edge/opskeeper-edge.env.example", "OPSKEEPER_EDGE_MODEL_API_KEY=" + credentialShaped() + "\n"},
		{"A5", "Makefile", "build:\n"},
	}
	if len(cases) != 5 {
		t.Fatalf("%d cases for 5 offline checks", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			root := fixture(t)
			write(t, root, tc.rel, tc.body)
			if got := statusOf(t, root, tc.id); got != statusFail {
				t.Errorf("%s = %s after breaking %s, want FAIL", tc.id, got, tc.rel)
			}
		})
	}
}

// A missing file is a failure, not a skip: the artifact this repository is
// supposed to ship is not optional.
func TestAMissingArtifactFailsRatherThanSkips(t *testing.T) {
	root := fixture(t)
	if err := os.Remove(filepath.Join(root, "deploy/Dockerfile.opskeeper-edge")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := statusOf(t, root, "A2"); got != statusFail {
		t.Errorf("A2 = %s with the Dockerfile absent, want FAIL", got)
	}
}

// A8 used to assert the opposite of what it was for: "a key is present, so
// pass". That is the check that pins a name instead of a property — the
// subject line says one thing and the assertion lets a credential that
// cannot buy a single token through as evidence that the node can hold a
// conversation. These four replace it, and between them they cover the four
// outcomes a real call can have.

func TestTheCredentialCheckPassesOnlyWhenAStreamComesBack(t *testing.T) {
	root := fixture(t)
	t.Setenv(keyEnv, "test-key-for-the-stream-case")
	t.Setenv(providerURLEnv, servingSSE(t))
	if got := statusOf(t, root, "A8"); got != statusPass {
		t.Errorf("A8 = %s with a working endpoint, want pass", got)
	}
}

// The case the old assertion called a pass.
func TestACredentialThatCannotBuyATokenFails(t *testing.T) {
	root := fixture(t)
	t.Setenv(keyEnv, "sk-not-a-real-credential")
	t.Setenv(providerURLEnv, unauthorized(t, "sk-not-a-real-credential"))
	if got := statusOf(t, root, "A8"); got != statusFail {
		t.Errorf("A8 = %s with a key the provider rejects, want FAIL: a credential that is "+
			"present is not a credential that works", got)
	}
}

// 200 with no frames is the failure mode a non-streaming probe would miss,
// and stage 0's acceptance is explicitly about streaming.
func TestAnEndpointThatAnswers200WithoutFramesFails(t *testing.T) {
	root := fixture(t)
	t.Setenv(keyEnv, "test-key-for-the-silent-endpoint")
	t.Setenv(providerURLEnv, silentButSuccessful(t))
	if got := statusOf(t, root, "A8"); got != statusFail {
		t.Errorf("A8 = %s from a 200 that carried no stream frames, want FAIL", got)
	}
}

// Providers quote the key back in their errors, and this command's output is
// what somebody pastes into a ticket. The redaction is the reason the check
// may safely report a failure at all.
func TestAFailedCallNeverPrintsTheCredential(t *testing.T) {
	root := fixture(t)
	// Assembled at run time for the reason credentialShaped gives: a literal
	// of that shape in the tracked tree is itself a release-gate violation,
	// so writing one here to test a different gate leaves this repository
	// red for a file that holds no credential.
	key := credentialShaped()
	t.Setenv(keyEnv, key)
	t.Setenv(providerURLEnv, unauthorized(t, key))
	for _, r := range run(root, stage0Checks) {
		if r.id == "A8" && strings.Contains(r.reason, key) {
			t.Fatalf("A8's reason quotes the credential verbatim: %s", r.reason)
		}
	}
}

// servingSSE is an endpoint that behaves like a provider answering a
// one-token streamed completion.
func servingSSE(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"k\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

// unauthorized is an endpoint that rejects the way providers do — by quoting
// the key back in the message, which is the case redaction exists for.
func unauthorized(t *testing.T, key string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided: ` + key + `"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

// silentButSuccessful answers 200 and sends nothing a browser or a node would
// treat as a frame.
func silentButSuccessful(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

// The command's whole claim is the distinction between 1 and 3, so the
// classification of every check is asserted on the real tree too.
func TestTheRealTreeHasNoSilentPass(t *testing.T) {
	for _, r := range run("../..", stage0Checks) {
		if r.st == statusPass && byID(t, r.id).needs != "" {
			continue // a needs-input check may legitimately pass here
		}
		if r.st == statusPass {
			continue
		}
		if r.reason == "" {
			t.Errorf("%s is %s with no reason; every non-pass names what is missing", r.id, r.st)
		}
	}
}

// A check that is only ever exercised in its failing state is a check nobody
// knows works. A6 had no test at all for the passing case, and that is how a
// search path that no build rule produces survived: the one assertion about
// it was that it reports MISSING on an empty fixture — which it did,
// correctly, for a fixture with no binary anywhere.
//
// The binary is named at the path a build rule writes, derived from the same
// GOOS/GOARCH, so this test does not hardcode a machine either. The shape is
// what matters, and the shape is what the Makefile and the check now share.
func TestALocallyBuiltPigIsFoundWhereTheBuildRulesWriteIt(t *testing.T) {
	root := fixture(t)

	if got := statusOf(t, root, "A6"); got != statusMissing {
		t.Fatalf("A6 on a tree with no binary = %s, want MISSING", got)
	}

	rel := filepath.Join("bin", runtime.GOOS+"-"+runtime.GOARCH, "pig")
	write(t, root, rel, "#!/bin/sh\necho 0.4.0+1.0.0\n")
	// The executable bit is part of the claim, not decoration: A6 execs the
	// binary, so a fixture written 0644 fails for a reason that has nothing
	// to do with the path this test is about. A real build product is 0755.
	if err := os.Chmod(filepath.Join(root, rel), 0o755); err != nil {
		t.Fatalf("chmod the fixture binary: %v", err)
	}

	t.Setenv("OPSKEEPER_PIG_BIN", "")
	if got := statusOf(t, root, "A6"); got != statusPass {
		t.Fatalf("A6 with a binary at the path the build rules write (%s) = %s, want PASS.\n"+
			"This is the case that was broken: the binary was on disk and the check could not "+
			"see it, because the check looked somewhere the build never writes.",
			filepath.Join(root, rel), got)
	}
}
