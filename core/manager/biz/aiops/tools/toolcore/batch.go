package toolcore

import (
	"context"
	"fmt"
	"sync"
)

// batch.go is the shared fan-out primitive for the batch-first BaseTools.
// Six per-id tools (get_host_load / get_process_list / get_edge_summary /
// get_incident_detail / correlate_incident / bash) take an ID array and fan
// out manager-side; this is what they share.
//
// Why one helper rather than a loop in each tool:
//
//   - Six tools, identical shape (semaphore + waitgroup + ordered result
//     slice). Diverging copies rot at different speeds.
//   - Generics keep it type-safe end to end. fn returns the tool-specific
//     result entry directly and the slice carries the same type — no
//     any-shaped boxing, no per-tool reflection.
//   - The semaphore is a hard ceiling, so a 16-id call does not light up
//     16 tunnel sessions at once. Four in flight is the same ceiling the
//     edge host_files batch handler picked.
//
// Per-call partial success is handled INSIDE fn: each invocation catches
// its own error and returns an entry carrying an Error field, so the slice
// is always full-length and the caller can count outcomes by one walk.

// BatchMaxIDs is the manager-side hard upper bound on len(ids) for every
// batch-flavoured BaseTool. The schema's maxItems=16 already enforces this
// LLM-side; this is server-side defense for hand-crafted args or a
// schema-validator bypass. Mirrors hostFilesMaxBatchPaths so the LLM sees
// one ceiling no matter which tool it picks.
const BatchMaxIDs = 16

// BatchConcurrency caps how many fan-out children run at once. Four was
// picked to match the edge-side host_files concurrency: beyond four we
// would be queueing past a typical edge's ability to service in parallel,
// burning manager goroutines for no throughput gain.
const BatchConcurrency = 4

// RunBatch fans fn out across ids concurrently with a fixed-size semaphore
// and returns the per-id results in input order. fn must not return an
// error — partial failure is encoded inside R (typically a string Error
// field on the entry struct) so the caller still gets a full-length slice.
//
// Cancellation: the helper does not propagate ctx.Done() into a fast
// shutdown — fn already runs under a derived ctx in the caller, and the
// per-tool timeout governs how long each child may take. The wait-group
// join is unconditional, so the slice is always fully written on return.
func RunBatch[ID any, R any](
	ctx context.Context,
	ids []ID,
	fn func(ctx context.Context, id ID) R,
) []R {
	results := make([]R, len(ids))
	if len(ids) == 0 {
		return results
	}
	sem := make(chan struct{}, BatchConcurrency)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		i, id := i, id
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = fn(ctx, id)
		}()
	}
	wg.Wait()
	return results
}

// ValidateBatchIDs enforces the 1..BatchMaxIDs constraint at the BaseTool
// layer. The schema's minItems/maxItems already covers the LLM-supplied
// happy path; this is a belt-and-braces check for the rare case where the
// LLM emits an empty array or the schema validator is bypassed (test
// harnesses, hand-crafted argsJSON). idLabel is the field name surfaced in
// the error, so the LLM sees "device_ids" rather than a generic "ids".
func ValidateBatchIDs[T any](idLabel string, ids []T) error {
	if len(ids) == 0 {
		return fmt.Errorf("%s: must contain at least 1 element", idLabel)
	}
	if len(ids) > BatchMaxIDs {
		return fmt.Errorf("%s: too many (%d > max %d)", idLabel, len(ids), BatchMaxIDs)
	}
	return nil
}
