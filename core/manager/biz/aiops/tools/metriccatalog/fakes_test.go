package metriccatalog

// fakes_test.go — the in-memory Prometheus querier and the list helper this
// cluster's tests need.
//
// Copies of the tools package's fakePromQuerier and containsName. Test
// doubles are not a contract between packages: sharing one would let a test
// here break because somebody widened it over there to satisfy an unrelated
// test, and the failure would point at this file rather than at the edit
// that caused it.

import (
	"context"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"
)

// fakePromQuerier captures the last QueryRange call.
type fakePromQuerier struct {
	mu       sync.Mutex
	gotExpr  string
	gotStep  time.Duration
	gotStart time.Time
	gotEnd   time.Time
	resp     *promquery.InstantResult
	err      error
}

func (f *fakePromQuerier) QueryRange(_ context.Context, expr string, start, end time.Time, step time.Duration) (*promquery.InstantResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotExpr = expr
	f.gotStart = start
	f.gotEnd = end
	f.gotStep = step
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func (f *fakePromQuerier) Query(_ context.Context, expr string, _ time.Time) (*promquery.InstantResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotExpr = expr
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
