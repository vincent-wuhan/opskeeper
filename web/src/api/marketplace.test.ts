import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import {
  catalogInstallSource,
  getMarketplaceCatalog,
  installableRegistries,
  type CatalogEntry,
  type RegistryEntry,
} from './marketplace';
import { server } from '@/test/msw-server';

const reg = (over: Partial<RegistryEntry>): RegistryEntry => ({
  name: 'opskeeper-official',
  url: 'https://registry.opskeeper.io/index.json',
  allowed: true,
  ...over,
});

const entry = (over: Partial<CatalogEntry>): CatalogEntry => ({
  name: 'opskeeper-sre-readonly',
  version: '0.2.0',
  origin: 'registry',
  targets: ['edge'],
  safety_level: 'L1',
  capability: 'read',
  tool_count: 3,
  ...over,
});

describe('installableRegistries', () => {
  it('keeps only allow-listed registries that carry a configured index url', () => {
    const got = installableRegistries([
      reg({ name: 'a', url: 'https://a/index.json' }),
      reg({ name: 'b', url: '' }), // no configured index
      reg({ name: 'c', allowed: false }), // not allowed
    ]);
    expect(got.map((r) => r.name)).toEqual(['a']);
  });
});

describe('catalogInstallSource', () => {
  it('maps a registry row to the registry install payload using entry.name as pack_id', () => {
    const src = catalogInstallSource(entry({}), [reg({})]);
    expect(src).toEqual({
      type: 'registry',
      registry: 'opskeeper-official',
      pack_id: 'opskeeper-sre-readonly',
      version: '0.2.0',
    });
  });

  it('refuses local-origin rows (tenant/system/builtin are already on disk)', () => {
    for (const origin of ['tenant', 'system', 'builtin'] as const) {
      expect(catalogInstallSource(entry({ origin }), [reg({})])).toBeNull();
    }
  });

  it('refuses a registry row when no registry is configured', () => {
    expect(catalogInstallSource(entry({}), [])).toBeNull();
    expect(catalogInstallSource(entry({}), [reg({ url: '' })])).toBeNull();
  });

  it('refuses a registry row it cannot attribute to exactly one registry', () => {
    const two = [reg({ name: 'a' }), reg({ name: 'b' })];
    expect(catalogInstallSource(entry({}), two)).toBeNull();
  });

  it('refuses a registry row with no version (registry install must pin)', () => {
    expect(catalogInstallSource(entry({ version: '' }), [reg({})])).toBeNull();
  });
});

describe('getMarketplaceCatalog', () => {
  it('parses the items/total envelope', async () => {
    server.use(
      http.get('/api/v1/marketplace/catalog', () =>
        HttpResponse.json({
          items: [
            {
              name: 'opskeeper-sre-readonly',
              version: '0.2.0',
              origin: 'registry',
              targets: ['edge'],
              safety_level: 'L1',
              capability: 'read',
              tool_count: 3,
            },
          ],
          total: 1,
        }),
      ),
    );
    const cat = await getMarketplaceCatalog();
    expect(cat.total).toBe(1);
    expect(cat.items[0]).toMatchObject({
      name: 'opskeeper-sre-readonly',
      origin: 'registry',
      version: '0.2.0',
    });
  });

  it('defaults total to the item count when the envelope omits it', async () => {
    server.use(
      http.get('/api/v1/marketplace/catalog', () =>
        HttpResponse.json({ items: [{ name: 'a', version: '1.0.0', targets: [], safety_level: 'L1', capability: 'read', tool_count: 1 }] }),
      ),
    );
    const cat = await getMarketplaceCatalog();
    expect(cat.total).toBe(1);
  });
});
