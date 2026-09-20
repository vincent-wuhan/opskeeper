import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

import {
  buildRuntimeSnapshot,
  normalizeHealthReport,
  normalizeIncidentMetrics,
  normalizeIncidentList,
  normalizeVersion,
} from './runtime.js';

import {
  buildInvestigationRequest,
  opskeeperApi,
  resolvePluginManagerBase,
  xhrTransport,
} from './api.js';
import { normalizeOpskeeperTab } from './tabs.js';

test('uses the foreground token for muted plugin text', () => {
  const extensionsDir = fileURLToPath(new URL('./', import.meta.url));
  const mutedTextPattern = /color:\s*['`]var\(--muted\)['`]/u;
  const violations = [];

  for (const entry of readdirSync(extensionsDir, { withFileTypes: true })) {
    if (!entry.isFile() || !/\.jsx?$/u.test(entry.name)) continue;
    const source = readFileSync(path.join(extensionsDir, entry.name), 'utf8');
    if (mutedTextPattern.test(source)) violations.push(entry.name);
  }

  assert.deepEqual(violations, []);
});

test('keeps installed-plugin descriptions readable in both themes', () => {
  const extensionsDir = fileURLToPath(new URL('./', import.meta.url));
  const source = readFileSync(path.join(extensionsDir, 'install-view.jsx'), 'utf8');
  assert.match(source, /fontSize:\s*11,\s*color:\s*['`]var\(--foreground\)['`],\s*opacity:\s*0\.76/u);
});

test('scopes adaptive text contrast to every extension entry point', () => {
  const extensionsDir = fileURLToPath(new URL('./', import.meta.url));
  const entryPoints = [
    'dashboard-widget.jsx',
    'detail-panel.jsx',
    'route.jsx',
    'unified-route.jsx',
  ];

  for (const entryPoint of entryPoints) {
    const source = readFileSync(path.join(extensionsDir, entryPoint), 'utf8');
    assert.match(
      source,
      /opskeeperPluginThemeStyle/u,
      `${entryPoint} must apply the plugin text contrast scope`,
    );
  }
});

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

test('normalizes incident list response wrappers', () => {
  const incidents = [{ id: 'inc-1', status: 'open' }];

  assert.deepEqual(normalizeIncidentList({ items: incidents }), incidents);
  assert.deepEqual(normalizeIncidentList({ incidents }), incidents);
  assert.deepEqual(normalizeIncidentList({ data: incidents }), incidents);
  assert.deepEqual(normalizeIncidentList(incidents), incidents);
  assert.deepEqual(normalizeIncidentList({ total: 1 }), []);
});

test('routes plugin manager calls around the port-13000 dashboard fallback', () => {
  assert.equal(
    resolvePluginManagerBase({ port: '13000', protocol: 'http:', hostname: 'plugin-manager.example.test' }),
    'http://plugin-manager.example.test/api/v1/plugins',
  );
  assert.equal(resolvePluginManagerBase({ port: '', protocol: 'http:', hostname: 'dashboard.example.test' }), '/api/v1/plugins');
  assert.equal(resolvePluginManagerBase({ port: '13001', protocol: 'http:', hostname: 'dashboard.example.test' }), '/api/v1/plugins');
});

test('builds a backend-compatible investigation request', () => {
  assert.deepEqual(buildInvestigationRequest({
    id: 35,
    rule_key: 'pg-pool-exhaustion',
    target_id: '900001',
    target_type: 'edge',
    labels: { source_id: 'pool-fixture' },
  }), {
    incident_id: '35',
    alert_group: ['pg-pool-exhaustion'],
    correlation_hints: {
      source_id: 'pool-fixture',
      device_id: '900001',
      resource_type: 'edge',
      incident_id: '35',
      target: '900001',
    },
  });
});

test('propagates PG pool incident bindings into investigation hints', () => {
  assert.deepEqual(buildInvestigationRequest({
    id: 36,
    rule_key: 'pg-pool-exhaustion',
    labels: {
      incident_id: 'incident-live-pool-smoke',
      target: 'pg:pool-fixture',
      scenario: 'pg-pool-exhaustion',
      pool_manifest_id: '7f5c60e593e68840f974789166cc3374',
      fault_family: 'capacity/connection_pool',
    },
  }), {
    incident_id: '36',
    alert_group: ['pg-pool-exhaustion'],
    correlation_hints: {
      incident_id: 'incident-live-pool-smoke',
      target: 'pg:pool-fixture',
      pool_manifest_id: '7f5c60e593e68840f974789166cc3374',
      fault_family: 'capacity/connection_pool',
      source_id: 'dashboard',
      device_id: '36',
      resource_type: 'pg',
    },
  });
});

test('runtime readback uses same-origin XMLHttpRequest requests', async () => {
  const originalCreateRequest = xhrTransport.createRequest;
  const requests = [];
  globalThis.XMLHttpRequest = function StubXMLHttpRequest() {
    const request = {
      status: 200,
      responseText: JSON.stringify({ manager_version: 'release20260905' }),
      open(method, url) {
        requests.push({ method, url });
      },
      setRequestHeader() {},
      getResponseHeader() {
        return 'application/json';
      },
      send() {
        request.onload();
      },
    };
    return request;
  };

  try {
    xhrTransport.createRequest = () => new globalThis.XMLHttpRequest();
    const version = await opskeeperApi.getVersion();
    assert.equal(version.manager_version, 'release20260905');
    assert.deepEqual(requests, [{ method: 'GET', url: '/api/opskeeper/version' }]);
  } finally {
    xhrTransport.createRequest = originalCreateRequest;
  }
});

test('deduplicates concurrent investigations for one incident', async () => {
  const originalCreateRequest = xhrTransport.createRequest;
  const requests = [];
  globalThis.XMLHttpRequest = function StubXMLHttpRequest() {
    const request = {
      status: 200,
      responseText: JSON.stringify({ data: { incident_id: 'inc-dedupe-1' } }),
      open(method, url) {
        requests.push({ method, url });
      },
      setRequestHeader() {},
      getResponseHeader() {
        return 'application/json';
      },
      send() {
        queueMicrotask(() => request.onload());
      },
    };
    return request;
  };

  try {
    xhrTransport.createRequest = () => new globalThis.XMLHttpRequest();
    const first = opskeeperApi.investigate({ incident_id: 'inc-dedupe-1' });
    const second = opskeeperApi.investigate({ incident_id: 'inc-dedupe-1' });

    assert.equal(requests.length, 1);
    const [firstResult, secondResult] = await Promise.all([first, second]);
    assert.deepEqual(firstResult, secondResult);

    const repeated = opskeeperApi.investigate({ incident_id: 'inc-dedupe-1' });
    assert.equal(requests.length, 2);
    await repeated;
  } finally {
    xhrTransport.createRequest = originalCreateRequest;
  }
});

test('normalizes the unified OpsKeeper entry tab', () => {
  assert.equal(normalizeOpskeeperTab('runtime'), 'runtime');
  assert.equal(normalizeOpskeeperTab('archive'), 'archive');
  assert.equal(normalizeOpskeeperTab('plugins'), 'plugins');
  assert.equal(normalizeOpskeeperTab('integration'), 'integration');
  assert.equal(normalizeOpskeeperTab('unknown'), 'diagnostics');
});

test('archive readback uses the Manager proxy endpoint', async () => {
  const originalCreateRequest = xhrTransport.createRequest;
  const requests = [];
  globalThis.XMLHttpRequest = function StubXMLHttpRequest() {
    const request = {
      status: 200,
      responseText: JSON.stringify({ data: { incident_id: 'inc-archive-1' } }),
      open(method, url) {
        requests.push({ method, url });
      },
      setRequestHeader() {},
      getResponseHeader() {
        return 'application/json';
      },
      send() {
        request.onload();
      },
    };
    return request;
  };

  try {
    xhrTransport.createRequest = () => new globalThis.XMLHttpRequest();
    const response = await opskeeperApi.getIncidentArchive('inc archive/1');
    assert.equal(response.data.incident_id, 'inc-archive-1');
    assert.deepEqual(requests, [{
      method: 'GET',
      url: '/api/opskeeper/incidents/inc%20archive%2F1/archive',
    }]);
    await assert.rejects(opskeeperApi.getIncidentArchive(''), /incident_id is required/);
  } finally {
    xhrTransport.createRequest = originalCreateRequest;
  }
});

test('archive index readback uses the Manager proxy endpoint', async () => {
  const originalCreateRequest = xhrTransport.createRequest;
  const requests = [];
  globalThis.XMLHttpRequest = function StubXMLHttpRequest() {
    const request = {
      status: 200,
      responseText: JSON.stringify({ items: [{ incident_id: 'inc-archive-index' }] }),
      open(method, url) {
        requests.push({ method, url });
      },
      setRequestHeader() {},
      getResponseHeader() {
        return 'application/json';
      },
      send() {
        request.onload();
      },
    };
    return request;
  };

  try {
    xhrTransport.createRequest = () => new globalThis.XMLHttpRequest();
    const response = await opskeeperApi.listArchiveIncidents();
    assert.equal(response.items[0].incident_id, 'inc-archive-index');
    assert.deepEqual(requests, [{ method: 'GET', url: '/api/opskeeper/incidents/archive-index' }]);
  } finally {
    xhrTransport.createRequest = originalCreateRequest;
  }
});

test('repair preview summary uses the Manager compact endpoint', async () => {
  const originalCreateRequest = xhrTransport.createRequest;
  const requests = [];
  globalThis.XMLHttpRequest = function StubXMLHttpRequest() {
    const request = {
      status: 200,
      responseText: JSON.stringify({ data: { run_id: 'run-preview-1' } }),
      open(method, url) { requests.push({ method, url }); },
      setRequestHeader() {},
      getResponseHeader() { return 'application/json'; },
      send() { request.onload(); },
    };
    return request;
  };

  try {
    xhrTransport.createRequest = () => new globalThis.XMLHttpRequest();
    const response = await opskeeperApi.getIncidentRepairPreviewSummary('inc preview/1');
    assert.equal(response.data.run_id, 'run-preview-1');
    assert.deepEqual(requests, [{
      method: 'GET',
      url: '/api/opskeeper/incidents/inc%20preview%2F1/repair-preview-summary',
    }]);
    await assert.rejects(opskeeperApi.getIncidentRepairPreviewSummary(''), /incident_id is required/);
  } finally {
    xhrTransport.createRequest = originalCreateRequest;
  }
});
