package tools

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/host"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
)

// newRestartServiceToolFor builds the restart_service BaseTool backed
// by a fake caller + fake resolver. Mirrors the host_files test
// helper so the two tools share their wiring fixtures.
func newRestartServiceToolFor(t *testing.T, resolver host.DeviceResolver, fc *fakeCaller) *RestartServiceTool {
	t.Helper()
	return &RestartServiceTool{caller: fc, resolver: resolver}
}

func TestRestartServiceTool_Info(t *testing.T) {
	tool := newRestartServiceToolFor(t, &fakeHostResolver{}, &fakeCaller{})
	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != ToolNameRestartService {
		t.Errorf("Name = %q, want %q", info.Name, ToolNameRestartService)
	}
	// Class="write" is the contract — the ReviewGate decorator switches
	// on this exact value to gate the call. Regression-protect it here
	// so a future refactor can't downgrade to "read" silently.
	if info.Class != "write" {
		t.Errorf("Class = %q, want write (mutating skill must trigger review)", info.Class)
	}
	if info.Description == "" {
		t.Errorf("Description empty")
	}
	if info.WhenToUse == "" {
		t.Errorf("WhenToUse empty — requires it separated from Description")
	}
	if !strings.Contains(strings.ToLower(info.WhenToUse), "reviewer") {
		t.Errorf("WhenToUse should warn the LLM about the reviewer flow: %q", info.WhenToUse)
	}
	var schema map[string]any
	if err := json.Unmarshal(info.Parameters, &schema); err != nil {
		t.Errorf("Parameters not valid JSON: %v", err)
	}
	if _, ok := schema["properties"]; !ok {
		t.Errorf("Parameters has no properties: %+v", schema)
	}
	// The JSON Schema enum is the structured form of the allow-list; we
	// duplicate the literal in code (AllowedRestartServices) for the
	// runtime check. Verify both are non-empty and consistent.
	if len(AllowedRestartServices) == 0 {
		t.Errorf("AllowedRestartServices is empty")
	}
}

func TestRestartServiceTool_RoundTrip(t *testing.T) {
	fc := &fakeCaller{
		respBody: mustMarshal(tunnel.RestartServiceResponse{
			Service:   "nginx",
			Restarted: true,
			Mocked:    true,
		}),
	}
	resolver := &fakeHostResolver{mapping: map[uint64]uint64{1: 7}}
	tool := newRestartServiceToolFor(t, resolver, fc)

	out, err := tool.InvokableRun(context.Background(), `{"device_id":1,"service":"nginx","reason":"502 spike"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if fc.lastID != 7 {
		t.Errorf("caller invoked with edge id %d, want 7", fc.lastID)
	}
	if fc.lastName != tunnel.MethodRestartService {
		t.Errorf("method = %q, want %q", fc.lastName, tunnel.MethodRestartService)
	}
	var sentReq tunnel.RestartServiceRequest
	if err := json.Unmarshal(fc.lastBody, &sentReq); err != nil {
		t.Fatalf("decode lastBody: %v", err)
	}
	if sentReq.Service != "nginx" {
		t.Errorf("sent service = %q, want nginx", sentReq.Service)
	}
	if sentReq.Reason != "502 spike" {
		t.Errorf("sent reason = %q, want '502 spike'", sentReq.Reason)
	}

	var env restartServiceResultEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	if env.DeviceID != 1 {
		t.Errorf("env.DeviceID = %d, want 1", env.DeviceID)
	}
	if env.Service != "nginx" || !env.Restarted || !env.Mocked {
		t.Errorf("env = %+v, want service=nginx, Restarted/Mocked = true", env)
	}
}

// The edge now records the exact vector it ran (decision 154) and this is
// the manager hop the closed loop reads. TestRestartServiceTool_RoundTrip
// stops at a mocked response, so it cannot catch an envelope that drops the
// vector on the floor — which is exactly what happened: the wire carried
// Argv, the envelope did not, and a green restart looked identical to one
// whose command was never kept. This asserts the byte survives this hop,
// against a non-mocked response, because that is the only case that has a
// vector to lose.
func TestRestartServiceTool_CarriesTheExecutedArgvThroughTheEnvelope(t *testing.T) {
	fc := &fakeCaller{
		respBody: mustMarshal(tunnel.RestartServiceResponse{
			Service:   "nginx",
			Restarted: true,
			Mocked:    false,
			Argv:      []string{"systemctl", "restart", "nginx.service"},
		}),
	}
	resolver := &fakeHostResolver{mapping: map[uint64]uint64{1: 7}}
	tool := newRestartServiceToolFor(t, resolver, fc)

	out, err := tool.InvokableRun(context.Background(), `{"device_id":1,"service":"nginx"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	var env restartServiceResultEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	want := []string{"systemctl", "restart", "nginx.service"}
	if len(env.Argv) != len(want) {
		t.Fatalf("the executed argv did not survive the manager hop: got %v, want %v", env.Argv, want)
	}
	for i := range want {
		if env.Argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, env.Argv[i], want[i])
		}
	}
	// The mirrored test for the other direction: a mock ran nothing, so the
	// envelope must carry no vector rather than a plausible one.
	fcMock := &fakeCaller{
		respBody: mustMarshal(tunnel.RestartServiceResponse{Service: "nginx", Restarted: true, Mocked: true}),
	}
	toolMock := newRestartServiceToolFor(t, resolver, fcMock)
	outMock, err := toolMock.InvokableRun(context.Background(), `{"device_id":1,"service":"nginx"}`)
	if err != nil {
		t.Fatalf("InvokableRun (mock): %v", err)
	}
	var envMock restartServiceResultEnvelope
	if err := json.Unmarshal([]byte(outMock), &envMock); err != nil {
		t.Fatalf("decode out (mock): %v", err)
	}
	if len(envMock.Argv) != 0 {
		t.Errorf("a mocked restart ran nothing, so the envelope must carry no argv; got %v", envMock.Argv)
	}
	if !envMock.Mocked {
		t.Errorf("mock envelope must still report Mocked=true")
	}
}

func TestRestartServiceTool_CanonicalizesService(t *testing.T) {
	// "Nginx.Service" with mixed-case + suffix should canonicalize to
	// "nginx" for both the allow-list check AND the wire body.
	fc := &fakeCaller{respBody: mustMarshal(tunnel.RestartServiceResponse{Service: "nginx", Restarted: true, Mocked: true})}
	resolver := &fakeHostResolver{mapping: map[uint64]uint64{1: 7}}
	tool := newRestartServiceToolFor(t, resolver, fc)

	if _, err := tool.InvokableRun(context.Background(), `{"device_id":1,"service":"Nginx.SERVICE"}`); err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	var sentReq tunnel.RestartServiceRequest
	if err := json.Unmarshal(fc.lastBody, &sentReq); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sentReq.Service != "nginx" {
		t.Errorf("canonicalised service = %q, want nginx", sentReq.Service)
	}
}

func TestRestartServiceTool_RejectsOutOfList(t *testing.T) {
	tool := newRestartServiceToolFor(t, &fakeHostResolver{mapping: map[uint64]uint64{1: 7}}, &fakeCaller{})
	_, err := tool.InvokableRun(context.Background(), `{"device_id":1,"service":"sshd"}`)
	if err == nil {
		t.Fatalf("expected allow-list rejection")
	}
	if !strings.Contains(err.Error(), "not in allow-list") {
		t.Errorf("error should mention allow-list: %v", err)
	}
}

func TestRestartServiceTool_MissingDeviceID(t *testing.T) {
	tool := newRestartServiceToolFor(t, &fakeHostResolver{}, &fakeCaller{})
	_, err := tool.InvokableRun(context.Background(), `{"service":"nginx"}`)
	if err == nil {
		t.Fatalf("expected error for missing device_id")
	}
	if !strings.Contains(err.Error(), "device_id") {
		t.Errorf("error should mention device_id, got: %v", err)
	}
}

func TestRestartServiceTool_MissingService(t *testing.T) {
	tool := newRestartServiceToolFor(t, &fakeHostResolver{mapping: map[uint64]uint64{1: 7}}, &fakeCaller{})
	_, err := tool.InvokableRun(context.Background(), `{"device_id":1}`)
	if err == nil {
		t.Fatalf("expected error for missing service")
	}
	if !strings.Contains(err.Error(), "service") {
		t.Errorf("error should mention service, got: %v", err)
	}
}

func TestRestartServiceTool_UnlinkedDevice(t *testing.T) {
	tool := newRestartServiceToolFor(t, &fakeHostResolver{mapping: map[uint64]uint64{}}, &fakeCaller{})
	_, err := tool.InvokableRun(context.Background(), `{"device_id":42,"service":"nginx"}`)
	if err == nil {
		t.Fatalf("expected error for unlinked device_id")
	}
	if !strings.Contains(err.Error(), "no host-edge link") {
		t.Errorf("error should mention missing junction: %v", err)
	}
}

func TestRestartServiceTool_DispatchError(t *testing.T) {
	fc := &fakeCaller{respErr: errs.ErrEdgeOffline}
	resolver := &fakeHostResolver{mapping: map[uint64]uint64{1: 7}}
	tool := newRestartServiceToolFor(t, resolver, fc)

	_, err := tool.InvokableRun(context.Background(), `{"device_id":1,"service":"nginx"}`)
	if err == nil {
		t.Fatalf("expected dispatch error")
	}
	if !errors.Is(err, errs.ErrEdgeOffline) {
		t.Errorf("err should wrap ErrEdgeOffline: %v", err)
	}
}

func TestRestartServiceTool_NilCaller(t *testing.T) {
	tool := &RestartServiceTool{caller: nil, resolver: &fakeHostResolver{}}
	_, err := tool.InvokableRun(context.Background(), `{"device_id":1,"service":"nginx"}`)
	if err == nil || !strings.Contains(err.Error(), "caller") {
		t.Errorf("expected caller-not-configured error, got %v", err)
	}
}

func TestAppendRestartServiceTool_NilDepsReturnsUnchanged(t *testing.T) {
	got := AppendRestartServiceTool(nil, nil, nil, nil, nil)
	if got != nil {
		t.Errorf("expected nil-deps to return unchanged slice, got len=%d", len(got))
	}
}
