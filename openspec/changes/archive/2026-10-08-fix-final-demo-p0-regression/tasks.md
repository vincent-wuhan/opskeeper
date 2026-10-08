## 1. Pool Metrics Fix

- [x] 1.1 Add `pool_manifest_id` labels to aggregate pool-fixture Prometheus series.
- [x] 1.2 Add a versioned, manifest-agnostic public-demo pool metrics proxy script.
- [x] 1.3 Add Go tests proving multi-fixture metrics are distinct and dynamically exposed.

## 2. AgentTeams Isolation Fix

- [x] 2.1 Make Manager identity reject explicit Worker/standalone roles and isolate Manager continuation state.
- [x] 2.2 Register Manager, Worker, and Team prompt sections only for their valid runtime roles.
- [x] 2.3 Add outbound reply sanitization for thinking blocks and literal thinking spans.
- [x] 2.4 Add configurable outbound burst limits and admin incident STOP circuit breaking.
- [x] 2.5 Add Python tests for role identity, prompt gating, continuation isolation, sanitization, rate limits, and STOP.

## 3. Verification and Packaging

- [x] 3.1 Run focused pool-fixture Go tests.
- [x] 3.2 Run TeamHarness Python unit and package validation tests.
- [x] 3.3 Update release version metadata and build local release artifacts on macOS.
- [x] 3.4 Obtain deployment approval, deploy the verified artifacts, and run a fresh-manifest public E2E.（公网证据：fresh pool manifest `477a8b2b4878eed3e1f87f1a9cdc7142`，HITL、恢复与归档全部通过。）
