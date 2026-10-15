import { useEffect, useRef, useState } from 'react';
import { NavLink, Link, useLocation, useNavigate } from 'react-router-dom';
import {
  Bell,
  Search,
  PanelLeftClose,
  PanelLeftOpen,
  Home,
  HardDrive,
  LayoutDashboard,
  Server,
  Boxes,
  AppWindow,
  Database,
  Network,
  Bot,
  LogOut,
  Settings,
  UsersRound,
  ChartLine,
  FileText,
  CalendarClock,
  Waypoints,
  Route,
  Siren,
  Wrench,
  BookOpen,
  GitBranch,
  ChevronDown,
  ChevronRight,
  Share2,
  Plug,
  Package,
  Sparkles,
  ClipboardCheck,
  Plus,
  FileBarChart,
} from 'lucide-react';
import { Avatar } from './Avatar';
import { SessionList } from './SessionList';
import { OpskeeperLogo } from './OpskeeperLogo';
import { useI18n } from '@/i18n/locale';
import { useThemeMode } from '@/store/mode';
import { Sun, Moon, Monitor, Languages } from 'lucide-react';
import { cn } from '@/lib/cn';
import type { IconType } from '@/lib/icon';
import { useAuth } from '@/store/auth';
import { useUi } from '@/store/ui';
import { useIncidentBadge } from '@/store/incidentBadge';
import { useApprovalBadge } from '@/store/approvalBadge';
import { useMe, usePermissions } from '@/store/me';
import { useChatSessions, invalidateChatSessions } from '@/store/chatSessions';
import { createSession, deleteSession, type ChatSession } from '@/api/chat';
import { Button } from '@/components/ui/Button';
import { listEdges, type EdgeRole } from '@/api/edges';
import { onDevicesChanged } from '@/lib/events';

export function Sidebar() {
  const { sidebarCollapsed, toggleSidebar } = useUi();
  const setPaletteOpen = useUi((s) => s.setPaletteOpen);
  const { email, role, logout } = useAuth();
  const { tr, toggleLocale, locale } = useI18n();
  // Prefer display_name when present — Bootstrap 时设的"admin"或后续
  // /v1/users/{id} 改的，比 email 在 sidebar / user menu 里更友好。
  const { me } = useMe();
  const { isAdmin } = usePermissions();
  const displayName = (me?.display_name?.trim() || email) ?? tr('OpsKeeper 用户', 'OpsKeeper user');
  const navigate = useNavigate();
  const location = useLocation();

  const sessions = useChatSessions((s) => s.sessions);
  const refreshSessions = useChatSessions((s) => s.refresh);
  // Unack'd incident count drives the red pill on 告警 items + a dot on
  // the collapsed icon-rail. Polled by useIncidentBadge in Layout.
  const incidentOpen = useIncidentBadge((s) => s.openCount);
  // Pending-approval count drives the red pill on the 审批中心 item.
  // Polled by useApprovalBadge in Layout; the store itself only calls
  // /approvals/count for admins, matching the group guard below.
  const approvalsPending = useApprovalBadge((s) => s.pending);
  const [showAllSessions, setShowAllSessions] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<ChatSession | null>(null);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  // Version + upgrade check moved to Settings → About / Upgrade (the brand mark
  // stays clean), so the sidebar no longer fetches version here.

  // Roles actually present in the user's fleet — drives which 设备 sub-
  // items appear. We don't render a role link for a role with 0 devices,
  // and we drop 未分类 entirely (zero edges = empty section). Initial
  // listEdges() on mount + refetch on the cross-component devices-changed
  // event so RolesEditorModal saves surface here within a tick instead of
  // requiring a full page reload.
  const [presentRoles, setPresentRoles] = useState<Set<EdgeRole>>(new Set());
  useEffect(() => {
    let cancelled = false;
    const load = () => {
      void listEdges()
        .then((r) => {
          if (cancelled) return;
          const present = new Set<EdgeRole>();
          for (const e of r.items ?? []) {
            for (const role of e.roles ?? []) present.add(role as EdgeRole);
          }
          setPresentRoles(present);
        })
        .catch(() => {});
    };
    load();
    const unsubscribe = onDevicesChanged(load);
    return () => {
      cancelled = true;
      unsubscribe();
    };
  }, []);

  async function confirmDelete(target: ChatSession) {
    setDeletingId(target.id);
    try {
      await deleteSession(target.id);
      invalidateChatSessions();
      // If the user is currently viewing the deleted session, bounce to
      // /chat (the new-session entry point) so they're not stuck on a
      // 404 thread.
      if (location.pathname === `/chat/${target.id}`) {
      navigate('/dashboard');
      }
    } finally {
      setDeletingId(null);
      setDeleteTarget(null);
    }
  }
  const [userMenuOpen, setUserMenuOpen] = useState(false);
  const userMenuRef = useRef<HTMLDivElement | null>(null);

  const visibleSessions = showAllSessions ? sessions.slice(0, 10) : sessions.slice(0, 5);
  const hasMoreSessions = sessions.length > 5;

  useEffect(() => {
    if (!userMenuOpen) return;
    function onDocClick(e: MouseEvent) {
      if (!userMenuRef.current) return;
      if (!userMenuRef.current.contains(e.target as Node)) {
        setUserMenuOpen(false);
      }
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') setUserMenuOpen(false);
    }
    document.addEventListener('mousedown', onDocClick);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('mousedown', onDocClick);
      document.removeEventListener('keydown', onKey);
    };
  }, [userMenuOpen]);

  const handleLogout = () => {
    setUserMenuOpen(false);
    logout();
    navigate('/login');
  };

  // 新对话 CTA — creates an empty session and jumps straight into it, the
  // same shape as Home.tsx's startSession minus the initial prompt: from the
  // sidebar there is no typed content to hand over, so the thread opens empty
  // and ChatThread owns everything from there.
  const startNewChat = async () => {
    const s = await createSession({ title: tr('新对话', 'New chat').slice(0, 30), agent_id: 'default' });
    navigate(`/chat/${s.id}`);
  };

  const { preference: themePref, resolved: themeMode, cycle: cycleTheme } = useThemeMode();
  const themeLabel =
    themePref === 'system'
      ? tr('跟随系统', 'System')
      : themePref === 'dark'
        ? tr('深色', 'Dark')
        : tr('浅色', 'Light');
  const ThemeIcon =
    themePref === 'system' ? Monitor : themeMode === 'dark' ? Moon : Sun;

  const renderUserMenuPanel = (variant: 'expanded' | 'collapsed') => (
    <div
      role="menu"
      className={cn(
        'anim-scale absolute z-50 w-56 rounded-lg bg-zinc-900 p-1 shadow-lg ring-1 ring-zinc-800',
        variant === 'expanded'
          ? 'left-3 top-full mt-1 origin-top-left'
          : 'bottom-full left-1/2 mb-2 -translate-x-1/2 origin-bottom'
      )}
    >
      <div className="px-3 py-2">
        <div className="truncate text-[13px] font-semibold text-zinc-100">
          {displayName}
        </div>
        {/* Show login email as secondary line when display_name is set
            (otherwise it'd duplicate the row above). */}
        {me?.display_name && email && (
          <div className="mt-0.5 truncate text-[11px] text-zinc-500">{email}</div>
        )}
        <div className="mt-0.5 text-[11px] text-zinc-500">
          {role || 'user'}
        </div>
      </div>
      <div className="my-1 h-px bg-zinc-800" />
      {/* Language toggle — zh-CN ↔ en-US. tr() updates everywhere
          immediately because components subscribe via useI18n(). */}
      <button
        type="button"
        role="menuitemradio"
        aria-checked={locale === 'en-US'}
        onClick={toggleLocale}
        className="flex w-full items-center justify-between gap-2 rounded-md px-3 py-1.5 text-[13px] text-zinc-300 hover:bg-zinc-800 hover:text-zinc-100"
      >
        <span className="flex items-center gap-2">
          <Languages size={14} />
          <span>{tr('语言', 'Language')}</span>
        </span>
        <span className="text-[11px] text-zinc-500">
          {locale === 'zh-CN' ? '中 / EN' : 'EN / 中'}
        </span>
      </button>
      {/* Theme — cycle through system / light / dark */}
      <button
        type="button"
        role="menuitem"
        onClick={cycleTheme}
        className="flex w-full items-center justify-between gap-2 rounded-md px-3 py-1.5 text-[13px] text-zinc-300 hover:bg-zinc-800 hover:text-zinc-100"
      >
        <span className="flex items-center gap-2">
          <ThemeIcon size={14} />
          <span>{tr('主题', 'Theme')}</span>
        </span>
        <span className="text-[11px] text-zinc-500">{themeLabel}</span>
      </button>
      <div className="my-1 h-px bg-zinc-800" />
      <button
        type="button"
        role="menuitem"
        onClick={handleLogout}
        className="flex w-full items-center gap-2 rounded-md px-3 py-1.5 text-[13px] text-zinc-300 hover:bg-zinc-800 hover:text-red-400"
      >
        <LogOut size={14} />
        <span>{tr('退出登录', 'Log out')}</span>
      </button>
    </div>
  );

  // Refresh on mount and on every route change. Route changes happen when
  // Home creates a new session and navigates to /chat/:id, so the new row
  // shows up in the sidebar without a manual reload.
  useEffect(() => {
    void refreshSessions();
  }, [location.pathname, refreshSessions]);

  if (sidebarCollapsed) {
    return (
      <aside className="flex h-full w-14 shrink-0 flex-col items-center gap-2 border-r border-zinc-800/60 bg-zinc-900 py-3">
        {/* Brand mark + expand toggle: logo doubles as the expand
            affordance — saves a row in the narrow column. */}
        <button
          type="button"
          onClick={toggleSidebar}
          aria-label={tr('展开侧边栏', 'Expand sidebar')}
          title={tr('OpsKeeper · 点击展开', 'OpsKeeper · click to expand')}
          className="rounded-lg p-1 hover:bg-zinc-800/60"
        >
          <OpskeeperLogo size={34} />
        </button>
        <button
          type="button"
          onClick={toggleSidebar}
          aria-label={tr('展开侧边栏', 'Expand sidebar')}
          className="rounded-lg p-2 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100"
        >
          <PanelLeftOpen size={16} />
        </button>
        <Link
          to="/home"
          aria-label={tr('首页', 'Home')}
          className="rounded-lg p-2 text-zinc-300 hover:bg-zinc-800 hover:text-zinc-100"
        >
          <Home size={16} />
        </Link>
        <Link
          to="/dashboard"
          aria-label={tr('仪表盘', 'Dashboard')}
          className="rounded-lg p-2 text-zinc-300 hover:bg-zinc-800 hover:text-zinc-100"
        >
          <LayoutDashboard size={16} />
        </Link>
        <Link
          to="/monitor"
          aria-label={tr('监控', 'Monitor')}
          className="rounded-lg p-2 text-zinc-300 hover:bg-zinc-800 hover:text-zinc-100"
        >
          <ChartLine size={16} />
        </Link>
        <Link
          to="/alerts"
          aria-label={incidentOpen > 0 ? tr(`告警（${incidentOpen} 未确认）`, `Alerts (${incidentOpen} open)`) : tr('告警', 'Alerts')}
          title={incidentOpen > 0 ? tr(`${incidentOpen} 个未确认告警`, `${incidentOpen} open alert(s)`) : tr('告警', 'Alerts')}
          className="relative rounded-lg p-2 text-zinc-300 hover:bg-zinc-800 hover:text-zinc-100"
        >
          <Siren size={16} />
          {incidentOpen > 0 && (
            <span
              className="absolute right-1 top-1 inline-flex h-2 w-2 rounded-full bg-red-500 ring-2 ring-zinc-950"
              aria-hidden
            />
          )}
        </Link>
        <Link
          to="/devices"
          aria-label={tr('设备', 'Devices')}
          className="rounded-lg p-2 text-zinc-300 hover:bg-zinc-800 hover:text-zinc-100"
        >
          <HardDrive size={16} />
        </Link>
        <Link
          to="/discover?tab=skills"
          aria-label={tr('技能', 'Skills')}
          className="rounded-lg p-2 text-zinc-300 hover:bg-zinc-800 hover:text-zinc-100"
        >
          <Wrench size={16} />
        </Link>
        <div className="mt-auto flex flex-col items-center gap-2">
          <Link
            to="/settings/health"
            aria-label={tr('设置', 'Settings')}
            className="rounded-lg p-2 text-zinc-300 hover:bg-zinc-800 hover:text-zinc-100"
          >
            <Settings size={16} />
          </Link>
          <div ref={userMenuRef} className="relative">
            <button
              type="button"
              onClick={() => setUserMenuOpen((v) => !v)}
              aria-label={tr('用户菜单', 'User menu')}
              aria-haspopup="menu"
              aria-expanded={userMenuOpen}
              className="rounded-full ring-1 ring-transparent transition hover:ring-zinc-700 focus:outline-none focus:ring-zinc-600"
            >
              <Avatar email={email} size={28} />
            </button>
            {userMenuOpen ? renderUserMenuPanel('collapsed') : null}
          </div>
        </div>
      </aside>
    );
  }

  return (
    <aside className="flex h-full w-64 shrink-0 flex-col border-r border-zinc-800/60 bg-zinc-900">
      {/* Brand row — keeps the product identity visible while the user
          row below still owns the avatar + menu. Clicking the wordmark
          goes home. */}
      <div className="flex items-center gap-1.5 border-b border-zinc-800/60 px-3 py-3">
        <Link
          to="/dashboard"
          aria-label={tr('OpsKeeper 首页', 'OpsKeeper home')}
          className="flex min-w-0 items-center gap-1.5 rounded-lg px-1 py-1 -ml-1 hover:bg-zinc-800/40"
        >
          <OpskeeperLogo size={32} className="-mr-0.5 shrink-0" />
          <span className="text-[16px] font-semibold tracking-tight text-zinc-100">OpsKeeper</span>
          {/* Version moved to Settings → About (the brand mark stays clean). */}
        </Link>
        {/* Version + upgrade CTA moved to Settings → About / Upgrade — the brand
            mark stays clean. */}
      </div>

      {/* user / collapse / bell */}
      <div ref={userMenuRef} className="relative flex items-center gap-2 px-3 py-3">
        <button
          type="button"
          onClick={() => setUserMenuOpen((v) => !v)}
          aria-label={tr('用户菜单', 'User menu')}
          aria-haspopup="menu"
          aria-expanded={userMenuOpen}
          className="flex min-w-0 flex-1 items-center gap-2 rounded-lg p-1 text-left transition hover:bg-zinc-800/60 focus:outline-none focus:ring-1 focus:ring-zinc-700"
        >
          <Avatar email={email} size={28} />
          <div className="min-w-0 flex-1">
            <div className="truncate text-[13px] font-medium text-zinc-100">
              {displayName}
            </div>
            <div className="truncate text-[11px] text-zinc-500">{tr('AIOps 工作台', 'AIOps Workbench')}</div>
          </div>
        </button>
        <button
          type="button"
          onClick={toggleSidebar}
          aria-label={tr('折叠侧边栏', 'Collapse sidebar')}
          className="rounded-lg p-1.5 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100"
        >
          <PanelLeftClose size={15} />
        </button>
        <button
          type="button"
          aria-label={tr('通知', 'Notifications')}
          className="rounded-lg p-1.5 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100"
        >
          <Bell size={15} />
        </button>
        {userMenuOpen ? renderUserMenuPanel('expanded') : null}
      </div>

      {/* search — opens the command palette (⌘P). The visual is still
          a search-input lookalike so users get the affordance, but the
          actual UI lives in CommandPalette so we don't have to ship
          two search code paths. */}
      <div className="px-3 pb-2">
        <button
          type="button"
          onClick={() => setPaletteOpen(true)}
          aria-label={tr('打开命令面板', 'Open command palette')}
          className="flex w-full items-center gap-2 rounded-md border border-zinc-800/60 bg-zinc-950/40 px-2.5 py-1.5 text-left text-[12px] text-zinc-500 hover:border-zinc-700 hover:bg-zinc-900 hover:text-zinc-300"
        >
          <Search size={13} className="text-zinc-500" />
          <span className="flex-1 truncate">{tr('搜索路由 / 会话', 'Search routes / sessions')}</span>
          <kbd className="rounded bg-zinc-800 px-1 py-0.5 text-[10px] text-zinc-500">⌘P</kbd>
        </button>
      </div>

      <nav className="flex-1 overflow-y-auto px-2 pb-3">
        {/* 新对话 是侧栏里唯一的主 CTA: 想开新线程时不需要先想清楚该落在哪个
            页面, 一个动作直达一个空会话。 */}
        <div className="mt-3">
          <Button
            variant="primary"
            className="w-full justify-center"
            onClick={() => void startNewChat()}
          >
            <Plus className="h-3.5 w-3.5" />
            {tr('新对话', 'New chat')}
          </Button>
        </div>

        {/* L1 顶级入口 — 不缩进，直接可点 */}
        <div className="mt-3 space-y-0.5">
          <SidebarNavItem to="/home" icon={Home} label={tr('首页', 'Home')} exact level={1} />
          <SidebarNavItem to="/dashboard" icon={LayoutDashboard} label={tr('仪表盘', 'Dashboard')} level={1} />
        </div>

        {/* IA 重排 (2.1): 对话 / Agent / Discover / 运维 / 日常 / 审批 六组,
            管理留在底部 footer 区。分组取代了旧的扁平堆叠, 但**没有新增页面**,
            也没有改动任何一条 route 字符串 —— 变的只是归属。
            组名 Agent / Discover 保持英文, 与既有 SectionLabel 一致。 */}

        {/* 对话 —— 会话列表。行本身的视觉(AgentAvatar 32 + persona 名 +
            标题摘要)与重命名 / 删除交互都在 SessionList 里; 这里只负责
            切片的条数、空态、展开按钮和删除 modal。传入的是
            visibleSessions 而不是 sessions, 否则 5 条上限会静默失效。 */}
        <SectionLabel>{tr('对话', 'Chats')}</SectionLabel>
        <div className="ml-2 space-y-0.5">
          {sessions.length === 0 ? (
            <div className="px-2 py-1.5 text-[12px] text-zinc-600">{tr('暂无会话', 'No sessions yet')}</div>
          ) : (
            <SessionList
              sessions={visibleSessions}
              onDelete={(s) => setDeleteTarget(s)}
            />
          )}
          {hasMoreSessions ? (
            <button
              type="button"
              onClick={() => setShowAllSessions((v) => !v)}
              className="flex items-center gap-1 rounded-md px-2 py-1.5 text-[12px] text-zinc-500 transition-colors hover:bg-zinc-800/60 hover:text-zinc-200"
            >
              {showAllSessions ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
              <span>{showAllSessions ? tr('收起', 'Collapse') : tr(`展开剩余 ${Math.min(sessions.length, 10) - 5} 条`, `Show ${Math.min(sessions.length, 10) - 5} more`)}</span>
            </button>
          ) : null}
        </div>

        {/* Agent —— agent 怎么定义、在哪跑、怎么编排。MCP 是 agent 的外部工具
            接线面, 与助理同属 agent 侧, 所以留在这里而不是挪去管理。 */}
        <SectionLabel>Agent</SectionLabel>
        <NavSection>
          <SidebarNavItem to="/agents" icon={Bot} label={tr('助理', 'Assistants')} />
          {/* A node agent is a running agent on an edge rather than a
              definition in the catalog, so it sits beside 助理 rather than
              inside it: the two pages answer different questions and an
              operator looking for "what can I install" should not have to
              scroll past "what is running right now". */}
          <SidebarNavItem to="/node-agents" icon={Server} label={tr('节点 Agent', 'Node Agents')} />
          <SidebarNavItem to="/workflows" icon={Route} label={tr('工作流', 'Workflows')} />
          <SidebarNavItem to="/mcp" icon={Plug} label="MCP" />
        </NavSection>

        {/* Discover —— agent 能拿到什么。三条链接都指向统一的 /discover 壳,
            用 ?tab= 定位面板; 旧路由 /skills /plugins /crystallized 保留为
            保参重定向, 老书签与深链仍然可用。 */}
        <SectionLabel>Discover</SectionLabel>
        <NavSection>
          {/* The marketplace sits here rather than in settings because it
              is the way packages get IN, which is a discover concern; the
              release console that puts them on hosts stays under Admin. */}
          <SidebarNavItem to="/discover?tab=plugins" icon={Package} label={tr('插件', 'Plugins')} />
          <SidebarNavItem to="/discover?tab=skills" icon={Wrench} label={tr('技能', 'Skills')} />
          {/* 自愈规则 是另一种"包"的来源: 市场是把它拿进来, 这一页是让平台
              请求人许可它从自己的历史里写出一个来。 */}
          <SidebarNavItem to="/discover?tab=crystals" icon={Sparkles} label={tr('自愈规则', 'Runbooks')} />
        </NavSection>

        {/* 运维 —— agent 的观测面与它操作的物理对象。折叠头本身即分组名,
            展开后 设备在前, 拓扑/监控/日志/链路/告警 紧随其后, 保持"先看对象
            再看数据"的顺序。storageKey 用 ops: 原来的 devices 与 observability
            两段被本 IA 合成一段, 1:1 的旧 key 映射已不存在。 */}
        <CollapsibleSection storageKey="ops" title={tr('运维', 'Operations')} defaultOpen={false}>
          {/* 设备 的角色子项按 presentRoles 过滤: 没有该角色的设备时整条不渲染,
              未分类(零 edge)直接省略, 见上方 presentRoles 的说明。 */}
          <SidebarNavItem to="/devices" icon={HardDrive} label={tr('设备', 'Devices')} />
          {presentRoles.has('server') && (
            <SidebarNavItem to="/devices?roles=server" icon={Server} label={tr('服务器', 'Servers')} />
          )}
          {presentRoles.has('storage') && (
            <SidebarNavItem to="/devices?roles=storage" icon={Boxes} label={tr('存储', 'Storage')} />
          )}
          {presentRoles.has('database') && (
            <SidebarNavItem to="/devices?roles=database" icon={Database} label={tr('数据库', 'Databases')} />
          )}
          {presentRoles.has('network') && (
            <SidebarNavItem to="/devices?roles=network" icon={Network} label={tr('网络设备', 'Network')} />
          )}
          <SidebarNavItem to="/topology" icon={Share2} label={tr('拓扑', 'Topology')} />
          <SidebarNavItem to="/monitor" icon={ChartLine} label={tr('监控', 'Monitor')} />
          <SidebarNavItem to="/logs" icon={FileText} label={tr('日志', 'Logs')} />
          <SidebarNavItem to="/traces" icon={Waypoints} label={tr('链路', 'Traces')} />
          <SidebarNavItem to="/alerts" icon={Siren} label={tr('告警', 'Alerts')} badge={incidentOpen} />
        </CollapsibleSection>

        {/* 日常 —— 团队的周期性工作产出。代码仓库与知识库并列, 二者是同一
            类"喂给 agent 的素材"。storageKey 沿用旧的 operations, 保住用户
            已有的折叠偏好。 */}
        <CollapsibleSection storageKey="operations" title={tr('日常', 'Daily')} defaultOpen={false}>
          <SidebarNavItem to="/tasks" icon={CalendarClock} label={tr('任务', 'Tasks')} />
          <SidebarNavItem to="/pages" icon={AppWindow} label={tr('产物', 'Artifacts')} />
          <SidebarNavItem to="/pages?tab=reports" icon={FileBarChart} label={tr('报表', 'Reports')} />
          <SidebarNavItem to="/knowledge" icon={BookOpen} label={tr('知识库', 'Knowledge')} />
          <SidebarNavItem to="/knowledge/repos" icon={GitBranch} label={tr('代码仓库', 'Repos')} />
        </CollapsibleSection>

        {/* 审批 —— 常驻入口 + 待审批红点。整个分组对非 admin 隐藏:
            /v1/approvals 的每个 handler 都在 requireAdmin 之后, 所以非
            admin 点进去必然 403。SectionLabel 一起包住, 否则非 admin 会
            看到一个空标题。store 里的 role 门禁是独立的第二道保险。 */}
        {isAdmin && (
          <>
            <SectionLabel>{tr('审批', 'Approvals')}</SectionLabel>
            <NavSection>
              <SidebarNavItem
                to="/approvals"
                icon={ClipboardCheck}
                label={tr('审批中心', 'Approvals')}
                badge={approvalsPending}
              />
            </NavSection>
          </>
        )}
      </nav>

      {/* 管理 —— 留在 footer 区: 这些是低频入口, 不该和上面的工作面抢视觉权重。 */}
      <div className="mb-4 border-t border-zinc-800/60 p-2">
        <SectionLabel>{tr('管理', 'Admin')}</SectionLabel>
        {isAdmin && (
          <SidebarNavItem to="/admin/users" icon={UsersRound} label={tr('用户管理', 'Users & Orgs')} level={2} />
        )}
        <SidebarNavItem to="/admin/audit" icon={UsersRound} label={tr('审计日志', 'Audit log')} level={2} />
        <SidebarNavItem to="/settings/health" icon={Settings} label={tr('设置', 'Settings')} level={2} />
      </div>

      {deleteTarget && (
        <DeleteSessionModal
          target={deleteTarget}
          deleting={deletingId === deleteTarget.id}
          onCancel={() => setDeleteTarget(null)}
          onConfirm={() => void confirmDelete(deleteTarget)}
        />
      )}
    </aside>
  );
}

function DeleteSessionModal({
  target,
  deleting,
  onCancel,
  onConfirm,
}: {
  target: ChatSession;
  deleting: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const { tr } = useI18n();
  return (
    <div
      role="dialog"
      aria-modal
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/70"
      onClick={onCancel}
    >
      <div
        className="w-full max-w-sm rounded-xl border border-zinc-800 bg-zinc-900 p-5 shadow-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="text-sm font-medium text-zinc-100">{tr('删除会话', 'Delete session')}</div>
        <div className="mt-2 text-[13px] text-zinc-400">
          {tr('确定要删除会话 ', 'Delete session ')}<span className="text-zinc-100">{target.title || tr('未命名会话', 'Untitled session')}</span>
          {tr(' 吗？此操作会一并删除所有消息和工具调用记录，无法恢复。', '? All messages and tool-call records will be removed and cannot be recovered.')}
        </div>
        <div className="mt-5 flex justify-end gap-2">
          <button
            type="button"
            onClick={onCancel}
            disabled={deleting}
            className="rounded-md border border-zinc-700 bg-zinc-900 px-3 py-1.5 text-xs text-zinc-300 hover:bg-zinc-800 disabled:opacity-50"
          >
            {tr('取消', 'Cancel')}
          </button>
          <button
            type="button"
            onClick={onConfirm}
            disabled={deleting}
            className="rounded-md bg-red-500 px-3 py-1.5 text-xs font-medium text-white hover:bg-red-400 disabled:opacity-50"
          >
            {deleting ? tr('删除中…', 'Deleting…') : tr('删除', 'Delete')}
          </button>
        </div>
      </div>
    </div>
  );
}

function NavSection({ children }: { children: React.ReactNode }) {
  return <div className="mt-1 space-y-0.5">{children}</div>;
}

function SectionLabel({ children }: { children: React.ReactNode }) {
  return (
    <div className="mt-5 px-2 pb-1.5 text-[13px] font-semibold text-zinc-300">
      {children}
    </div>
  );
}

// CollapsibleSection is the same SectionLabel + NavSection pair, but the
// header is a button that toggles its children's visibility. State
// persists in localStorage so users don't have to re-fold their AIOps-
// supplemental sections (设备 / 监控告警) on every page load.
//
// Why we have this: opskeeper is AIOps-first. The agent + context + chat
// flows are the primary surface; observability + device management are
// data sources for the agent. Keeping them collapsed by default puts
// visual weight where the product's value is.
//
// The 2.1 IA regroups those same data-source items under a single 运维
// heading, which is *longer* than either original section — 运维 alone is
// 10 rows once the device role filters render. That makes the fold more
// worth keeping, not less, so 运维 and 日常 stay collapsible while the
// short groups (对话 / Agent / Discover / 审批 / 管理) use SectionLabel.
function CollapsibleSection({
  storageKey,
  title,
  defaultOpen = false,
  children,
}: {
  storageKey: string;
  title: string;
  defaultOpen?: boolean;
  children: React.ReactNode;
}) {
  const [open, setOpen] = useState(() => {
    try {
      const raw = localStorage.getItem(`sidebar.section.${storageKey}`);
      if (raw === 'open') return true;
      if (raw === 'closed') return false;
    } catch {
      /* localStorage unavailable — fall through to default */
    }
    return defaultOpen;
  });
  const toggle = () => {
    setOpen((prev) => {
      const next = !prev;
      try {
        localStorage.setItem(`sidebar.section.${storageKey}`, next ? 'open' : 'closed');
      } catch {
        /* ignore */
      }
      return next;
    });
  };
  return (
    <div>
      <button
        type="button"
        onClick={toggle}
        className="group mt-5 flex w-full items-center justify-between px-2 pb-1.5 text-left text-[13px] font-semibold text-zinc-300 transition-colors hover:text-zinc-100"
      >
        <span>{title}</span>
        <ChevronRight
          size={11}
          className={cn(
            'shrink-0 text-zinc-600 transition-transform duration-150 group-hover:text-zinc-400',
            open && 'rotate-90',
          )}
        />
      </button>
      {open && <div className="space-y-0.5">{children}</div>}
    </div>
  );
}

function SidebarNavItem({
  to,
  icon: Icon,
  label,
  exact,
  disabled,
  muted,
  level,
  badge,
}: {
  to?: string;
  icon: IconType;
  label: string;
  exact?: boolean;
  disabled?: boolean;
  muted?: boolean;
  // level 1 = 一级（首页 / 仪表盘）不缩进；默认 2 = 二级，缩 ml-2 比之前 ml-3 更紧凑。
  level?: 1 | 2;
  // badge: optional unread count rendered as a red pill on the right.
  // Hidden when 0 or undefined; capped to "99+" so widths stay sane.
  badge?: number;
}) {
  const { tr } = useI18n();
  const indentCls = level === 1 ? '' : 'ml-2';
  const baseCls = cn(
    indentCls,
    'flex items-center gap-2 rounded-md px-2 py-1.5 text-[13px] transition-colors'
  );
  const location = useLocation();
  if (disabled || !to) {
    return (
      <div
        aria-disabled
        className={cn(
          baseCls,
          'cursor-not-allowed opacity-50',
          muted ? 'text-zinc-300' : 'text-zinc-500'
        )}
      >
        <Icon size={14} className="text-zinc-500" />
        <span>{label}</span>
      </div>
    );
  }

  // We need pathname AND query-string to participate in active detection,
  // because items like "/edges?roles=server" share a pathname with
  // "/edges" but should highlight independently. NavLink's default isActive
  // ignores search; build our own.
  const [targetPath, targetQuery] = to.split('?');
  const hasQuery = Boolean(targetQuery);
  const targetParams = hasQuery ? new URLSearchParams(targetQuery) : null;
  const currentParams = new URLSearchParams(location.search);
  let isActive: boolean;
  if (location.pathname !== targetPath) {
    isActive = false;
  } else if (hasQuery) {
    // sub-item: every key in `to` must match the current URL exactly,
    // and current must not have extra "roles" filters that disagree.
    isActive = paramsEqualOnDefinedKeys(targetParams!, currentParams);
  } else if (exact) {
    isActive = location.pathname === targetPath;
  } else {
    // pathname-only items — match unless one of the role sub-items would
    // otherwise claim it. The "全部" entry uses this branch and lights up
    // only when no role filter is present.
    isActive = !currentParams.get('roles');
  }

  return (
    <NavLink
      to={to}
      end={exact}
      className={cn(
        baseCls,
        'text-zinc-300 hover:bg-zinc-800/60 hover:text-zinc-100',
        isActive && 'bg-zinc-800 text-zinc-100'
      )}
    >
      <Icon size={14} className="text-zinc-400" />
      <span className="flex-1 truncate">{label}</span>
      {badge != null && badge > 0 && (
        <span
          className="ml-auto inline-flex min-w-[18px] items-center justify-center rounded-full bg-red-500/90 px-1.5 text-[10px] font-medium text-white"
          aria-label={tr(`${badge} 未确认`, `${badge} open`)}
        >
          {badge > 99 ? '99+' : badge}
        </span>
      )}
    </NavLink>
  );
}

// paramsEqualOnDefinedKeys reports whether every key in `target` has the
// same value in `current`. Extra keys on `current` are ignored. Used to
// decide whether a sidebar sub-item with `?roles=server` should highlight
// against a URL like `/edges?roles=server&status=online`.
function paramsEqualOnDefinedKeys(target: URLSearchParams, current: URLSearchParams) {
  for (const [k, v] of target.entries()) {
    if (current.get(k) !== v) return false;
  }
  return true;
}
