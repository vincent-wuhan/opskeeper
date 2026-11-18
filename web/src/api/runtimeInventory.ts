// Runtime inventory client — talks to GET /v1/runtimes (protected, any
// authenticated role; per-role field hiding is a presentation decision in
// the page, not a backend authorization change).
//
// The backend is core/manager/server/runtimemgmt/http.go, and the shapes
// mirror Go's runtimeView. The inventory is the manager-authoritative list
// of every runtime surface: registry rows (self-registered via
// POST /v1/runtimes) merged with live projections from the five built-in
// read-only observers (edge / chat worker / agentteams worker / plugin
// manager / preview-pg). A row's `source` says which side won.
import { request } from './client';

// `string & Record<never, never>` keeps editor completion on the five
// closed-set literals while still accepting any other string the backend
// may grow — the same spelling ReleaseNodeState uses in pluginReleases.ts.
export type RuntimeKind =
  | 'node_edge'
  | 'chat_worker'
  | 'agentteams_worker'
  | 'plugin_manager'
  | 'pool_preview'
  | (string & Record<never, never>);

export type RuntimeHealth =
  | 'starting'
  | 'online'
  | 'degraded'
  | 'recently_lost'
  | 'offline'
  | 'unknown'
  | (string & Record<never, never>);

export type RuntimeView = {
  runtime_id: string;
  kind: RuntimeKind;
  profile: string;
  owner: string;
  deployment_location: string;
  labels: Record<string, string>;
  observed_version: string;
  version_source: string;
  artifact_digest: string;
  capability_digest: string;
  capabilities: Record<string, unknown>;
  health: RuntimeHealth;
  health_reason: string;
  last_heartbeat_at: string | null;
  total_slots: number;
  free_slots: number;
  active_tasks: number;
  command_ack_count: number;
  source: string;
};

export type InventoryResponse = {
  total: number;
  items: RuntimeView[];
};

// The wire payload tolerates null / missing anywhere (#97 pattern): the
// backend uses omitempty on most fields, and an older manager must not
// crash a newer page.
export type InventoryResponsePayload = Partial<InventoryResponse> & {
  items?: RuntimeView[] | null;
};

function str(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function num(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}

function record(value: unknown): Record<string, unknown> {
  return value && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function normalizeRuntime(raw: Partial<RuntimeView> | null | undefined): RuntimeView {
  const v = raw ?? {};
  return {
    runtime_id: str(v.runtime_id),
    kind: str(v.kind),
    profile: str(v.profile),
    owner: str(v.owner),
    deployment_location: str(v.deployment_location),
    labels: record(v.labels) as Record<string, string>,
    observed_version: str(v.observed_version),
    version_source: str(v.version_source),
    artifact_digest: str(v.artifact_digest),
    capability_digest: str(v.capability_digest),
    capabilities: record(v.capabilities),
    health: str(v.health) || 'unknown',
    health_reason: str(v.health_reason),
    last_heartbeat_at: v.last_heartbeat_at ?? null,
    total_slots: num(v.total_slots),
    free_slots: num(v.free_slots),
    active_tasks: num(v.active_tasks),
    command_ack_count: num(v.command_ack_count),
    source: str(v.source),
  };
}

export function normalizeInventoryResponse(payload: InventoryResponsePayload | null | undefined): InventoryResponse {
  const items = Array.isArray(payload?.items) ? payload!.items! : [];
  return {
    total: num(payload?.total),
    items: items.map(normalizeRuntime),
  };
}

export async function getRuntimeInventory(): Promise<InventoryResponse> {
  return normalizeInventoryResponse(
    await request<InventoryResponsePayload>('GET', '/runtimes'),
  );
}
