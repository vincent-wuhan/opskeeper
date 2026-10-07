package approval_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	bizapproval "github.com/vincent-wuhan/opskeeper/core/manager/biz/approval"
	store "github.com/vincent-wuhan/opskeeper/core/manager/data/approval/store"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/approval"
	srvapproval "github.com/vincent-wuhan/opskeeper/core/manager/server/approval"
)

// ADR-019 says two people have to sign before something destructive runs.
// Until decision 362 nothing in this file's chain could say that, and the
// tests below are the first executable statement that it now can.
//
// They run against the real store, the real usecase and the real chi router,
// because the parts that were missing were a column and an accumulation: a
// unit test of the usecase with a map-backed repo would have passed on the
// day the control was still absent.

// twoSignGate is the rule this file needs and no more: a row that says it is
// destructive needs two distinct people, everything else needs one. The real
// gate is in cmd/opskeeper and is tested against a real rule file there;
// what is under test here is the accumulation, not the rule.
type twoSignGate struct{}

func (twoSignGate) Missing(_ context.Context, s bizapproval.Scope, signers []bizapproval.Signer) []string {
	if s.RiskClass != "destructive" && s.BlastRadius != "cluster" {
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

func newDualSignUC(t *testing.T, ran *[]string) *bizapproval.Usecase {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	uc := bizapproval.NewUsecase(store.NewRepo(db), slog.Default()).WithDualSignGate(twoSignGate{})
	uc.RegisterExecutor("restart_service", func(_ context.Context, payload string) (string, error) {
		*ran = append(*ran, payload)
		return `{"ok":true}`, nil
	})
	return uc
}

func proposeDestructive(t *testing.T, uc *bizapproval.Usecase) string {
	t.Helper()
	row, err := uc.Propose(context.Background(), bizapproval.ProposeInput{
		Kind: "restart_service", Title: "restart web-1",
		Payload:    map[string]string{"device": "web-1"},
		RiskClass:  "destructive",
		ProposedBy: 3,
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	return row.ID
}

// signOverHTTP goes through the same chi router, the same auth context and
// the same audit slot that the neighbouring tests use, so "the first
// signature is accepted" is a statement about the HTTP surface and not about
// the usecase called directly.
func signOverHTTP(t *testing.T, h *srvapproval.Handler, id string, userID uint64) int {
	t.Helper()
	rec, _, _ := call(t, h, "POST", "/v1/approvals/"+id+"/approve", "admin", userID, "")
	return rec.Code
}

// The first signature is a real event and the command must not have run.
// This is the assertion that could not be written before the column existed.
func TestOneSignatureOnADestructiveRowRunsNothing(t *testing.T) {
	var ran []string
	uc := newDualSignUC(t, &ran)
	h := srvapproval.NewHandler(uc)
	id := proposeDestructive(t, uc)

	if code := signOverHTTP(t, h, id, 7); code != http.StatusAccepted {
		t.Fatalf("first signature returned %d, want 202 — it is accepted, not failed", code)
	}
	if len(ran) != 0 {
		t.Fatalf("the action ran on one signature: %v", ran)
	}
	row, err := uc.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Status != model.StatusPending {
		t.Errorf("status = %q, want pending", row.Status)
	}
	if row.ApprovedBy != nil {
		t.Errorf("approved_by = %v, want NULL while the row is still pending", *row.ApprovedBy)
	}
	var signers []bizapproval.Signer
	if row.SignersJSON == nil {
		t.Fatal("the first signature was not recorded anywhere")
	}
	if err := json.Unmarshal([]byte(*row.SignersJSON), &signers); err != nil {
		t.Fatalf("the signer column is unreadable: %v", err)
	}
	if len(signers) != 1 || signers[0].UserID != 7 {
		t.Errorf("signers = %v, want exactly the one who signed", signers)
	}
}

// Two different people, and only then does it run.
func TestTwoDistinctSignersRunTheAction(t *testing.T) {
	var ran []string
	uc := newDualSignUC(t, &ran)
	h := srvapproval.NewHandler(uc)
	id := proposeDestructive(t, uc)

	if code := signOverHTTP(t, h, id, 7); code != http.StatusAccepted {
		t.Fatalf("first signature = %d, want 202", code)
	}
	if code := signOverHTTP(t, h, id, 8); code != http.StatusOK {
		t.Fatalf("second signature = %d, want 200", code)
	}
	if len(ran) != 1 {
		t.Fatalf("the action ran %d times, want once", len(ran))
	}
	row, err := uc.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Status != model.StatusExecuted {
		t.Errorf("status = %q, want executed", row.Status)
	}
	if row.ApprovedBy == nil || *row.ApprovedBy != 8 {
		t.Errorf("approved_by = %v, want the second signer", row.ApprovedBy)
	}
	var signers []bizapproval.Signer
	if err := json.Unmarshal([]byte(*row.SignersJSON), &signers); err != nil {
		t.Fatalf("unmarshal signers: %v", err)
	}
	if len(signers) != 2 {
		t.Errorf("the decided row records %d signers, want both of them", len(signers))
	}
}

// The hole this control exists to close: one person clicking twice.
func TestOnePersonSigningTwiceDoesNotRunTheAction(t *testing.T) {
	var ran []string
	uc := newDualSignUC(t, &ran)
	h := srvapproval.NewHandler(uc)
	id := proposeDestructive(t, uc)

	for i := 0; i < 3; i++ {
		if code := signOverHTTP(t, h, id, 7); code != http.StatusAccepted {
			t.Fatalf("signature %d returned %d, want 202", i+1, code)
		}
	}
	if len(ran) != 0 {
		t.Fatalf("one person signed three times and the action ran: %v", ran)
	}
	row, _ := uc.Get(context.Background(), id)
	var signers []bizapproval.Signer
	if err := json.Unmarshal([]byte(*row.SignersJSON), &signers); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(signers) != 1 {
		t.Errorf("the row records %d signatures from one person, want 1", len(signers))
	}
}

// A row nobody classified is not a row somebody invented a risk level for.
func TestAnUnclassifiedRowStillDecidesOnOneSignature(t *testing.T) {
	var ran []string
	uc := newDualSignUC(t, &ran)
	h := srvapproval.NewHandler(uc)
	row, err := uc.Propose(context.Background(), bizapproval.ProposeInput{
		Kind: "restart_service", Title: "no classification", Payload: map[string]string{},
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if code := signOverHTTP(t, h, row.ID, 7); code != http.StatusOK {
		t.Fatalf("signature returned %d, want 200", code)
	}
	if len(ran) != 1 {
		t.Fatalf("the action ran %d times, want once", len(ran))
	}
}

// A rejection is still a single decision: the second signer must not be able
// to overturn a row that was already rejected.
func TestARejectedRowRefusesFurtherSignatures(t *testing.T) {
	var ran []string
	uc := newDualSignUC(t, &ran)
	id := proposeDestructive(t, uc)
	if err := uc.Reject(context.Background(), 9, id, "no"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if _, _, err := uc.Sign(context.Background(), bizapproval.Signer{UserID: 7, Role: "admin"}, id); err == nil {
		t.Fatal("a rejected row accepted a signature")
	}
	if len(ran) != 0 {
		t.Fatalf("the action ran: %v", ran)
	}
}
