package approval

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/approval"
)

// TestDualSignCannotBeEnforcedBecauseNowhereStoresTwoSigners is the
// assertion behind this file, and it exists because two pieces of the tree
// said otherwise.
//
// What is true: ADR-019's dual sign is implemented as a validator
// (DualSignPolicy.Validate) that is correct, is unit-tested, and has a
// well-specified rule file. What is also true: nothing calls it. The
// composition root loads the rule file at boot, validates its syntax, and
// prints "hitl: dual sign policy loaded (N rules)" — and then drops the
// policy on the floor, because a local variable going out of scope is the
// only thing that happens to it. This package's own doc comment said the HITL
// web channel calls Validate once the signatures are in. It does not, and it
// could not: the row it would read them from has nowhere to put them.
//
// model.Approval carries ApprovedBy *uint64 and model.Proposal carries
// ApprovedBy *uint64 and ResumedBy *uint64. Three single-valued identity
// columns across the two proposal tables, none of which is a list, so a
// second signer has nowhere to be written. Service.Approve sets
// StatusApproved on its first call and returns.
//
// So the control is not "configured but off". It is absent, and the artefact
// an operator would look at to check on it — a green boot log line — says the
// opposite. That is the specific harm this assertion is here to prevent.
//
// The test is written as a positive statement about the storage rather than a
// negative statement about the code, because the negative one is
// untestable: "nothing calls Validate" is a claim about the whole repository,
// and a test that asserted it would keep passing after the day somebody wires
// it correctly. This one instead watches the shape that makes wiring
// possible. Add a field that can hold two identities and this fails, and its
// message says what has to change with it — the wiring, the boot log line, and
// this package's doc comment.
//
// The second of those three is the one that does the damage, and it is not
// covered by a test here: a test cannot read what main.go logs without running
// the process, so what would have been its assertion is a sentence in this
// comment instead. Writing it as a test — a test that asserts a string
// constant contains its own words — would have been the eighth kind of thing
// this repository keeps refusing: an assertion that cannot fail, wearing the
// costume of a guard.
func TestTheApprovalsRowCanNowHoldTwoSigners(t *testing.T) {
	// This test used to be named
	// TestDualSignCannotBeEnforcedBecauseNowhereStoresTwoSigners, and it
	// watched the SHAPE of the two proposal tables for a field that could
	// hold more than one identity. It was a proxy, and decision 362 showed
	// how weak a proxy it was: the storage it was watching for is a JSON
	// column, and a proxy that recognises only slices and maps does not see
	// it. The test would have stayed green through the entire change it
	// existed to announce.
	//
	// So the assertion is now the behaviour rather than the shape. A row that
	// has been signed once, and is still waiting for the second signature, is
	// the fact ADR-019 needs; the field that holds it is an implementation
	// detail and the test below does not care what it is called or what
	// shape it has.
	row := &model.Approval{ID: "a1", Status: model.StatusPending}

	first := Signer{UserID: 7, Role: "admin", At: time.Unix(1700000000, 0).UTC()}
	blob, err := json.Marshal([]Signer{first})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	row.SignersJSON = ptr(string(blob))

	if row.SignersJSON == nil {
		t.Fatal("a signed row cannot record its signer")
	}

	var readBack []Signer
	if err := json.Unmarshal([]byte(*row.SignersJSON), &readBack); err != nil {
		t.Fatalf("the signer column is not readable back: %v", err)
	}
	if len(readBack) != 1 || readBack[0].UserID != 7 {
		t.Fatalf("read back %v, want the one signer that was recorded", readBack)
	}

	// And the second signature has somewhere to go, which is the part the
	// old test was actually about: a second element appended to the same
	// value, on the same row, without a second column.
	second := Signer{UserID: 8, Role: "admin", At: time.Unix(1700000600, 0).UTC()}
	blob2, err := json.Marshal(append(readBack, second))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	row.SignersJSON = ptr(string(blob2))
	var both []Signer
	if err := json.Unmarshal([]byte(*row.SignersJSON), &both); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(both) != 2 {
		t.Fatalf("a row cannot hold two signers: read back %d", len(both))
	}
	if both[0].UserID != 7 || both[1].UserID != 8 {
		t.Errorf("signers = %v, want 7 then 8 in signature order", both)
	}
}

func ptr[T any](v T) *T { return &v }

// What gets stored is a separate question from what gets counted, and it is
// the one an operator reads.
//
// A row whose signer list names the same person twice says two people signed.
// It does not matter that the gate would have counted one: the column is what
// the audit chain, the inbox card and the next reviewer look at, and a
// duplicated entry there is a statement the system is making about itself.
func TestOnePersonsRepeatedSignatureIsStoredOnce(t *testing.T) {
	var ran []string
	uc := newCountingUC(t, &ran)
	row, err := uc.Propose(context.Background(), ProposeInput{
		Kind: "restart_service", Title: "t", Payload: map[string]string{},
		RiskClass: "destructive",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := uc.Sign(context.Background(), Signer{UserID: 7, Role: "admin"}, row.ID); err != nil {
			t.Fatalf("Sign %d: %v", i+1, err)
		}
	}
	got, err := uc.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var signers []Signer
	if err := json.Unmarshal([]byte(*got.SignersJSON), &signers); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(signers) != 1 {
		t.Errorf("the row records %d signatures from one person: %v", len(signers), signers)
	}
}

// newCountingUC is a usecase over a fake repo, for the storage assertions that
// do not need a database. The gate is a double that counts signers the way the
// real one does, so these tests stay about the column rather than about the
// rules.
func newCountingUC(t *testing.T, ran *[]string) *Usecase {
	t.Helper()
	repo := &countingRepo{rows: map[string]*model.Approval{}}
	uc := NewUsecase(repo, nil).WithDualSignGate(countingGate{})
	uc.RegisterExecutor("restart_service", func(_ context.Context, p string) (string, error) {
		*ran = append(*ran, p)
		return `{"ok":true}`, nil
	})
	return uc
}

type countingGate struct{}

func (countingGate) Missing(_ context.Context, s Scope, signers []Signer) []string {
	if s.RiskClass != "destructive" {
		if len(signers) == 0 {
			return []string{"one signature"}
		}
		return nil
	}
	if len(signers) < 2 {
		return []string{"a second signature"}
	}
	return nil
}

type countingRepo struct{ rows map[string]*model.Approval }

func (r *countingRepo) Create(_ context.Context, a *model.Approval) error {
	if a.ID == "" {
		a.ID = "row-1"
	}
	r.rows[a.ID] = a
	return nil
}

func (r *countingRepo) Get(_ context.Context, id string) (*model.Approval, error) {
	a, ok := r.rows[id]
	if !ok {
		return nil, errs.ErrNotFound
	}
	return a, nil
}

func (r *countingRepo) List(context.Context, string, int) ([]*model.Approval, error) {
	return nil, nil
}

func (r *countingRepo) CountPending(context.Context) (int64, error) { return 0, nil }

func (r *countingRepo) Decide(_ context.Context, id string, fields map[string]any) error {
	a, ok := r.rows[id]
	if !ok {
		return errs.ErrNotFound
	}
	if a.Status != model.StatusPending {
		return errs.ErrNotFound
	}
	if s, ok := fields["status"].(string); ok {
		a.Status = s
	}
	if u, ok := fields["approved_by"].(uint64); ok {
		a.ApprovedBy = &u
	}
	if blob, ok := fields["signers_json"].(string); ok {
		a.SignersJSON = &blob
	}
	return nil
}

func (r *countingRepo) AddSigner(_ context.Context, id, signersJSON string) error {
	a, ok := r.rows[id]
	if !ok {
		return errs.ErrNotFound
	}
	if a.Status != model.StatusPending {
		return errs.ErrNotFound
	}
	a.SignersJSON = &signersJSON
	return nil
}

func (r *countingRepo) SetResult(_ context.Context, id, status, result string, _ time.Time) error {
	a, ok := r.rows[id]
	if !ok {
		return errs.ErrNotFound
	}
	a.Status = status
	a.ResultJSON = &result
	return nil
}

// dedupeSigners guards a different thing from the append-time check, and
// this is the only assertion that reaches it.
//
// Sign refuses to append a user it already has, so a row written through
// Sign can never contain a duplicate. What Sign cannot do is distrust what it
// reads: a row whose signer column was written by an older build, restored
// from a backup, or edited by hand can name the same person twice, and the
// judgement made from that list is the moment a two-person rule would
// otherwise be satisfied by one person clicking twice and asking nicely.
func TestAStoredDuplicateDoesNotBecomeASecondSignature(t *testing.T) {
	var ran []string
	uc := newCountingUC(t, &ran)
	row, err := uc.Propose(context.Background(), ProposeInput{
		Kind: "restart_service", Title: "t", Payload: map[string]string{},
		RiskClass: "destructive",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	// Forge the column the way a restore or a hand edit would.
	forged, err := json.Marshal([]Signer{
		{UserID: 7, Role: "admin"}, {UserID: 7, Role: "admin"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	uc.repo.(*countingRepo).rows[row.ID].SignersJSON = ptr(string(forged))

	// The second signer is the same person. The row must still wait.
	if _, decided, err := uc.Sign(context.Background(), Signer{UserID: 7, Role: "admin"}, row.ID); err != nil {
		t.Fatalf("Sign: %v", err)
	} else if decided {
		t.Fatal("a row whose stored signers named one person twice was decided")
	}
	if len(ran) != 0 {
		t.Fatalf("the action ran: %v", ran)
	}
}

// And once a genuinely different person signs, the row decides — so the
// dedup is a filter and not a permanent block.
func TestADifferentPersonStillDecidesARowWithAStoredDuplicate(t *testing.T) {
	var ran []string
	uc := newCountingUC(t, &ran)
	row, err := uc.Propose(context.Background(), ProposeInput{
		Kind: "restart_service", Title: "t", Payload: map[string]string{},
		RiskClass: "destructive",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	forged, _ := json.Marshal([]Signer{{UserID: 7, Role: "admin"}, {UserID: 7, Role: "admin"}})
	uc.repo.(*countingRepo).rows[row.ID].SignersJSON = ptr(string(forged))

	if _, decided, err := uc.Sign(context.Background(), Signer{UserID: 8, Role: "admin"}, row.ID); err != nil {
		t.Fatalf("Sign: %v", err)
	} else if !decided {
		t.Fatal("a row signed by two different people was not decided")
	}
	if len(ran) != 1 {
		t.Errorf("the action ran %d times, want once", len(ran))
	}
}
