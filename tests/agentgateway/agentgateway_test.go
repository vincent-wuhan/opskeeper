// Package agentgateway_test is the end-to-end proof that a node can reach a
// model.
//
// Everything else in this repository's AI path is checked one side at a time:
// that the node writes a configuration the agent's schema accepts, that the
// gateway authenticates a credential and translates a transcript, that the
// translation preserves tool-call identity. Each of those can be right while
// the chain is broken, because the seams are exactly where a wrong assumption
// hides — a header name the client does not send, a default the client
// overrides, a streaming flag the gateway ignores.
//
// So this test runs the whole thing: the real `pig` binary, pointed at a
// configuration written by the real node code, talking to the real gateway
// handler. The only fake in it is the upstream model, which is the one party
// in the chain OpsKeeper does not own.
package agentgateway_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/edge/agentmodel"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/domains/server/llmgw"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

const (
	testAccessKey = "ak-node-1"
	testSecretKey = "sk-node-1"
)

// stubAuth is the tunnel's authenticator reduced to a map: it answers exactly
// the pair a real node presents and nothing else, so a gateway that
// authenticated anything else would fail here.
type stubAuth struct {
	mu    sync.Mutex
	calls []string
}

func (s *stubAuth) Authenticate(_ context.Context, accessKey, secretKey string) (tunnel.Session, error) {
	s.mu.Lock()
	s.calls = append(s.calls, accessKey+":"+secretKey)
	s.mu.Unlock()
	if accessKey != testAccessKey || secretKey != testSecretKey {
		return tunnel.Session{}, errs.ErrUnauthorized
	}
	return tunnel.Session{EdgeID: 1}, nil
}

// recordingCompleter is the upstream OpsKeeper does not own.
type recordingCompleter struct {
	mu   sync.Mutex
	seen []string
}

func (c *recordingCompleter) Complete(_ context.Context, req pigmodel.Request) (*pigai.AssistantMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, msg := range req.Messages {
		c.seen = append(c.seen, msgType(msg))
	}
	reply := pigai.AssistantMessage{}
	reply.Content = append(reply.Content, pigai.TextContent{Text: "the root filesystem is full; ext4 is at 100%"})
	return &reply, nil
}

func msgType(msg pigai.Message) string {
	switch msg.(type) {
	case pigai.SystemMessage:
		return "system"
	case pigai.UserMessage:
		return "user"
	case pigai.AssistantMessage:
		return "assistant"
	case pigai.ToolResultMessage:
		return "tool"
	default:
		return "other"
	}
}

func (c *recordingCompleter) roles() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

func (a *stubAuth) attempts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

// TestANodesAgentReachesAModelThroughTheGateway is the test this file exists
// for.
//
// Four facts are established here that no unit test can establish, because
// each is a property of a program this repository does not own:
//
//  1. the agent reads a custom provider from the directory the node names,
//     which is not its default and not the working directory;
//  2. it presents the credential as `Authorization: Bearer <what the node
//     injected>` — the exact string the gateway's authenticator splits;
//  3. it streams by default, so a gateway that only implemented the
//     non-streaming shape would answer a request the client then abandons
//     with "stream ended without finish_reason";
//  4. the reply the gateway synthesises from a settled PiG message is
//     consumable by the client, and the words come back out of it.
func TestANodesAgentReachesAModelThroughTheGateway(t *testing.T) {
	binary := agentBinary(t)

	auth := &stubAuth{}
	completer := &recordingCompleter{}
	gateway, err := llmgw.NewHandler(llmgw.Options{Auth: auth, Completer: completer})
	if err != nil {
		t.Fatalf("build the gateway: %v", err)
	}
	router := chi.NewRouter()
	gateway.Register(router)
	server := httptest.NewServer(router)
	defer server.Close()

	agentHome := t.TempDir()
	workDir := t.TempDir()

	// The node's own writer, with the gateway's address. Nothing here is
	// hand-written JSON: if this file's model of the configuration drifts
	// from what a node writes, the test stops passing, which is the entire
	// reason it lives in this package rather than next to the gateway.
	cfg := agentmodel.Config{
		BaseURL: server.URL + "/v1",
		Token:   testAccessKey + ":" + testSecretKey,
		Model:   "opskeeper-default",
		Dir:     agentHome,
	}
	if _, err := agentmodel.Write(cfg); err != nil {
		t.Fatalf("write the node's agent configuration: %v", err)
	}

	// #nosec G204 -- the path is this repository's own build output.
	cmd := exec.Command(binary,
		"--print",
		"why is the root filesystem full",
		"--provider", agentmodel.ProviderID,
		"--model", cfg.Model,
		"--no-session",
	)
	cmd.Dir = workDir
	// HOME is a fresh empty directory and PIG_CODING_AGENT_DIR points at
	// the node's own scope. The agent therefore has exactly one place to
	// find this configuration, and it is the place a node writes.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"PIG_CODING_AGENT_DIR=" + agentHome,
		agentmodel.TokenEnv + "=" + cfg.Token,
	}

	done := make(chan struct{})
	var output []byte
	var runErr error
	go func() {
		defer close(done)
		output, runErr = cmd.CombinedOutput()
	}()

	select {
	case <-done:
	case <-time.After(120 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the agent did not finish within two minutes; it is most likely waiting on a " +
			"request the gateway never received")
	}

	// The credential reached the gateway, in the form it splits.
	attempts := auth.attempts()
	if len(attempts) == 0 {
		t.Fatalf("the gateway authenticated nobody; the agent never sent a request.\nagent said:\n%s", output)
	}
	if attempts[0] != testAccessKey+":"+testSecretKey {
		t.Errorf("the gateway saw credential %q, want the node's own pair", attempts[0])
	}

	// The transcript arrived, with a system turn in front of the user's.
	roles := completer.roles()
	if len(roles) < 2 || roles[0] != "system" || roles[1] != "user" {
		t.Errorf("the model was given roles %v; the agent's own prompt has to survive the hop "+
			"or the node's tool policy arrives as unattributed text", roles)
	}

	// And the answer came back out of the client.
	text := string(output)
	if !strings.Contains(text, "root filesystem is full") {
		t.Errorf("the model's answer did not reach the agent's output.\nagent said:\n%s", text)
	}
	if runErr != nil {
		t.Errorf("the agent exited with %v.\nagent said:\n%s", runErr, text)
	}
}

// TestAStreamedReplyIsTheOneTheAgentAsksFor pins the default.
//
// The agent streams. A gateway that answered a streamed request with a single
// JSON body is not slower — it is broken, and the client says so in five
// words: "Stream ended without finish_reason". This records that the streamed
// path is the one that runs in production, so the day someone adds a
// non-streaming shortcut it has to come back here.
func TestAStreamedReplyIsTheOneTheAgentAsksFor(t *testing.T) {
	binary := agentBinary(t)

	auth := &stubAuth{}
	completer := &recordingCompleter{}
	gateway, err := llmgw.NewHandler(llmgw.Options{Auth: auth, Completer: completer})
	if err != nil {
		t.Fatalf("build the gateway: %v", err)
	}
	router := chi.NewRouter()
	gateway.Register(router)
	server := httptest.NewServer(router)
	defer server.Close()

	agentHome := t.TempDir()
	cfg := agentmodel.Config{
		BaseURL: server.URL + "/v1",
		Token:   testAccessKey + ":" + testSecretKey,
		Model:   "opskeeper-default",
		Dir:     agentHome,
	}
	if _, err := agentmodel.Write(cfg); err != nil {
		t.Fatalf("write the node's agent configuration: %v", err)
	}

	// #nosec G204 -- the path is this repository's own build output.
	cmd := exec.Command(binary, "--print", "hello",
		"--provider", agentmodel.ProviderID, "--model", cfg.Model, "--no-session")
	cmd.Dir = t.TempDir()
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"PIG_CODING_AGENT_DIR=" + agentHome,
		agentmodel.TokenEnv + "=" + cfg.Token,
	}
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("the agent failed against the streamed path: %v\n%s", runErr, out)
	}
	if strings.Contains(string(out), "Stream ended without finish_reason") {
		t.Errorf("the agent reported a truncated stream, which is what a gateway that answers a "+
			"streamed request with one JSON body produces.\nagent said:\n%s", out)
	}
}

// TestTheModelsCatalogueIsServedAndIsNotFree pins the discovery route.
//
// An unauthenticated catalogue is a map of the deployment: it names which
// model this cluster runs and therefore which provider account is in use.
func TestTheModelsCatalogueIsServedAndIsNotFree(t *testing.T) {
	auth := &stubAuth{}
	gateway, err := llmgw.NewHandler(llmgw.Options{Auth: auth, Completer: &recordingCompleter{}})
	if err != nil {
		t.Fatalf("build the gateway: %v", err)
	}
	router := chi.NewRouter()
	gateway.Register(router)
	server := httptest.NewServer(router)
	defer server.Close()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 401 {
		t.Errorf("an unauthenticated /v1/models returned %d; a node-facing catalogue that anyone "+
			"can read is a map of the deployment", rec.Code)
	}

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+testAccessKey+":"+testSecretKey)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("an authenticated /v1/models returned %d: %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("the catalogue is not the OpenAI list shape: %v", err)
	}
	if listed.Object != "list" {
		t.Errorf("object is %q, want list", listed.Object)
	}
}

// agentBinary finds the built agent, or skips.
//
// It is skipped rather than failed because a developer who has not run
// `make build-pig-all` should not see this file go red over a binary they do
// not have — but it is not skipped in CI, where the agent is built as part of
// the release, and that is the run that matters.
func agentBinary(t *testing.T) string {
	t.Helper()
	if override := strings.TrimSpace(os.Getenv("OPSKEEPER_TEST_PIG_BIN")); override != "" {
		if _, err := os.Stat(override); err != nil {
			t.Fatalf("OPSKEEPER_TEST_PIG_BIN=%s does not exist: %v", override, err)
		}
		return override
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "bin", runtime.GOOS+"-"+runtime.GOARCH, "pig")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skipf("no built agent; run 'make build-pig-%s-%s' (or set OPSKEEPER_TEST_PIG_BIN)",
		runtime.GOOS, runtime.GOARCH)
	return ""
}
