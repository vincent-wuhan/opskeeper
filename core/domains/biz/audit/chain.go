package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	store "github.com/vincent-wuhan/opskeeper/core/domains/data/audit/store"
	model "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
)

// Chain support for HLD-010.
//
// The ledger is a keyed hash chain: entry N carries the digest of entry
// N-1, so an edit or a deletion cannot be undone by writing a
// plausible-looking later row. The key lives only in the host. A plugin
// process, a node, and a model all see the chain as something they can
// read and nothing they can extend, because the append path
// (EmitWithID) is reached only from host code and the kernel's ledger
// port is implemented by an adapter over it.
//
// Canonicalisation. The digest is taken over a length-prefixed encoding
// of the row rather than over a delimiter-joined string. The difference
// is not cosmetic: with a delimiter, a writer who controls row content
// can move a character across the boundary — target "device-1" with an
// empty action and target "device" with action "-1" are the same bytes —
// and the chain verifies while describing a different event. Length
// prefixes make every field's extent unambiguous.

// ErrChainDisabled reports that the deployment has no audit chain key
// configured, so rows are being written unchained.
//
// It is deliberately distinct from a nil result. "The chain is intact"
// and "there is no chain" are different facts, and the caller that most
// needs to know which is true is an operator asking whether a record was
// tampered with.
var ErrChainDisabled = auditport.ErrChainDisabled

// ErrChainBroken reports the first entry whose digest did not match, or
// whose PrevHash did not match its predecessor's Hash.
//
// The shape moved to core/base/pkg/audit in decision 328 for the same
// reason ChainState did: the gateway's own verification surface has to name
// these two to answer an operator, and it must not import this package to
// do it — a reporting surface that has to reach the writer to describe the
// writer's failures is a dependency the throat table is right to refuse.
type ErrChainBroken = auditport.ErrChainBroken

// GenesisHash is the PrevHash of the first chained entry. It is the empty
// string rather than a constant digest so that "this row starts a chain"
// and "this row's predecessor is missing" stay distinguishable in a
// database dump.
const GenesisHash = ""

// ChainStamper turns a row into a chained row and checks one back.
//
// The key is held for the lifetime of the process and never logged,
// exported, or included in an error. A stamper with a nil or empty key
// is valid and writes unchained rows; callers gate on Enabled() rather
// than assuming a stamper implies a chain.
type ChainStamper struct {
	key []byte
}

// NewChainStamper builds a stamper. An empty key yields a stamper whose
// Enabled reports false — the deployment still records rows, they simply
// carry no tamper-evidence, and the operator is told so out loud rather
// than being handed a clean bill of health from an absence.
func NewChainStamper(key string) *ChainStamper {
	return &ChainStamper{key: []byte(key)}
}

// Enabled reports whether rows will be chained.
func (c *ChainStamper) Enabled() bool { return c != nil && len(c.key) > 0 }

// sealRow computes the chain columns for a row appended after want.
// It is the only place a row's position and links are decided.
func (c *ChainStamper) sealRow(row *model.Log, want store.Head) (store.Seal, error) {
	row.Seq = want.Seq + 1
	row.PrevHash = want.Hash
	row.Hash = c.Digest(row)
	return store.Seal{Seq: row.Seq, PrevHash: row.PrevHash, Hash: row.Hash}, nil
}

// Digest computes the hash of a row. It is the single definition used by
// both stamping and verification — a second implementation of the same
// encoding would be a second opinion nobody checked.
func (c *ChainStamper) Digest(row *model.Log) string {
	mac := hmac.New(sha256.New, c.key)
	var buf [8]byte
	putU64 := func(v uint64) {
		binary.BigEndian.PutUint64(buf[:], v)
		mac.Write(buf[:])
	}
	putStr := func(s string) {
		putU64(uint64(len(s)))
		mac.Write([]byte(s))
	}

	putU64(row.Seq)
	putStr(row.PrevHash)
	// UTC + RFC3339Nano so the digest does not depend on the writer's
	// local zone or on the precision the driver happened to keep.
	putStr(row.OccurredAt.UTC().Format(time.RFC3339Nano))
	// A nullable user id is encoded as present-flag + value rather than
	// as a sentinel number: the unauthenticated-actor path writes NULL
	// and a NULL must not collide with a real user whose id happens to
	// be zero.
	if row.UserID == nil {
		putU64(0)
	} else {
		putU64(1)
		putU64(*row.UserID)
	}
	putStr(row.UserEmail)
	putStr(row.Role)
	putStr(row.IP)
	putStr(row.UserAgent)
	putStr(row.Action)
	putStr(row.ResourceType)
	putStr(row.ResourceID)
	putStr(row.ResourceName)
	putStr(row.Status)
	putStr(row.ErrorCode)
	putStr(row.ErrorMessage)
	putStr(row.PayloadJSON)
	putStr(row.RequestID)
	return hex.EncodeToString(mac.Sum(nil))
}

// ChainStore is the chain-specific persistence the usecase needs. It is
// narrower than Repo on purpose: retention and the admin list query do
// not participate in the chain and must not be reachable through it.
type ChainStore interface {
	EnsureHead(ctx context.Context) error
	AppendChained(ctx context.Context, row *model.Log, seal func(store.Head) (store.Seal, error)) error
	ListChained(ctx context.Context, afterSeq uint64, limit int) ([]model.Log, error)
	MaxChainedSeq(ctx context.Context) (uint64, error)
	TruncateExpiredPrefix(ctx context.Context, cutoff time.Time) (removed int64, newAnchor uint64, err error)
	DeleteUnchainedOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

// ChainState is what an operator-facing surface reports about the chain:
// whether it is on, and if not, why. It is returned rather than inferred
// from a nil error so a console can render "verification unavailable"
// without having to string-match an error value.
// The shape moved to core/base/pkg/audit in decision 327: a second process
// (the gateway, which holds its own chain) has to be able to hold and report
// on one without importing this package, and an alias keeps every spelling
// here unchanged.
type ChainState = auditport.ChainState

// VerifyChain walks the whole chain and reports the first entry that
// does not check out, or nil when every entry does.
//
// It returns ErrChainDisabled when the deployment has no key: reporting
// nil there would tell an operator asking "was this record tampered
// with?" that it was not, which is the one answer this function must
// never invent.
func (u *Usecase) VerifyChain(ctx context.Context) error {
	if u == nil || u.chain == nil || !u.chain.Enabled() {
		return ErrChainDisabled
	}
	if u.chainStore == nil {
		return errors.New("audit: chain store is not configured")
	}

	// The anchor is the first entry still present. Its PrevHash is
	// whatever the row before it had, which after a retention sweep is a
	// digest nothing in the table can reproduce. So the first row is
	// checked for internal consistency (its own Hash must match its
	// contents) and is accepted as the anchor, and every later row must
	// both hash correctly and link to its predecessor.
	var (
		afterSeq uint64
		prevHash = GenesisHash
		first    = true
	)
	for {
		rows, err := u.chainStore.ListChained(ctx, afterSeq, verifyPage)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for i := range rows {
			row := &rows[i]
			want := u.chain.Digest(row)
			if !hmac.Equal([]byte(want), []byte(row.Hash)) {
				return &ErrChainBroken{Seq: row.Seq, Reason: fmt.Sprintf(
					"digest mismatch: row hashes to %s but stores %s", want, row.Hash)}
			}
			if !first && row.PrevHash != prevHash {
				return &ErrChainBroken{Seq: row.Seq, Reason: fmt.Sprintf(
					"link mismatch: PrevHash is %s but the preceding entry hashes to %s",
					row.PrevHash, prevHash)}
			}
			prevHash = row.Hash
			first = false
			afterSeq = row.Seq
		}
		if len(rows) < verifyPage {
			return nil
		}
	}
}

// verifyPage is one page of chain rows. It trades a longer walk for a
// bounded query, because a ledger that has run for a year holds millions
// of rows and a verifier that loads them all is a verifier that OOMs
// exactly when an operator needs it.
const verifyPage = 500

// ChainState reports whether the chain is on and how much of it survives.
func (u *Usecase) ChainState(ctx context.Context) (ChainState, error) {
	if u == nil || u.chain == nil || !u.chain.Enabled() {
		return ChainState{Enabled: false}, nil
	}
	if u.chainStore == nil {
		return ChainState{Enabled: false}, errors.New("audit: chain store is not configured")
	}
	head, err := u.chainStore.MaxChainedSeq(ctx)
	if err != nil {
		return ChainState{Enabled: true}, err
	}
	state := ChainState{Enabled: true, HeadSeq: head}
	if head == 0 {
		return state, nil
	}
	first, err := u.chainStore.ListChained(ctx, 0, 1)
	if err != nil || len(first) == 0 {
		return state, err
	}
	state.AnchorSeq = first[0].Seq
	return state, nil
}
