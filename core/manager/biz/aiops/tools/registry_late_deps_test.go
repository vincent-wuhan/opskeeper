package tools

import (
	"context"
	"log/slog"
	"testing"

	edgebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
)

type stubLateIMSender struct{}

func (stubLateIMSender) ListIMChannels(context.Context) ([]IMChannel, error) { return nil, nil }
func (stubLateIMSender) SendIM(context.Context, uint64, string, string) error {
	return nil
}

type stubLatePageStore struct{}

func (stubLatePageStore) SavePage(context.Context, string, string) (string, string, error) {
	return "p-1", "/pages/p-1", nil
}

// TestAToolWhoseSeamIsSetLaterIsAbsentUntilItIsSet is the reason the MCP
// surface is assembled at the end of main.go's wiring rather than where its
// handler is created.
//
// Three tools are gated on seams that are set with the *approval flow*, minutes
// of wiring after the registry exists: cloud_bash on the proposer,
// send_im_message on the IM sender, serve_page on the page store. A tool list
// read before those assignments is not a list with three tools missing — it is
// a different list, and anything holding on to it (an MCP handler, a skills
// inventory, a flow invoker) keeps serving the earlier platform.
//
// host_bash is asserted on both sides of the change because it is the control:
// its gate is the tunnel triple, which exists from the start, so it must not
// move. If a future refactor makes the registry eager, this test is where that
// shows up.
func TestAToolWhoseSeamIsSetLaterIsAbsentUntilItIsSet(t *testing.T) {
	uc := edgebiz.NewUsecase(newFakeEdgeRepo(), nil, nil, slog.Default())
	dc := &fakeDevicesForToolBag{}
	reg := NewRegistry(&fakeCaller{}, uc, dc.usecase(), &fakePromQuerier{}, &fakeLogQuerier{}, &fakeTraceQuerier{}, &fakeAlertUC{}, slog.Default())

	late := []string{ToolNameCloudBash, ToolNameSendIMMessage, ToolNameServePage}
	before := toolInfoNames(t, reg.BuildBaseTools().AllTools())
	for _, name := range late {
		if containsName(before, name) {
			t.Fatalf("%q is already in a bag built before its seam is set; this test's premise (and the assembly-order comment in main.go) needs revisiting", name)
		}
	}
	if !containsName(before, ToolNameBash) {
		t.Fatalf("%q must be gated on the tunnel triple, not on a late seam: a diagnostic shell is callable as soon as the tunnel is", ToolNameBash)
	}

	reg.SetCloudBashProposer(&recProposer{})
	reg.SetIMSender(stubLateIMSender{})
	reg.SetPageStore(stubLatePageStore{})

	after := toolInfoNames(t, reg.BuildBaseTools().AllTools())
	for _, name := range late {
		if !containsName(after, name) {
			t.Fatalf("%q did not appear after its seam was set, so assembling the surface later would not have changed anything", name)
		}
	}
	if !containsName(after, ToolNameBash) {
		t.Fatalf("%q disappeared once the proposals were wired", ToolNameBash)
	}
}
