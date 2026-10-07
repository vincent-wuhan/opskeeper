package federation

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// Service tests.
//
// The property under test is availability, not correctness of the maths: a
// root with no release key must be a control plane that can still enrol a
// child and answer questions about it, with exactly one thing missing. The
// dangerous version of this feature is the one that is "off" by taking the
// whole control plane with it.

func keylessService(t *testing.T) (*Service, federation.ClusterID, string) {
	t.Helper()
	svc, err := NewService(NewRegistry(nil), nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	id, err := federation.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	token, err := svc.Enroll(id, "north")
	if err != nil {
		t.Fatalf("Enroll on a keyless root: %v", err)
	}
	return svc, id, token
}

// TestAKeylessRootStillEnrols is the whole reason the publisher is optional.
// A deployment that was never given a release key has one feature off, not a
// platform that will not start.
func TestAKeylessRootStillEnrols(t *testing.T) {
	svc, id, token := keylessService(t)

	if svc.CanPublish() {
		t.Error("CanPublish = true on a root with no release key")
	}
	m, ok := svc.Member(id)
	if !ok {
		t.Fatal("a cluster enrolled on a keyless root is not in the registry")
	}
	if m.Cluster.Name != "north" {
		t.Errorf("cluster name = %q, want north", m.Cluster.Name)
	}
	if token == "" {
		t.Error("enrolment returned no token; a keyless root can still issue credentials")
	}
	if len(svc.Members()) != 1 {
		t.Errorf("Members = %d, want 1", len(svc.Members()))
	}
}

// TestAKeylessRootRefusesWithoutSpendingAVersion is the part that would be a
// data-integrity bug rather than a missing feature. HighestIssued is what the
// monotonic guarantee is built on: burn one on a decision this root could
// never make and the cluster is permanently one version ahead of any policy it
// can receive, with no way back that does not involve renumbering.
func TestAKeylessRootRefusesWithoutSpendingAVersion(t *testing.T) {
	svc, id, _ := keylessService(t)

	_, err := svc.Publish(context.Background(), id, PublishRequest{StagedRoot: "/tmp/whatever"})
	if !errors.Is(err, ErrNoReleaseKey) {
		t.Fatalf("publish on a keyless root = %v, want ErrNoReleaseKey", err)
	}
	if errors.Is(err, ErrNothingToPublish) {
		t.Error("a missing key was reported as a bad tree; those send an operator to different pages")
	}
	m, _ := svc.Member(id)
	if m.HighestIssued != 0 {
		t.Errorf("HighestIssued = %d after a keyless refusal, want 0", m.HighestIssued)
	}
	if len(svc.Members()) != 1 {
		t.Errorf("the refused publish disturbed the member list: %d members", len(svc.Members()))
	}
}

// TestAKeylessRootStillAuthenticatesAndRecordsAnswers: the two things it
// still has to do are decide who is allowed to speak for a cluster, and
// remember what that cluster said.
func TestAKeylessRootStillAuthenticatesAndRecordsAnswers(t *testing.T) {
	svc, id, token := keylessService(t)

	if _, err := svc.Registry().Authenticate(id, token, federation.Cluster{ID: id, Name: "north"}); err != nil {
		t.Fatalf("a keyless root refused its own token: %v", err)
	}
	if _, err := svc.Registry().Authenticate(id, "tok-wrong", federation.Cluster{ID: id}); !errors.Is(err, ErrRefused) {
		t.Errorf("a wrong token = %v, want ErrRefused", err)
	}
	// Version 0 is "no version", and recording it would wipe the record
	// of a refusal — the one answer a root that stops hearing about goes
	// on to misread as a cluster that is merely behind.
	if err := svc.Acknowledge(id, federation.Outcome{Version: 0, Accepted: false}); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("acknowledging version 0 = %v, want ErrUnknownVersion", err)
	}
	if m, _ := svc.Member(id); !m.LastAck.At.IsZero() {
		t.Error("a versionless acknowledgement wrote to the ledger")
	}
}

func TestAServiceNeedsARegistry(t *testing.T) {
	if _, err := NewService(nil, nil); err == nil {
		t.Error("a service with no registry was built; it could not answer who it has enrolled")
	}
}

// TestTheKeylessAndKeyedRootsAreTheSameType is what makes the degradation
// safe to wire: the assembly root hands the HTTP layer the same thing either
// way, so there is no second shape of handler to forget to configure.
func TestTheKeylessAndKeyedRootsAreTheSameType(t *testing.T) {
	signer, _, err := pluginmanifest.GenerateSigner("release-2026")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	pub, err := NewPublisher(NewRegistry(nil), signer)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	keyed, err := NewService(pub.Registry(), pub)
	if err != nil {
		t.Fatalf("NewService with a key: %v", err)
	}
	if !keyed.CanPublish() {
		t.Error("CanPublish = false on a root that holds a key")
	}
	keyless, _, _ := keylessService(t)
	if keyless.CanPublish() {
		t.Error("CanPublish = true on a root that holds no key")
	}
}
