package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
)

// newRevokeTestDB is newTestRepo without the repo, because RevokeIdentities
// is called with a transaction the *caller* owns — a test that could not
// hand it one could not tell the two apart.
//
// It opens a file under t.TempDir() rather than ":memory:". With :memory:,
// every pooled connection is a *separate* empty database, so an
// implementation that wrongly reached for r.db instead of the passed tx
// would fail with "no such table: edges" — red, but for a reason that has
// nothing to do with the transaction. On a file database that mistake shows
// up as what it is: a revocation that committed on its own.
func newRevokeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "revoke.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("gorm.Open sqlite %s: %v", path, err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func mkEdge(t *testing.T, db *gorm.DB, ak, status string) *model.Edge {
	t.Helper()
	e := &model.Edge{AccessKeyID: ak, SecretKeyHash: "secret-" + ak, Status: status}
	if err := db.Create(e).Error; err != nil {
		t.Fatalf("create edge %s: %v", ak, err)
	}
	return e
}

func TestRevokeIdentitiesTombstonesThenSoftDeletes(t *testing.T) {
	db := newRevokeTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()

	live := mkEdge(t, db, "ak-live", model.StatusOffline)
	alreadyGone := mkEdge(t, db, "ak-gone", model.StatusOffline)
	if err := db.Delete(&model.Edge{}, alreadyGone.ID).Error; err != nil {
		t.Fatalf("soft-delete the untouched edge: %v", err)
	}

	if err := db.Transaction(func(tx *gorm.DB) error {
		return repo.RevokeIdentities(ctx, tx, []uint64{live.ID})
	}); err != nil {
		t.Fatalf("RevokeIdentities: %v", err)
	}

	var got model.Edge
	if err := db.Unscoped().First(&got, live.ID).Error; err != nil {
		t.Fatalf("load revoked edge unscoped: %v", err)
	}
	if got.DeletedAt == nil {
		t.Errorf("revoked edge was not soft-deleted")
	}
	if want := "deleted-1"; got.AccessKeyID != want {
		t.Errorf("access key = %q, want %q", got.AccessKeyID, want)
	}
	if got.SecretKeyHash != "" {
		t.Errorf("secret hash = %q, want empty", got.SecretKeyHash)
	}
	// The default scope must hide it.
	if err := db.First(&got, live.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("revoked edge still visible in default scope: %v", err)
	}

	// An edge nobody asked about keeps its credentials. A revocation that
	// tombstoned "every edge" would look identical on the happy path and
	// would quietly break every other node's access.
	var kept model.Edge
	if err := db.Unscoped().First(&kept, alreadyGone.ID).Error; err != nil {
		t.Fatalf("load untouched edge: %v", err)
	}
	if kept.AccessKeyID != "ak-gone" || kept.SecretKeyHash != "secret-ak-gone" {
		t.Errorf("untouched edge credentials changed: access=%q secret=%q",
			kept.AccessKeyID, kept.SecretKeyHash)
	}
}

func TestRevokeIdentitiesRefusesWhileAnyIsOnline(t *testing.T) {
	db := newRevokeTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()

	offline := mkEdge(t, db, "ak-off", model.StatusOffline)
	online := mkEdge(t, db, "ak-on", model.StatusOnline)

	err := db.Transaction(func(tx *gorm.DB) error {
		return repo.RevokeIdentities(ctx, tx, []uint64{offline.ID, online.ID})
	})
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("RevokeIdentities err = %v, want ErrConflict", err)
	}
	// The whole point of the shared transaction: the offline sibling must not
	// have been half-revoked, because the caller is going to roll back.
	for _, e := range []*model.Edge{offline, online} {
		var got model.Edge
		if err := db.Unscoped().First(&got, e.ID).Error; err != nil {
			t.Fatalf("load edge %d: %v", e.ID, err)
		}
		if got.AccessKeyID != e.AccessKeyID || got.SecretKeyHash != e.SecretKeyHash {
			t.Errorf("edge %d credentials changed on a refused revocation: access=%q secret=%q",
				e.ID, got.AccessKeyID, got.SecretKeyHash)
		}
		if got.DeletedAt != nil {
			t.Errorf("edge %d was soft-deleted on a refused revocation", e.ID)
		}
	}
}

func TestRevokeIdentitiesDeduplicatesAndIgnoresZero(t *testing.T) {
	db := newRevokeTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()

	e := mkEdge(t, db, "ak-dup", model.StatusOffline)
	// A junction can carry the same edge twice, and a caller with nothing
	// linked sends no ids at all rather than a nil slice.
	if err := db.Transaction(func(tx *gorm.DB) error {
		return repo.RevokeIdentities(ctx, tx, []uint64{0, e.ID, e.ID, 0})
	}); err != nil {
		t.Fatalf("RevokeIdentities with duplicates: %v", err)
	}
	var got model.Edge
	if err := db.Unscoped().First(&got, e.ID).Error; err != nil {
		t.Fatalf("load revoked edge: %v", err)
	}
	if want := "deleted-1"; got.AccessKeyID != want {
		t.Errorf("access key = %q, want %q — a duplicate was revoked twice", got.AccessKeyID, want)
	}

	if err := db.Transaction(func(tx *gorm.DB) error {
		return repo.RevokeIdentities(ctx, tx, nil)
	}); err != nil {
		t.Fatalf("RevokeIdentities with no ids: %v", err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return repo.RevokeIdentities(ctx, tx, []uint64{0, 0})
	}); err != nil {
		t.Fatalf("RevokeIdentities with only zero ids: %v", err)
	}
}

// TestRevokeIdentifiesUsesTheCallersTransaction is the test that keeps this
// method from being "simplified" into r.db. Every other test in this file
// passes on either implementation; this one does not.
func TestRevokeIdentifiesUsesTheCallersTransaction(t *testing.T) {
	db := newRevokeTestDB(t)
	repo := NewRepo(db)
	ctx := context.Background()

	e := mkEdge(t, db, "ak-rollback", model.StatusOffline)

	errRollback := errors.New("caller changed its mind")
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := repo.RevokeIdentities(ctx, tx, []uint64{e.ID}); err != nil {
			return err
		}
		// Revocation already "happened" inside tx. Rolling back has to undo it.
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("Transaction err = %v, want the caller's own error", err)
	}

	var got model.Edge
	if err := db.First(&got, e.ID).Error; err != nil {
		t.Fatalf("edge did not survive the caller's rollback: %v", err)
	}
	if got.AccessKeyID != "ak-rollback" || got.SecretKeyHash != "secret-ak-rollback" {
		t.Errorf("credentials changed outside the caller's transaction: access=%q secret=%q",
			got.AccessKeyID, got.SecretKeyHash)
	}
	if got.DeletedAt != nil {
		t.Errorf("edge was soft-deleted outside the caller's transaction")
	}
}
