import { request } from './client';

// Plugin release client — talks to /v1/plugins/releases (admin only).
//
// The backend is core/manager/server/plugin/http.go, and the shapes
// mirror Go's release.Status. A release is a job that outlives the request
// that started it, so the console polls status and calls advance / halt /
// rollback separately rather than expecting one call to do all three.
//
// The one field worth reading carefully is `pending`: a node the wave has
// not heard from. It is NOT a failure — the wave cannot advance past it —
// and rendering the two the same way is how an operator ends up restarting
// a release that is simply waiting.

// `string & Record<never, never>` is the standard spelling of "these four
// literals, but any other string is still allowed and still keeps its
// editor completion". It is spelled with Record rather than `{}` because
// `{}` means "any non-nullish value" to the type system and to the linter
// alike, and the whole point of the intersection is that it is NOT that.
export type ReleaseNodeState =
  | 'installed'
  | 'refused'
  | 'failed'
  | 'pending'
  | (string & Record<never, never>);

export interface PluginInfo {
  name: string;
  version: string;
  digest?: string;
}

export interface ReleaseStatus {
  plugin: string;
  version: string;
  /** 1-based index of the wave currently out. */
  wave: number;
  /** Total waves the plan has. */
  waves: number;
  /** Nodes the current wave has not heard from. Blocks `advance`. */
  pending?: number[] | null;
  /** Nodes that answered with a failure. Counted as answered, so they do
   *  not block the wave — but they are why it moved early. */
  failed?: number[] | null;
  installed?: Record<string, PluginInfo> | null;
  summary?: string;
  halted?: boolean;
  rolled_back?: boolean;
  reason?: string;
}

export interface ReleaseList {
  items: ReleaseStatus[];
  total: number;
}

export interface StartReleaseRequest {
  plugin: string;
  version: string;
  url: string;
  sha256: string;
  signature: string;
  /** Optional; identifies which key in the node's trust store signed it. */
  key_id?: string;
  /** Required — the backend refuses to default it. A package the operator
   *  chose deliberately must not silently get a canary it never asked for,
   *  and a package that asked for one must not go out in a single wave
   *  because the field was empty. */
  strategy: 'rolling' | 'pin';
  /** Optional subset of edge ids. Empty means the whole fleet. */
  nodes?: number[];
}

export interface StartReleaseResponse extends ReleaseStatus {
  waves: number;
}

export interface AdvanceResponse extends ReleaseStatus {
  /** False when the current wave is not accounted for. The release did not
   *  move, and the audit trail records that as a failure. */
  moved: boolean;
}

export async function listReleases(): Promise<ReleaseStatus[]> {
  const r = await request<ReleaseList>('GET', '/plugins/releases');
  return r.items ?? [];
}

export async function startRelease(
  body: StartReleaseRequest
): Promise<StartReleaseResponse> {
  return request<StartReleaseResponse>('POST', '/plugins/releases', body);
}

export async function getRelease(name: string): Promise<ReleaseStatus> {
  return request<ReleaseStatus>('GET', `/plugins/releases/${encodeURIComponent(name)}`);
}

export async function advanceRelease(name: string): Promise<AdvanceResponse> {
  return request<AdvanceResponse>('POST', `/plugins/releases/${encodeURIComponent(name)}/advance`);
}

/** halt stops the release and LEAVES what is installed installed. Taking it
 *  back off is rollback, a separate call, because an operator who has just
 *  watched a canary go bad may want the release stopped now and the
 *  decision about the canary taken calmly. */
export async function haltRelease(name: string, reason: string): Promise<ReleaseStatus> {
  return request<ReleaseStatus>('POST', `/plugins/releases/${encodeURIComponent(name)}/halt`, {
    reason,
  });
}

export async function rollbackRelease(name: string): Promise<ReleaseStatus> {
  return request<ReleaseStatus>('POST', `/plugins/releases/${encodeURIComponent(name)}/rollback`);
}

/**
 * Verdict is one node's answer in the compatibility matrix.
 *
 * There is no third state: a node that has not reported a version it can be
 * compared against is `hostable: false`, and `reason` says so. Counting it
 * as hostable would put it in the first canary wave and fail there, where
 * the failure costs a wave and a rollback rather than a table row.
 */
export interface CompatibilityVerdict {
  node_id: number;
  name?: string;
  edge_version?: string;
  pig_version?: string;
  hostable: boolean;
  /**
   * Which axis refused: 'version' (the edge build) or 'agent_version' (the
   * PiG the node runs). The same values the node's own review step reports,
   * so an operator reading "this node runs edge 0.7.2" here and the same
   * sentence on the node is reading one message rather than two that
   * happen to agree today.
   */
  step?: 'version' | 'agent_version' | '';
  reason?: string;
}

/** CompatibilityMatrix answers "which of my nodes can host this package".
 *
 *  `hostable` and `refused` partition the fleet rather than sitting in one
 *  list behind a flag, so the page cannot accidentally render a refusal as
 *  a pending row. */
export interface CompatibilityMatrix {
  plugin: string;
  version: string;
  min_edge_version?: string;
  min_pig_version?: string;
  hostable: CompatibilityVerdict[];
  refused: CompatibilityVerdict[];
}

/** getCompatibility asks the pre-flight question for one package.
 *
 *  The requirement is supplied by the caller rather than read from a
 *  manifest the manager holds — a release carries a URL, a digest and a
 *  signature, and the node fetches and reviews the package itself. The
 *  matrix echoes what it was asked, so asking about the wrong requirement
 *  produces a visibly wrong answer rather than a plausible one.
 *
 *  A manager with no version snapshot answers `not-wired`, and that error
 *  has to stay an error all the way to the console: rendering it as an
 *  empty matrix would tell an operator their whole fleet is ready when the
 *  control plane simply cannot tell. */
export async function getCompatibility(
  name: string,
  req: { version?: string; min_edge_version?: string; min_pig_version?: string }
): Promise<CompatibilityMatrix> {
  const qs = new URLSearchParams();
  if (req.version) qs.set('version', req.version);
  if (req.min_edge_version) qs.set('min_edge_version', req.min_edge_version);
  if (req.min_pig_version) qs.set('min_pig_version', req.min_pig_version);
  const suffix = qs.toString() ? `?${qs.toString()}` : '';
  return request<CompatibilityMatrix>(
    'GET',
    `/plugins/${encodeURIComponent(name)}/compatibility${suffix}`
  );
}

/** InstalledOnNode is what one node is actually running.
 *
 *  `packages` is always an array on the wire, never null. That is the
 *  handler's doing, not a JSON detail: a node that runs nothing and a node
 *  the manager never reached are different HTTP answers (200 with `[]`
 *  versus 502), and a null in the success body would put them back
 *  together in the console — `null` reads as absent, and absent reads as
 *  "nothing is installed", which is a claim about a node nobody asked. */
export interface InstalledOnNode {
  edge_id: number;
  packages: PluginInfo[];
}

/**
 * getNodeInstalled reports one node's active package set.
 *
 * Per node, not fleet-wide, and that is the server's decision as much as
 * this client's: the answer comes from one tunnel call per node, so a
 * fleet-wide endpoint would hold the request open for as long as the
 * slowest node and would have to invent a partial answer for the rest.
 * Asking about one node also means a failure is that node's alone.
 *
 * The three failures are distinct and the console branches on them:
 * 403 (not admin), 503 (this manager has no tunnel, so it cannot tell),
 * and 502 (this node did not answer, so it is the node's problem).
 */
export async function getNodeInstalled(edgeId: number): Promise<InstalledOnNode> {
  return request<InstalledOnNode>(
    'GET',
    `/plugins/nodes/${edgeId}/installed`
  );
}
