// ToolCallBlock — 工具调用展示块。
// 在阶段详情（展开后）内展示一次 LLM/MCP 工具调用：状态点 + 工具名 +
// 入参 + 出参 + 延迟。
//
// 设计要点：
//   - 紧贴 mockup 01 "investigated" 段：font-mono + truncate 防止长
//     promql 撑破布局
//   - args/result 都可选；缺失对应行不渲染（不显示空白）
//   - 颜色：成功 emerald / 失败 red / 跳过 zinc；保持与状态点语义一致
import { cn } from '@/lib/cn';
import { PhaseStatusDot, type PhaseStatus } from './PhaseStatusDot';

interface ToolCallBlockProps {
  /** 工具方法名（snake_case 形如 `query_promql` / `linkRuntimeToCommit`） */
  name: string;
  /** 入参渲染文本（已格式化为单行） */
  args?: string;
  /** 出参渲染文本（已格式化为单行） */
  result?: string;
  status: PhaseStatus;
  /** 工具执行延迟，毫秒 */
  latencyMs?: number;
  className?: string;
}

export function ToolCallBlock({
  name,
  args,
  result,
  status,
  latencyMs,
  className,
}: ToolCallBlockProps) {
  return (
    <div className={cn('px-3 py-2 flex items-start gap-2', className)}>
      <PhaseStatusDot status={status} className="mt-1.5" />
      <div className="flex-1 min-w-0">
        <div className="flex items-center justify-between gap-2">
          <span className="text-xs font-mono text-zinc-200 truncate">{name}</span>
          {latencyMs !== undefined && (
            <span className="text-[10px] text-zinc-500 font-mono tabular-nums shrink-0">
              {latencyMs}ms
            </span>
          )}
        </div>
        {args && (
          <div className="mt-0.5 text-[11px] font-mono text-zinc-400 truncate" title={args}>
            {args}
          </div>
        )}
        {result && (
          <div className="mt-0.5 text-[11px] text-zinc-500 truncate" title={result}>
            <span className="text-zinc-600">value: </span>
            <span className="font-mono text-zinc-300">{result}</span>
          </div>
        )}
      </div>
    </div>
  );
}
