import { beforeEach, describe, expect, it } from 'vitest';
import { useContextPanel } from './contextPanel';

beforeEach(() => {
  useContextPanel.setState({ expanded: false });
  localStorage.clear();
});

describe('contextPanel store', () => {
  it('默认折叠', () => {
    expect(useContextPanel.getState().expanded).toBe(false);
  });

  it('toggle 翻转展开态并写入持久化存储', () => {
    useContextPanel.getState().toggle();
    expect(useContextPanel.getState().expanded).toBe(true);
    expect(localStorage.getItem('opskeeper.context-panel') ?? '').toContain('"expanded":true');
  });

  it('setExpanded 显式设值', () => {
    useContextPanel.getState().setExpanded(true);
    expect(useContextPanel.getState().expanded).toBe(true);
    useContextPanel.getState().setExpanded(false);
    expect(useContextPanel.getState().expanded).toBe(false);
  });
});
