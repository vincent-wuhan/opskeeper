// Package audit is the BC-level seam for HLD-010 audit logging. It
// exposes a single Emit method that callers (middleware, handlers,
// the retention goroutine) use to record observations. Failure to
// write is logged but never returned — audit must not block business.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	store "github.com/vincent-wuhan/opskeeper/core/domains/data/audit/store"
	model "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
)

// Repo is the persistence seam the usecase consumes. Implemented by
// data/audit/store.Repo.
//
// DeleteOlderThan is unchained deletion by age and exists only for rows
// written before the chain was switched on (Seq 0). Once the chain is on,
// retention goes through ChainStore.DeleteChainedThrough so it can only
// ever remove a prefix — see the retention comment in RunRetention.
type Repo interface {
	Insert(ctx context.Context, log *model.Log) error
	List(ctx context.Context, f ListFilters) ([]model.Log, int64, error)
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

// ListFilters is an alias to the store-level filter struct so handlers
// can depend only on biz/audit without importing data/audit/store.
type ListFilters = store.ListFilters

// Event is the row shape this usecase accepts. The definition lives in
// core/base/pkg/audit (decision 109) because a handler in another
// bounded context has to be able to name an audit row without importing
// this package — see the note on the alias: it is an alias, not a second
// type, so a value built through the port and a value built here are the
// same type and the throat in this package stays the only writer.
type Event = auditport.Event

// Usecase is the BC façade.
type Usecase struct {
	repo Repo
	log  *slog.Logger

	// chain is nil-or-disabled on a deployment with no HMAC key. The
	// write path checks Enabled() rather than assuming a chain exists,
	// so "audit is on" and "audit is tamper-evident" never collapse into
	// one silent truth.
	chain *ChainStamper
	// chainStore is nil when the deployment wired no chain store. A nil
	// chainStore with an enabled chainer is a misconfiguration, and
	// EmitWithID reports it instead of writing unchained rows under the
	// impression that they are chained.
	chainStore ChainStore
}

// Option configures a Usecase at construction.
type Option func(*Usecase)

// WithChain turns on the keyed hash chain. key is the deployment's
// audit HMAC key; an empty key leaves the chain off, which callers should
// surface rather than treat as a working audit trail.
//
// The store is required alongside the key. A chainer with nowhere to
// commit its head would stamp rows that verify against nothing, so the
// two are wired together by construction and a caller cannot half-enable
// the chain.
func WithChain(key string, chainStore ChainStore) Option {
	return func(u *Usecase) {
		u.chain = NewChainStamper(key)
		u.chainStore = chainStore
	}
}

// New builds a Usecase. log is mandatory for the warn-on-failure path.
func New(repo Repo, log *slog.Logger, opts ...Option) *Usecase {
	if log == nil {
		log = slog.Default()
	}
	u := &Usecase{repo: repo, log: log}
	for _, opt := range opts {
		if opt != nil {
			opt(u)
		}
	}
	if u.chain != nil && u.chain.Enabled() && u.chainStore != nil {
		if err := u.chainStore.EnsureHead(context.Background()); err != nil {
			u.log.Warn("audit: chain head could not be ensured; appends will retry per row",
				slog.Any("err", err))
		}
	}
	return u
}

// Emit persists one Event. Returns nothing — failures are warn-logged
// (HLD-010 "audit write failure must never block business").
func (u *Usecase) Emit(ctx context.Context, ev Event) {
	_, err := u.EmitWithID(ctx, ev)
	if err != nil {
		u.log.Warn("audit: insert failed; observation lost",
			slog.String("action", ev.Action),
			slog.String("resource_type", ev.ResourceType),
			slog.String("resource_id", ev.ResourceID),
			slog.Any("err", err))
	}
}

// EmitWithID synchronously persists one Event and returns the audit_logs
// primary key. It is reserved for callers that must hand the durable row ID
// to an external caller; Emit remains the non-blocking default.
func (u *Usecase) EmitWithID(ctx context.Context, ev Event) (uint64, error) {
	if u == nil || u.repo == nil {
		return 0, errors.New("audit: repository is not configured")
	}
	if ev.Action == "" || ev.Status == "" {
		return 0, errors.New("audit: action and status are required")
	}
	row := &model.Log{
		OccurredAt:   time.Now().UTC(),
		UserID:       ev.UserID,
		UserEmail:    ev.UserEmail,
		Role:         ev.Role,
		IP:           ev.IP,
		UserAgent:    ev.UserAgent,
		Action:       ev.Action,
		ResourceType: ev.ResourceType,
		ResourceID:   ev.ResourceID,
		ResourceName: ev.ResourceName,
		Status:       ev.Status,
		ErrorCode:    ev.ErrorCode,
		ErrorMessage: truncate(ev.ErrorMessage, 512),
		RequestID:    ev.RequestID,
	}
	if ev.Payload != nil {
		if b, err := json.Marshal(ev.Payload); err == nil {
			row.PayloadJSON = string(b)
		} else {
			u.log.Warn("audit: payload marshal failed; storing empty",
				slog.String("action", ev.Action),
				slog.Any("err", err))
		}
	}
	// The chained path is the only path that can report an ID, because
	// the ID is the row's position in a chain that must be committed
	// together with the row. Falling back to an unchained insert when
	// the chain is misconfigured would be the worst of both: a ledger
	// that looks chained and verifies as nothing.
	if u.chainEnabled() {
		stamper := u.chain
		if err := u.chainStore.AppendChained(ctx, row, func(head store.Head) (store.Seal, error) {
			return stamper.sealRow(row, head)
		}); err != nil {
			return 0, err
		}
		return row.ID, nil
	}
	// Unchained: either no chain was configured, or a chainer was
	// configured with an empty key. Rows are still recorded — losing
	// audit rows because nobody set a key would be a worse failure than
	// having rows that carry no tamper-evidence — and ChainState /
	// VerifyChain report the difference to whoever asks.
	if err := u.repo.Insert(ctx, row); err != nil {
		return 0, err
	}
	return row.ID, nil
}

// chainEnabled reports whether writes must go through the chain. It is a
// method rather than a field read so the "enabled but nowhere to commit"
// misconfiguration has exactly one answer everywhere it is asked.
func (u *Usecase) chainEnabled() bool {
	return u != nil && u.chain != nil && u.chain.Enabled() && u.chainStore != nil
}

// AutonomyReplayRow is one self-heal decision as it arrives from a node.
//
// It is the manager's own shape rather than core/floor/tunnel's, for the
// same reason every other biz type here is: the biz layer agrees with the
// domain, and the transport type agrees with the wire. The frontierbound
// adapter converts, which keeps a wire change from rippling into the audit
// ledger's vocabulary.
type AutonomyReplayRow = auditport.AutonomyReplayRow

// AutonomyReplayResult reports how a batch was taken.
type AutonomyReplayResult = auditport.AutonomyReplayResult

// RecordAutonomyReplay writes a node's self-heal rows into the chain.
//
// This is the "补写中心审计链" half of the plan's
// line: the node writes its decisions locally before they run, and this is
// where they enter the tamper-evident ledger. Every row goes through
// EmitWithID, which is the only path that both chain-stamps and reports an
// ID, so a replayed row is not a second-class entry — it is the same kind of
// record as one written live by the console.
//
// Shape is checked for the **whole batch before any row is written**, and
// storage then writes the whole batch or fails the whole batch. Both halves
// are deliberate:
//
//   - Checking first means a batch that contains a row the chain cannot
//     store writes *nothing*. If it wrote a prefix and then failed, the
//     node's all-or-nothing pump would resend the whole prefix and the
//     chain — which is append-only and has no dedupe key — would record
//     those rows twice. Verifying before the first append is what keeps a
//     retry free of duplicates.
//   - Rejecting the whole batch on shape rather than the bad row alone is
//     the same reasoning: a node's own arbiter writes rows that always
//     carry an action and a phase, so a malformed row is a bug in the
//     sender rather than data to salvage, and stopping the batch is the
//     loud outcome an operator can act on.
//
// A caller with no chain configured still records: the rows land in the
// audit_logs table unchained, exactly as any other row would in a
// deployment with no HMAC key. Losing a node's self-heal history because
// nobody configured a key would be the worse failure.
func (u *Usecase) RecordAutonomyReplay(ctx context.Context, edgeID uint64, rows []AutonomyReplayRow) (AutonomyReplayResult, error) {
	if len(rows) == 0 {
		return AutonomyReplayResult{}, nil
	}
	// Shape pass, before anything is written.
	for _, r := range rows {
		if r.Action == "" || r.Phase == "" {
			u.log.Warn("audit: autonomy replay batch refused for shape; nothing written",
				slog.Uint64("edge_id", edgeID),
				slog.String("action", r.Action),
				slog.String("phase", r.Phase),
				slog.Int("rows", len(rows)))
			return AutonomyReplayResult{Rejected: len(rows)}, nil
		}
	}
	var res AutonomyReplayResult
	for _, r := range rows {
		status := model.StatusSuccess
		if r.Verdict != "run" {
			// A refusal or a deferral is recorded, not dropped: "the node
			// considered self-healing and decided not to" is exactly what
			// an investigator wants when asking why an outage was not
			// mitigated. Denied is the honest status for both.
			status = model.StatusDenied
		}
		_, err := u.EmitWithID(ctx, Event{
			Role:         "edge",
			Action:       model.ActionAutonomyExecute,
			ResourceType: model.ResourceEdge,
			ResourceID:   strconv.FormatUint(edgeID, 10),
			ResourceName: r.Package,
			Status:       status,
			// The actor is the node, not a user. UserEmail is left empty
			// rather than invented: an operator reading the row should see
			// "a node did this", and a synthetic address would make it look
			// like a person.
			RequestID: r.Key,
			Payload: map[string]any{
				"at":             r.At.UTC().Format(time.RFC3339Nano),
				"action":         r.Action,
				"package":        r.Package,
				"tool":           r.Tool,
				"target":         r.Target,
				"argv":           r.Argv,
				"trigger_kind":   r.Kind,
				"trigger_metric": r.Metric,
				"threshold":      r.Threshold,
				"key":            r.Key,
				"verdict":        r.Verdict,
				"reason":         r.Reason,
				"phase":          r.Phase,
				"result":         r.Result,
				"exit_code":      r.ExitCode,
				// The origin is explicit because this row was not written
				// by a console operator, and a reader who does not know
				// that would look for the user who approved it.
				"origin": "node_autonomy",
			},
		})
		if err != nil {
			return res, fmt.Errorf("audit: record autonomy replay for edge %d: %w", edgeID, err)
		}
		res.Accepted++
	}
	return res, nil
}

// List is the read path for the admin UI.
func (u *Usecase) List(ctx context.Context, f ListFilters) ([]model.Log, int64, error) {
	if u == nil || u.repo == nil {
		return nil, 0, nil
	}
	return u.repo.List(ctx, f)
}

// ListChanges is the RCA-facing convenience over List (HLD-013 Phase 2):
// returns the mutating audit rows in [from, to], optionally narrowed to a
// resource type and/or action, capped at limit. Backs the
// query_change_events AIOps tool's "what changed near the incident" step.
// Failures (status=failure/denied) are intentionally included — "someone
// tried to change X right before the symptom" is itself a root-cause lead.
//
// It returns the projection, not model.Log, and that return type is the whole
// point of decision 273: the tool across this seam used to name this domain's
// GORM entity, which made the audit domain the last domain in the tree with an
// inbound cross-context import. The translation lives in projectChanges below
// rather than at the call site, so the seventeen storage columns — three of
// them chain columns — stop at this line and the reader is handed the nine it
// asks for.
//
// Nothing about the write path changes, and the method is still satisfied
// structurally by auditport.ChangeLister, so cmd/opskeeper still passes this
// *Usecase straight into the registry with no adapter.
func (u *Usecase) ListChanges(ctx context.Context, from, to time.Time, resourceType, action string, limit int) ([]auditport.ChangeRow, error) {
	if u == nil || u.repo == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	logs, _, err := u.List(ctx, ListFilters{
		From:         from,
		To:           to,
		ResourceType: resourceType,
		Action:       action,
		Limit:        limit,
	})
	if err != nil {
		return nil, err
	}
	return projectChanges(logs), nil
}

// projectChanges narrows storage rows to the published read projection.
//
// It returns nil for an empty input rather than an empty slice, matching what
// every caller here does with the result (both append to it, and the JSON
// encoder writes `[]` either way) and keeping the difference between "the
// query found nothing" and "the query was never run" out of the wire format,
// where neither is distinguishable to the model anyway.
func projectChanges(logs []model.Log) []auditport.ChangeRow {
	if len(logs) == 0 {
		return nil
	}
	out := make([]auditport.ChangeRow, 0, len(logs))
	for _, l := range logs {
		out = append(out, auditport.ChangeRow{
			OccurredAt:   l.OccurredAt,
			UserEmail:    l.UserEmail,
			Role:         l.Role,
			Action:       l.Action,
			ResourceType: l.ResourceType,
			ResourceID:   l.ResourceID,
			ResourceName: l.ResourceName,
			Status:       l.Status,
			PayloadJSON:  l.PayloadJSON,
		})
	}
	return out
}

// RunRetention runs the daily cleanup at the next 03:00 wall clock and
// every 24h thereafter. retentionDays <= 0 disables the sweep entirely
// (operator may prefer to manage retention via external archival).
// Blocks until ctx is cancelled.
func (u *Usecase) RunRetention(ctx context.Context, retentionDays int) error {
	if u == nil || u.repo == nil || retentionDays <= 0 {
		<-ctx.Done()
		return nil
	}
	for {
		// Next 03:00 local. Cheap arithmetic; we don't need a cron lib
		// for once-a-day.
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		timer := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		cutoff := time.Now().UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)
		removed, err := u.sweep(ctx, cutoff)
		if err != nil {
			u.log.Warn("audit retention: delete failed", slog.Any("err", err))
			continue
		}
		u.log.Info("audit retention swept",
			slog.Int("retention_days", retentionDays),
			slog.Int64("rows_removed", removed))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// sweep removes rows older than cutoff while keeping the chain
// verifiable.
//
// It is two deletes with different rules, and the difference is the
// whole point. Unchained rows (Seq 0) are removed by age because nothing
// links to them. Chained rows are removed as a *prefix of the chain*:
// the walk stops at the first entry still inside the retention window.
// Deleting by age alone would remove whatever row happened to carry an
// old timestamp, and a single clock correction or retried insert is
// enough to put one in the middle of the chain — where it produces a
// verification failure no operator can distinguish from real tampering.
// A chain that only ever loses its oldest entries has a describable
// boundary, the anchor, which ChainState reports. A chain with a hole has
// a mystery.
// recordTruncation writes the chain's own receipt for having cut its front.
//
// This is the only operation in the repository that deletes rows from an
// append-only ledger, and until decision 329 it left behind exactly one
// thing: a log line. That is not a record. It is not covered by the digest,
// it lives wherever logs live, and — the case that matters — **it does not
// survive a restore from a backup taken before the deletion**. An operator
// who finds a chain that starts at seq 40,000 and asks "did retention do
// that, or did somebody?" had no way to answer, and the honest answer is
// that the ledger had thrown away the very row that would have said.
//
// So the chain records its own truncation: one row, appended after the cut
// (so it lands on the new head and is itself sealed), naming how many rows
// went, where the new anchor is, and what the cutoff was. **唯一能删掉证据的
// 操作，必须在链上留下「我删过」的证据。**
//
// It cannot fail the sweep. A sweep that deleted rows and then returned an
// error would be retried, and the second run would find nothing to delete
// and report success — losing the receipt for the rows already gone. The
// failure is logged loudly instead, because the alternative (refusing to
// report the truncation) is what this method exists to prevent.
func (u *Usecase) recordTruncation(ctx context.Context, removed int64, anchor uint64, cutoff time.Time) {
	u.Emit(ctx, Event{
		Action:       model.ActionRetentionTruncate,
		ResourceType: model.ResourceAuditChain,
		// The resource id is the new anchor: "what this row replaced" is
		// the first thing an operator wants to know, and putting it in the
		// id column means it is searchable the way every other resource in
		// this ledger is.
		ResourceID:   strconv.FormatUint(anchor, 10),
		ResourceName: "audit_chain",
		Status:       model.StatusSuccess,
		Payload: map[string]any{
			"rows_removed":   removed,
			"new_anchor_seq": anchor,
			"cutoff":         cutoff.UTC().Format(time.RFC3339Nano),
		},
	})
}

func (u *Usecase) sweep(ctx context.Context, cutoff time.Time) (int64, error) {
	var removed int64
	if u.chainEnabled() {
		cut, anchor, err := u.chainStore.TruncateExpiredPrefix(ctx, cutoff)
		if err != nil {
			return 0, err
		}
		removed += cut
		if cut > 0 {
			u.log.Info("audit retention: chain truncated at the front",
				slog.Int64("rows_removed", cut),
				slog.Uint64("new_anchor_seq", anchor))
			u.recordTruncation(ctx, cut, anchor, cutoff)
		}
	}
	// Pre-chain rows sit outside the chain and are swept by age alone.
	var legacy int64
	var err error
	if u.chainEnabled() {
		legacy, err = u.chainStore.DeleteUnchainedOlderThan(ctx, cutoff)
	} else {
		legacy, err = u.repo.DeleteOlderThan(ctx, cutoff)
	}
	if err != nil {
		return removed, err
	}
	return removed + legacy, nil
}
