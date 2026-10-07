package main

import (
	"context"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/policygate"
	"github.com/vincent-wuhan/opskeeper/core/edge/toolbroker"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
	// The executors have to be registered for skill.Get to find them.
	_ "github.com/vincent-wuhan/opskeeper/core/floor/skill/builtin"
)

// manifestOf builds a governance manifest declaring exactly the named
// tools at exactly the named classes, which is all the allow-list is made
// of.
func manifestOf(plugin string, tools ...domain.ToolDecl) []domain.PluginManifest {
	return []domain.PluginManifest{{
		Metadata: domain.PluginMeta{Name: plugin},
		Spec:     domain.PluginSpec{Tools: tools},
	}}
}

// fakeReceipts stands in for the gate's grant store, so the authoriser
// can be tested without a real approval queue.
type fakeReceipts struct {
	granted map[string]bool
	claims  int
	// seen is the last call handed to the gate. An approval card is
	// rendered from this struct, so a column nobody fills is a card that
	// cannot say what it is asking permission for — and the gate is the
	// only place that knows the arguments were ever there.
	seen policygate.Call
}

func (f *fakeReceipts) ClaimReceipt(c policygate.Call) bool {
	f.claims++
	f.seen = c
	if f.granted == nil {
		return false
	}
	// Consumed on use, exactly as the real one is: a grant that could be
	// read twice would authorise the same action over and over.
	key := c.SessionID + "/" + c.ToolName + "/" + string(c.Arguments)
	if !f.granted[key] {
		return false
	}
	delete(f.granted, key)
	return true
}

// grant marks one exact call as approved.
func (f *fakeReceipts) grant(sessionID, tool string, args []byte) *fakeReceipts {
	if f.granted == nil {
		f.granted = map[string]bool{}
	}
	f.granted[sessionID+"/"+tool+"/"+string(args)] = true
	return f
}

// permit calls the authoriser the way the broker does.
func permit(auth toolbroker.Authorizer, role, session, tool string, args []byte) (bool, string) {
	return auth(context.Background(), toolbroker.Call{
		SessionID: session,
		ToolName:  tool,
		Arguments: args,
		Actor:     role,
	})
}

func registryOf(t *testing.T, manifests ...domain.PluginManifest) *policygate.Registry {
	t.Helper()
	r, err := policygate.RegistryFromManifests(manifests)
	if err != nil {
		t.Fatalf("RegistryFromManifests: %v", err)
	}
	return r
}

func TestTheBrokerPermitsAToolTheManifestDeclaresAsRead(t *testing.T) {
	auth := toolAuthorizer(registryOf(t,
		manifestOf("p", domain.ToolDecl{Name: "host_dmesg", Class: domain.ClassRead})...), &fakeReceipts{}, nil)

	for _, role := range []string{RoleAdmin, RoleOperator, RoleViewer, "", "nonsense"} {
		permitted, reason := permit(auth, role, "s", "host_dmesg", []byte(`{}`))
		if !permitted {
			t.Errorf("role %q: %s", role, reason)
		}
	}
}

func TestTheBrokerRefusesAToolNoManifestDeclares(t *testing.T) {
	// This is the check a package cannot get past by shipping an
	// undeclared tool: the model can be told it exists, and it is still
	// refused at the only place that matters.
	auth := toolAuthorizer(registryOf(t,
		manifestOf("p", domain.ToolDecl{Name: "host_dmesg", Class: domain.ClassRead})...), &fakeReceipts{}, nil)

	permitted, reason := permit(auth, RoleAdmin, "s", "host_reboot", []byte(`{}`))
	if permitted {
		t.Fatal("a tool no manifest declares was permitted")
	}
	if !strings.Contains(reason, "not in this node's tool set") {
		t.Errorf("reason = %q, want the allow-list refusal", reason)
	}
}

// A package that declares host_restart_service as read has lied at install
// time. The node holds the real executor, so it knows the truth, and the
// whole point of asking the gate with that truth is that a human reading
// YAML is not the only thing standing between a manifest and a restart.
func TestTheBrokerRefusesAToolWhoseRealClassIsWorseThanTheManifestClaims(t *testing.T) {
	if _, ok := skill.Get("host_restart_service"); !ok {
		t.Skip("host_restart_service is not registered in this build")
	}
	auth := toolAuthorizer(registryOf(t,
		manifestOf("liar", domain.ToolDecl{Name: "host_restart_service", Class: domain.ClassRead})...), &fakeReceipts{}, nil)

	for _, role := range []string{RoleAdmin, RoleOperator, RoleViewer} {
		permitted, reason := permit(auth, role, "s", "host_restart_service", []byte(`{}`))
		if permitted {
			t.Errorf("role %q ran a tool its package under-declared", role)
			continue
		}
		if !strings.Contains(reason, "host_restart_service") {
			t.Errorf("role %q: reason = %q, want it to name the offending tool", role, reason)
		}
	}
}

func TestTheBrokerAppliesTheRoleCeilingToToolsItCannotCrossCheck(t *testing.T) {
	// A control-plane tool has no local executor, so the broker has no
	// independent class for it and the manifest's word stands. The role
	// ceiling is therefore the only thing narrowing it, and it has to
	// narrow it for the role the host resolved.
	auth := toolAuthorizer(registryOf(t,
		manifestOf("p",
			domain.ToolDecl{Name: "get_topology", Class: domain.ClassRead},
			domain.ToolDecl{Name: "draft_config_change", Class: domain.ClassWrite},
		)...), &fakeReceipts{}, nil)

	if permitted, reason := permit(auth, RoleViewer, "s", "get_topology", []byte(`{}`)); !permitted {
		t.Errorf("a viewer was refused a read tool: %s", reason)
	}
	if permitted, _ := permit(auth, RoleViewer, "s", "draft_config_change", []byte(`{}`)); permitted {
		t.Error("a viewer ran a write tool")
	}

	// The write tool reaches the receipt check for the two roles allowed to
	// run it at all. That is the point: the ceiling and the approval are
	// different questions, and passing the first must not settle the
	// second.
	receipts := &fakeReceipts{}
	auth = toolAuthorizer(registryOf(t,
		manifestOf("p", domain.ToolDecl{Name: "draft_config_change", Class: domain.ClassWrite})...), receipts, nil)

	if permitted, reason := permit(auth, RoleOperator, "s", "draft_config_change", []byte(`{}`)); permitted {
		t.Error("a write tool ran with no approval behind it")
	} else if !strings.Contains(reason, "approval") {
		t.Errorf("reason = %q, want it to name the missing approval", reason)
	}
	if permitted, reason := permit(auth, RoleAdmin, "s", "draft_config_change", []byte(`{}`)); permitted {
		t.Error("a write tool ran for an admin with no approval behind it")
	} else if !strings.Contains(reason, "approval") {
		t.Errorf("reason = %q, want it to name the missing approval", reason)
	}
}

// The hole this closes: a package that replaced the courier extension
// would silence the gate entirely, and a mutating tool would then reach
// the broker with nobody asked. The broker demands the gate's receipt, so
// the only way a write tool runs is a human having actually granted it.
func TestAWriteToolRunsOnlyOnceAHumanHasGrantedThatExactCall(t *testing.T) {
	auth := toolAuthorizer(registryOf(t,
		manifestOf("repair", domain.ToolDecl{Name: "host_restart_service", Class: domain.ClassWrite})...),
		&fakeReceipts{}, nil)

	// Nobody has been asked, so there is nothing to claim.
	if permitted, reason := permit(auth, RoleAdmin, "sess-1", "host_restart_service", []byte(`{"service":"orders-api"}`)); permitted {
		t.Fatal("a mutating tool ran with no approval behind it")
	} else if !strings.Contains(reason, "approval") {
		t.Errorf("reason = %q, want it to name the missing approval", reason)
	}

	// A human answers yes to that exact call.
	receipts := &fakeReceipts{}
	auth = toolAuthorizer(registryOf(t,
		manifestOf("repair", domain.ToolDecl{Name: "host_restart_service", Class: domain.ClassWrite})...),
		receipts, nil)
	receipts.grant("sess-1", "host_restart_service", []byte(`{"service":"orders-api"}`))

	if permitted, reason := permit(auth, RoleAdmin, "sess-1", "host_restart_service", []byte(`{"service":"orders-api"}`)); !permitted {
		t.Errorf("a granted call was refused: %s", reason)
	}
	// And the grant is spent: a second identical call has no approval
	// behind it, because one click is one execution.
	if permitted, _ := permit(auth, RoleAdmin, "sess-1", "host_restart_service", []byte(`{"service":"orders-api"}`)); permitted {
		t.Error("one approval executed the same call twice")
	}
	// A different call, in the same conversation, is still unapproved.
	if permitted, _ := permit(auth, RoleAdmin, "sess-1", "host_restart_service", []byte(`{"service":"payments"}`)); permitted {
		t.Error("an approval for one service restarted another")
	}
}

func TestANodeWithWriteToolsAndNoGateRefusesThemAll(t *testing.T) {
	// A gate is where a human's consent comes from. A node that has write
	// tools and no gate has no way to obtain one, so running them would be
	// running them with nobody asked — which is the one answer that is
	// certainly wrong.
	auth := toolAuthorizer(registryOf(t,
		manifestOf("repair", domain.ToolDecl{Name: "host_restart_service", Class: domain.ClassWrite})...),
		nil, nil)

	for _, role := range []string{RoleAdmin, RoleOperator, RoleViewer} {
		if permitted, _ := permit(auth, role, "s", "host_restart_service", []byte(`{}`)); permitted {
			t.Errorf("role %q ran a write tool on a node with no gate", role)
		}
	}
	// A read is unaffected: nobody needs to be asked about a read.
	readAuth := toolAuthorizer(registryOf(t,
		manifestOf("p", domain.ToolDecl{Name: "host_dmesg", Class: domain.ClassRead})...), nil, nil)
	if permitted, reason := permit(readAuth, RoleViewer, "s", "host_dmesg", []byte(`{}`)); !permitted {
		t.Errorf("a read was refused on a node with no gate: %s", reason)
	}
}

func TestAnUnrecognisedRoleGetsTheBottomOfTheLadder(t *testing.T) {
	// The ceiling is what stops a mutating call. A role the host cannot
	// read has to land at the bottom, or a typo in a role name hands out
	// the top of the ladder to whoever typed it.
	auth := toolAuthorizer(registryOf(t,
		manifestOf("p", domain.ToolDecl{Name: "draft_config_change", Class: domain.ClassWrite})...), &fakeReceipts{}, nil)

	for _, role := range []string{"", "root", "Admin", "ADMIN", "superuser", "system"} {
		if permitted, _ := permit(auth, role, "s", "draft_config_change", []byte(`{}`)); permitted {
			t.Errorf("role %q was treated as privileged", role)
		}
	}
}

func TestASkillClassTheHostHasNotLearnedToReadIsTreatedAsTheWorst(t *testing.T) {
	// A skill author who adds a class the host does not know gets the
	// strictest reading, not a default that happens to be permissive.
	if got := classOfSkill(skill.Class("brand-new")); got != domain.ClassDestructive {
		t.Errorf("classOfSkill(unknown) = %q, want destructive", got)
	}
	// And the three known classes map the conservative way.
	for in, want := range map[skill.Class]domain.ToolClass{
		skill.ClassSafe:      domain.ClassRead,
		skill.ClassMutating:  domain.ClassWrite,
		skill.ClassDangerous: domain.ClassDestructive,
	} {
		if got := classOfSkill(in); got != want {
			t.Errorf("classOfSkill(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRoleCeilingPutsUnknownRolesAtTheBottom(t *testing.T) {
	// Stated separately from the authoriser test so the ladder itself is
	// pinned: everything else is a consequence of this function.
	if got := roleCeiling(RoleAdmin); got != domain.ClassDestructive {
		t.Errorf("admin ceiling = %q, want destructive", got)
	}
	if got := roleCeiling(RoleOperator); got != domain.ClassWrite {
		t.Errorf("operator ceiling = %q, want write", got)
	}
	for _, role := range []string{RoleViewer, "", "nope"} {
		if got := roleCeiling(role); got != domain.ClassRead {
			t.Errorf("role %q ceiling = %q, want read", role, got)
		}
	}
}

// TestTheNodeDerivesWhatTheCallTouches: the in-package tool path used to hand
// the gate a call with no Target and no Summary, and the gate passes both
// straight into the approval request it emits. So an operator was asked to
// approve a card that named no resource — and the ledger entry fell back to
// naming the call by its tool name, because targetOf had nothing else.
//
// The derivation itself is not this file's business: the control plane's
// kernel and the packaged gate courier both call wire.ToolSummary /
// wire.ToolTarget, which is what stops the same call from being described
// three ways. What this test pins is that the node's own path calls it too.
func TestTheNodeDerivesWhatTheCallTouches(t *testing.T) {
	receipts := &fakeReceipts{}
	auth := toolAuthorizer(registryOf(t,
		manifestOf("repair", domain.ToolDecl{Name: "host_restart_service", Class: domain.ClassWrite})...),
		receipts, nil)

	permit(auth, RoleAdmin, "sess-1", "host_restart_service", []byte(`{"service":"orders-api"}`))

	if receipts.seen.Target != "orders-api" {
		t.Errorf("the gate was handed target %q, want the service the arguments name — an "+
			"approval card with no target is a card nobody can judge", receipts.seen.Target)
	}
	if want := "host_restart_service on orders-api"; receipts.seen.Summary != want {
		t.Errorf("summary = %q, want %q", receipts.seen.Summary, want)
	}
}

// A call whose arguments name no resource still gets a summary: the tool's
// own name is the one part that is always true, and an empty card is worse
// than a bare one.
func TestACallWithNoResourceStillSaysWhatItIs(t *testing.T) {
	receipts := &fakeReceipts{}
	auth := toolAuthorizer(registryOf(t,
		manifestOf("repair", domain.ToolDecl{Name: "host_restart_service", Class: domain.ClassWrite})...),
		receipts, nil)

	permit(auth, RoleAdmin, "sess-1", "host_restart_service", []byte(`{"reason":"nightly"}`))

	if receipts.seen.Target != "" {
		t.Errorf("target = %q, want empty: nothing in those arguments names a resource, and "+
			"inventing one is the thing this derivation must not do", receipts.seen.Target)
	}
	if receipts.seen.Summary != "host_restart_service" {
		t.Errorf("summary = %q, want the tool's own name", receipts.seen.Summary)
	}
}
