package proposal

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"

	"github.com/google/uuid"

	"github.com/vincent-wuhan/opskeeper/internal/manager/data/aiops/store"
)

// Expirer is the D.4.7 background loop that auto-declines pending
// proposals whose TTL has elapsed. It implements the "Expired +
// auto-decline" branch of the proposal state machine: every
// `Interval` seconds it scans for pending proposals whose effective
// deadline (created_at + ttl_seconds) is in the past, transitions
// each to DecisionExpired via the store, and appends a hash-chain
// audit entry so the auto-decline is tamper-evident.
//
// Lifecycle: New() → Start(ctx) returns immediately; the loop runs
// in a goroutine until ctx is cancelled. Start is idempotent —
// calling it twice is a no-op.
type Expirer struct {
	repo     *store.MutatingProposalRepo
	audit    *store.ProposalAuditRepo
	interval time.Duration
	logger   *slog.Logger

	// Tunable for tests. Zero = use defaults.
	batchLimit int

	// Started flag prevents double-start.
	started bool
}

// ExpirerConfig configures the expirer. Zero values use defaults.
type ExpirerConfig struct {
	// Interval is the scan period. Default 5 minutes.
	Interval time.Duration
	// BatchLimit caps the number of proposals expired per scan.
	// 0 = 100. Keeps one slow scan from blocking everything else.
	BatchLimit int
	// Logger is the structured logger. nil = slog.Default().
	Logger *slog.Logger
}

// NewExpirer constructs an expirer. Both repo and audit are
// required — the expirer is the first production caller of the
// audit chain, so wiring it in main.go is what lights up the
// tamper-evident audit log.
func NewExpirer(repo *store.MutatingProposalRepo, audit *store.ProposalAuditRepo, cfg ExpirerConfig) *Expirer {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.BatchLimit <= 0 {
		cfg.BatchLimit = 100
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Expirer{
		repo:       repo,
		audit:      audit,
		interval:   cfg.Interval,
		logger:     cfg.Logger,
		batchLimit: cfg.BatchLimit,
	}
}

// Start launches the background loop. It returns immediately; the
// loop exits when ctx is cancelled. Subsequent calls are no-ops.
func (e *Expirer) Start(ctx context.Context) {
	if e.started {
		return
	}
	e.started = true
	go e.run(ctx)
}

// run is the background goroutine body. It ticks on e.interval and
// runs scanOnce each tick. The first scan happens immediately so
// restarts don't wait a full interval before recovering pending
// expirations.
func (e *Expirer) run(ctx context.Context) {
	e.logger.Info("proposal.expirer started",
		slog.Duration("interval", e.interval),
		slog.Int("batch_limit", e.batchLimit))
	defer e.logger.Info("proposal.expirer stopped")

	if err := e.ScanOnce(ctx); err != nil {
		e.logger.Warn("proposal.expirer initial scan failed",
			slog.String("err", err.Error()))
	}

	t := time.NewTicker(e.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.ScanOnce(ctx); err != nil {
				e.logger.Warn("proposal.expirer scan failed",
					slog.String("err", err.Error()))
			}
		}
	}
}

// ScanOnce performs one expiration scan. Exposed publicly so tests
// and operational tooling can trigger a scan on demand.
func (e *Expirer) ScanOnce(ctx context.Context) error {
	now := time.Now().UTC()
	candidates, err := e.repo.ListPendingBefore(ctx, now, e.batchLimit)
	if err != nil {
		return fmt.Errorf("expirer: list candidates: %w", err)
	}
	if len(candidates) == 0 {
		return nil
	}

	expired := 0
	for i := range candidates {
		p := candidates[i]
		reason := fmt.Sprintf("auto-decline: exceeded TTL (%d seconds)", p.EffectiveTTL())
		if err := e.expireOne(ctx, &p, reason); err != nil {
			e.logger.Warn("proposal.expirer expire failed",
				slog.String("proposal_id", p.ID),
				slog.String("err", err.Error()))
			continue
		}
		expired++
	}
	if expired > 0 {
		e.logger.Info("proposal.expirer expired proposals",
			slog.Int("count", expired),
			slog.Int("candidates", len(candidates)))
	}
	return nil
}

// expireOne transitions one proposal to DecisionExpired and appends
// the corresponding hash-chain audit entry. Errors from the store
// propagate so ScanOnce can log them.
func (e *Expirer) expireOne(ctx context.Context, p *model.MutatingProposal, reason string) error {
	if err := e.repo.UpdateDecisionToExpired(ctx, p.ID, reason); err != nil {
		return err
	}
	// Append audit entry. If the chain rejects (concurrent writer),
	// we still log the discrepancy; the proposal IS expired, the
	// chain is just slightly behind.
	if e.audit != nil {
		tail, _ := e.audit.GetChainTail(ctx)
		payloadJSON := fmt.Sprintf(`{"reason":%q,"expired_at":%q,"severity":%q,"tool_name":%q}`,
			reason, time.Now().UTC().Format(time.RFC3339Nano), p.SeverityTier, p.ToolName)
		entry := &store.ProposalAuditEntry{
			ID:         uuid.NewString(),
			ProposalID: p.ID,
			Action:     "expire",
			Payload:    payloadJSON,
			PrevHash:   tail,
			Hash:       store.ComputeHash(tail, p.ID, "expire", payloadJSON),
			CreatedAt:  time.Now().UTC(),
		}
		if err := e.audit.AppendEntry(ctx, entry); err != nil {
			e.logger.Warn("proposal.expirer audit append failed",
				slog.String("proposal_id", p.ID),
				slog.String("err", err.Error()))
		}
	}
	return nil
}
