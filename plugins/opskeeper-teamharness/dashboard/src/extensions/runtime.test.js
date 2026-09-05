import assert from 'node:assert/strict';
import test from 'node:test';

import {
  buildRuntimeSnapshot,
  normalizeHealthReport,
  normalizeIncidentMetrics,
  normalizeVersion,
} from './runtime.js';

test('normalizes health response wrappers and checks', () => {
  const report = normalizeHealthReport({
    data: {
      status: 'degraded',
      checked_at: '2026-09-05T08:00:00Z',
      checks: [{ id: 'database', group: 'core', label: 'Database', status: 'ok' }],
    },
  });

  assert.equal(report.status, 'degraded');
  assert.equal(report.checkedAt, '2026-09-05T08:00:00Z');
  assert.equal(report.summary.ok, 1);
  assert.equal(report.checks[0].label, 'Database');
});

test('normalizes version and metrics response wrappers', () => {
  assert.deepEqual(normalizeVersion({ manager_version: '2026.09.05' }), {
    managerVersion: '2026.09.05',
  });

  const metrics = normalizeIncidentMetrics({
    data: {
      incident_count: 3,
      mean_localization_seconds: 12.5,
      wrong_closure_count: 1,
      repeated_action_count: 2,
      recommendation_success_rate: 0.5,
      audit_required_event_count: 4,
      complete_audit_event_count: 3,
      audit_evidence_completeness: 0.75,
    },
  });
  assert.equal(metrics.meanLocalizationSeconds, 12.5);
  assert.equal(metrics.recommendationSuccessRate, 0.5);
  assert.equal(metrics.auditEvidenceCompleteness, 0.75);
});

test('builds a runtime snapshot and counts active incidents', () => {
  const snapshot = buildRuntimeSnapshot({
    health: { status: 'ok', checked_at: '2026-09-05T08:00:00Z', checks: [] },
    version: { manager_version: 'v1' },
    metrics: { incident_count: 2 },
    incidents: [
      { id: 'inc-1', status: 'investigating' },
      { id: 'inc-2', status: 'resolved' },
    ],
  });

  assert.equal(snapshot.overallStatus, 'ok');
  assert.equal(snapshot.activeIncidentCount, 1);
  assert.equal(snapshot.totalIncidentCount, 2);
  assert.equal(snapshot.version.managerVersion, 'v1');
});
