export const SITE = {
  name: 'OpsKeeper',
  shortName: 'OpsKeeper',
  tagline: 'Auditable operations for multi-agent incident response.',
  description:
    'OpsKeeper connects alert intake, evidence collection, root-cause analysis, human approval, narrowly authorized recovery, independent verification, and post-incident learning in one closed loop.',
  url: 'https://opskeeper.dev',
  repo: 'https://github.com/vincent-wuhan/opskeeper',
  repoShort: 'louloulin/opskeeper',
  license: 'Apache-2.0',
  version: '0.6.0',
} as const;

export type NavLink = { label: string; href: string };

export const PRIMARY_NAV: NavLink[] = [
  { label: 'Platform', href: '/platform' },
  { label: 'Workers', href: '/workers' },
  { label: 'Use cases', href: '/use-cases' },
  { label: 'Integrations', href: '/integrations' },
  { label: 'Docs', href: '/docs' },
  { label: 'Open source', href: '/open-source' },
  { label: 'Roadmap', href: '/roadmap' },
];

export const SECONDARY_NAV: NavLink[] = [
  { label: 'FAQ', href: '/faq' },
  { label: 'Changelog', href: '/changelog' },
  { label: 'Brand', href: '/brand' },
  { label: 'GitHub', href: SITE.repo },
];
