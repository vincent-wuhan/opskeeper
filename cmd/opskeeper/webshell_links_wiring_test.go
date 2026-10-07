package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	managerbizdevice "github.com/vincent-wuhan/opskeeper/core/manager/biz/device"
	managerdevicedata "github.com/vincent-wuhan/opskeeper/core/manager/data/device/store"
	devicemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/device"
	managerwebshellserver "github.com/vincent-wuhan/opskeeper/core/manager/server/webshell"
)

// This file is the boot-path guard for the `webshell -> device` cut.
//
// The cut itself lives in two other files and neither of them can see the
// wiring: server/webshell narrowed its port, and biz/device already had the
// two-argument question. What decided the cut was in main.go, and what it
// changed was invisible from both sides.
//
// Measured before the cut: NewHandler was being handed
// managerdevicedata.NewEdgeDeviceRepo(db) — the GORM store — as its
// DeviceLinks. The handler therefore talked to the store directly. The biz
// layer was in main's dependency graph and in nobody else's, which means:
//
//   - the relation type was chosen by the HTTP handler, not by the domain
//     that owns the enum. The handler could have asked for `Discovered` and
//     nothing in the device domain would have objected.
//   - a wiring mistake showed up as a nil dereference on the first shell
//     rather than as the ErrNotWiredYet the usecase returns.
//
// So the port is now two arguments wide and only the usecase can fill it:
// *store.EdgeDeviceRepo's method takes the relation, so the store no longer
// satisfies the interface at all. That is a compile-time fact and it is the
// strongest guard available, but it is invisible — a reader of main.go sees
// one argument change and cannot tell what stopped being possible. These two
// tests say it out loud, and the second one proves the chain works with the
// real producer at one end and the real consumer at the other.

// TestOnlyTheUsecaseCanFillTheWebshellDevicePort states the structural
// consequence in both directions, because each half fails differently.
//
// The positive half is what the wiring at main.go does. The negative half is
// the one that matters: it is the assertion that will go red if the port is
// ever widened back to three parameters, and its message is the answer to
// "what did that arity buy us".
func TestOnlyTheUsecaseCanFillTheWebshellDevicePort(t *testing.T) {
	// Positive: the wiring main.go performs must be a legal one.
	var _ managerwebshellserver.DeviceLinks = (*managerbizdevice.Usecase)(nil)

	// Negative: the store must not be able to satisfy it.
	//
	// This half is honest bookkeeping rather than a guard, and it is worth
	// saying why, because a mutation round tried to make it red and could
	// not. Any state in which the store satisfies this port is a state in
	// which the usecase does not — the port would be three parameters wide
	// and the usecase's method is two — so `go build` rejects the wiring in
	// main.go first, with the same sentence this assertion would have
	// written. The assertion can therefore only go red in a world the
	// compiler has already refused to build.
	//
	// It stays because it is the shortest statement of what the arity bought,
	// and because a reader who wants to know why the store cannot be passed
	// here should not have to reconstruct it from a compiler error. The
	// enforceable half of the same claim is the test below.
	if _, ok := any((*managerdevicedata.EdgeDeviceRepo)(nil)).(managerwebshellserver.DeviceLinks); ok {
		t.Error("*store.EdgeDeviceRepo satisfies webshell.DeviceLinks again. The port has " +
			"been widened to take a relation type, which means which edge owns a device " +
			"is once more a choice the HTTP handler makes instead of a fact the device " +
			"domain states — and main.go can be handed the store again without anyone " +
			"noticing that the biz layer has been bypassed")
	}
}

// junction is a junction store that answers one question and records how it
// was asked. It implements the whole device.EdgeDeviceRepo so that the real
// usecase can be constructed over it; the four methods this test never calls
// exist only to keep the interface satisfied, and they fail loudly rather
// than returning a plausible zero.
type junction struct {
	managerbizdevice.EdgeDeviceRepo

	askedEdgeID uint64
	askedType   devicemodel.EdgeDeviceRelationType
	calls       int
}

func (j *junction) LookupEdgeForDevice(_ context.Context, deviceID uint64, t devicemodel.EdgeDeviceRelationType) (uint64, error) {
	j.calls++
	j.askedType = t
	j.askedEdgeID = deviceID
	if deviceID != 77 {
		return 0, context.Canceled // a sentinel the test never expects to see
	}
	return 4321, nil
}

func (j *junction) Link(context.Context, uint64, uint64, devicemodel.EdgeDeviceRelationType) error {
	panic("Link is not on the webshell path; a shell does not register a junction")
}

func (j *junction) Unlink(context.Context, uint64, uint64, devicemodel.EdgeDeviceRelationType) error {
	panic("Unlink is not on the webshell path")
}

// offlineEdge answers the presence question with "not answering". That makes
// resolveEdge fail, which is the point: it stops the request at the lookup
// under test and returns before the handler reaches the session router, the
// SSH client or the websocket upgrade — none of which this test wants to
// stand up.
//
// It is a second boot-path guard, added with decision 251: main.go used to
// hand this handler the edge GORM store, and the store answers a different
// question than the port now asks. `*store.Repo` cannot satisfy
// domain.EdgeStatusQuery at all, so the argument that would restore the old
// wiring no longer compiles — the same compile-time guard the device
// argument got in decision 248, and for the same reason.
type offlineEdge struct{ calls int }

func (o *offlineEdge) PresenceStatus(_ context.Context, _ uint64) (string, error) {
	o.calls++
	return domain.EdgeStatusOffline, nil
}

// TestTheShellAsksTheDeviceDomainWhichEdgeItBelongsTo is the seam with no
// witness turned into a seam with one: the real usecase, over a store that
// records the question, handed to the real handler, driven by a real request
// through the real route.
//
// The assertion is not the 503. The 503 is only proof the request got far
// enough to be refused. The assertion is the recorded relation: it is Host,
// and the handler is not the thing that chose it.
func TestTheShellAsksTheDeviceDomainWhichEdgeItBelongsTo(t *testing.T) {
	store := &junction{}
	edges := &offlineEdge{}
	// nil repo: LookupEdgeForDevice does not touch it, and passing nil is
	// the honest way to say so rather than building a device store to
	// satisfy a constructor.
	uc := managerbizdevice.NewUsecase(nil, store, nil)

	handler := managerwebshellserver.NewHandler(nil, nil, nil, uc, edges, nil)

	router := chi.NewRouter()
	handler.Register(router)

	req := httptest.NewRequest(http.MethodGet, "/v1/devices/77/shell", nil)
	req = req.WithContext(tenantctx.With(req.Context(), tenantctx.Tenant{UserID: 1}))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("the shell route answered %d, want 503. An offline edge must not produce a "+
			"session, and if this is not 503 then the request never reached resolveEdge, "+
			"which means nothing below is measuring the lookup", rec.Code)
	}
	if store.calls != 1 {
		t.Fatalf("the junction was asked %d times, want 1: the device-to-edge relation is a "+
			"single row read", store.calls)
	}
	if store.askedEdgeID != 77 {
		t.Errorf("the junction was asked about device %d, want 77 — the id out of the route",
			store.askedEdgeID)
	}
	if store.askedType != devicemodel.EdgeDeviceRelationHost {
		t.Errorf("the relation chosen was %d, want %d. The handler passes no relation any "+
			"more, so this value is the device domain's answer to \"which edge owns this "+
			"device\" — and it is the whole point of the cut that it is Host for a reason "+
			"that lives on the other side of the boundary",
			store.askedType, devicemodel.EdgeDeviceRelationHost)
	}
	if edges.calls != 1 {
		t.Errorf("the edge status was read %d times, want 1", edges.calls)
	}
}
