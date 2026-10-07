package federation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	floorfed "github.com/vincent-wuhan/opskeeper/core/floor/federation"
)

// The whole point of the ledger is that a root which restarts still knows
// who it governs. Until it shipped, every child cluster was refused on its
// next hello with its own still-valid token, and the only recovery was a
// human re-enrolling them one at a time and handing out new tokens.

func ledgerPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "federation", "ledger.json")
}

// The scenario the ledger exists for, driven end to end: enrol, publish, and
// then come back as a different Registry reading the same file.
func TestARestartedRootStillKnowsItsClusters(t *testing.T) {
	path := ledgerPath(t)
	id, err := floorfed.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}

	before := NewRegistry(NewFileLedger(path))
	token, err := before.Enroll(id, "华东生产一区")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	bundle, err := before.Publish(id, newEnvelope(t, "policy", "1.0.0"), "first")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := before.Acknowledge(id, floorfed.Outcome{Version: bundle.Version, Accepted: true}); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}

	after := NewRegistry(NewFileLedger(path))
	if err := after.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// The token the child still holds has to keep working, or the ledger
	// saved a membership nobody can authenticate against.
	if _, err := after.Authenticate(id, token, floorfed.Cluster{ID: id, Name: "华东生产一区"}); err != nil {
		t.Fatalf("the child's own valid token was refused after a restart: %v", err)
	}

	// And the version counter has to have survived, or the next publish
	// reissues a number this cluster has already seen.
	member, ok := after.Member(id)
	if !ok {
		t.Fatal("the restored root has no member")
	}
	if member.HighestIssued != bundle.Version {
		t.Errorf("HighestIssued = %d after restart, want %d; a reissued version "+
			"is one a child that already refused it would refuse again",
			member.HighestIssued, bundle.Version)
	}
	if member.Acknowledged != bundle.Version {
		t.Errorf("Acknowledged = %d, want %d", member.Acknowledged, bundle.Version)
	}
	if member.TokenHash != before.members[id].TokenHash {
		t.Error("the restored token hash differs; the operator's token would stop working")
	}
}

// A version number must never go backwards, even if the caller replays an
// older write. That is the whole guarantee the column exists for.
func TestHighestIssuedNeverGoesBackwards(t *testing.T) {
	ledger := NewFileLedger(ledgerPath(t))
	id, err := floorfed.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	r := NewRegistry(ledger)
	if _, err := r.Enroll(id, "华东"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	if err := ledger.SaveHighestIssued(id, 9); err != nil {
		t.Fatalf("SaveHighestIssued: %v", err)
	}
	if err := ledger.SaveHighestIssued(id, 4); err != nil {
		t.Fatalf("SaveHighestIssued: %v", err)
	}
	members, err := ledger.LoadMembers()
	if err != nil {
		t.Fatalf("LoadMembers: %v", err)
	}
	if members[0].HighestIssued != 9 {
		t.Errorf("HighestIssued = %d, want 9; an out-of-order write un-recorded a version",
			members[0].HighestIssued)
	}
}

// Recording a version for a cluster that was never enrolled must not invent
// one. Otherwise a stray write produces a member with no token, which no
// child can ever authenticate against and which Members() would list.
func TestRecordingAVersionDoesNotEnrolACluster(t *testing.T) {
	ledger := NewFileLedger(ledgerPath(t))
	id, err := floorfed.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	if err := ledger.SaveHighestIssued(id, 3); err != nil {
		t.Fatalf("SaveHighestIssued: %v", err)
	}
	members, err := ledger.LoadMembers()
	if err != nil {
		t.Fatalf("LoadMembers: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("a stray version write enrolled %d cluster(s)", len(members))
	}
}

// A root that has enrolled nothing has no file yet, and that is a fact
// rather than a failure — otherwise every fresh install would refuse to
// start.
func TestARootWithNoLedgerFileHasEnrolledNothing(t *testing.T) {
	ledger := NewFileLedger(ledgerPath(t))
	members, err := ledger.LoadMembers()
	if err != nil {
		t.Fatalf("LoadMembers on a missing file: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("a missing ledger returned %d members", len(members))
	}
	r := NewRegistry(ledger)
	if err := r.Restore(); err != nil {
		t.Fatalf("Restore on a fresh root: %v", err)
	}
	if len(r.Members()) != 0 {
		t.Error("a fresh root came up with members")
	}
}

// A damaged ledger is refused, not read as an empty membership. The second
// would re-enrol every cluster and rotate every token, which is the worst
// possible response to a recoverable problem.
func TestADamagedLedgerIsRefusedRatherThanReadAsEmpty(t *testing.T) {
	path := ledgerPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	r := NewRegistry(NewFileLedger(path))
	err := r.Restore()
	if err == nil {
		t.Fatal("a damaged ledger restored as an empty membership; " +
			"every enrolled cluster would be silently re-enrolled with a new token")
	}
	if !strings.Contains(err.Error(), "not readable") {
		t.Errorf("error = %q, want it to say the file is unreadable", err)
	}
}

// The file holds a credential verifier and an inventory of every cluster the
// root governs. Neither is any local user's business.
func TestTheLedgerIsNotReadableByOtherUsers(t *testing.T) {
	path := ledgerPath(t)
	id, err := floorfed.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	if _, err := NewRegistry(NewFileLedger(path)).Enroll(id, "华东"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("ledger mode = %o; it holds a provisioning-token verifier", mode)
	}
}

// Two rows for one cluster means the store has no primary key. Picking a
// winner would make which token works depend on read order.
func TestRestoreRefusesTwoMembersForOneCluster(t *testing.T) {
	path := ledgerPath(t)
	id, err := floorfed.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	member := Member{Cluster: floorfed.Cluster{ID: id, Name: "华东"}}
	// Written behind the ledger's back, the way a store with no unique
	// index would end up: SaveMember would have collapsed these.
	if err := writeLedgerDocument(path, ledgerFile{Members: []Member{member, member}}); err != nil {
		t.Fatalf("writeLedgerDocument: %v", err)
	}

	r := NewRegistry(NewFileLedger(path))
	if err := r.Restore(); err == nil {
		t.Fatal("two members for one cluster were resolved by picking one, " +
			"which would make the working token depend on read order")
	}
}

// A row with no identity cannot be authenticated against, and carrying it
// would make Members() list something nothing can ever bind to.
func TestRestoreRefusesAMemberWithNoIdentity(t *testing.T) {
	path := ledgerPath(t)
	doc := ledgerFile{Members: []Member{{Cluster: floorfed.Cluster{Name: "nameless"}}}}
	if err := writeLedgerDocument(path, doc); err != nil {
		t.Fatalf("writeLedgerDocument: %v", err)
	}
	r := NewRegistry(NewFileLedger(path))
	if err := r.Restore(); err == nil {
		t.Fatal("a member with no cluster identity was restored")
	}
}

// writeLedgerDocument puts a document on disk without going through
// SaveMember, so a test can build states the API refuses to create.
func writeLedgerDocument(path string, doc ledgerFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}
