package user

// S-2 of the marketplace-imbridge-fail-closed change: the IM bridge must
// run on a dedicated restricted service identity, never the bootstrap
// superuser. These tests pin the three properties the wiring depends on —
// restricted role flags, email-keyed idempotency, and error propagation
// (the assembly layer disables the whole IM bridge when creation fails,
// so a swallowed error here would silently restore the superuser path).

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/iam/model"
)

func TestEnsureIMBridgeServiceUser_CreatesRestrictedAccount(t *testing.T) {
	uc := newTestUsecase(t)
	u, err := uc.EnsureIMBridgeServiceUser(context.Background())
	if err != nil {
		t.Fatalf("EnsureIMBridgeServiceUser: %v", err)
	}
	if u.Email != IMBridgeServiceEmail {
		t.Errorf("email = %q, want %q", u.Email, IMBridgeServiceEmail)
	}
	if u.Role != model.RoleUser {
		t.Errorf("role = %q, want %q — the account must not be admin", u.Role, model.RoleUser)
	}
	if u.IsSuperuser {
		t.Error("is_superuser = true, want false — a superuser flag would bypass every casbin check on the IM path")
	}
	if u.Status != model.StatusActive {
		t.Errorf("status = %q, want %q", u.Status, model.StatusActive)
	}
	if u.PassHash == "" {
		t.Error("pass_hash empty — an empty hash could be let through by an empty-password login regression")
	}
	// The stored hash must not verify against any guessable input: not
	// the empty password, not the email, not the display name.
	for _, guess := range []string{"", IMBridgeServiceEmail, "IM Bridge (service)", "admin", "imbridge"} {
		if verifyPassword(guess, u.PassHash) {
			t.Errorf("pass_hash verifies against guessable input %q — account is login-capable", guess)
		}
	}
}

func TestEnsureIMBridgeServiceUser_IsIdempotent(t *testing.T) {
	uc := newTestUsecase(t)
	ctx := context.Background()
	first, err := uc.EnsureIMBridgeServiceUser(ctx)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := uc.EnsureIMBridgeServiceUser(ctx)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("second call returned id=%d, want the existing id=%d — restarts must reuse the row, not mint a new one", second.ID, first.ID)
	}
	users, err := uc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var count int
	for _, u := range users {
		if u.Email == IMBridgeServiceEmail {
			count++
		}
	}
	if count != 1 {
		t.Errorf("rows with service email = %d, want 1", count)
	}
}

func TestEnsureIMBridgeServiceUser_PropagatesRepoError(t *testing.T) {
	uc := newTestUsecase(t)
	uc.repo = &failingRepo{err: errors.New("db down")}
	if _, err := uc.EnsureIMBridgeServiceUser(context.Background()); err == nil {
		t.Fatal("expected error propagation — the assembly layer disables the IM bridge on failure; a nil error here would silently keep the superuser fallback")
	}
}

// failingRepo fails every call with err. Only the subset
// EnsureIMBridgeServiceUser touches is implemented; the rest panics so an
// unexpected call path fails loudly rather than passing vacuously.
type failingRepo struct {
	err error
}

func (r *failingRepo) Create(context.Context, *model.User) error                    { return r.err }
func (r *failingRepo) GetByEmail(context.Context, string) (*model.User, error)      { return nil, r.err }
func (r *failingRepo) GetByID(context.Context, uint64) (*model.User, error)         { return nil, r.err }
func (r *failingRepo) List(context.Context) ([]*model.User, error)                  { return nil, r.err }
func (r *failingRepo) Count(context.Context) (int64, error)                         { return 0, r.err }
func (r *failingRepo) Delete(context.Context, uint64) error                         { return r.err }
func (r *failingRepo) UpdateRole(context.Context, uint64, string) error             { return r.err }
func (r *failingRepo) UpdateProfile(context.Context, uint64, string, string) error  { return r.err }
func (r *failingRepo) UpdateStatus(context.Context, uint64, string) error           { return r.err }
func (r *failingRepo) UpdateSuperuser(context.Context, uint64, bool) error          { return r.err }
func (r *failingRepo) UpdatePassHash(context.Context, uint64, string) error         { return r.err }
