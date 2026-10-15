// persona store —— Agent 列表的唯一前端缓存，供全部展示位查 avatar。
//
// 设计要点（Design Doc §3.1）：
//   - ensureAgents() 拉取一次并缓存；并发调用共享同一 in-flight promise，
//     成功后再调不重拉。
//   - 拉取失败静默：头像自然回退角色图标，不产生用户可见错误。
//   - 启动时预取一次（ensureAgentsOnBoot），挂在 main.tsx，不在每个组件里拉。
//   - avatarFor 是纯函数（可安全用在 .map 循环里，无需 hook）。
import { create } from 'zustand';
import { listAgents, type AgentSummary } from '@/api/agents';
import { normalizeAgentId } from '@/components/AgentAvatar';

type AgentsState = {
  byName: Record<string, AgentSummary>;
  loading: Promise<void> | null;
  loaded: boolean;
  ensureAgents(): Promise<void>;
};

/** 纯查找：别名归一后取 avatar；空白 / 未知 → undefined（不编造）。 */
export function avatarFor(
  byName: Record<string, AgentSummary>,
  name?: string | null,
): string | undefined {
  if (!name) return undefined;
  const norm = normalizeAgentId(name);
  const v = (byName[norm] ?? byName[name])?.avatar;
  return v && v.trim() ? v : undefined;
}

export const useAgents = create<AgentsState>((set, get) => ({
  byName: {},
  loading: null,
  loaded: false,
  ensureAgents: () => {
    if (get().loaded) return Promise.resolve();
    const inflight = get().loading;
    if (inflight) return inflight;
    const p = listAgents()
      .then((r) => {
        const byName: Record<string, AgentSummary> = {};
        for (const a of r.items ?? []) byName[a.name] = a;
        set({ byName, loaded: true, loading: null });
      })
      .catch(() => {
        // 静默：回退角色图标，不产生用户可见错误。
        set({ loading: null });
      });
    set({ loading: p });
    return p;
  },
}));

/** 启动预取：main.tsx 调一次即可，失败静默。 */
export function ensureAgentsOnBoot(): void {
  void useAgents.getState().ensureAgents();
}
