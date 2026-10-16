// getVersionDeployment — pulls the rich deployment view
// (Manager / Worker / plugin / server composition,
// health, dependencies, one worked recovery example). Reuses the
// generic request client so auth + error handling stay consistent.
import { request } from './client';

export type ManagerVersion = string;

export interface PluginManifest {
  apiVersion?: string;
  kind?: string;
  id: string;
  name: string;
  version: string;
  description?: string;
  author?: string;
  entry?: Record<string, string>;
  dashboardVersion?: string;
  min_version?: string;
  extensionPoints?: string[];
  permissions?: string[];
  dependencies?: string[];
}

export interface SkillVersion {
  id: string;
  version: string;
  role: string;
  path: string;
}

export interface HealthSummary {
  overall: string;
  db?: string;
  prom?: string;
  logs?: string;
  traces?: string;
  llm?: string;
  note?: string;
}

export interface DeployDependency {
  name: string;
  configured: boolean;
  required: boolean;
  note?: string;
}

export interface TimelinePhaseLite {
  phase: string;
  phase_label: string;
  status: string;
  worker_role: string;
  skill_version: string;
  duration?: string;
  summary?: string;
}

export interface RecoveryOperation {
  incident_id: string;
  title: string;
  fault_family: string;
  phases_observed: number;
  phases_expected: number;
  recovery_signal: boolean;
  closed: boolean;
  duration_sec: number;
  audit_kinds: string[];
  evidence?: Record<string, string>;
  phases?: TimelinePhaseLite[];
}

export interface DeploymentResponse {
  manager_version: ManagerVersion;
  go_version: string;
  os_arch: string;
  build_at?: string;
  plugin: PluginManifest;
  skills: SkillVersion[];
  health: HealthSummary;
  dependencies: DeployDependency[];
  recovery_example: RecoveryOperation;
  generated_at: string;
}

export type DeploymentResponsePayload = Partial<DeploymentResponse> & {
  plugin?: PluginManifest | null;
  skills?: SkillVersion[] | null;
  health?: HealthSummary | null;
  dependencies?: DeployDependency[] | null;
  recovery_example?: RecoveryOperation | null;
};

function text(value: string | null | undefined, fallback = '—') {
  return value || fallback;
}

function list<T>(value: T[] | null | undefined) {
  return Array.isArray(value) ? value : [];
}

export function normalizeDeploymentResponse(payload: DeploymentResponsePayload): DeploymentResponse {
  const recovery = payload.recovery_example;
  return {
    manager_version: text(payload.manager_version),
    go_version: text(payload.go_version),
    os_arch: text(payload.os_arch),
    build_at: payload.build_at || undefined,
    plugin: payload.plugin ?? {
      id: 'unknown',
      name: 'Unknown plugin',
      version: 'unknown',
    },
    skills: list(payload.skills),
    health: payload.health ?? { overall: 'unknown' },
    dependencies: list(payload.dependencies),
    recovery_example: recovery ?? {
      incident_id: 'unknown',
      title: 'No worked recovery example',
      fault_family: 'unknown',
      phases_observed: 0,
      phases_expected: 0,
      recovery_signal: false,
      closed: false,
      duration_sec: 0,
      audit_kinds: [],
      evidence: {},
      phases: [],
    },
    generated_at: text(payload.generated_at, ''),
  };
}

export async function getVersionDeployment() {
  return normalizeDeploymentResponse(
    await request<DeploymentResponsePayload>('GET', '/version/deployment'),
  );
}
