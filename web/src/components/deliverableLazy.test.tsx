import { renderHook } from '@testing-library/react';
import { describe, expect, it, afterEach } from 'vitest';
import { THUMB_CAP, shouldRenderThumb, useInViewOnce } from './deliverableLazy';
import { stubIntersectionObserver, triggerVisibleAt, unstubIntersectionObserver, getIOInstance } from '@/test/mockIO';

afterEach(unstubIntersectionObserver);

describe('shouldRenderThumb / THUMB_CAP', () => {
  it('allows indexes below the cap and rejects the rest', () => {
    expect(THUMB_CAP).toBe(3);
    expect(shouldRenderThumb(0)).toBe(true);
    expect(shouldRenderThumb(2)).toBe(true);
    expect(shouldRenderThumb(3)).toBe(false);
    expect(shouldRenderThumb(7)).toBe(false);
  });
  it('rejects invalid negative indexes', () => {
    expect(shouldRenderThumb(-1)).toBe(false);
  });
  it('honours an explicit cap instead of the default', () => {
    expect(shouldRenderThumb(1, 2)).toBe(true);
    expect(shouldRenderThumb(1, 1)).toBe(false);
    expect(shouldRenderThumb(0, 0)).toBe(false);
  });
});

describe('useInViewOnce', () => {
  it('starts invisible, flips once on intersect, stays visible (single fire)', () => {
    stubIntersectionObserver();
    const ref = { current: document.createElement('div') };
    const { result } = renderHook(() => useInViewOnce(ref));
    expect(result.current).toBe(false);
    const io = triggerVisibleAt(0);
    expect(result.current).toBe(true);
    // 单次触发:命中后必须断开观察器,不再产生后续变化
    expect(io.disconnect).toHaveBeenCalled();
    triggerVisibleAt(0);
    expect(result.current).toBe(true);
  });
  it('ignores non-intersecting entries', () => {
    stubIntersectionObserver();
    const ref = { current: document.createElement('div') };
    const { result } = renderHook(() => useInViewOnce(ref));
    expect(result.current).toBe(false);
    // 非命中不得误翻 visible
    triggerVisibleAt(0, false);
    expect(result.current).toBe(false);
    // 同一实例随后命中才翻转——证明 false 分支确实走了 .some() 的否定路径
    triggerVisibleAt(0, true);
    expect(result.current).toBe(true);
  });
  it('stays invisible without IntersectionObserver (jsdom default)', () => {
    const ref = { current: document.createElement('div') };
    const { result } = renderHook(() => useInViewOnce(ref));
    expect(result.current).toBe(false);
  });
  it('disconnects the observer on unmount', () => {
    stubIntersectionObserver();
    const ref = { current: document.createElement('div') };
    const { unmount } = renderHook(() => useInViewOnce(ref));
    // 卸载前尚未触发任何回调,disconnect 只能来自 effect 清理
    unmount();
    expect(getIOInstance(0).disconnect).toHaveBeenCalled();
  });
});
