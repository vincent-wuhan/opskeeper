// AgentAvatar — the single source of Agent identity across sidebar,
// chat bubbles, the Agents gallery and the incident group chat (D2).
//
// 11-persona table = the 10 `agents/*.md` personas + the implicit API
// `default`. Aliases normalize historical ids so an old session's
// `sre-agent` still renders the SRE persona instead of the fallback.
import { useState } from 'react';
import {
  Activity, BadgeCheck, BarChart3, Bot, Cpu, Gavel, HardDrive, Network,
  Radar, Server, ShieldCheck,
  type LucideIcon,
} from 'lucide-react';
import { cn } from '@/lib/cn';

export type PersonaTone = 'violet' | 'rose' | 'amber' | 'emerald' | 'sky' | 'cyan';

export type PersonaVisual = { icon: LucideIcon; tone: PersonaTone };

// Pale fill + 300 icon + hairline same-hue ring. The html.light remap
// (index.css) flips fill/icon to -100 fill / -700 icon, so avatars stay
// legible in either theme. The `ring-1 ring-inset` at /20 gives the avatar
// a defined edge without a hard border, so it doesn't read flat when it sits
// next to a `surface-card` (the former look had no rim at all).
export const TONE_CLASS: Record<PersonaTone, string> = {
  violet: 'bg-violet-500/10 text-violet-300 ring-1 ring-inset ring-violet-500/20',
  rose: 'bg-rose-500/10 text-rose-300 ring-1 ring-inset ring-rose-500/20',
  amber: 'bg-amber-500/10 text-amber-300 ring-1 ring-inset ring-amber-500/20',
  emerald: 'bg-emerald-500/10 text-emerald-300 ring-1 ring-inset ring-emerald-500/20',
  sky: 'bg-sky-500/10 text-sky-300 ring-1 ring-inset ring-sky-500/20',
  cyan: 'bg-cyan-500/10 text-cyan-300 ring-1 ring-inset ring-cyan-500/20',
};

export const PERSONA_VISUALS: Record<string, PersonaVisual> = {
  'incident-investigator': { icon: Radar, tone: 'violet' },
  critic: { icon: Gavel, tone: 'rose' },
  reviewer: { icon: ShieldCheck, tone: 'amber' },
  verifier: { icon: BadgeCheck, tone: 'emerald' },
  reporter: { icon: BarChart3, tone: 'sky' },
  'specialist-sre': { icon: Activity, tone: 'cyan' },
  'specialist-network': { icon: Network, tone: 'cyan' },
  'specialist-disk': { icon: HardDrive, tone: 'amber' },
  'specialist-compute': { icon: Cpu, tone: 'violet' },
  'specialist-ops': { icon: Server, tone: 'emerald' },
  default: { icon: Bot, tone: 'violet' },
};

// Historical / prompt-facing ids that must resolve to a listed persona.
const PERSONA_ALIASES: Record<string, string> = {
  'sre-agent': 'specialist-sre',
  'loop-controller': 'default',
};

export function normalizeAgentId(agentId?: string | null): string {
  if (!agentId) return 'default';
  return PERSONA_ALIASES[agentId] ?? agentId;
}

export function personaVisual(agentId?: string | null): PersonaVisual {
  const normalized = normalizeAgentId(agentId);
  return PERSONA_VISUALS[normalized] ?? PERSONA_VISUALS.default;
}

const BOX_CLASS: Record<32 | 40, string> = { 32: 'h-8 w-8 rounded-rk-sm', 40: 'h-10 w-10 rounded-rk-md' };
const ICON_CLASS: Record<32 | 40, string> = { 32: 'h-4 w-4', 40: 'h-5 w-5' };
const EMOJI_CLASS: Record<32 | 40, string> = { 32: 'text-[18px] leading-none', 40: 'text-[22px] leading-none' };

export function AgentAvatar({
  agentId,
  size = 32,
  avatar,
  className,
}: {
  agentId?: string | null;
  size?: 32 | 40;
  /** Optional persisted image URL; the icon renders until/ unless it loads. */
  avatar?: string;
  className?: string;
}) {
  const { icon: Icon, tone } = personaVisual(agentId);
  const [imgBroken, setImgBroken] = useState(false);
  const isUrl = Boolean(avatar) && /^https?:\/\//i.test(avatar as string);
  const showImage = isUrl && !imgBroken;
  const showEmoji = Boolean(avatar) && !isUrl;

  return (
    <span
      data-testid="agent-avatar"
      className={cn(
        'inline-flex shrink-0 select-none items-center justify-center overflow-hidden',
        BOX_CLASS[size],
        TONE_CLASS[tone],
        className,
      )}
    >
      {showImage ? (
        <img
          src={avatar}
          alt=""
          className="h-full w-full object-cover"
          onError={() => setImgBroken(true)}
        />
      ) : showEmoji ? (
        <span className={EMOJI_CLASS[size]} aria-hidden="true">
          {avatar}
        </span>
      ) : (
        <Icon className={ICON_CLASS[size]} aria-hidden="true" />
      )}
    </span>
  );
}