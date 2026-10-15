import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { PhaseIndicator } from './PhaseIndicator';

// The status chip used to print the raw phase token ("success"); it now
// prints the same localized label PhaseStatusDot announces to screen
// readers. Locate the chip through its transition classes rather than its
// text so these assertions stay about motion, not copy.
function statusChip(container: HTMLElement): Element | null {
  return container.querySelector('span[class*="motion-safe:transition-colors"]');
}

describe('PhaseIndicator motion', () => {
  it('gives the status chip a motion-safe 200ms colour transition', () => {
    const { container } = render(<PhaseIndicator phase="recovered" status="success" />);
    const chip = statusChip(container);
    expect(chip?.className).toContain('motion-safe:transition-colors');
    expect(chip?.className).toContain('motion-safe:duration-200');
  });

  it('gates the colour transition behind prefers-reduced-motion (no bare transition)', () => {
    const { container } = render(<PhaseIndicator phase="recovered" status="success" />);
    const chip = statusChip(container);
    // 断言是 motion-safe 变体，而非裸工具类 —— 减少动态效果下 Tailwind 不生成该声明。
    expect(chip?.className).not.toMatch(/(^|\s)transition-colors(\s|$)/);
  });
});

describe('PhaseIndicator status label', () => {
  it('shows a localized status label instead of the raw phase token', () => {
    render(<PhaseIndicator phase="recovered" status="success" />);
    expect(screen.queryByText('success')).toBeNull();
    expect(screen.getByText(/Success|成功/)).toBeTruthy();
  });

  it('falls back to the localized label for a pending phase', () => {
    render(<PhaseIndicator phase="detected" status="pending" />);
    expect(screen.getByText(/Pending|待执行/)).toBeTruthy();
  });
});
