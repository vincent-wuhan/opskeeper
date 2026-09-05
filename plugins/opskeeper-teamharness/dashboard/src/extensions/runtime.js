const ACTIVE_STATUSES = new Set([
  'open',
  'in_progress',
  'investigating',
  'repairing',
  'verifying',
]);

export function normalizeHealthReport(input) {
  const report = input?.data || input;
  if (!report || typeof report !== 'object') return null;
  const checks = Array.isArray(report.checks) ? report.checks : [];
  return {
    status: report.status || 'unknown',
    checkedAt: report.checked_at || report.checkedAt || null,
    summary: {
      ok: report.summary?.ok ?? countStatus(checks, 'ok'),
      degraded: report.summary?.degraded ?? countStatus(checks, 'degraded'),
      failed: report.summary?.failed ?? countStatus(checks, 'failed'),
      unknown: report.summary?.unknown ?? countStatus(checks, 'unknown'),
    },
    checks: checks.map((check) => ({
      id: check.id || '',
      group: check.group || 'other',
      label: check.label || check.id || 'Unknown',
      status: check.status || 'unknown',
      message: check.message || '',
      durationMs: check.duration_ms ?? check.durationMs ?? null,
    })),
  };
}

export function normalizeVersion(input) {
  const version = input?.data || input;
  if (!version || typeof version !== 'object') return null;
  return {
    managerVersion: version.manager_version || version.managerVersion || version.version || null,
  };
}

export function normalizeIncidentMetrics(input) {
  const metrics = input?.data || input;
  if (!metrics || typeof metrics !== 'object') return null;
  return {
    incidentCount: numberOrNull(metrics.incident_count ?? metrics.incidentCount),
    meanLocalizationSeconds: numberOrNull(
      metrics.mean_localization_seconds ?? metrics.meanLocalizationSeconds,
    ),
    wrongClosureCount: numberOrNull(
      metrics.wrong_closure_count ?? metrics.wrongClosureCount,
    ),
    repeatedActionCount: numberOrNull(
      metrics.repeated_action_count ?? metrics.repeatedActionCount,
    ),
    approvedRecommendationCount: numberOrNull(
      metrics.approved_recommendation_count ?? metrics.approvedRecommendationCount,
    ),
    recoveryConfirmedRecommendationCount: numberOrNull(
      metrics.recovery_confirmed_recommendation_count
        ?? metrics.recoveryConfirmedRecommendationCount,
    ),
    recommendationSuccessRate: numberOrNull(
      metrics.recommendation_success_rate ?? metrics.recommendationSuccessRate,
    ),
    auditRequiredEventCount: numberOrNull(
      metrics.audit_required_event_count ?? metrics.auditRequiredEventCount,
    ),
    completeAuditEventCount: numberOrNull(
      metrics.complete_audit_event_count ?? metrics.completeAuditEventCount,
    ),
    auditEvidenceCompleteness: numberOrNull(
      metrics.audit_evidence_completeness ?? metrics.auditEvidenceCompleteness,
    ),
  };
}

export function buildRuntimeSnapshot({ health, version, metrics, incidents = [] }) {
  const normalizedHealth = normalizeHealthReport(health);
  const normalizedVersion = normalizeVersion(version);
  const normalizedMetrics = normalizeIncidentMetrics(metrics);
  const incidentList = Array.isArray(incidents) ? incidents : [];
  const activeIncidents = incidentList.filter((incident) =>
    ACTIVE_STATUSES.has(String(incident?.status || '').toLowerCase()),
  );

  return {
    health: normalizedHealth,
    version: normalizedVersion,
    metrics: normalizedMetrics,
    overallStatus: normalizedHealth?.status || 'unknown',
    activeIncidentCount: activeIncidents.length,
    totalIncidentCount: incidentList.length,
    latestIncidents: incidentList.slice(0, 8),
  };
}

function countStatus(checks, status) {
  return checks.filter((check) => check?.status === status).length;
}

function numberOrNull(value) {
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : null;
}
