package restart_service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// fakeClient is a tunnel.Client stub. Same shape as the host_files
// test fixture; kept local to avoid depending on biz/test internals.
type fakeClient struct {
	mu       sync.Mutex
	handlers map[string]tunnel.Handler
}

func newFakeClient() *fakeClient { return &fakeClient{handlers: map[string]tunnel.Handler{}} }

func (f *fakeClient) Dial(_ context.Context) error                     { return nil }
func (f *fakeClient) Call(_ context.Context, _ string, _, _ any) error { return nil }
func (f *fakeClient) OnReconnect(_ func())                             {}
func (f *fakeClient) Close() error                                     { return nil }
func (f *fakeClient) AcceptStream() (tunnel.StreamConn, error)         { return nil, nil }
func (f *fakeClient) RegisterHandler(method string, h tunnel.Handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = h
}
func (f *fakeClient) handler(method string) tunnel.Handler {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handlers[method]
}

func TestRegister_RegistersMethodAndDefaultsAreSane(t *testing.T) {
	c := newFakeClient()
	if err := Register(c, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if c.handler(tunnel.MethodRestartService) == nil {
		t.Fatalf("handler not registered for %q", tunnel.MethodRestartService)
	}
	sb := DefaultSandboxConfig()
	if !sb.Mocked {
		t.Errorf("the shipped default must stay Mocked=true; an edge that restarts services unasked is worse than one that pretends")
	}
	if sb.SystemctlPath != "systemctl" {
		t.Errorf("SystemctlPath default = %q, want systemctl", sb.SystemctlPath)
	}
	if len(sb.AllowedUnits) == 0 {
		t.Errorf("the shipped allow-list must not be empty")
	}
	if err := sb.Validate(); err != nil {
		t.Errorf("default sandbox should validate: %v", err)
	}
}

func TestSandbox_AllowsCanonicalForms(t *testing.T) {
	sb := DefaultSandboxConfig()
	cases := []struct {
		input string
		want  bool
	}{
		{"nginx", true},
		{"NGINX", true},
		{"  nginx  ", true},
		{"nginx.service", true},
		{"sshd", false},
		{"", false},
		{"nginx; rm -rf /", false},
	}
	for _, c := range cases {
		got := sb.Allows(c.input)
		if got != c.want {
			t.Errorf("Allows(%q) = %v, want %v", c.input, got, c.want)
		}
	}
}

func TestSandbox_ValidateRejectsEmpty(t *testing.T) {
	sb := &SandboxConfig{}
	if err := sb.Validate(); err == nil {
		t.Errorf("empty AllowedUnits should fail Validate")
	}
}

func TestHandler_MockSuccess(t *testing.T) {
	c := newFakeClient()
	if err := Register(c, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h := c.handler(tunnel.MethodRestartService)
	body, _ := json.Marshal(tunnel.RestartServiceRequest{Service: "nginx", Reason: "502s"})
	respBody, err := h(context.Background(), tunnel.Session{}, tunnel.MethodRestartService, body)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var resp tunnel.RestartServiceResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if !resp.Restarted {
		t.Errorf("Restarted should be true on mock success")
	}
	if !resp.Mocked {
		t.Errorf("Mocked must be true on the shipped default path")
	}
	if len(resp.Argv) != 0 {
		t.Errorf("a mock ran nothing, so it must carry no argv; got %v, which is a runbook promoted on a command nobody executed", resp.Argv)
	}
	if resp.Service != "nginx" {
		t.Errorf("Service echo = %q, want nginx", resp.Service)
	}
	if resp.StartedAt.After(resp.EndedAt) {
		t.Errorf("StartedAt should be <= EndedAt")
	}
}

func TestHandler_RejectsOutOfList(t *testing.T) {
	c := newFakeClient()
	if err := Register(c, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h := c.handler(tunnel.MethodRestartService)
	body, _ := json.Marshal(tunnel.RestartServiceRequest{Service: "sshd"})
	_, err := h(context.Background(), tunnel.Session{}, tunnel.MethodRestartService, body)
	if err == nil {
		t.Fatalf("expected allow-list rejection")
	}
	if !strings.Contains(err.Error(), "allow-list") {
		t.Errorf("error should mention allow-list: %v", err)
	}
}

func TestHandler_RejectsEmpty(t *testing.T) {
	c := newFakeClient()
	if err := Register(c, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h := c.handler(tunnel.MethodRestartService)
	_, err := h(context.Background(), tunnel.Session{}, tunnel.MethodRestartService, []byte(`{"service":""}`))
	if err == nil {
		t.Errorf("empty service should error")
	}
}

// fakeSystemctl writes a script that stands in for systemctl, records the
// vector it was handed, and exits with a chosen code.
//
// It is a script rather than an injected function so that the thing under
// test is the real one: exec.CommandContext, argv as separate words, no
// shell. A stubbed runner would prove the argument list the code *meant* to
// build, which is not the same claim as "this is what reached the process".
func fakeSystemctl(t *testing.T, exitCode int, stderr string) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "argv.txt")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(argsFile) + "\n" +
		"if [ -n " + shellQuote(stderr) + " ]; then echo " + shellQuote(stderr) + " >&2; fi\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	bin = filepath.Join(dir, "systemctl")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake systemctl: %v", err)
	}
	return bin, argsFile
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func realSandbox(bin string) *SandboxConfig {
	sb := DefaultSandboxConfig()
	sb.Mocked = false
	sb.SystemctlPath = bin
	return sb
}

func invoke(t *testing.T, sb *SandboxConfig, req tunnel.RestartServiceRequest) (tunnel.RestartServiceResponse, error) {
	t.Helper()
	c := newFakeClient()
	c.RegisterHandler(tunnel.MethodRestartService, makeRestartHandler(sb, nil))
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	raw, err := c.handler(tunnel.MethodRestartService)(context.Background(), tunnel.Session{}, tunnel.MethodRestartService, body)
	if err != nil {
		return tunnel.RestartServiceResponse{}, err
	}
	var resp tunnel.RestartServiceResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp, nil
}

func TestTheRealPathRunsExactlyTheVectorItReports(t *testing.T) {
	bin, argsFile := fakeSystemctl(t, 0, "")

	resp, err := invoke(t, realSandbox(bin), tunnel.RestartServiceRequest{Service: "nginx", Reason: "502s"})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !resp.Restarted || resp.Mocked {
		t.Fatalf("Restarted=%v Mocked=%v, want true/false on the real path", resp.Restarted, resp.Mocked)
	}
	want := []string{bin, "restart", "nginx.service"}
	if len(resp.Argv) != len(want) || resp.Argv[0] != want[0] || resp.Argv[1] != want[1] || resp.Argv[2] != want[2] {
		t.Fatalf("Argv = %v, want %v", resp.Argv, want)
	}

	// What the process actually received, which is the claim that matters:
	// the vector in the response is worth nothing if it is not the vector
	// that ran.
	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the fake systemctl was never run: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(recorded), "\n"), "\n")
	if len(got) != 2 || got[0] != "restart" || got[1] != "nginx.service" {
		t.Fatalf("systemctl received %v, want [restart nginx.service]", got)
	}
}

func TestTheCanonicalSuffixIsRestoredForSystemd(t *testing.T) {
	bin, _ := fakeSystemctl(t, 0, "")
	// "NGINX.service" canonicalises to "nginx"; the vector handed to
	// systemd has to carry the suffix back, because systemctl does not
	// guess it.
	resp, err := invoke(t, realSandbox(bin), tunnel.RestartServiceRequest{Service: "NGINX.service"})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if resp.Argv[2] != "nginx.service" {
		t.Fatalf("argv unit = %q, want nginx.service", resp.Argv[2])
	}
}

func TestAFailedRestartIsReportedWithoutLosingTheVector(t *testing.T) {
	bin, _ := fakeSystemctl(t, 3, "Job for nginx.service failed")

	resp, err := invoke(t, realSandbox(bin), tunnel.RestartServiceRequest{Service: "nginx"})
	if err != nil {
		t.Fatalf("a restart that ran and failed is an answer, not a transport fault; got %v", err)
	}
	if resp.Restarted {
		t.Errorf("Restarted=true although systemctl exited 3")
	}
	if resp.Mocked {
		t.Errorf("Mocked=true on a real run")
	}
	if len(resp.Argv) == 0 {
		t.Errorf("a failed run must still report what it ran; an error without the vector teaches nobody anything")
	}
	if !strings.Contains(resp.Error, "systemctl restart failed") {
		t.Errorf("Error = %q, want it to say the restart failed", resp.Error)
	}
	if !strings.Contains(resp.Error, "Job for nginx.service failed") {
		t.Errorf("Error = %q, want the command's own message included", resp.Error)
	}
}

func TestNothingRunsForAUnitOutsideTheAllowList(t *testing.T) {
	bin, argsFile := fakeSystemctl(t, 0, "")
	_, err := invoke(t, realSandbox(bin), tunnel.RestartServiceRequest{Service: "sshd"})
	if err == nil || !strings.Contains(err.Error(), "allow-list") {
		t.Fatalf("want an allow-list rejection, got %v", err)
	}
	if _, statErr := os.Stat(argsFile); statErr == nil {
		t.Fatalf("systemctl ran for a unit outside the allow-list")
	}
}

func TestAServiceNameCannotCarryASecondCommand(t *testing.T) {
	bin, argsFile := fakeSystemctl(t, 0, "")
	_, err := invoke(t, realSandbox(bin), tunnel.RestartServiceRequest{Service: "nginx.service; reboot"})
	if err == nil {
		t.Fatalf("a service name containing a command separator must be refused, not sanitised")
	}
	if _, statErr := os.Stat(argsFile); statErr == nil {
		t.Fatalf("a refused service name still reached the process")
	}
}

func TestRestartFailureNamesTheNodesOwnBudget(t *testing.T) {
	got := restartFailure(context.DeadlineExceeded, nil)
	if !strings.Contains(got, "10s") {
		t.Errorf("a killed systemctl must be reported as this node's budget running out, got %q", got)
	}
	withOutput := restartFailure(context.DeadlineExceeded, []byte("still shutting down"))
	if !strings.Contains(withOutput, "still shutting down") {
		t.Errorf("output from the killed process must survive into the report, got %q", withOutput)
	}
}

func TestRegisterWithRefusesANilSandbox(t *testing.T) {
	if err := RegisterWith(newFakeClient(), nil, nil); err == nil {
		t.Fatalf("a nil sandbox must be refused rather than defaulted to the shipped mock")
	}
}

func TestRegisterWithDefaultsTheSystemctlPath(t *testing.T) {
	sb := &SandboxConfig{AllowedUnits: DefaultAllowedUnits(), Mocked: true}
	if err := RegisterWith(newFakeClient(), sb, nil); err != nil {
		t.Fatalf("RegisterWith: %v", err)
	}
	if sb.SystemctlPath != "systemctl" {
		t.Fatalf("SystemctlPath = %q, want it filled in to systemctl", sb.SystemctlPath)
	}
}
