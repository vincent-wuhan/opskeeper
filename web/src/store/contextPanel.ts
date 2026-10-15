import { create } from 'zustand';
import { persist, createJSONStorage } from 'zustand/middleware';

// useContextPanel 记忆「会话上下文面板」是否展开。默认折叠——面板是一条
// 安静的侧栏,直到用户主动展开;选择持久化在独立 key 下,不与 C6 第一层
// 偏好持久化耦合。
type ContextPanelState = {
  expanded: boolean;
  setExpanded(v: boolean): void;
  toggle(): void;
};

export const useContextPanel = create<ContextPanelState>()(
  persist(
    (set) => ({
      expanded: false,
      setExpanded: (expanded) => set({ expanded }),
      toggle: () => set((s) => ({ expanded: !s.expanded })),
    }),
    {
      name: 'opskeeper.context-panel',
      storage: createJSONStorage(() => localStorage),
    },
  ),
);
