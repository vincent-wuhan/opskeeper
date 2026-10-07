package querybackend

import (
	"context"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/logquery"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tracequery"
)

// These three fakes are copies of the ones the tools package keeps for its own
// tests. That duplication is deliberate and is the same shape as every other
// extracted cluster: test helpers do not cross package boundaries, because the
// moment they do, the new package inherits the old package's test fixtures and
// with them a reason to keep importing it. Three small structs that record the
// call they received are cheaper than that.

// fakePromQuerier records the last range query it was asked for.
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

// Query is the instant-form sibling of QueryRange. query_promql never calls
// it, but PromQuerier requires it, and the stub mirrors QueryRange's
// response/error behaviour.
func (f *fakePromQuerier) Query(_ context.Context, expr string, _ time.Time) (*promquery.InstantResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotExpr = expr
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// fakeLogQuerier records the last LogQL query it was asked for.
type fakeLogQuerier struct {
	mu   sync.Mutex
	got  logquery.QueryRangeOptions
	resp *logquery.QueryRangeResult
	err  error
}

func (f *fakeLogQuerier) QueryRange(_ context.Context, opts logquery.QueryRangeOptions) (*logquery.QueryRangeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = opts
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// fakeTraceQuerier records the last TraceQL search it was asked for.
type fakeTraceQuerier struct {
	mu   sync.Mutex
	got  tracequery.SearchOptions
	resp *tracequery.SearchResult
	err  error
}

func (f *fakeTraceQuerier) SearchTraces(_ context.Context, opts tracequery.SearchOptions) (*tracequery.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = opts
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}
