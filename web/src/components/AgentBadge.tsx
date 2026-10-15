// AgentBadge renders the persona pinned to a chat session. Single
// source of truth for the visual treatment so /agents page, sidebar
// session list, and ChatThread header all stay aligned.
//
// agentId conventions:
//   - undefined / null / empty → coordinator default; nothing rendered
//     (we don't want a "default" badge cluttering every session)
//   - non-empty string → render a small chip with the persona icon +
//     Chinese display name (mapped from agent_id; falls back to
//     agent_id when unknown so future personas still show something)
//
// Color follows the persona tone (D2, via personaVisual) instead of a
// fixed indigo, so the chip agrees with the AgentAvatar elsewhere.
import { cn } from '@/lib/cn';
import { personaVisual, TONE_CLASS, type PersonaTone } from './AgentAvatar';
import { tr as trInline, useI18n } from '@/i18n/locale';

export type AgentBadgeSize = 'xs' | 'sm';

const AGENT_LABELS_ZH: Record<string, string> = {
  default: '默认助理',
  'incident-investigator': '故障诊断',
  'specialist-sre': 'SRE 专家',
  'specialist-ops': '运维专家',
  'specialist-compute': '计算专家',
  'specialist-network': '网络专家',
  'specialist-disk': '磁盘专家',
  reviewer: '审核员',
  critic: '评审员',
  verifier: '验证员',
  reporter: '报告员',
};
const AGENT_LABELS_EN: Record<string, string> = {
  default: 'Default',
  'incident-investigator': 'Incident investigator',
  'specialist-sre': 'SRE specialist',
  'specialist-ops': 'Ops specialist',
  'specialist-compute': 'Compute specialist',
  'specialist-network': 'Network specialist',
  'specialist-disk': 'Disk specialist',
  reviewer: 'Reviewer',
  critic: 'Critic',
  verifier: 'Verifier',
  reporter: 'Reporter',
};

// Border + ring tint per persona tone. Kept as full literal class names
// (never template-interpolated) so Tailwind's scanner sees every one.
const TONE_EDGE: Record<PersonaTone, string> = {
  violet: 'border-violet-500/40 ring-violet-500/20',
  rose: 'border-rose-500/40 ring-rose-500/20',
  amber: 'border-amber-500/40 ring-amber-500/20',
  emerald: 'border-emerald-500/40 ring-emerald-500/20',
  sky: 'border-sky-500/40 ring-sky-500/20',
  cyan: 'border-cyan-500/40 ring-cyan-500/20',
};

// personaLabel resolves an agent_id to its localized display name using the
// same two tables AgentBadge renders from, falling back to the raw agent_id
// so a persona added later still shows something instead of blank.
//
// The optional `translate` argument exists because trInline reads
// localStorage once at call time and never re-reads on locale change. A
// component that wants to repaint when the user flips the language must pass
// its own useI18n() `tr`; callers without that need (module-level constants,
// AgentBadge itself, which already re-renders via useI18n) can omit it.
// See src/i18n/locale.ts on the standalone tr caveat.
//
// Empty string for a falsy agent_id: an unset agent_id has no persona to
// name, and the row that renders this shouldn't grow a "default" label just
// to fill space.
export function personaLabel(
  agentId?: string | null,
  translate: (zh: string, en: string) => string = trInline,
): string {
  if (!agentId) return '';
  const zh = AGENT_LABELS_ZH[agentId];
  return zh ? translate(zh, AGENT_LABELS_EN[agentId] ?? zh) : agentId;
}

export function AgentBadge({
  agentId,
  size = 'xs',
  className,
}: {
  agentId?: string | null;
  size?: AgentBadgeSize;
  className?: string;
}) {
  const { tr } = useI18n();
  if (!agentId) return null;
  const label = personaLabel(agentId);
  const { icon: Icon, tone } = personaVisual(agentId);
  const base = cn(
    'inline-flex items-center gap-1 rounded-md border ring-1 ring-inset',
    TONE_EDGE[tone],
    TONE_CLASS[tone],
  );
  const sizeCls =
    size === 'sm'
      ? 'px-1.5 py-0.5 text-[11px]'
      : 'px-1 py-0.5 text-[10px]';
  const iconSize = size === 'sm' ? 11 : 9;
  return (
    <span
      title={tr(`此会话固定使用 ${label}（${agentId}）`, `This session is pinned to ${label} (${agentId})`)}
      className={cn(base, sizeCls, className)}
    >
      <Icon size={iconSize} aria-hidden="true" />
      {label}
    </span>
  );
}
