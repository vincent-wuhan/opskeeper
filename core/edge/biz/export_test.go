package biz

import (
	"context"

	"github.com/vincent-wuhan/opskeeper/core/edge/telemetrywal"
)

// Test hooks. This file is compiled only into the test binary, so the
// external biz_test package can drive the drain directly without the
// production Agent growing a method whose only caller is a test.

// DrainBatchesForTest runs the write-ahead log's sender once.
func (a *Agent) DrainBatchesForTest(ctx context.Context, batches []telemetrywal.Batch) (int, error) {
	return a.drainBatches(ctx, batches)
}

// RejectedForTest reports how many rows the center refused outright.
func (a *Agent) RejectedForTest() uint64 { return a.telemetryRejected.Load() }
