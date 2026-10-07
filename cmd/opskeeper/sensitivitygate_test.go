package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/manager/dataguard"
	iambizauthz "github.com/vincent-wuhan/opskeeper/core/manager/iam/biz/authz"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// This is the answer half of the reader-tier promise. The gate was written
// months before anything called it, and the tests below are the first thing
// that ever has.

// stubLabels answers with a fixed sensitivity.
type stubLabels struct {
	sensitivity dataguard.Sensitivity
	err         error
	asked       int
}

func (s *stubLabels) ResolveEffective(context.Context, string, string) (dataguard.Sensitivity, float64, bool, error) {
	s.asked++
	return s.sensitivity, 1, false, s.err
}

// stubAuthz answers per org.
type stubAuthz struct {
	allowed map[uint64]bool
	orgs    []uint64
	err     error
	calls   int
}

func (s *stubAuthz) AllowWithSensitivity(_ context.Context, _, orgID uint64, _, _ string,
	_ dataguard.Sensitivity, _ iambizauthz.SensitivityTierRepo) (bool, error) {
	s.calls++
	if s.err != nil {
		return false, s.err
	}
	return s.allowed[orgID], nil
}

func (s *stubAuthz) UserOrgs(context.Context, uint64) ([]uint64, error) {
	return s.orgs, nil
}

func userCtx(id uint64) context.Context {
	return tenantctx.With(context.Background(), tenantctx.Tenant{UserID: id, Role: "user"})
}

// An unlabeled resource needs no tier at all, and asking the authorization
// stack about it is a database round trip that can only answer yes.
func TestAnUnlabeledResourceIsAllowedWithoutAskingIam(t *testing.T) {
	labels := &stubLabels{sensitivity: dataguard.Internal}
	authz := &stubAuthz{}
	gate := newSensitivityGate(labels, authz, nil, nil)

	if err := gate.Check(userCtx(7), "device", "db-1"); err != nil {
		t.Fatalf("an Internal resource was refused: %v", err)
	}
	if authz.calls != 0 {
		t.Errorf("iam was asked %d times about an unlabeled resource", authz.calls)
	}
}

// A labelled resource with a tier that reaches it is allowed, and asking in
// every org the caller belongs to is deliberate: membership in a second org
// must not be a downgrade.
func TestAReachedTierAllowsTheCall(t *testing.T) {
	labels := &stubLabels{sensitivity: dataguard.Confidential}
	authz := &stubAuthz{orgs: []uint64{1, 2}, allowed: map[uint64]bool{2: true}}
	gate := newSensitivityGate(labels, authz, nil, nil)

	if err := gate.Check(userCtx(7), "device", "db-1"); err != nil {
		t.Fatalf("a satisfied tier was refused: %v", err)
	}
	if authz.calls != 2 {
		t.Errorf("iam was asked %d times, want once per org until one allows", authz.calls)
	}
}

// A tier that does not reach the label is refused, and the refusal names the
// resource and the level: "forbidden" is a support ticket.
func TestAnUnreachedTierIsRefusedByName(t *testing.T) {
	labels := &stubLabels{sensitivity: dataguard.Restricted}
	authz := &stubAuthz{orgs: []uint64{1}, allowed: map[uint64]bool{}}
	gate := newSensitivityGate(labels, authz, nil, nil)

	err := gate.Check(userCtx(7), "device", "db-7")
	if err == nil {
		t.Fatal("a Restricted resource was served to an Internal reader")
	}
	if !strings.Contains(err.Error(), "device/db-7") || !strings.Contains(err.Error(), "Restricted") {
		t.Errorf("the refusal does not name the resource and the level: %s", err)
	}
}

// A lookup that fails is not an unlabeled resource. Treating a database
// outage as "no label" is an authorization downgrade with a UI on it.
func TestALookupFailureRefusesRatherThanDowngrading(t *testing.T) {
	labels := &stubLabels{err: errors.New("connection refused")}
	authz := &stubAuthz{}
	gate := newSensitivityGate(labels, authz, nil, nil)

	err := gate.Check(userCtx(7), "device", "db-1")
	if err == nil {
		t.Fatal("a failed label lookup allowed the call")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the refusal loses the cause: %s", err)
	}
	if authz.calls != 0 {
		t.Errorf("iam was consulted %d times after the label lookup failed", authz.calls)
	}
}

// Public is below every tier and needs no round trip either.
func TestPublicIsAllowed(t *testing.T) {
	labels := &stubLabels{sensitivity: dataguard.Public}
	authz := &stubAuthz{orgs: []uint64{1}}
	gate := newSensitivityGate(labels, authz, nil, nil)

	if err := gate.Check(userCtx(7), "device", "db-1"); err != nil {
		t.Fatalf("a Public resource was refused: %v", err)
	}
	if authz.calls != 0 {
		t.Errorf("iam was asked about a Public resource")
	}
}

// A call that names no resource is allowed here for the reason the port
// states: this gate's answer is about a labelled resource, and inventing a
// denial for "unidentifiable" would refuse the tools that carry no resource.
func TestACallThatNamesNoResourceIsAllowed(t *testing.T) {
	labels := &stubLabels{sensitivity: dataguard.TopSecret}
	authz := &stubAuthz{}
	gate := newSensitivityGate(labels, authz, nil, nil)

	if err := gate.Check(userCtx(7), "", ""); err != nil {
		t.Fatalf("a resource-less call was refused: %v", err)
	}
	if labels.asked != 0 {
		t.Errorf("the label store was asked about a call with no resource")
	}
}

// Not wired is not closed. A deployment without Data-Guard keeps running the
// tools it had, and the gate never pretends to be a control in that case.
func TestAnUnwiredGateAllowsEverything(t *testing.T) {
	if gate := newSensitivityGate(nil, nil, nil, nil); gate != nil {
		t.Error("a gate was built with nothing to ask")
	}
	gate := &sensitivityGate{}
	if err := gate.Check(userCtx(7), "device", "db-1"); err != nil {
		t.Errorf("a half-built gate refused a call: %v", err)
	}
}

// A caller with no human identity is not gated. Service identities have no
// tier row and cannot satisfy one; they are governed by their token's tool
// allowlist instead.
func TestAServiceIdentityIsNotGated(t *testing.T) {
	labels := &stubLabels{sensitivity: dataguard.TopSecret}
	authz := &stubAuthz{orgs: []uint64{1}}
	gate := newSensitivityGate(labels, authz, nil, nil)

	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{
		Role:       "worker",
		AgentTeams: &tenantctx.AgentTeamsIdentity{TenantID: "acme", Worker: "opskeeper-sre"},
	})
	if err := gate.Check(ctx, "device", "db-1"); err != nil {
		t.Errorf("a service identity was refused by a reader tier: %v", err)
	}
	if authz.calls != 0 {
		t.Errorf("iam was asked %d times for a service identity", authz.calls)
	}
}

// An org lookup that fails is a system error, not an authorization answer.
func TestAnOrgLookupFailureRefuses(t *testing.T) {
	labels := &stubLabels{sensitivity: dataguard.Confidential}
	gate := newSensitivityGate(labels, &failingOrgs{}, nil, nil)
	if err := gate.Check(userCtx(7), "device", "db-1"); err == nil {
		t.Fatal("a failed org lookup allowed the call")
	}
}

type failingOrgs struct{}

func (failingOrgs) AllowWithSensitivity(context.Context, uint64, uint64, string, string,
	dataguard.Sensitivity, iambizauthz.SensitivityTierRepo) (bool, error) {
	return false, errors.New("never reached")
}

func (failingOrgs) UserOrgs(context.Context, uint64) ([]uint64, error) {
	return nil, errors.New("membership store is down")
}

// perDeviceLabels answers each id with its own level, so a set can be given
// a strictest member the caller did not put first.
type perDeviceLabels struct {
	levels map[string]dataguard.Sensitivity
	fail   string
	asked  int
}

func (l *perDeviceLabels) ResolveEffective(_ context.Context, _, id string) (dataguard.Sensitivity, float64, bool, error) {
	l.asked++
	if l.fail != "" && id == l.fail {
		return "", 0, false, errors.New("label store unreachable")
	}
	level, ok := l.levels[id]
	if !ok {
		return "", 0, false, nil
	}
	return level, 1, false, nil
}

// orgCountingAuthz notes how often the caller's orgs were resolved and which
// object the last authorization was asked about.
type orgCountingAuthz struct {
	orgCalls int
	lastObj  string
	lastSens dataguard.Sensitivity
	allow    bool
}

// AllowWithSensitivity notes what it was asked. The level is the part that
// carries the decision: the enforcer is told the object's type, not its id,
// so the tier table is what "may a caller at this tier read this level"
// actually turns on.
func (a *orgCountingAuthz) AllowWithSensitivity(_ context.Context, _, _ uint64, obj, _ string,
	sens dataguard.Sensitivity, _ iambizauthz.SensitivityTierRepo) (bool, error) {
	a.lastObj, a.lastSens = obj, sens
	return a.allow, nil
}

func (a *orgCountingAuthz) UserOrgs(context.Context, uint64) ([]uint64, error) {
	a.orgCalls++
	return []uint64{1}, nil
}

// A deployment that has no Data-Guard has a gate object with nothing in it,
// and it must behave like the gate it was before this file existed. The case
// is reachable without going through the constructor, because the wiring
// stores the result in an interface field that any holder can fill.
func TestAnUnwiredGateObjectAllowsEverything(t *testing.T) {
	var gate ports.SensitivityGate = &sensitivityGate{}
	if err := gate.Check(userCtx(7), "device", "db-7"); err != nil {
		t.Fatalf("an unwired gate refused: %v", err)
	}
	if err := batch(t, gate).CheckAll(userCtx(7), "device", []string{"db-7", "db-8"}); err != nil {
		t.Fatalf("an unwired gate refused a set: %v", err)
	}
}

// A list of devices is authorized by the strictest label in it, so the
// sensitive one cannot be hidden by its position in the caller's list, and
// the caller is resolved once rather than once per device.
func TestAListIsAuthorizedByItsStrictestLabel(t *testing.T) {
	labels := &perDeviceLabels{levels: map[string]dataguard.Sensitivity{
		"db-1": dataguard.Internal,
		"db-2": dataguard.Restricted,
		"db-3": dataguard.Internal,
	}}
	authz := &orgCountingAuthz{allow: false}
	gate := newSensitivityGate(labels, authz, nil, nil)

	if err := batch(t, gate).CheckAll(userCtx(7), "device", []string{"db-1", "db-2", "db-3"}); err == nil {
		t.Fatal("a set holding a Restricted device was allowed to a caller without the tier")
	}
	if authz.orgCalls != 1 {
		t.Errorf("the caller's orgs were resolved %d times, want once for the set", authz.orgCalls)
	}
	if authz.lastSens != dataguard.Restricted {
		t.Errorf("the set was decided at level %q, want the strictest member's Restricted", authz.lastSens)
	}
}

// A set whose lookup fails is refused, not partially authorized: half a list
// is not half a permission.
func TestALookupFailureInASetRefuses(t *testing.T) {
	labels := &perDeviceLabels{
		levels: map[string]dataguard.Sensitivity{"db-1": dataguard.Restricted},
		fail:   "db-404",
	}
	gate := newSensitivityGate(labels, &orgCountingAuthz{allow: true}, nil, nil)

	if err := batch(t, gate).CheckAll(userCtx(7), "device", []string{"db-1", "db-404"}); err == nil {
		t.Fatal("a set containing an unreadable device was allowed")
	}
}

// A set of unlabeled devices needs no tier at all, so iam is never asked:
// the rule is about labels, and there are none.
func TestAnUnlabeledSetIsAllowedWithoutAskingIam(t *testing.T) {
	authz := &orgCountingAuthz{}
	gate := newSensitivityGate(&perDeviceLabels{}, authz, nil, nil)

	if err := batch(t, gate).CheckAll(userCtx(7), "device", []string{"db-1", "db-2"}); err != nil {
		t.Fatalf("an unlabeled set was refused: %v", err)
	}
	if authz.orgCalls != 0 {
		t.Errorf("iam was consulted %d times for an unlabeled set", authz.orgCalls)
	}
}

// TopSecret is the level a caller without its tier must not reach, and the
// ranking that decides a set is the dataguard module's own rather than a
// second table written here.
func TestTopSecretOutranksRestrictedWhenDecidingASet(t *testing.T) {
	labels := &perDeviceLabels{levels: map[string]dataguard.Sensitivity{
		"db-1": dataguard.Restricted,
		"db-2": dataguard.TopSecret,
	}}
	authz := &orgCountingAuthz{allow: false}
	gate := newSensitivityGate(labels, authz, nil, nil)

	if err := batch(t, gate).CheckAll(userCtx(7), "device", []string{"db-1", "db-2"}); err == nil {
		t.Fatal("a set holding a TopSecret device was allowed")
	}
	if authz.lastSens != dataguard.TopSecret {
		t.Errorf("the set was decided at level %q, want the TopSecret member's level", authz.lastSens)
	}
}

// batch asserts that the assembled gate really does answer sets, so these
// tests fail if the wiring ever falls back to a single-resource gate that
// would decide a list by asking about it one at a time.
func batch(t *testing.T, gate ports.SensitivityGate) ports.MultiResourceGate {
	t.Helper()
	multi, ok := gate.(ports.MultiResourceGate)
	if !ok {
		t.Fatalf("the assembled gate does not answer sets: %T", gate)
	}
	return multi
}
