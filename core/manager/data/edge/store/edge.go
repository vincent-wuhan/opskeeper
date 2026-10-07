package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	biz "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
)

// Repo is the GORM-backed biz/edge.Repo.
type Repo struct {
	db *gorm.DB
}

// NewRepo constructs the repo around an opened *gorm.DB.
// Exposed as a biz.Repo via provider.go's NewRepo factory for wiring.
func NewRepo(db *gorm.DB) *Repo { return &Repo{db: db} }

// compile-time interface check.
var _ biz.Repo = (*Repo)(nil)

// Create inserts e. Any insert-side failure (unique violation, etc.) is
// returned unwrapped so the caller can errors.Is on gorm.ErrDuplicatedKey
// if desired.
func (r *Repo) Create(ctx context.Context, e *model.Edge) error {
	if e == nil {
		return errs.ErrInvalid
	}
	return r.db.WithContext(ctx).Create(e).Error
}

// GetByID returns the edge by primary key. Soft-deleted rows are scoped out
// by gorm's default DeletedAt handling.
func (r *Repo) GetByID(ctx context.Context, id uint64) (*model.Edge, error) {
	var e model.Edge
	if err := r.db.WithContext(ctx).First(&e, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &e, nil
}

// GetByAccessKey returns the edge matching the access_key_id column. Does
// NOT return soft-deleted rows (gorm default).
func (r *Repo) GetByAccessKey(ctx context.Context, accessKey string) (*model.Edge, error) {
	var e model.Edge
	if err := r.db.WithContext(ctx).Where("access_key_id = ?", accessKey).First(&e).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &e, nil
}

// GetByName returns the edge matching the human-readable name column. Does
// NOT return soft-deleted rows (gorm default).
func (r *Repo) GetByName(ctx context.Context, name string) (*model.Edge, error) {
	var e model.Edge
	if err := r.db.WithContext(ctx).Where("name = ?", name).First(&e).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &e, nil
}

// List returns edges matching f. Sorted by id DESC so the most recently
// registered edges appear first. Soft-deleted rows excluded.
//
// Post-split (May 2026): role filtering moved to the device repo —
// callers that filter by role should query devices and resolve back to
// edges through the edge_devices junction.
func (r *Repo) List(ctx context.Context, f biz.ListFilter) ([]*model.Edge, error) {
	tx := r.db.WithContext(ctx).Model(&model.Edge{})
	if f.Status != "" {
		tx = tx.Where("status = ?", f.Status)
	}
	if f.Name != "" {
		tx = tx.Where("name LIKE ?", "%"+f.Name+"%")
	}
	if f.CreatedBy != nil {
		tx = tx.Where("created_by = ?", *f.CreatedBy)
	}
	if f.Limit > 0 {
		tx = tx.Limit(f.Limit)
	}
	if f.Offset > 0 {
		tx = tx.Offset(f.Offset)
	}
	var out []*model.Edge
	if err := tx.Order("id DESC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateSecretHash replaces secret_key_hash.
func (r *Repo) UpdateSecretHash(ctx context.Context, id uint64, hash string) error {
	res := r.db.WithContext(ctx).Model(&model.Edge{}).Where("id = ?", id).Update("secret_key_hash", hash)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// UpdateStatus sets status + last_seen_at together.
func (r *Repo) UpdateStatus(ctx context.Context, id uint64, status string, lastSeen time.Time) error {
	res := r.db.WithContext(ctx).Model(&model.Edge{}).Where("id = ?", id).Updates(map[string]any{
		"status":       status,
		"last_seen_at": lastSeen,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// UpdateName overwrites the operator-friendly display name. Used by
// edge.HandleRegister to back-fill blank names with the host's
// reported hostname on first tunnel handshake — admins who created
// the edge without a name see it auto-populate when the agent boots.
func (r *Repo) UpdateName(ctx context.Context, id uint64, name string) error {
	res := r.db.WithContext(ctx).Model(&model.Edge{}).Where("id = ?", id).Update("name", name)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// SetDeviceID links an edge row to a device row (post-split data model).
// Called by HandleRegister once the host's Device row has been
// upserted, so subsequent reads can join Device for host facts.
func (r *Repo) SetDeviceID(ctx context.Context, edgeID, deviceID uint64) error {
	res := r.db.WithContext(ctx).Model(&model.Edge{}).Where("id = ?", edgeID).Update("device_id", deviceID)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// SetAgentVersion records the agent's self-reported binary version on
// register_edge. Caller filters empty values upstream so we don't blank
// the column when a buggy build reports nothing.
func (r *Repo) SetAgentVersion(ctx context.Context, id uint64, version string) error {
	res := r.db.WithContext(ctx).Model(&model.Edge{}).Where("id = ?", id).Update("agent_version", version)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// SetPigVersion records the PiG agent build the node reports on its
// heartbeat. It mirrors SetAgentVersion's contract — the caller filters
// empty and unchanged values — and for the same reason: blanking the
// column on a node that stopped reporting would destroy the last version
// an operator can still compare a release against.
func (r *Repo) SetPigVersion(ctx context.Context, id uint64, version string) error {
	res := r.db.WithContext(ctx).Model(&model.Edge{}).Where("id = ?", id).Update("pig_version", version)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// Delete soft-deletes an edge (gorm's DeletedAt). Subsequent Get/List hide
// the row.
func (r *Repo) Delete(ctx context.Context, id uint64) error {
	res := r.db.WithContext(ctx).Delete(&model.Edge{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// RevokeIdentities tombstones the credentials on the edge identities named
// by edgeIDs and soft-deletes the rows, all inside tx — the caller's
// transaction, because the reason this method exists at all is that a
// device row and the edge rows behind it have to move together or not at
// all (decision 112).
//
// It refuses with errs.ErrConflict while any of them is still online: a
// node that is talking to the control plane right now must not lose its
// access key underneath the running tunnel.
//
// tx is the transaction the caller opened, not a handle this repo owns,
// which is why the method is on *Repo but does not use r.db.
func (r *Repo) RevokeIdentities(ctx context.Context, tx *gorm.DB, edgeIDs []uint64) error {
	ids := uniqueIDs(edgeIDs)
	if len(ids) == 0 {
		return nil
	}
	var online int64
	if err := tx.WithContext(ctx).Model(&model.Edge{}).
		Where("id IN ? AND status = ?", ids, model.StatusOnline).
		Count(&online).Error; err != nil {
		return err
	}
	if online > 0 {
		return fmt.Errorf("%w: linked edge must be offline before device deletion", errs.ErrConflict)
	}
	// Credentials go first and the row is soft-deleted after, so that a
	// crash between the two leaves an edge that cannot authenticate rather
	// than one that is gone but still holds a reusable key.
	for _, id := range ids {
		res := tx.Unscoped().Model(&model.Edge{}).
			Where("id = ?", id).
			Updates(map[string]any{
				"access_key_id":   fmt.Sprintf("deleted-%d", id),
				"secret_key_hash": "",
			})
		if res.Error != nil {
			return res.Error
		}
	}
	return tx.Where("id IN ?", ids).Delete(&model.Edge{}).Error
}

// uniqueIDs drops zero and duplicate ids while keeping the caller's order,
// so a junction that linked the same edge twice is revoked once.
func uniqueIDs(ids []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(ids))
	out := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// Count returns the number of non-soft-deleted edges.
func (r *Repo) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.WithContext(ctx).Model(&model.Edge{}).Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}
