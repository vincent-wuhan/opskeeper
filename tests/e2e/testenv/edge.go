//go:build e2e

package testenv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Edge is one running node process under test, with the credentials it
// dialled home with.
//
// It is a process, not a simulation. That is the entire point of the
// delivery acceptance: the questions being answered are "does a node boot,
// connect, and serve a turn", and a harness that answers them with an
// in-process stand-in answers a different set of questions — the ones the
// nodefleet e2e already answers.
type Edge struct {
	// ID is the manager's row id for this node, i.e. the number the console
	// routes a conversation by.
	ID uint64
	// AccessKey / SecretKey are the tunnel credential pair. The agent is
	// configured with the *same* pair as its model credential, which is the
	// whole gateway design: a node holds no provider key, and the only
	// secret on it is the one that identifies it to its own manager.
	AccessKey string
	SecretKey string

	// ConfigDir is the node's agent scope (models.json, the piglet
	// profile). It is a temp dir, and the test asserts on what is in it.
	ConfigDir string
	// WorkDir is the agent's working directory, where its packages would be
	// unpacked.
	WorkDir string

	// TelemetryWALDir is where the node's telemetry write-ahead log
	// lives. A test can watch the directory directly, which is the only
	// way to see "the samples are on disk" from outside the process.
	TelemetryWALDir string

	// environ is the environment this node process was actually started
	// with, kept so a test can assert on it. The plan's acceptance
	// criterion is about the node's *process environment*, and a test that
	// only reasons about the map it passed in would be asserting on its own
	// input rather than on the process.
	environ []string

	cmd     *exec.Cmd
	logBuf  *bytes.Buffer
	stopped sync.Once
}

var (
	edgeBinOnce sync.Once
	edgeBinPath string
	edgeBinErr  error

	pigBinOnce sync.Once
	pigBinPath string
	pigBinErr  error
)

// EdgeBinary builds cmd/opskeeper-edge once per `go test`.
func EdgeBinary(t *testing.T) string {
	t.Helper()
	edgeBinOnce.Do(func() {
		repo := repoRoot()
		if repo == "" {
			edgeBinErr = fmt.Errorf("cannot locate repo root from testenv source")
			return
		}
		dir, err := mkTempDir("opskeeper-e2e-edge-")
		if err != nil {
			edgeBinErr = err
			return
		}
		out := filepath.Join(dir, "opskeeper-edge")
		// The version is stamped, exactly as a release stamps it, and the
		// reason it has to be is worth stating: a node refuses to host a
		// package whose min_edge_version it cannot compare against, and
		// the comparison needs a plain dotted numeric. An unstamped build
		// reports "dev", every package is refused at the version step, and
		// the node comes up as "no AI agent" with a boot log that has to be
		// read to explain. (The repository's own VERSION file is
		// v2026.09.14-rc4, which is *not* a form a node can compare — a
		// discrepancy between the release version string and the admission
		// check, worth reconciling, and one this harness does not paper
		// over by quietly defaulting to something that happens to parse.)
		cmd := exec.Command("go", "build",
			"-ldflags", "-X main.version="+nodeAdvertisedVersion,
			"-o", out, "./cmd/opskeeper-edge")
		cmd.Dir = repo
		cmd.Env = buildEnv()
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Run(); err != nil {
			edgeBinErr = fmt.Errorf("go build ./cmd/opskeeper-edge: %w\n%s", err, buf.String())
			return
		}
		edgeBinPath = out
	})
	if edgeBinErr != nil {
		t.Fatalf("testenv: %v", edgeBinErr)
	}
	return edgeBinPath
}

// PigBinary builds the node's agent once per `go test`, from core/pig with
// the workspace off.
//
// GOWORK=off is not a detail: that is how the node's agent is built for
// real (Makefile build-pig-*), and building it through the workspace would
// link the repository's own packages into a binary that ships without
// them. A harness that built the agent the easy way would pass against a
// binary no node ever runs.
func PigBinary(t *testing.T) string {
	t.Helper()
	pigBinOnce.Do(func() {
		repo := repoRoot()
		pigDir := filepath.Join(repo, "core", "pig")
		if _, err := os.Stat(filepath.Join(pigDir, "go.mod")); err != nil {
			pigBinErr = fmt.Errorf("core/pig module not found: %w", err)
			return
		}
		dir, err := mkTempDir("opskeeper-e2e-pig-")
		if err != nil {
			pigBinErr = err
			return
		}
		out := filepath.Join(dir, "pig")
		cmd := exec.Command("go", "build", "-trimpath", "-o", out, "github.com/MichaelKinsy/PiG/cmd/pig")
		cmd.Dir = pigDir
		cmd.Env = append(buildEnv(), "CGO_ENABLED=0")
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Run(); err != nil {
			pigBinErr = fmt.Errorf("go build pig (GOWORK=off): %w\n%s", err, buf.String())
			return
		}
		pigBinPath = out
	})
	if pigBinErr != nil {
		t.Fatalf("testenv: %v", pigBinErr)
	}
	return pigBinPath
}

// nodeAdvertisedVersion is the version this harness's node reports.
//
// It is a plain dotted numeric on purpose — see EdgeBinary.
const nodeAdvertisedVersion = "0.9.0"

// EdgeOptions is what a node needs to be a node.
type EdgeOptions struct {
	// FrontierEdgeAddr is the broker's edgebound listener.
	FrontierEdgeAddr string
	// AccessKey / SecretKey are the credentials from POST /api/v1/edges.
	AccessKey string
	SecretKey string
	// GatewayBaseURL is the manager's OpenAI-compatible endpoint, root
	// form (http://host:port/v1). The node's agent is pointed at this and
	// nowhere else, which is the property the whole edge-holds-no-provider-
	// key design rests on.
	GatewayBaseURL string
	// Model is the slug the gateway will serve. Must be a model the
	// manager's registry can resolve, or the turn fails at the gateway
	// with a message about the cluster.
	Model string
	// CollectorMode is the node's periodic metric-push path. Empty means
	// "off": the harness default, and what a fresh install uses when the
	// hostmetrics / procmetrics plugins expose the same data by direct
	// scrape. A test about the telemetry write-ahead log has to say
	// "embedded" out loud, because the default node samples nothing and
	// would make "the log drained" true of a log that was never
	// written to in the first place.
	CollectorMode string
	// PackageTools names the tools the node's admitted package declares.
	// Empty -- the harness default, and what a governance-only package
	// looks like -- means the node's agent is offered no plugin tool at
	// all. A test about the tool path has to name one out loud, because a
	// node with an empty tool list makes "the agent called a tool" true of
	// nothing.
	PackageTools []string
	// ShippedPackageTools names the tools the node's package declares when
	// the package is the REAL shipped one: plugins/pig-ops/
	// opskeeper-sre-readonly, copied verbatim, extensions and all.
	//
	// It is a separate field from PackageTools because the two fixtures are
	// different claims. PackageTools is a governance manifest with no
	// extension behind it: it proves the node admitted a declaration, and
	// it proves nothing about a tool being callable, because a declaration
	// is a promise and nothing in that fixture keeps it. ShippedPackageTools
	// copies the artifact a node actually installs, so the agent builds the
	// real extension, registers the real tools, and a call the model makes
	// lands in the real host executor.
	//
	// The two are not alternatives; the empty manifest is the cheaper test
	// and this one is the one that can fail for the reasons a production
	// node fails (the extension will not build, the gate refuses the call,
	// the broker has no executor under that name).
	ShippedPackageTools []string
	// CollectorInterval is how often the node samples. Empty leaves the
	// production default (10s), which is longer than these tests wait.
	CollectorInterval time.Duration
	// HeartbeatInterval is how often the node proves its link is alive.
	// Empty leaves the production default (30s).
	//
	// It is a field rather than a constant in the harness for the same
	// reason it is one on the node: a test that has to observe link state
	// inside a 30s tick either waits 30s or gives up on the observation.
	// Decision 133 could only keep its outage window short because the
	// window was not something the test chose — it was whatever the
	// production heartbeat left over.
	HeartbeatInterval time.Duration
	// TunnelStuckThreshold is how many consecutive failed heartbeats the
	// node tolerates before exiting for respawn. Zero leaves the
	// production default (5), which with a fast heartbeat is five
	// seconds — comfortably longer than the outage a test wants to
	// survive, and long enough that the node proves it did not give up.
	TunnelStuckThreshold int
}

// StartEdge spawns a node process and waits for it to answer for itself.
//
// Waiting is on the manager's view of the node's supervisor, not on a sleep
// and not on the tunnel connect. "The tunnel is up" and "the node's agent
// is running" are different facts, and only the second one means a
// conversation has anything to talk to.
func StartEdge(t *testing.T, env *Env, bearer string, opts EdgeOptions) *Edge {
	t.Helper()

	edge := &Edge{
		AccessKey: opts.AccessKey,
		SecretKey: opts.SecretKey,
		ConfigDir: t.TempDir(),
		WorkDir:   t.TempDir(),
	}
	// The node writes its agent scope and profile into ConfigDir; the
	// working directory is where packages are unpacked. Both are temp dirs
	// so the assertion "no cloud credential on this node" is about a
	// directory the test owns end to end.
	for _, d := range []string{edge.ConfigDir, edge.WorkDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("testenv: prepare node dir: %v", err)
		}
	}
	edge.TelemetryWALDir = filepath.Join(edge.WorkDir, "telemetry")
	packageRoot := writeAdmittedPackage(t, filepath.Join(edge.WorkDir, "packages"), opts.PackageTools...)
	if len(opts.ShippedPackageTools) > 0 {
		packageRoot = writeShippedReadonlyPackage(t, filepath.Join(edge.WorkDir, "packages"),
			opts.ShippedPackageTools)
	}

	collectorMode := opts.CollectorMode
	if collectorMode == "" {
		collectorMode = "off"
	}
	collectorInterval := ""
	if opts.CollectorInterval > 0 {
		collectorInterval = opts.CollectorInterval.String()
	}

	edgeEnv := map[string]string{
		"OPSKEEPER_EDGE_CLOUD_ADDR":           opts.FrontierEdgeAddr,
		"OPSKEEPER_EDGE_ACCESS_KEY":           opts.AccessKey,
		"OPSKEEPER_EDGE_SECRET_KEY":           opts.SecretKey,
		"OPSKEEPER_EDGE_COLLECTOR_MODE":       collectorMode,
		"OPSKEEPER_EDGE_TELEMETRY_WAL_DIR":    edge.TelemetryWALDir,
		"OPSKEEPER_EDGE_CHANGE_EVENT_WAL_DIR": filepath.Join(edge.WorkDir, "changes"),
		"OPSKEEPER_EDGE_UPGRADE_STAGE_DIR":    filepath.Join(edge.WorkDir, "upgrade"),
		"OPSKEEPER_EDGE_PLUGIN_WORK_DIR":      filepath.Join(edge.WorkDir, "plugins"),
		"OPSKEEPER_EDGE_PLUGIN_STORE_DIR":     filepath.Join(edge.WorkDir, "plugins"),
		// The agent's own scope. ConfigDir is the node's; the binary is
		// the one built above; the working dir is where its packages would
		// live. Nothing here is a cloud credential: the token is the node's
		// own tunnel pair, formatted the way the gateway authenticates.
		"OPSKEEPER_EDGE_AGENT_CONFIG_DIR": edge.ConfigDir,
		"OPSKEEPER_EDGE_AGENT_DIR":        edge.WorkDir,
		"OPSKEEPER_EDGE_AGENT_BIN":        PigBinary(t),
		"OPSKEEPER_EDGE_AGENT_BASE_URL":   opts.GatewayBaseURL,
		"OPSKEEPER_EDGE_AGENT_TOKEN":      opts.AccessKey + ":" + opts.SecretKey,
		"OPSKEEPER_EDGE_AGENT_MODEL":      opts.Model,
		// The node's package set, named explicitly rather than left to the
		// default path. A node that starts with the default looks for a
		// boot bundle under /var/lib, finds none, and logs a refusal that
		// reads like a permissions problem; naming it here means the
		// package this test admits is the package under test.
		"OPSKEEPER_EDGE_AGENT_PACKAGES": packageRoot,
	}
	if collectorInterval != "" {
		edgeEnv["OPSKEEPER_EDGE_COLLECTOR_INTERVAL"] = collectorInterval
	}
	// Only set when a test asked. Leaving them unset is not a shortcut,
	// it is the default posture: a node here should run the same numbers
	// a node in production runs, and a test that wants different ones
	// says so where a reader can see it.
	if opts.HeartbeatInterval > 0 {
		edgeEnv["OPSKEEPER_EDGE_HEARTBEAT_INTERVAL"] = opts.HeartbeatInterval.String()
	}
	if opts.TunnelStuckThreshold > 0 {
		edgeEnv["OPSKEEPER_EDGE_TUNNEL_STUCK_THRESHOLD"] = strconv.Itoa(opts.TunnelStuckThreshold)
	}

	edge.logBuf = &bytes.Buffer{}
	cmd := exec.Command(EdgeBinary(t))
	edge.environ = mergedEnv(edgeEnv)
	cmd.Env = edge.environ
	cmd.Stdout = edge.logBuf
	cmd.Stderr = edge.logBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("testenv: start edge: %v", err)
	}
	edge.cmd = cmd
	t.Cleanup(edge.Stop)
	return edge
}

// Stop terminates the node process. Idempotent.
func (e *Edge) Stop() {
	e.stopped.Do(func() {
		if e.cmd == nil || e.cmd.Process == nil {
			return
		}
		_ = e.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			_ = e.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = e.cmd.Process.Kill()
			<-done
		}
	})
}

// Logs returns everything the node has written, for a failure message.
func (e *Edge) Logs() string {
	if e.logBuf == nil {
		return ""
	}
	return e.logBuf.String()
}

// PID is the node process's own pid, for checks that have to read the
// kernel's view of it rather than the harness's.
func (e *Edge) PID() int {
	if e.cmd == nil || e.cmd.Process == nil {
		return 0
	}
	return e.cmd.Process.Pid
}

// Environ returns the environment this node process was started with.
//
// It is the harness's own construction, so it answers "what did the node
// inherit" exactly. For the stronger claim — what the running process
// actually holds — use LiveEnviron, which is only available where the
// operating system will show it to us.
func (e *Edge) Environ() []string {
	return append([]string(nil), e.environ...)
}

// LiveEnviron reads a running process's environment from the kernel.
//
// It returns ok=false where the OS will not show one process another
// process's environment: Linux exposes /proc/<pid>/environ, and macOS does
// not expose it at all — `ps e` is refused for a process you did not
// exec, which is the correct default and not something to work around. The
// caller is expected to say so out loud rather than quietly pass, because a
// skipped check and a passing check must not look alike in the output.
func LiveEnviron(pid int) (env []string, ok bool) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil, false
	}
	for _, kv := range strings.Split(string(raw), "\x00") {
		if kv != "" {
			env = append(env, kv)
		}
	}
	return env, len(env) > 0
}

// AgentPIDs returns the pids of `pig` processes this node started.
//
// It reads the process table rather than asking the node, because the
// acceptance is "an operator running ps on the host sees an independent
// agent process" and asking the node whether it started a process cannot
// answer that — a supervisor that lost track of its child would happily
// report success.
func (e *Edge) AgentPIDs(t *testing.T) []int {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("testenv: ps: %v", err)
	}
	binary := PigBinary(t)
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// Match the exact binary this node was told to run, not any pig on
		// the developer's machine: the test asserts about its own child.
		if strings.HasPrefix(fields[1], binary) {
			var pid int
			if _, err := fmt.Sscanf(fields[0], "%d", &pid); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

// WaitForRunningAgent polls the manager until the node reports a running
// agent, and returns that report.
//
// The report is returned rather than discarded because a node that is
// running a crash-looping agent also eventually answers "running": the
// restarts and last_error fields are what distinguish a node with an agent
// from a node that keeps failing to start one, and a test that only checked
// the boolean would pass on the second one.
func (e *Edge) WaitForRunningAgent(t *testing.T, env *Env, bearer string, edgeID uint64, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last map[string]any
	var lastStatus int
	for time.Now().Before(deadline) {
		status, body, err := env.DoJSON("GET", fmt.Sprintf("/api/v1/node-agents/%d/health", edgeID), nil, bearer)
		if err == nil {
			lastStatus = status
			last = body
			if status == 200 {
				if running, _ := body["running"].(bool); running {
					if degraded, _ := body["degraded"].(bool); degraded {
						t.Fatalf("node agent is running but degraded: %s", MustJSON(body))
					}
					return body
				}
			}
		}
		if e.cmd.ProcessState != nil && e.cmd.ProcessState.Exited() {
			t.Fatalf("edge exited before its agent came up (code %d):\n%s",
				e.cmd.ProcessState.ExitCode(), e.Logs())
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("node agent did not come up within %s (last status %d: %s)\n=== edge logs ===\n%s\n=== end edge logs ===",
		timeout, lastStatus, MustJSON(last), e.Logs())
	return nil
}

// MustJSON renders a body for a failure message.
func MustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// CreateEdge registers a node through the console's own API and returns its
// id and credential pair.
//
// Through the API rather than by inserting a row: the credential pair is
// minted by the same code a real operator's is, and a harness that generated
// one itself would be testing a node that can never be created by the
// product.
func (e *Env) CreateEdge(t *testing.T, bearer, name string) (uint64, string, string) {
	t.Helper()
	status, body, err := e.DoJSON("POST", "/api/v1/edges", map[string]string{"name": name}, bearer)
	if err != nil {
		t.Fatalf("create edge: transport: %v", err)
	}
	if status != 201 {
		t.Fatalf("create edge: status=%d body=%s", status, MustJSON(body))
	}
	id := uint64(0)
	if raw, ok := body["id"].(float64); ok {
		id = uint64(raw)
	}
	access, _ := body["access_key_id"].(string)
	secret, _ := body["secret_key"].(string)
	if id == 0 || access == "" || secret == "" {
		t.Fatalf("create edge: incomplete response: %s", MustJSON(body))
	}
	return id, access, secret
}

// StreamConversation opens a conversation's SSE stream and returns the
// frames as they arrive on a channel, plus a stop function.
//
// The console's order is load-bearing and this reproduces it: attach first,
// then send. A nodeagent Service refuses a turn nobody is watching
// (ErrNoStream -> 409 not_streaming), which is the right product decision
// and means a harness that posts the message first is testing a path the
// console never takes.
func (e *Env) StreamConversation(t *testing.T, bearer, sessionID string) (<-chan map[string]any, func()) {
	t.Helper()
	url := fmt.Sprintf("%s/api/v1/node-agents/sessions/%s/stream", e.BaseURL(), sessionID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "text/event-stream")

	ctx, cancel := context.WithCancel(context.Background())
	client := &http.Client{}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		t.Fatalf("stream: %v", err)
	}
	if resp.StatusCode != 200 {
		cancel()
		t.Fatalf("stream: status=%d", resp.StatusCode)
	}

	frames := make(chan map[string]any, 64)
	go func() {
		defer close(frames)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var frame map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
				continue
			}
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()
	return frames, cancel
}

// writeAdmittedPackage creates one governance manifest the node will admit,
// and returns the package set root.
//
// It is written here rather than copied from plugins/pig-ops/ on purpose.
// The shipped packages carry Go extension sources a node has to build
// before it can host them, and building five extensions is not what this
// step is testing — the conversation is. Copying a real package and then
// stripping its extensions would produce a package that exists nowhere and
// claims to be one of the shipped ones, which is worse than a small
// manifest that says what it is: a node's package set, admitted by the real
// validator, carrying no tools.
//
// The tool call is the next increment of this acceptance, and when it comes
// it belongs to a package with real extensions in it. A green conversation
// here must not be read as "tools work on a node".
func writeAdmittedPackage(t *testing.T, base string, tools ...string) string {
	t.Helper()
	root := filepath.Join(base, "e2e-delivery")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("testenv: create package root: %v", err)
	}
	// The declared tool list is a parameter rather than a constant because the
	// empty list is what most of these tests want and the non-empty one is
	// the only way to ask the question this fixture exists for -- whether a
	// tool the node was offered is one the agent can actually be made to
	// call. Baking in either value would make the other kind of test
	// impossible to write.
	// Object form, not a bare string list: the manifest's tool entry is a
	// ToolDecl, and the node's validation refuses a string where it expects
	// one. Getting this shape wrong does not merely fail the test -- it fails
	// the *node*, which comes up with no agent at all, so a fixture written
	// from a guess about the schema reads exactly like a broken product.
	toolList := "[]"
	if len(tools) > 0 {
		decls := make([]string, 0, len(tools))
		for _, name := range tools {
			decls = append(decls, fmt.Sprintf("{name: %s, class: read}", name))
		}
		toolList = "[" + strings.Join(decls, ", ") + "]"
	}
	manifest := `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: e2e-delivery
  version: 0.1.0
  vendor: opskeeper
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  tools: ` + toolList + `
  required_scopes: []
  audit:
    emits: true
    mutates: false
  approval:
    required: false
  install:
    strategy: rolling
    min_edge_version: 0.7.0
`
	if err := os.WriteFile(filepath.Join(root, "pig-ops.yaml"), []byte(manifest), 0o640); err != nil {
		t.Fatalf("testenv: write package manifest: %v", err)
	}
	// The package root itself, not the directory holding it: the node's
	// list is a list of packages, each reviewed on its own, so handing it
	// the parent is a node looking for a manifest one level up.
	return root
}

// writeShippedReadonlyPackage copies the real shipped read-only package into
// the node's package set and narrows its governance manifest to the tools
// under test.
//
// It copies rather than re-declares, because the point of this fixture is the
// artifact a node installs: two Go extensions the agent has to build on the
// node, a Pi package manifest, skills, and the governance manifest whose tool
// list IS the host allow-list. A hand-written package with one fabricated tool
// would pass without ever loading a real extension, which is the exact class
// of green this repository has been burned by twice.
//
// The manifest is rewritten for one reason only: the shipped one declares
// eighteen tools, and a test that wants to prove ONE tool crossed the gate
// would have to make the model pick the right one of eighteen. Narrowing the
// declaration narrows the allow-list to the same set, so the test asks for
// the tool it declared and the gate is still the thing that allowed it. The
// copy keeps the shipped identity (name, version, scopes, audit and approval
// posture) so the node sees a package that looks like the real one.
func writeShippedReadonlyPackage(t *testing.T, base string, tools []string) string {
	t.Helper()
	repo := repoRoot()
	if repo == "" {
		t.Fatalf("testenv: cannot locate repo root to copy the shipped package from")
	}
	src := filepath.Join(repo, "plugins", "pig-ops", "opskeeper-sre-readonly")
	if _, err := os.Stat(filepath.Join(src, "pig-ops.yaml")); err != nil {
		t.Fatalf("testenv: shipped read-only package not found at its shipped path: %v", err)
	}
	root := filepath.Join(base, "opskeeper-sre-readonly")
	if err := copyTree(src, root); err != nil {
		t.Fatalf("testenv: copy the shipped package: %v", err)
	}

	decls := make([]string, 0, len(tools))
	for _, name := range tools {
		decls = append(decls, fmt.Sprintf("    - { name: %s, class: read }", name))
	}
	manifest := `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: opskeeper-sre-readonly
  version: 0.1.0
  vendor: opskeeper
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  tools:
` + strings.Join(decls, "\n") + `
  required_scopes:
    - host.read
  audit:
    emits: true
    mutates: false
  approval:
    required: false
  install:
    strategy: rolling
    min_edge_version: 0.7.0
`
	if err := os.WriteFile(filepath.Join(root, "pig-ops.yaml"), []byte(manifest), 0o640); err != nil {
		t.Fatalf("testenv: rewrite the shipped package's governance manifest: %v", err)
	}
	return root
}

// copyTree copies a directory tree, preserving the executable bit, which a
// Go extension does not need but a package that ships a helper script does.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o750)
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o750)
		case info.Mode()&os.ModeSymlink != 0:
			// A symlink in a shipped package is something to look at, not to
			// reproduce: following it could pull in something outside the
			// package, which is the shape of a supply-chain accident.
			return fmt.Errorf("refusing to copy symlink %s from the shipped package", rel)
		default:
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, body, info.Mode().Perm())
		}
	})
}

// NodeConversations asks the control plane what conversations it believes
// are open on a node.
//
// It is a diagnostic accessor rather than an assertion: the interesting
// question when a turn produces no frames is whether the control plane
// thinks the conversation exists at all, and that is a different fact from
// the one the SSE stream reports.
func (e *Env) NodeConversations(t *testing.T, bearer string) any {
	t.Helper()
	status, body, err := e.DoJSON("GET", "/api/v1/node-agents/sessions", nil, bearer)
	if err != nil {
		return fmt.Sprintf("unavailable: %v", err)
	}
	return map[string]any{"status": status, "body": body}
}

// ReadFileOrEmpty reads a node-owned file for a failure message, and never
// fails the test doing it: a missing file is itself information.
func ReadFileOrEmpty(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(%s: %v)", filepath.Base(path), err)
	}
	return string(body)
}
