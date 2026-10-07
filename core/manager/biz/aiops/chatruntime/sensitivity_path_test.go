package chatruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
)

// identityGate records the caller it was asked on behalf of.
//
// This is the test that decision 360 said could not be written: it claimed the
// tool chain carried no caller identity, so there was nothing to gate on. The
// identity is there — it arrives on the context that the HTTP auth middleware
// populated — and the only question left is whether the runtime hands that
// same context to the gate rather than a fresh one.
type identityGate struct {
	asked   int
	userID  uint64
	hasTena bool
}

func (g *identityGate) Check(ctx context.Context, _, _ string) error {
	g.asked++
	if tenant, ok := tenantctx.From(ctx); ok {
		g.hasTena = true
		g.userID = tenant.UserID
	}
	return nil
}

// The gate is asked, and it is asked on behalf of the signed-in user rather
// than on behalf of nobody. A gate wired to the chain but handed a bare
// context would pass every caller and look exactly like this one from the
// outside, which is why the assertion is on the identity and not on the call.
func TestTheGateIsAskedOnBehalfOfTheSignedInUser(t *testing.T) {
	sess := &model.Session{ID: "s1", UserID: 7}
	apply := &captureApplyTool{
		resp: `{"kind":"config_apply_result","domain":"alert_rule","action":"create","status":"applied","resource_id":42,"resource":{"name":"r","type":"alert_rule"}}`,
	}
	gate := &identityGate{}
	rt, err := NewRuntime(Config{
		Sessions:      newMemSessions(sess),
		Kernel:        newScriptedKernel("should not run"),
		ToolBag:       []basetool.BaseTool{apply},
		Sensitivity:   gate,
		MaxIterations: 5,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}

	payload := `{"action":"create","draft_id":"draft-1","rule":{"rule_key":"k","name":"n","kind":"metric_raw","severity":"warning","spec":{"expr":"up > 0","for":"5m"}}}`
	userText := "确认创建这条告警规则\ndomain: alert_rule\naction: create\n" +
		"apply_tool: apply_config_change\ndraft_hash: sha256:" +
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
		"payload:\n```json\n" + payload + "\n```"

	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{UserID: 7, Role: "user"})
	if _, err := rt.Handle(ctx, &Request{
		SessionID: sess.ID, UserID: sess.UserID, Role: "user", UserText: userText,
		Emit: func(Event) {},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if gate.asked == 0 {
		t.Fatal("the tool ran without ever being offered to the gate")
	}
	if !gate.hasTena {
		t.Fatal("the gate was asked on a context with no tenant, so it could not tell who was calling")
	}
	if gate.userID != 7 {
		t.Errorf("the gate was asked on behalf of user %d, want 7", gate.userID)
	}
}

// A gate that refuses must stop the tool before it runs, on the runtime's own
// path rather than on a decorator's.
func TestARefusedCallNeverReachesTheTool(t *testing.T) {
	sess := &model.Session{ID: "s1", UserID: 7}
	apply := &captureApplyTool{resp: `{"kind":"config_apply_result","status":"applied"}`}
	rt, err := NewRuntime(Config{
		Sessions:      newMemSessions(sess),
		Kernel:        newScriptedKernel("should not run"),
		ToolBag:       []basetool.BaseTool{apply},
		Sensitivity:   &refusingGate{},
		MaxIterations: 5,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	payload := `{"action":"create","draft_id":"draft-1","rule":{"rule_key":"k","name":"n","kind":"metric_raw","severity":"warning","spec":{"expr":"up > 0","for":"5m"}}}`
	userText := "确认创建这条告警规则\ndomain: alert_rule\naction: create\n" +
		"apply_tool: apply_config_change\ndraft_hash: sha256:" +
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
		"payload:\n```json\n" + payload + "\n```"

	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{UserID: 7, Role: "user"})
	if _, err := rt.Handle(ctx, &Request{
		SessionID: sess.ID, UserID: sess.UserID, Role: "user", UserText: userText,
		Emit: func(Event) {},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if apply.calls.Load() != 0 {
		t.Fatalf("the tool ran %d times despite the gate refusing", apply.calls.Load())
	}
}

var errRefused = errors.New("reader tier does not reach it")

type refusingGate struct{}

func (*refusingGate) Check(context.Context, string, string) error {
	return errRefused
}
