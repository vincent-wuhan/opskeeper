import type { NavLink } from './site';

export const SITE_ZH = {
  name: 'OpsKeeper',
  shortName: 'OpsKeeper',
  tagline: '授权可控、全程可审计的多智能体运维事件响应平台。',
  description:
    'OpsKeeper 把告警接入、证据采集、根因分析、人工审批、窄域授权恢复、独立验证、事后复盘七个环节，串成同一条闭环。',
  url: 'https://opskeeper.dev',
  repo: 'https://github.com/vincent-wuhan/opskeeper',
  repoShort: 'vincent-wuhan/opskeeper',
  license: 'Apache-2.0',
  version: '0.6.0',
} as const;

export const PRIMARY_NAV_ZH: NavLink[] = [
  { label: '平台', href: '/zh/platform' },
  { label: '工作流', href: '/zh/workers' },
  { label: '应用场景', href: '/zh/use-cases' },
  { label: '在线演示', href: '/zh/demo' },
  { label: '集成', href: '/zh/integrations' },
  { label: '文档', href: '/zh/docs' },
  { label: '开源', href: '/zh/open-source' },
  { label: '路线图', href: '/zh/roadmap' },
];

export const SECONDARY_NAV_ZH: NavLink[] = [
  { label: '常见问题', href: '/zh/faq' },
  { label: '更新日志', href: '/zh/changelog' },
  { label: '品牌资产', href: '/zh/brand' },
  { label: 'GitHub', href: SITE_ZH.repo },
];

export const FOOTER_COLS_ZH: { title: string; links: NavLink[] }[] = [
  {
    title: '产品',
    links: [
      { label: '平台', href: '/zh/platform' },
      { label: '工作流角色', href: '/zh/workers' },
      { label: '闭环', href: '/zh/platform#closed-loop' },
      { label: '在线演示', href: '/zh/demo' },
      { label: '安全边界', href: '/zh/security' },
      { label: '路线图', href: '/zh/roadmap' },
      { label: '更新日志', href: '/zh/changelog' },
    ],
  },
  {
    title: '文档',
    links: [
      { label: '快速开始', href: '/zh/docs/getting-started' },
      { label: '架构', href: '/zh/docs/architecture' },
      { label: '部署', href: '/zh/docs/deployment' },
      { label: '运维', href: '/zh/docs/operations' },
      { label: 'API 参考', href: '/zh/docs/api' },
      { label: '集成', href: '/zh/docs/integrations' },
    ],
  },
  {
    title: '项目',
    links: [
      { label: 'GitHub', href: SITE_ZH.repo },
      { label: '品牌资产', href: '/zh/brand' },
      { label: '安全策略', href: '/zh/security' },
      { label: '商标政策', href: '/zh/trademark' },
      { label: '许可证', href: 'https://github.com/vincent-wuhan/opskeeper/blob/main/LICENSE' },
      { label: '行为准则', href: 'https://github.com/vincent-wuhan/opskeeper/blob/main/CODE_OF_CONDUCT.md' },
    ],
  },
];
