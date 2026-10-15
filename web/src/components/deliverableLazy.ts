// deliverableLazy — 交付物卡懒挂载纯逻辑(design §5)。无 UI,可单测。
import { useEffect, useState, type RefObject } from 'react';

// 单条消息缩略上限(初值 3,build 阶段可调)。
export const THUMB_CAP = 3;

// useInViewOnce — 元素首次进入视口后 visible 恒为 true(单次触发)。
// IntersectionObserver 不可用(测试环境)时恒 false:调用方不 fetch、不挂 iframe。
export function useInViewOnce(ref: RefObject<Element | null>): boolean {
  const [visible, setVisible] = useState(false);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (typeof IntersectionObserver === 'undefined') return;
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          setVisible(true);
          io.disconnect();
        }
      },
      { threshold: 0.1 },
    );
    io.observe(el);
    return () => io.disconnect();
  }, [ref]);
  return visible;
}

// shouldRenderThumb — 超出单条消息缩略上限的交付物返回 false(紧凑卡)。
export function shouldRenderThumb(index: number, cap: number = THUMB_CAP): boolean {
  return index >= 0 && index < cap;
}
