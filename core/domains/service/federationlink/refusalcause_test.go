package federationlink

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	fedbiz "github.com/vincent-wuhan/opskeeper/core/domains/biz/federation"
)

// A refusal the operator cannot act on is an outage with extra steps.
//
// The wire cannot say why: "no such cluster" and "wrong token" are one error
// on purpose, because telling them apart hands anyone who can open a
// connection a free enumeration oracle for every cluster on the root. That
// trade is right, and it does not have to be paid twice — Registry.Known
// lets the server-side log answer the question the wire must not. These
// tests hold both halves at once.

// capturingLink builds a link whose log goes to a buffer, so the two
// refusals can be compared by what the operator would actually have read.
func capturingLink(t *testing.T, reg Clusters) (*Links, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	l, err := NewLink(&fakeCaller{}, reg, WithLogger(slog.New(slog.NewTextHandler(buf, nil))))
	if err != nil {
		t.Fatalf("NewLink: %v", err)
	}
	return l, buf
}

func TestARefusalNamesItsCauseInTheLogAndNotOnTheWire(t *testing.T) {
	id := clusterID(t, "prod-cn-north")

	// The two situations are the ones an operator has to tell apart: a
	// root that has never heard of the cluster, and a root that knows it
	// and was handed the wrong token.
	unknownLink, unknownLog := capturingLink(t, newScriptedRegistrar())
	unknown := sayHello(t, unknownLink, childEdgeID, id, "tok-north")

	knownReg := newScriptedRegistrar()
	knownReg.enrol(id, "tok-north", fedbiz.Member{})
	knownLink, knownLog := capturingLink(t, knownReg)
	wrongToken := sayHello(t, knownLink, childEdgeID, id, "not-the-token")

	// The wire: identical, because the caller must not be able to learn
	// which clusters exist on a root it holds no credential for.
	if unknown.Reason != wrongToken.Reason {
		t.Fatalf("the wire distinguished the two refusals: %q vs %q. "+
			"that is the enumeration oracle ErrRefused exists to prevent",
			unknown.Reason, wrongToken.Reason)
	}
	if unknown.Accepted || wrongToken.Accepted {
		t.Fatal("a refusal was reported as accepted")
	}

	// The log: specific, which is the whole point of the change.
	if got := unknownLog.String(); !strings.Contains(got, "no member") ||
		!strings.Contains(got, "durable Ledger") {
		t.Errorf("the unknown-cluster log does not name the real cause:\n%s", got)
	}
	got := knownLog.String()
	if !strings.Contains(got, "provisioning token does not match") {
		t.Errorf("the wrong-token log does not name the token as the cause:\n%s", got)
	}
	if strings.Contains(got, "no member") {
		t.Errorf("a known cluster was logged as unknown:\n%s", got)
	}
}

// The commonest cause by far is a root that restarted and forgot every
// member, which then refuses a child's own valid token. This drives the
// real registry with the nil ledger the production wiring passes, so the
// finding does not depend on a fake agreeing with production about who is
// enrolled.
func TestARestartedRootSaysSoRatherThanBlamingTheToken(t *testing.T) {
	id := clusterID(t, "prod-cn-north")

	before := fedbiz.NewRegistry(nil)
	token, err := before.Enroll(id, "华东生产一区")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	// Sanity: the child is let in before the restart, so what follows is
	// the restart and not a mis-built fixture.
	pre, preLog := capturingLink(t, asPort(before))
	if resp := sayHello(t, pre, childEdgeID, id, token); !resp.Accepted {
		t.Fatalf("hello refused before the restart: %s\n%s", resp.Reason, preLog)
	}

	after, buf := capturingLink(t, asPort(fedbiz.NewRegistry(nil)))
	resp := sayHello(t, after, childEdgeID, id, token)

	if resp.Accepted {
		t.Fatal("a child whose root restarted was let back in; the " +
			"registry would have had to persist the member across the restart")
	}
	if !strings.Contains(buf.String(), "no member") {
		t.Errorf("an operator reading this would go looking for a rotated token "+
			"or an attack, neither of which happened:\n%s", buf)
	}
}

// The tunnel response is what the child's own operator sees too, so it
// carries no hint either. Pinned separately so a future "just add it to the
// message" edit cannot pass by only breaking one of the two tests above.
func TestTheRefusalMessageNamesNoClusterAndNoToken(t *testing.T) {
	id := clusterID(t, "prod-cn-north")
	knownReg := newScriptedRegistrar()
	knownReg.enrol(id, "tok-north", fedbiz.Member{})
	l, _ := capturingLink(t, knownReg)

	resp := sayHello(t, l, childEdgeID, id, "not-the-token")
	if resp.Reason != "this root does not serve that cluster" {
		t.Fatalf("refusal reason = %q", resp.Reason)
	}
}
