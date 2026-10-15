// Static catalog of in-app routes used by the command palette (⌘P).
// Kept out of Sidebar.tsx so non-sidebar callers (palette, future
// onboarding hints, etc.) can import without dragging in the full
// sidebar component graph.
//
// Each entry has:
//   - path:    the route to navigate to on selection
//   - label:   the user-facing Chinese name (matches sidebar wording)
//   - keywords: extra fuzzy-match terms (English / pinyin-ish / synonyms)
//   - group:   coarse grouping for display in the palette
import { tr as trInline } from '@/i18n/locale';

export type AppRouteGroup = '主页' | 'Agent' | '知识库' | '设备' | '监控告警' | '日常' | '设置' | '用户管理';

export type AppRoute = {
  path: string;
  label: string;
  keywords?: string[];
  group: AppRouteGroup;
};

type RouteDef = { path: string; zh: string; en: string; keywords?: string[]; group: AppRouteGroup };

const ROUTE_DEFS: RouteDef[] = [
  { path: '/home', zh: '首页', en: 'Home', keywords: ['home', 'teamily', 'workbench', 'shouye'], group: '主页' },
  { path: '/dashboard', zh: '仪表盘', en: 'Dashboard', keywords: ['dashboard', 'overview', 'yibiaopan'], group: '主页' },

  { path: '/agents', zh: '助理', en: 'Assistants', keywords: ['agents', 'assistant', 'bot', 'zhuli', 'zhushou'], group: 'Agent' },
  { path: '/node-agents', zh: '节点 Agent', en: 'Node Agents', keywords: ['node', 'agent', 'jiedian', 'edge agent'], group: 'Agent' },
  // 统一的 Discover 壳 + 三条带 tab 的入口(sidebar 三条链接的落点,按 query
  // 算独立条目)。旧路由 /skills /plugins /crystallized 保留为保参重定向,
  // 所以它们下面的目录条目也一并保留。
  { path: '/discover', zh: '发现', en: 'Discover', keywords: ['discover', 'faxian', 'hub'], group: 'Agent' },
  { path: '/discover?tab=skills', zh: '发现 / 技能', en: 'Discover / Skills', keywords: ['discover', 'skills', 'jineng'], group: 'Agent' },
  { path: '/discover?tab=plugins', zh: '发现 / 插件', en: 'Discover / Plugins', keywords: ['discover', 'plugins', 'chajian'], group: 'Agent' },
  { path: '/discover?tab=crystals', zh: '发现 / 自愈规则', en: 'Discover / Runbooks', keywords: ['discover', 'crystallized', 'runbook', 'ziyu'], group: 'Agent' },
  { path: '/plugins', zh: '插件市场', en: 'Plugins', keywords: ['plugins', 'marketplace', 'chajian', 'shichang'], group: 'Agent' },
  { path: '/crystallized', zh: '自愈规则', en: 'Runbooks', keywords: ['crystallized', 'runbook', 'self-heal', 'ziyu', 'guize'], group: 'Agent' },
  { path: '/workflows', zh: '工作流', en: 'Workflows', keywords: ['workflows', 'flow', 'gongzuoliu'], group: 'Agent' },
  { path: '/skills', zh: '技能', en: 'Skills', keywords: ['skills', 'tools', 'jineng'], group: 'Agent' },
  { path: '/mcp', zh: 'MCP', en: 'MCP', keywords: ['mcp', 'model context protocol', 'server'], group: 'Agent' },

  { path: '/knowledge', zh: '知识库', en: 'Knowledge', keywords: ['knowledge', 'kb', 'docs', 'rag', 'zhishiku', 'upload'], group: '知识库' },
  { path: '/knowledge/repos', zh: '代码仓库', en: 'Code repos', keywords: ['repos', 'git', 'code', 'cangku'], group: '知识库' },

  { path: '/devices', zh: '设备 / 全部', en: 'Devices / All', keywords: ['devices', 'edges', 'shebei'], group: '设备' },
  { path: '/devices?roles=server', zh: '设备 / 服务器', en: 'Devices / Servers', keywords: ['server', 'host', 'fuwuqi'], group: '设备' },
  { path: '/devices?roles=storage', zh: '设备 / 存储', en: 'Devices / Storage', keywords: ['storage', 'disk', 'cunchu'], group: '设备' },
  { path: '/devices?roles=database', zh: '设备 / 数据库', en: 'Devices / Database', keywords: ['database', 'db', 'shujuku'], group: '设备' },
  { path: '/devices?roles=network', zh: '设备 / 网络设备', en: 'Devices / Network', keywords: ['network', 'switch', 'router', 'wangluo'], group: '设备' },
  { path: '/topology', zh: '拓扑', en: 'Topology', keywords: ['topology', 'graph', 'tuopu', 'services', 'clusters'], group: '设备' },
  { path: '/edges/shell-sessions', zh: 'WebSSH 会话', en: 'WebSSH sessions', keywords: ['webssh', 'shell', 'sessions', 'huihua', 'duankaou'], group: '设备' },

  { path: '/monitor', zh: '监控', en: 'Monitor', keywords: ['monitor', 'metrics', 'jiankong'], group: '监控告警' },
  { path: '/logs', zh: '日志', en: 'Logs', keywords: ['logs', 'rizhi'], group: '监控告警' },
  { path: '/traces', zh: '链路', en: 'Traces', keywords: ['traces', 'tracing', 'lianlu'], group: '监控告警' },
  { path: '/alerts', zh: '告警', en: 'Alerts', keywords: ['alerts', 'incidents', 'gaojing'], group: '监控告警' },
  { path: '/alerts/rules', zh: '告警规则', en: 'Alert rules', keywords: ['rules', 'guize'], group: '监控告警' },

  { path: '/tasks', zh: '任务', en: 'Tasks', keywords: ['tasks', 'renwu', 'schedules', 'schedule'], group: '日常' },
  { path: '/pages', zh: '产物', en: 'Artifacts', keywords: ['pages', 'artifacts', 'chanwu', 'serve_page'], group: '日常' },
  { path: '/pages?tab=reports', zh: '报表', en: 'Reports', keywords: ['reports', 'report', 'baobiao', 'weekly'], group: '日常' },
  { path: '/approvals', zh: '审批', en: 'Approvals', keywords: ['approvals', 'shenpi', 'inbox', 'review'], group: '日常' },

  { path: '/settings/health', zh: '设置 / 健康', en: 'Settings / Health', keywords: ['settings', 'health', 'jiankang', 'shezhi'], group: '设置' },
  { path: '/settings/llm', zh: '设置 / LLM', en: 'Settings / LLM', keywords: ['llm', 'model', 'moxing'], group: '设置' },
  { path: '/settings/agent', zh: '设置 / 助理', en: 'Settings / Agent', keywords: ['agent', 'write', 'gate', 'assistant', 'xiequan'], group: '设置' },
  { path: '/settings/secrets', zh: '设置 / 密钥', en: 'Settings / Secrets', keywords: ['secrets', 'keys', 'miyao', 'credentials'], group: '设置' },
  { path: '/settings/notifications', zh: '设置 / 通知', en: 'Settings / Notifications', keywords: ['notifications', 'tongzhi', 'communications'], group: '设置' },
  { path: '/settings/channels', zh: '设置 / 渠道', en: 'Settings / Channels', keywords: ['channels', 'qudao', 'bots', 'im'], group: '设置' },
  { path: '/settings/integrations', zh: '设置 / 集成', en: 'Settings / Integrations', keywords: ['integrations', 'jicheng'], group: '设置' },
  { path: '/settings/upgrade', zh: '设置 / 升级', en: 'Settings / Upgrade', keywords: ['upgrade', 'shengji', 'version'], group: '设置' },
  { path: '/settings/preferences', zh: '设置 / 偏好', en: 'Settings / Preferences', keywords: ['preferences', 'pianhao'], group: '设置' },
  { path: '/settings/about', zh: '设置 / 关于', en: 'Settings / About', keywords: ['about', 'guanyu'], group: '设置' },

  { path: '/admin/users', zh: '用户管理 / 用户', en: 'Admin / Users', keywords: ['users', 'yonghu', 'admin'], group: '用户管理' },
  { path: '/admin/orgs', zh: '用户管理 / 组织', en: 'Admin / Orgs', keywords: ['orgs', 'org', 'team', 'zuzhi'], group: '用户管理' },
  { path: '/admin/audit', zh: '审计日志', en: 'Audit log', keywords: ['audit', 'shenji', 'rizhi', 'log'], group: '用户管理' },
  { path: '/admin/runtime', zh: '运行时', en: 'Runtime', keywords: ['runtime', 'deployment', 'yunxing', 'manager', 'worker'], group: '用户管理' },
  { path: '/admin/plugins', zh: '插件发布', en: 'Plugin releases', keywords: ['plugins', 'releases', 'fabufenfa'], group: '用户管理' },
];

// APP_ROUTES: labels resolved per current locale via trInline (reads
// the current locale from localStorage each call, so a locale swap is
// reflected on next palette open). `keywords` retain pinyin/synonym
// hints so fuzzyMatch still works for users who type Chinese acronyms
// regardless of UI language.
export const APP_ROUTES: AppRoute[] = ROUTE_DEFS.map((r) => Object.defineProperty(
  { path: r.path, keywords: r.keywords, group: r.group, label: '' } as AppRoute,
  'label',
  { get: () => trInline(r.zh, r.en), enumerable: true },
));

// fuzzyMatchScore returns a non-negative score when `query` matches
// `target`, or -1 when it doesn't. Higher = better. The algorithm is
// the standard subsequence match: every char of the (lowercased) query
// must appear in `target` in order; consecutive matches and matches at
// word boundaries score higher.
//
// We deliberately keep this dependency-free — pulling in fuse.js for a
// 25-row palette would be silly.
export function fuzzyMatchScore(query: string, target: string): number {
  if (!query) return 0;
  const q = query.toLowerCase();
  const t = target.toLowerCase();
  let qi = 0;
  let score = 0;
  let prevMatch = -2;
  for (let ti = 0; ti < t.length && qi < q.length; ti++) {
    if (t[ti] === q[qi]) {
      // Consecutive char bonus.
      if (ti === prevMatch + 1) score += 2;
      // Word-boundary bonus.
      else if (ti === 0 || /[\s/_-]/.test(t[ti - 1])) score += 2;
      else score += 1;
      prevMatch = ti;
      qi++;
    }
  }
  if (qi < q.length) return -1;
  // Prefer shorter targets when scores tie.
  return score - t.length * 0.01;
}

// scoreRoute computes the best score across an entry's label, path,
// and keyword list. -1 means no match.
export function scoreRoute(query: string, route: AppRoute): number {
  if (!query) return 0;
  const candidates = [route.label, route.path, ...(route.keywords ?? [])];
  let best = -1;
  for (const c of candidates) {
    const s = fuzzyMatchScore(query, c);
    if (s > best) best = s;
  }
  return best;
}
