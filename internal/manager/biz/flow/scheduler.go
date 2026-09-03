// scheduler.go — the trigger.cron driver. One ticker scans enabled
// flows for trigger.cron nodes and fires those whose next-fire time has
// passed. Schedules live in memory (re-derived from each cron spec on
// boot / first sighting), same model as the report scheduler — in-flight
// runs don't survive a restart. Cron specs are evaluated in UTC.
package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/flow"
)

// cronTriggerConfig is the trigger.cron node's config.
type cronTriggerConfig struct {
	Cron string `json:"cron"` // standard 5-field cron, e.g. "0 8 * * *" (UTC)
}

// Scheduler drives trigger.cron.
type Scheduler struct {
	uc        *Usecase
	stateRepo ScheduleStateRepo
	log       *slog.Logger
	interval  time.Duration
	mu        sync.Mutex
	next      map[string]*model.FlowScheduleNextFire // "flowID:nodeID" → cursor
	lastPrune time.Time                              // last run-retention sweep (UTC)

	// Lifecycle channels used by Stop() to wait for the tick loop to
	// exit after the leader election manager has signalled loss of
	// leadership. closed in Start, then closed again (via doneCh) by
	// the loop's defer.
	stopCh chan struct{}
	doneCh chan struct{}
	// started guards Start against double-launch (idempotent).
	started bool
}

// NewScheduler builds the cron driver (30s tick).
func NewScheduler(uc *Usecase, stateRepo ScheduleStateRepo, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{uc: uc, stateRepo: stateRepo, log: log, interval: 30 * time.Second, next: map[string]*model.FlowScheduleNextFire{}}
}

// Start launches the tick loop until ctx is cancelled or Stop is called.
//
// Idempotent — the leader.Manager registers the same Scheduler
// instance for "scheduler:flow" and re-starts it on each leadership
// acquisition; subsequent Start calls without a matching Stop are
// no-ops.
//
// Returns nil unconditionally; the loop runs in its own goroutine
// and surfaces failures via slog. The error return matches the
// leader.WorkerFunc signature so the scheduler can be wired
// directly into leader.Manager.Register.
func (s *Scheduler) Start(ctx context.Context) error {
	if s == nil || s.uc == nil {
		return nil
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	s.stopCh = stopCh
	s.doneCh = doneCh
	s.started = true
	s.mu.Unlock()

	if s.stateRepo != nil {
		states, err := s.stateRepo.LoadScheduleStates(ctx)
		if err != nil {
			s.log.Error("flow cron: load persisted schedule state failed", slog.Any("err", err))
		} else {
			s.mu.Lock()
			for _, state := range states {
				s.next[s.scheduleKey(state.FlowID, state.NodeID)] = state
			}
			s.mu.Unlock()
		}
	}
	go s.loop(ctx, stopCh, doneCh)
	return nil
}

// Stop signals the tick loop to exit and waits for it to finish, up to
// ctx's deadline. Idempotent — calling Stop on an already-stopped
// scheduler returns nil immediately. The leader.Manager calls Stop
// with a 5s timeout on leader loss; this is plumbed through to the
// loop's select on stopCh.
//
// On successful Stop the started flag is reset so the next
// leadership acquisition can call Start again to re-launch the
// loop. The nil-uc path (Start was a no-op because uc was nil) is
// also a no-op for Stop — there's no loop to wait on.
//
// On a never-Started scheduler Stop is a no-op (returns nil).
func (s *Scheduler) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	stopCh := s.stopCh
	doneCh := s.doneCh
	s.started = false
	s.mu.Unlock()

	if stopCh != nil {
		select {
		case <-stopCh:
			// already closed (e.g. concurrent Stop)
		default:
			close(stopCh)
		}
	}
	if doneCh == nil {
		return nil
	}
	select {
	case <-doneCh:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("flow scheduler stop: %w", ctx.Err())
	}
}

func (s *Scheduler) loop(ctx context.Context, stopCh, doneCh chan struct{}) {
	defer close(doneCh)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-t.C:
			now := time.Now().UTC()
			s.tick(ctx, now)
			// Run retention sweep at most hourly — piggybacks the cron tick
			// so there's no second goroutine to manage.
			if now.Sub(s.lastPrune) >= time.Hour {
				s.lastPrune = now
				s.uc.PruneOldRuns(ctx)
			}
		}
	}
}

func (s *Scheduler) tick(ctx context.Context, now time.Time) {
	flows, err := s.uc.ListEnabledFlows(ctx)
	if err != nil {
		s.log.Warn("flow cron: list enabled failed", slog.Any("err", err))
		return
	}
	live := map[string]ScheduleStateKey{}
	for _, f := range flows {
		g, err := ParseGraph(f.GraphJSON)
		if err != nil {
			continue
		}
		for _, node := range g.Triggers() {
			if node.Type != NodeTriggerCron {
				continue
			}
			spec := parseCronSpec(node.Config)
			if spec == "" {
				continue
			}
			sched, err := cron.ParseStandard(spec)
			if err != nil {
				continue // invalid cron — UI validates; skip silently here
			}
			key := fmt.Sprintf("%d:%s", f.ID, node.ID)
			live[key] = ScheduleStateKey{FlowID: f.ID, NodeID: node.ID}

			s.mu.Lock()
			state, seen := s.next[key]
			if !seen {
				// First sighting: arm for the next occurrence, don't fire now.
				state = &model.FlowScheduleNextFire{FlowID: f.ID, NodeID: node.ID, CronSpec: spec, NextFireAt: sched.Next(now), LastHeartbeatAt: now, Status: model.FlowScheduleStatusEnabled}
				s.next[key] = state
				s.mu.Unlock()
				s.persistState(ctx, state)
				continue
			}
			state.CronSpec = spec
			state.LastHeartbeatAt = now
			if now.Before(state.NextFireAt) {
				s.mu.Unlock()
				s.persistState(ctx, state)
				continue
			}
			scheduledAt := state.NextFireAt
			state.LastFireAt = &scheduledAt
			state.NextFireAt = sched.Next(now)
			state.Status = model.FlowScheduleStatusEnabled
			state.MissedCount = 0
			s.mu.Unlock()
			s.persistState(ctx, state)

			payload := map[string]any{"fired_at": now.Format(time.RFC3339), "cron": spec}
			if _, err := s.uc.TriggerEvent(ctx, f.ID, NodeTriggerCron, payload); err != nil {
				s.log.Warn("flow cron trigger failed", slog.Uint64("flow_id", f.ID), slog.Any("err", err))
			} else {
				s.log.Info("flow triggered by cron", slog.Uint64("flow_id", f.ID), slog.String("cron", spec))
			}
		}
	}
	// Forget schedules whose flow/trigger vanished (disabled/deleted/edited)
	// so an edited cron re-arms cleanly next sighting.
	s.mu.Lock()
	keys := make([]ScheduleStateKey, 0, len(live))
	for _, key := range live {
		keys = append(keys, key)
	}
	for k := range s.next {
		if _, ok := live[k]; !ok {
			delete(s.next, k)
		}
	}
	s.mu.Unlock()
	if s.stateRepo != nil {
		if err := s.stateRepo.DeleteScheduleStatesNotIn(ctx, keys); err != nil {
			s.log.Warn("flow cron: delete stale schedule state failed", slog.Any("err", err))
		}
	}
}

func (s *Scheduler) persistState(ctx context.Context, state *model.FlowScheduleNextFire) {
	if s.stateRepo == nil || state == nil {
		return
	}
	if err := s.stateRepo.UpsertScheduleState(ctx, state); err != nil {
		s.log.Warn("flow cron: persist schedule state failed", slog.Uint64("flow_id", state.FlowID), slog.String("node_id", state.NodeID), slog.Any("err", err))
	}
}

func (s *Scheduler) scheduleKey(flowID uint64, nodeID string) string {
	return fmt.Sprintf("%d:%s", flowID, nodeID)
}

func parseCronSpec(cfgRaw json.RawMessage) string {
	var cfg cronTriggerConfig
	if len(cfgRaw) > 0 {
		_ = json.Unmarshal(cfgRaw, &cfg)
	}
	return strings.TrimSpace(cfg.Cron)
}
