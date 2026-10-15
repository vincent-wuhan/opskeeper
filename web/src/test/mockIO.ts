// 测试专用 IntersectionObserver 桩:jsdom 无 IO。手动触发回调驱动 useInViewOnce;
// test-local 手写,不新增运行时依赖(Non-Goal)。
import { act } from '@testing-library/react';
import { vi } from 'vitest';

type MockEntry = { isIntersecting: boolean; target: Element };

let instances: Array<{ callback: (entries: MockEntry[]) => void; observe: ReturnType<typeof vi.fn>; disconnect: ReturnType<typeof vi.fn> }> = [];

class MockIntersectionObserver {
  observe = vi.fn();
  unobserve = vi.fn();
  disconnect = vi.fn();
  callback: (entries: MockEntry[]) => void;
  constructor(cb: (entries: MockEntry[]) => void) {
    this.callback = cb;
    instances.push(this);
  }
}

export function stubIntersectionObserver() {
  instances = [];
  vi.stubGlobal('IntersectionObserver', MockIntersectionObserver);
}

// 触发第 index 个创建的观察器回调(实例按创建顺序入数组 = 卡片挂载顺序)。
// isIntersecting 默认 true:既有调用方 triggerVisibleAt(i) 语义不变;传 false 构造
// 非命中 entry,用于覆盖 useInViewOnce 的 entries.some() 否定分支。返回被触发的实例,
// 便于测试断言 disconnect 等契约。
export function triggerVisibleAt(index: number, isIntersecting: boolean = true) {
  const io = instances[index];
  if (!io) throw new Error(`no IntersectionObserver instance at ${index}; created: ${instances.length}`);
  act(() => io.callback([{ isIntersecting, target: document.body }]));
  return io;
}

export function unstubIntersectionObserver() {
  vi.unstubAllGlobals();
  instances = [];
}

// 只读访问第 index 个观察器实例:用于断言 stub 自身的契约(如 unmount 后的
// disconnect),不产生回调、不断言调用顺序。index 越界抛错,便于定位。
export function getIOInstance(index: number) {
  const io = instances[index];
  if (!io) throw new Error(`no IntersectionObserver instance at ${index}; created: ${instances.length}`);
  return io;
}
