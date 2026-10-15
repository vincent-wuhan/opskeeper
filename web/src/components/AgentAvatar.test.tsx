import { fireEvent, render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { AgentAvatar, PERSONA_VISUALS } from './AgentAvatar';
import { personaLabel } from './AgentBadge';

// lucide-react stamps `lucide-<kebab-name>` on every icon's <svg>. Asserting
// on that class (rather than "some svg exists") is what makes these cases
// meaningful: the fallback branch also renders an svg, so a bare presence
// check would pass no matter which persona was mapped.
//
// NOTE: the class comes from the icon's *internal* name, not the export
// name — e.g. `BarChart3` renders `lucide-chart-column`.
function iconClass(container: HTMLElement) {
  return container.querySelector('svg')?.getAttribute('class') ?? '';
}

describe('AgentAvatar', () => {
  it('renders the mapped icon and tone for a known persona', () => {
    const { container } = render(<AgentAvatar agentId="incident-investigator" />);
    expect(iconClass(container)).toContain('lucide-radar');
    expect(container.firstElementChild).toHaveClass('bg-violet-500/10');
  });

  it('normalizes the sre-agent alias onto specialist-sre', () => {
    const { container } = render(<AgentAvatar agentId="sre-agent" />);
    expect(iconClass(container)).toContain('lucide-activity');
    expect(container.firstElementChild).toHaveClass('bg-cyan-500/10');
  });

  it('normalizes loop-controller onto default and falls back for unknown ids', () => {
    const { container: a } = render(<AgentAvatar agentId="loop-controller" />);
    expect(iconClass(a)).toContain('lucide-bot');
    expect(a.firstElementChild).toHaveClass('bg-violet-500/10');
    const { container: b } = render(<AgentAvatar agentId="mystery-agent" />);
    expect(iconClass(b)).toContain('lucide-bot');
    expect(b.firstElementChild).toHaveClass('bg-violet-500/10');
  });

  it('gives each distinct persona its own icon', () => {
    const cases: Array<[string, string]> = [
      ['incident-investigator', 'lucide-radar'],
      ['critic', 'lucide-gavel'],
      ['reviewer', 'lucide-shield-check'],
      ['verifier', 'lucide-badge-check'],
      ['reporter', 'lucide-chart-column'],
      ['specialist-sre', 'lucide-activity'],
      ['specialist-network', 'lucide-network'],
      ['specialist-disk', 'lucide-hard-drive'],
      ['specialist-compute', 'lucide-cpu'],
      ['specialist-ops', 'lucide-server'],
      ['default', 'lucide-bot'],
    ];
    for (const [agentId, expected] of cases) {
      const { container } = render(<AgentAvatar agentId={agentId} />);
      expect(iconClass(container), `${agentId} icon`).toContain(expected);
    }
  });

  it('prefers the avatar image and falls back to the icon when it fails to load', () => {
    const { container } = render(<AgentAvatar agentId="default" avatar="http://x/y.png" />);
    const img = container.querySelector('img');
    expect(img).not.toBeNull();
    fireEvent.error(img as HTMLImageElement);
    expect(container.querySelector('img')).toBeNull();
    expect(iconClass(container)).toContain('lucide-bot');
  });

  it('scales 40px variants larger than 32px ones', () => {
    const { container: small } = render(<AgentAvatar agentId="default" />);
    const { container: large } = render(<AgentAvatar agentId="default" size={40} />);
    expect(small.firstElementChild).toHaveClass('h-8');
    expect(large.firstElementChild).toHaveClass('h-10');
  });

  // Guards the radius namespace. `rounded-rk-*` must stay on the `rk`
  // scale — reverting it to `rounded-s-*` silently re-collides with
  // Tailwind's built-in logical-corner namespace and the avatar renders
  // with two square corners. The class-name assertion catches the rename;
  // whether the class actually emits CSS is verified by grepping the
  // build artifact (see task report).
  it('uses the namespaced rk radius scale, not the logical-corner s-*', () => {
    const { container: small } = render(<AgentAvatar agentId="default" />);
    const { container: large } = render(<AgentAvatar agentId="default" size={40} />);
    expect(small.firstElementChild).toHaveClass('rounded-rk-sm');
    expect(large.firstElementChild).toHaveClass('rounded-rk-md');
    expect(small.firstElementChild?.className).not.toContain('rounded-s-');
    expect(large.firstElementChild?.className).not.toContain('rounded-s-');
  });

  it('renders an emoji avatar as text, not an <img>, when it is not an http(s) URL', () => {
    const { container } = render(<AgentAvatar agentId="default" avatar="🛰️" />);
    expect(container.querySelector('img')).toBeNull();
    expect(container.textContent).toContain('🛰️');
  });

  it('renders an https URL avatar as an <img>', () => {
    const { container } = render(<AgentAvatar agentId="default" avatar="https://x/y.png" />);
    const img = container.querySelector('img');
    expect(img).not.toBeNull();
    expect(img?.getAttribute('src')).toBe('https://x/y.png');
  });

  it('falls back to the role icon when avatar is absent', () => {
    const { container } = render(<AgentAvatar agentId="default" />);
    expect(container.querySelector('img')).toBeNull();
    expect(iconClass(container)).toContain('lucide-bot');
  });

  // phase4-ui-deepening:头像与 surface-card 邻接时补同色细环,消除扁平感。
  // (原先还打算加可访问名,但发言人姓名在各调用点已是可见文本,给头像再加
  //  aria-label 会造成屏幕阅读器重复播报 —— 保持图标 aria-hidden 才是正解。)
  it('carries a same-hue hairline ring so it keeps definition next to a card', () => {
    const { container } = render(<AgentAvatar agentId="incident-investigator" />);
    const cls = container.firstElementChild?.className ?? '';
    expect(cls).toContain('ring-1');
    expect(cls).toContain('ring-inset');
    expect(cls).toContain('ring-violet-500/20');
  });
});

// personaLabel (AgentBadge) must resolve a display name for every persona
// the design system knows about. A persona present in PERSONA_VISUALS but
// missing from AGENT_LABELS_ZH/EN falls through personaLabel's unknown-id
// branch and renders the raw agent_id — the exact drift that shipped
// critic / verifier / reporter unlocalized (Task 14C regression guard).
describe('personaLabel coverage', () => {
  it('resolves a localized label for every key in PERSONA_VISUALS', () => {
    const zhOnly = (zh: string) => zh;
    for (const agentId of Object.keys(PERSONA_VISUALS)) {
      expect(personaLabel(agentId, zhOnly), `${agentId} label`).not.toBe(agentId);
    }
  });
});