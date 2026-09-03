// AgentChip — @agent 前端 chip 渲染。
// 用于在聊天输入 / 已渲染消息中，把用户输入的 @sre-agent / @reporter /
// @incident-investigator / @critic / @loop-controller 等前缀可视化为
// indigo 配色的紧凑 chip。
//
// 设计要点：
//   - 不参与路由：路由由后端 MentionedAgent 解析（Day 2.7 后端部分）
//   - 大小两档：sm（10px，行内）和 md（12px，段落首 chip）
//   - 未知 agent 走 agentId 原值，保证新角色立即可用
//   - 与项目 AgentBadge 的差别：AgentBadge 用于 chat session 元数据（带
//     边框 + Bot icon + tooltip），AgentChip 用于消息流内的 @-mention
//     （无边框 + 透明背景）。视觉锁版任务 2.7 草图。
import { useI18n } from '@/i18n/locale';
import { cn } from '@/lib/cn';

const AGENT_LABEL_ZH: Record<string, string> = {
  'sre-agent': 'SRE 助手',
  'incident-investigator': '事故调查',
  critic: '评审',
  reporter: '报告',
  'loop-controller': '闭环控制',
};

const AGENT_LABEL_EN: Record<string, string> = {
  'sre-agent': 'SRE Agent',
  'incident-investigator': 'Incident Investigator',
  critic: 'Critic',
  reporter: 'Reporter',
  'loop-controller': 'Loop Controller',
};

export interface AgentChipProps {
  /** agent ID（不含 @ 前缀） */
  agent: string;
  size?: 'sm' | 'md';
  className?: string;
}

export function AgentChip({ agent, size = 'sm', className }: AgentChipProps) {
  const { tr } = useI18n();
  const zh = AGENT_LABEL_ZH[agent] ?? agent;
  const en = AGENT_LABEL_EN[agent] ?? agent;
  return (
    <span
      className={cn(
        'inline-flex items-center gap-0.5 rounded-md bg-indigo-500/10 text-indigo-300 font-mono',
        size === 'sm' ? 'px-1.5 py-0.5 text-[10px]' : 'px-2 py-1 text-xs',
        className,
      )}
      data-agent={agent}
    >
      <span className="opacity-60">@</span>
      <span>{tr(zh, en)}</span>
    </span>
  );
}
