// routes.test.ts — guards the command palette (⌘P) against the failure that
// actually shipped once: the catalog was hand-maintained, the app grew nine
// more navigable pages, and ⌘P silently could not find them. Nothing failed —
// the palette just quietly stopped being a way to navigate.
//
// The test reads the real navigation sources (App.tsx routes + Sidebar.tsx
// nav items) rather than restating a list, because a restated list is the
// same hand-maintenance that rotted. Adding a page to the app now fails here
// until the palette learns about it.

import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
import { APP_ROUTES, scoreRoute } from '@/lib/routes';

const here = dirname(fileURLToPath(import.meta.url));
const srcDir = resolve(here, '..');
const read = (rel: string) => readFileSync(resolve(srcDir, rel), 'utf8');

const appSource = read('App.tsx');
const sidebarSource = read('components/Sidebar.tsx');

// The palette's own paths. A route with a query ("/devices?roles=server") is
// cataloged as its own entry, so this is an exact set, not a prefix match.
const cataloged = new Set(APP_ROUTES.map((r) => r.path));

describe('command palette catalog', () => {
  it('has no duplicate paths', () => {
    const seen = new Set<string>();
    const dupes = APP_ROUTES.map((r) => r.path).filter((p) => {
      if (seen.has(p)) return true;
      seen.add(p);
      return false;
    });
    expect(dupes).toEqual([]);
  });

  it('covers every page the sidebar links to', () => {
    // SidebarNavItem to="/x" is navigation an operator can actually see.
    const sidebarPaths = [...sidebarSource.matchAll(/SidebarNavItem\s+to="([^"]+)"/g)].map((m) => m[1]);
    expect(sidebarPaths.length).toBeGreaterThan(10);
    const missing = sidebarPaths.filter((p) => !cataloged.has(p));
    expect(missing).toEqual([]);
  });

  it('covers every navigable leaf route declared in App.tsx', () => {
    const leaves = [...appSource.matchAll(/<Route\s+path="([^"]+)"\s+element/g)].map((m) => m[1]);
    // Redirects are aliases, not pages: there is nothing to search for.
    const redirects = new Set(
      [...appSource.matchAll(/<Route\s+path="([^"]+)"\s+element=\{<Navigate/g)].map((m) => m[1]),
    );
    // Layout routes are containers, not destinations: /settings and /admin
    // render a rail around their children, so they are reached through a
    // child (/settings/llm, /admin/users) and have no page of their own to
    // search for. Detected structurally rather than listed, so a new layout
    // is classified without editing this test.
    const containers = new Set(
      [...appSource.matchAll(/<Route\s+path="([^"]+)"\s+element=\{\{?<(\w*Layout)\b/g)].map((m) => m[1]),
    );
    // /login is not a destination either — it only renders for an
    // unauthenticated visitor, who cannot reach the palette to search it.
    const concrete = leaves.filter(
      (p) =>
        p.startsWith('/') &&
        !p.includes(':') &&
        !redirects.has(p) &&
        !containers.has(p) &&
        p !== '/login',
    );
    const missing = concrete.filter((p) => !cataloged.has(p));
    expect(missing).toEqual([]);
  });

  it('gives every entry a label it can be found by', () => {
    for (const route of APP_ROUTES) {
      expect(route.label, `route ${route.path} has no label`).toBeTruthy();
      // A route that scores -1 against its own label can never be found by
      // typing the name shown on screen.
      expect(scoreRoute(route.label, route), `route ${route.path} unsearchable`).toBeGreaterThanOrEqual(0);
    }
  });
});
