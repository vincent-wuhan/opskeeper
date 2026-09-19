import type { MetadataRoute } from 'next';
import { SITE } from '@/lib/site';

const EN_PATHS = [
  '/',
  '/platform',
  '/workers',
  '/use-cases',
  '/demo',
  '/integrations',
  '/security',
  '/open-source',
  '/faq',
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

export default function sitemap(): MetadataRoute.Sitemap {
  const now = new Date();
  const base = SITE.url;

  const liveIncidentEntries: MetadataRoute.Sitemap = ['/live-incident', '/en/live-incident'].map((path) => ({
    url: `${base}${path}`,
    lastModified: now,
    changeFrequency: 'weekly',
    priority: 0.9,
    alternates: {
      languages: {
        en: `${base}/en/live-incident`,
        'zh-CN': `${base}/live-incident`,
      },
    },
  }));

  const localeEntries: MetadataRoute.Sitemap = EN_PATHS.flatMap((p) => {
    if (p === '/') {
      return [
        {
          url: base,
          lastModified: now,
          changeFrequency: 'weekly',
          priority: 1,
          alternates: {
            languages: {
              en: `${base}/en`,
              'zh-CN': base,
            },
          },
        },
        {
          url: `${base}/en`,
          lastModified: now,
          changeFrequency: 'weekly',
          priority: 0.9,
          alternates: {
            languages: {
              en: `${base}/en`,
              'zh-CN': base,
            },
          },
        },
      ];
    }

    return [
      {
        url: `${base}${p}`,
        lastModified: now,
        changeFrequency: 'weekly',
        priority: 0.7,
        alternates: {
          languages: {
            en: `${base}${p}`,
            'zh-CN': `${base}/zh${p}`,
          },
        },
      },
      {
        url: `${base}/zh${p}`,
        lastModified: now,
        changeFrequency: 'weekly',
        priority: 0.6,
        alternates: {
          languages: {
            en: `${base}${p}`,
            'zh-CN': `${base}/zh${p}`,
          },
        },
      },
    ];
  });

  return [...localeEntries, ...liveIncidentEntries];
}
