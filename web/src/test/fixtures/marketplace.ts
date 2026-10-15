import type {
  CapabilityDeclaration,
  CatalogEntry,
  InstalledPack,
  RegistryEntry,
} from '@/api/marketplace';

// Shared fixtures for the marketplace UI tests. Backed by the wire shape
// returned by /v1/marketplace/* — see web/src/api/marketplace.ts.

export const etcdCapabilities: CapabilityDeclaration = {
  pack_id: 'etcd-troubleshoot',
  version: '0.1.0',
  skills: [
    {
      name: 'etcd.health',
      scope: 'manager',
      requires: { bins: ['etcdctl'], config: ['ETCD_ENDPOINTS'] },
      tool_classes: ['read'],
    },
  ],
  agent_count: 1,
  summary: {
    tool_classes: ['read', 'write'],
    bins: ['etcdctl'],
    config_keys: ['ETCD_ENDPOINTS'],
  },
};

export const etcdPack: InstalledPack = {
  id: 1,
  pack_id: 'etcd-troubleshoot',
  display_name: 'etcd troubleshoot',
  version: '0.1.0',
  source: 'local',
  source_url: '/var/lib/opskeeper/uploads/etcd-troubleshoot',
  install_path: '/var/lib/opskeeper/packs/etcd-troubleshoot',
  manifest_sha256: 'abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789',
  signature_state: 'unsigned',
  capabilities: etcdCapabilities,
  bindings: {},
  installed_by: 1,
  installed_at: '2026-05-07T10:00:00Z',
  updated_at: '2026-05-07T10:00:00Z',
};

export const registries: RegistryEntry[] = [
  { name: 'opskeeper-official', url: 'https://registry.opskeeper.io', allowed: true },
];

export const registriesEmpty: RegistryEntry[] = [];

// One usable registry (allow-listed + configured index url) so a registry
// catalog row can be uniquely attributed during install mapping.
export const registryOfficial: RegistryEntry[] = [
  { name: 'opskeeper-official', url: 'https://registry.opskeeper.io/index.json', allowed: true },
];

export function catalogFixture(over: Partial<CatalogEntry>): CatalogEntry {
  return {
    name: 'opskeeper-sre-readonly',
    version: '0.2.0',
    origin: 'registry',
    targets: ['edge'],
    safety_level: 'L1',
    capability: 'read',
    tool_count: 3,
    ...over,
  };
}
