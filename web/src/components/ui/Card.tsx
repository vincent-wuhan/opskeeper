import type { HTMLAttributes } from 'react';
import { cn } from '@/lib/cn';

// Card — unified card surface used across pages (DocCard / AgentCard /
// IncidentCard / etc.). Visual rules per HLD-style guide:
//   rounded-2xl + .surface-card (which carries the card background, the
//   weak semantic border and the shadow-card token layer)
//   default p-4, compact p-3.5
//   hover (when interactive) hover:border-border hover:bg-card
// The container no longer hardcodes bg-zinc-*/border-zinc-*, nor repeats
// the surface rules as utilities: .surface-card (styles/index.css) is
// token-driven, so the light/dark flip is automatic and the light-mode
// zinc remap in index.css is not load-bearing here. The interactive
// hover utilities stay as utilities — at (0,2,0) they still override
// the plain (0,1,0) .surface-card rule.
type CardProps = HTMLAttributes<HTMLDivElement> & {
  /** When true, the card is clickable / hover affordances kick in. */
  interactive?: boolean;
  /** Tighter padding for dense rows (e.g. AgentSessionCard). */
  compact?: boolean;
  as?: 'div' | 'section' | 'article';
};

export function Card({
  className,
  interactive,
  compact,
  as: Tag = 'section',
  ...rest
}: CardProps) {
  return (
    <Tag
      className={cn(
        'rounded-2xl surface-card',
        compact ? 'p-3.5' : 'p-4',
        interactive && 'transition-colors hover:border-border hover:bg-card',
        className,
      )}
      {...rest}
    />
  );
}
