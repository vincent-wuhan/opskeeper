import type { MetadataRoute } from 'next';
import { SITE } from '@/lib/site';

export default function sitemap(): MetadataRoute.Sitemap {
  const now = new Date();
  const base = SITE.url;
  const paths = [
    '/',
    '/platform',
    '/workers',
    '/integrations',
    '/security',
    '/roadmap',
    '/changelog',
    '/brand',
    '/trademark',
    '/docs',
    '/docs/getting-started',
    '/docs/architecture',
    '/docs/deployment',
    '/docs/operations',
    '/docs/security-model',
    '/docs/plugins',
    '/docs/integrations',
    '/docs/api',
    '/docs/workflow-catalog',
    '/docs/harness-guide',
    '/docs/migration',
  ];
  return paths.map((p) => ({
    url: `${base}${p}`,
    lastModified: now,
    changeFrequency: 'weekly',
    priority: p === '/' ? 1 : 0.7,
  }));
}
