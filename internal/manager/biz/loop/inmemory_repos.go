// Package loop — inmemory_repos.go
//
// Day 5 integration test seam: in-memory implementations of EventRepo
// + ContractRepo + WorkerRegistry support. Production wires the real
// *sql.DB-backed adapter (added in Day 5 main.go); tests use these
// fakes to exercise the orchestrator's Run() end-to-end without
// spinning up a database.
//
// Threading: all mutating methods take the receiver mutex so the
// dry-run integration test runs cleanly under -race.
package loop

import (
	"context"
	"errors"
	"sort"
	"sync"

	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

// InMemoryEventRepo is an EventRepo backed by a slice. The slice is
// kept ordered by CreatedAt ASC so ReadEvents returns chronological
// order without a sort step.
//
// AppendEvent respects the IdempotencyKey uniqueness constraint:
// replays with the same key are silently coalesced (the previously
// written row is returned in place of an error). This matches the
// production UNIQUE-index behaviour the orchestrator relies on for
// exactly-once writes.
type InMemoryEventRepo struct {
	mu     sync.Mutex
	events map[string]*loopmodel.Event // key: idempotency_key
}

// NewInMemoryEventRepo constructs the repo with empty storage.
func NewInMemoryEventRepo() *InMemoryEventRepo {
	return &InMemoryEventRepo{events: make(map[string]*loopmodel.Event)}
}

// AppendEvent writes the event; on UNIQUE collision returns the
// previously stored row (matching the production adapter's behaviour).
func (r *InMemoryEventRepo) AppendEvent(_ context.Context, e *loopmodel.Event) error {
	if e == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.events[e.IdempotencyKey]; ok {
		// Coalesce — copy the existing row's id into the caller's
		// pointer so subsequent contract rows can FK-link.
		e.ID = existing.ID
		return nil
	}
	if e.ID == 0 {
		// Synthesize a monotonically increasing id (loop_event_log
		// uses snowflake in production; tests use a local counter).
		e.ID = int64(len(r.events) + 1)
	}
	cp := *e
	r.events[e.IdempotencyKey] = &cp
	return nil
}

// ReadEvents returns events ordered by CreatedAt ASC.
func (r *InMemoryEventRepo) ReadEvents(_ context.Context, tenantID, incidentID string) ([]loopmodel.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]loopmodel.Event, 0, len(r.events))
	for _, e := range r.events {
		if e.TenantID != tenantID || e.IncidentID != incidentID {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Len returns the count of events stored (test helper).
func (r *InMemoryEventRepo) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// InMemoryContractRepo stores loop_contract rows by (incidentID,
// phase, type) tuple. The orchestrator reads via ReadContract and
// writes via WriteContract (Day 5 worker outputs land here).
type InMemoryContractRepo struct {
	mu        sync.Mutex
	contracts map[string]*loopmodel.Contract // key: incidentID|phase|type|schema
}

// NewInMemoryContractRepo constructs the repo with empty storage.
func NewInMemoryContractRepo() *InMemoryContractRepo {
	return &InMemoryContractRepo{contracts: make(map[string]*loopmodel.Contract)}
}

func (r *InMemoryContractRepo) key(c *loopmodel.Contract) string {
	return c.TenantID + "|" + c.IncidentID + "|" + c.Phase + "|" + c.Type + "|" + c.SchemaVer
}

// WriteContract persists a new contract row; successive writes for
// the same (incident, phase, type, schema) tuple overwrite (matching
// loop_contract's "most recent wins" semantics — read returns the
// latest).
func (r *InMemoryContractRepo) WriteContract(_ context.Context, c *loopmodel.Contract) error {
	if c == nil {
		return nil
	}
	if c.TenantID == "" {
		return errors.New("loop in-memory contract write: tenantID required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.ID == 0 {
		c.ID = int64(len(r.contracts) + 1)
	}
	if c.StorageBackend == "" {
		c.StorageBackend = loopmodel.StorageBackendDB
	}
	cp := *c
	r.contracts[r.key(c)] = &cp
	return nil
}

// ReadContract returns the most recent contract for (incident, phase,
// contractType); nil when none exists.
func (r *InMemoryContractRepo) ReadContract(_ context.Context, tenantID, incidentID string, phase Phase, contractType string) (*loopmodel.Contract, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.contracts {
		if c.TenantID != tenantID || c.IncidentID != incidentID || string(phase) != c.Phase || contractType != c.Type {
			continue
		}
		cp := *c
		return &cp, nil
	}
	return nil, nil
}

// Len returns the count of contracts stored (test helper).
func (r *InMemoryContractRepo) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.contracts)
}
