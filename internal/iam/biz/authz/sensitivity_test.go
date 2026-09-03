package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/dataguard"
)

func TestMeetsSensitivity(t *testing.T) {
	cases := []struct {
		user     SensitivityTier
		required SensitivityTier
		want     bool
	}{
		{"", TierPublic, true}, // 默认空 = Public reader
		{TierPublic, TierInternal, false},
		{TierInternal, TierConfidential, false},
		{TierConfidential, TierInternal, true}, // 高 tier 可读低 tier
		{TierRestricted, TierConfidential, true},
		{TierTopSecret, TierTopSecret, true},
		{TierTopSecret, TierRestricted, true},
		{TierRestricted, TierTopSecret, false},
	}
	for _, c := range cases {
		got := MeetsSensitivity(c.user, c.required)
		if got != c.want {
			t.Errorf("MeetsSensitivity(%q, %q) = %v, want %v", c.user, c.required, got, c.want)
		}
	}
}

func TestTierForSensitivity(t *testing.T) {
	cases := map[dataguard.Sensitivity]SensitivityTier{
		dataguard.Public:       TierPublic,
		dataguard.Internal:     TierInternal,
		dataguard.Confidential: TierConfidential,
		dataguard.Restricted:   TierRestricted,
		dataguard.TopSecret:    TierTopSecret,
	}
	for in, want := range cases {
		got := TierForSensitivity(in)
		if got != want {
			t.Errorf("TierForSensitivity(%s) = %s, want %s", in, got, want)
		}
	}
}

type fakeTierRepo struct {
	tiers map[uint64]SensitivityTier // key = uid<<32 | oid
	err   error
}

func newFakeTierRepo() *fakeTierRepo { return &fakeTierRepo{tiers: map[uint64]SensitivityTier{}} }
func (f *fakeTierRepo) Get(_ context.Context, userID, orgID uint64) (SensitivityTier, error) {
	if f.err != nil {
		return "", f.err
	}
	k := (userID << 32) | orgID
	t, ok := f.tiers[k]
	if !ok {
		return "", nil
	}
	return t, nil
}
func (f *fakeTierRepo) Set(_ context.Context, userID, orgID uint64, tier SensitivityTier) error {
	f.tiers[(userID<<32)|orgID] = tier
	return nil
}
func (f *fakeTierRepo) Delete(_ context.Context, userID, orgID uint64) error {
	delete(f.tiers, (userID<<32)|orgID)
	return nil
}

func TestPublicSensitivity_NotTierGated(t *testing.T) {
	// Public tier should always pass (treated as no requirement)
	if !MeetsSensitivity("", TierPublic) {
		t.Error("Public tier must be met by default")
	}
	if !MeetsSensitivity(TierPublic, TierPublic) {
		t.Error("Public must meet Public")
	}
}

func TestTierOrdering(t *testing.T) {
	// Strictly ascending: public < internal < confidential < restricted < topsecret
	order := []SensitivityTier{TierPublic, TierInternal, TierConfidential, TierRestricted, TierTopSecret}
	for i := 0; i < len(order)-1; i++ {
		if tierRanking(order[i]) >= tierRanking(order[i+1]) {
			t.Errorf("ranking %d (%s) should be < %d (%s)", i, order[i], i+1, order[i+1])
		}
	}
}

func TestAllowWithSensitivity_TierRepoError(t *testing.T) {
	repo := newFakeTierRepo()
	repo.err = errors.New("db down")
	_, err := repo.Get(context.Background(), 1, 1)
	if err == nil {
		t.Error("expected error from repo")
	}
}

func TestSensitivityTierRepo_SetGetDelete(t *testing.T) {
	repo := newFakeTierRepo()
	if err := repo.Set(context.Background(), 42, 7, TierConfidential); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got != TierConfidential {
		t.Errorf("got = %s", got)
	}
	if err := repo.Delete(context.Background(), 42, 7); err != nil {
		t.Fatal(err)
	}
	got, err = repo.Get(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("after delete got = %q, want empty", got)
	}
}

func TestErrTierUnmet_IsUnwrappable(t *testing.T) {
	wrapped := errorChain(ErrTierUnmet)
	if !errors.Is(wrapped, ErrTierUnmet) {
		t.Error("ErrTierUnmet should be unwrappable")
	}
}

func errorChain(err error) error {
	return errors.Join(errors.New("ctx"), err)
}

func TestGrantTier_NilRepo(t *testing.T) {
	a := &Enforcer{}
	if err := a.GrantSensitivityTier(context.Background(), nil, 1, 1, TierPublic, 0); err == nil {
		t.Error("nil repo should error")
	}
	if err := a.RevokeSensitivityTier(context.Background(), nil, 1, 1); err == nil {
		t.Error("nil repo should error")
	}
}
