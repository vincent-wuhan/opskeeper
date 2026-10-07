// Package recovery closes the loop on a fix: it runs the approved action,
// then verifies the target metric came back to its pre-alert baseline.
//
// It is the zero-manual-ops-loop's Day 3 delivery, and it ships four things
// that belong together because the loop is what makes them one thing:
//
//	verify_recovery   the verify tool. Takes a baseline window before the
//	                  alert and a compare window before now, and reports
//	                  whether each watched metric is back inside tolerance.
//	recovery.execute  runs an approved proposal, behind a repair-preview
//	                  gate and with its audit record written first.
//	the adapters      the seams those two declare (metric querier, retry
//	                  state store, the fixture-pool recovery client) with
//	                  their production and dry-run implementations.
//	the retry store   in-memory here, DB-backed in production, behind one
//	                  interface so the orchestrator never learns which.
//
// Design constraints, which are the reason this is a package and not four
// files:
//
//   - The metric allowlist is hard-coded to four names (cpu_usage,
//     mem_usage, qps, latency_p99) with per-resource subsets. An
//     unlisted metric is rejected before any IO happens.
//   - Every dependency arrives through the constructor. The tools hold no
//     package state, which is what lets the harness test them against a
//     fixture pool with no manager running.
//   - The output is loop.VerifiedDelta's shape with schema_version=v1, and
//     the orchestrator validates it back. The two sides agree on a wire
//     contract rather than sharing a struct, because loop redeclares the
//     store interface to stay independent of this package.
//
// Known deviation, carried over from the Day 3 design and still true: the
// allowlist uses the four high-level names the closed loop and the harness
// agree on, where the original spec listed seven underlying Prometheus
// names. Those four cover host.cpu-spike, pg.long-running-tx and
// redis.memory-burst, which is the three golden cases the harness runs.
// Mapping the underlying names is cmdpolicy's job.
package recovery
