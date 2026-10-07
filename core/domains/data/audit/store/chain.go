package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	model "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
)

// ErrHeadConflict reports that another writer advanced the chain head
// between this writer's read and its compare-and-swap. It is a retryable
// condition, not a failure: the caller re-reads the head and tries again.
var ErrHeadConflict = errors.New("audit: chain head conflict")

// Head is the chain tail: the position and digest of the most recently
// appended entry. Seq 0 with an empty Hash means no entry has been
// chained yet, which is the state of a fresh install and of every install
// that ran before the chain was switched on.
type Head struct {
	Seq  uint64
	Hash string
}

// Seal is the chain position a row must carry, decided by the caller
// (which owns the key and the digest) and applied here inside the
// transaction that appends it.
//
// The split is deliberate: this layer owns atomicity and ordering, the
// caller owns cryptography. Neither half can be correct without the
// other — a digest computed outside the transaction can be computed
// against a head that another writer has already moved past.
type Seal struct {
	Seq      uint64
	PrevHash string
	Hash     string
}

// ChainStore is the chain-specific half of the audit persistence. It is
// a separate type from Repo so that the retention sweep and the admin
// list query — neither of which participates in the chain — cannot
// accidentally reach the head.
type ChainStore struct {
	db *gorm.DB
}

// NewChainStore builds a ChainStore over db.
func NewChainStore(db *gorm.DB) *ChainStore { return &ChainStore{db: db} }

// Head returns the current chain tail. A missing head row is reported as
// the genesis state rather than an error: the migrator seeds the row, but
// a deployment that restored a partial dump may not have it, and refusing
// to append because of that would turn a recoverable state into a dead
// ledger.
//
// The read is deliberately outside any write transaction. On SQLite a
// transaction that starts by reading takes a read snapshot, and a later
// UPDATE in that same transaction cannot be promoted to a writer once
// another connection has committed — the driver returns
// SQLITE_BUSY_SNAPTABLE, which no busy timeout retries. The append
// therefore reads the head here and makes its compare-and-swap the first
// statement inside the transaction, so the transaction is a writer from
// the start.
func (s *ChainStore) Head(ctx context.Context) (Head, error) {
	var row model.ChainHead
	err := s.db.WithContext(ctx).
		Where("id = ?", model.ChainHeadID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Head{}, nil
	}
	if err != nil {
		return Head{}, err
	}
	return Head{Seq: row.Seq, Hash: row.Hash}, nil
}

// AdvanceHead moves the head from want to next, but only if it is still
// at want. A zero RowsAffected means another writer won and the caller
// must re-read and retry; anything else is a real storage error.
//
// The update is deliberately a compare-and-swap on Seq and not a blind
// write. A blind write under a row lock would serialise correctly on
// MySQL but silently do nothing useful on SQLite, where the whole
// database is the lock and a stale in-process cache would be the only
// thing ordering the writers.
func (s *ChainStore) AdvanceHead(ctx context.Context, tx *gorm.DB, want Head, next Head) error {
	res := s.tx(tx).WithContext(ctx).
		Model(&model.ChainHead{}).
		Where("id = ? AND seq = ?", model.ChainHeadID, want.Seq).
		Updates(map[string]any{"seq": next.Seq, "hash": next.Hash, "updated_at": gorm.Expr("CURRENT_TIMESTAMP")})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrHeadConflict
	}
	return nil
}

// EnsureHead inserts the genesis head row if it is absent. It is
// idempotent so both the migrator and a lazily-initialised chainer can
// call it.
func (s *ChainStore) EnsureHead(ctx context.Context) error {
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&model.ChainHead{ID: model.ChainHeadID, Seq: 0, Hash: ""}).Error
}

// ListChained returns the entries that belong to the chain, oldest first.
// Rows with Seq 0 predate the chain and are excluded — they cannot be
// verified, and a verifier that included them would report a break at
// the first row of every upgraded install.
//
// The limit bounds one verification page; the caller pages by asking for
// entries with a Seq greater than the last one it verified.
func (s *ChainStore) ListChained(ctx context.Context, afterSeq uint64, limit int) ([]model.Log, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	var rows []model.Log
	err := s.db.WithContext(ctx).
		Where("seq > ?", afterSeq).
		Order("seq ASC").
		Limit(limit).
		Find(&rows).Error
	return rows, err
}

// MaxChainedSeq returns the highest Seq present, or 0 when no entry has
// been chained. Retention uses it to truncate a prefix of the chain
// rather than an arbitrary subset.
func (s *ChainStore) MaxChainedSeq(ctx context.Context) (uint64, error) {
	var max *uint64
	err := s.db.WithContext(ctx).Model(&model.Log{}).
		Where("seq > 0").
		Select("MAX(seq)").Scan(&max).Error
	if err != nil || max == nil {
		return 0, err
	}
	return *max, nil
}

// DeleteChainedThrough implements the prefix-only retention delete. It
// removes entries with Seq at or below through, which is the only shape
// of delete that leaves a verifiable chain behind: removing a row from
// the middle would leave its successors pointing at a hash nothing in
// the table produces any more, and the resulting "tampering" report
// would be indistinguishable from a real one.
func (s *ChainStore) DeleteChainedThrough(ctx context.Context, through uint64) (int64, error) {
	if through == 0 {
		return 0, nil
	}
	res := s.db.WithContext(ctx).
		Where("seq > 0 AND seq <= ?", through).
		Delete(&model.Log{})
	return res.RowsAffected, res.Error
}

func (s *ChainStore) tx(tx *gorm.DB) *gorm.DB {
	if tx != nil {
		return tx
	}
	return s.db
}

// appendBudget bounds the compare-and-swap retry loop in *time*, not in
// rounds.
//
// It used to be eight rounds, and eight was wrong in a way this comment
// itself predicted: "past this many rounds the deployment is either far
// busier than a single ledger writer was designed for, or the head row is
// being updated by something that is not appending." Neither is true of a
// burst of ordinary appenders on one ledger. A round count measures
// contention the wrong way round — how many times *this* writer loses
// depends on how many other writers there are, not on how long anyone is
// willing to wait — so the same eight rounds that are generous for two
// writers are not enough for eight. The test that pins this
// (TestChain_ConcurrentAppendsAllVerify, 8 writers) lost a row under -race,
// which is exactly the failure the bound exists to make loud rather than
// silent. It made it loud, so the bound moved to where it belongs: bound
// the wait, keep telling the caller, drop nothing.
//
// The caller is still told. A ledger that quietly gives up looks identical
// to a ledger nobody is writing, and that is the one thing an append path
// must not do.
const appendBudget = 15 * time.Second

// appendBackoff is the first sleep between attempts and the cap it doubles
// up to. The sleep is not politeness: a retry loop that spins re-reads the
// same head and loses the same race, which turns contention into a
// busy-wait that makes the contention worse. The jitter keeps two losers
// from waking together and colliding again in lockstep.
const (
	appendBackoff    = 200 * time.Microsecond
	appendBackoffMax = 20 * time.Millisecond
)

// AppendChained assigns row's chain columns and inserts it, atomically
// with advancing the head.
//
// seal is called with the head observed inside the transaction and
// returns the columns to write. The compare-and-swap that commits the
// new head is what makes two manager instances safe: the loser of the
// race updates zero rows, re-reads, and tries again with fresh columns.
//
// The head is advanced before the insert, not after. A crash between the
// two then leaves a head one ahead of the newest row — a state
// verification reports as a gap. Inserting first and advancing after
// would leave a row whose PrevHash points at a digest the table no
// longer agrees with, which is indistinguishable from tampering, and
// that ambiguity is the one thing a tamper-evidence feature must not
// manufacture.
func (s *ChainStore) AppendChained(ctx context.Context, row *model.Log, seal func(Head) (Seal, error)) error {
	if seal == nil {
		return errors.New("audit: AppendChained requires a seal function")
	}
	backoff := appendBackoff
	deadline := time.Now().Add(appendBudget)
	for {
		want, err := s.Head(ctx)
		if err != nil {
			return err
		}
		got, err := seal(want)
		if err != nil {
			return err
		}
		next := Head{Seq: got.Seq, Hash: got.Hash}
		row.Seq = got.Seq
		row.PrevHash = got.PrevHash
		row.Hash = got.Hash

		// The compare-and-swap is the first statement in the
		// transaction, so the transaction is a writer from the start and
		// two appenders serialise on it rather than racing a read
		// snapshot upgrade. The insert follows inside the same
		// transaction, so a rollback undoes the head movement too.
		err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := s.AdvanceHead(ctx, tx, want, next); err != nil {
				return err
			}
			return tx.WithContext(ctx).Create(row).Error
		})
		if errors.Is(err, ErrHeadConflict) {
			// Another appender moved the head between our read and our
			// update. Re-read and reseal: the row's Seq, PrevHash and
			// Hash all depend on the head, so they must be recomputed,
			// not just retried.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if !time.Now().Before(deadline) {
				return fmt.Errorf("audit: chain head still contended after %s", appendBudget)
			}
			time.Sleep(backoff/2 + rand.N(backoff/2+1))
			if backoff < appendBackoffMax {
				backoff *= 2
			}
			continue
		}
		return err
	}
}

// TruncateExpiredPrefix removes the leading run of chained entries whose
// OccurredAt is before cutoff, stopping at the first entry that is still
// inside the retention window. It returns how many rows went and the Seq
// of the new oldest surviving entry (0 when the chain is now empty).
//
// The walk is forward from the anchor and stops one row past the last
// deletion, so the cost is proportional to what is being deleted plus
// one row. A "delete everything older than the cutoff" query is cheaper
// and wrong: OccurredAt and Seq diverge the moment a clock steps or an
// insert is retried, and a delete driven by OccurredAt then removes a
// row from the middle of the chain. The result still hashes correctly
// on both sides of the hole, so verification reports a break that no
// operator can distinguish from real tampering — a tamper-evidence
// feature that manufactures phantom tampering is worse than none.
func (s *ChainStore) TruncateExpiredPrefix(ctx context.Context, cutoff time.Time) (removed int64, newAnchor uint64, err error) {
	var (
		afterSeq   uint64
		lastDead   uint64
		haveDead   bool
		liveAnchor uint64
	)
	for {
		rows, err := s.ListChained(ctx, afterSeq, 1000)
		if err != nil {
			return removed, liveAnchor, err
		}
		if len(rows) == 0 {
			break
		}
		stopped := false
		for i := range rows {
			row := &rows[i]
			afterSeq = row.Seq
			if row.OccurredAt.Before(cutoff) {
				lastDead = row.Seq
				haveDead = true
				continue
			}
			// First live row: the chain is retained from here.
			liveAnchor = row.Seq
			stopped = true
			break
		}
		if stopped {
			break
		}
		if len(rows) < 1000 {
			break
		}
	}
	if !haveDead {
		return 0, 0, nil
	}
	cut, err := s.DeleteChainedThrough(ctx, lastDead)
	return cut, liveAnchor, err
}

// DeleteUnchainedOlderThan removes pre-chain rows by age. They carry
// Seq 0 and are not part of any chain, so deleting them by timestamp
// cannot break verification — which is exactly why they are deleted on a
// different rule from the chained ones.
func (s *ChainStore) DeleteUnchainedOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res := s.db.WithContext(ctx).
		Where("seq = 0 AND occurred_at < ?", cutoff).
		Delete(&model.Log{})
	return res.RowsAffected, res.Error
}
