// ChatDrawer — 诊断对话抽屉（D13-D16 of zero-manual-ops-loop）。
//
// 两种形态：
//   1. 嵌入式右抽屉（default）：在 ClosedLoopTimeline / IncidentDetail
//      旁边 240px 宽弹出，复用同一个组件
//   2. 独立路由 `/incidents/:id/diagnose`：独立页面也用同一组件
//
// 设计要点：
//   - 视觉锁版 v1 mockup（参考 mockup 04-chat-drawer-thread）
//   - 复用 DPO 组件：PhaseIndicator / ToolCallBlock / ProcessTimeline
//   - 复用 opskeeper/ui：Card / Chip / ChatInput
//   - i18n：tr('中文','English')
//   - a11y：role="dialog" + aria-modal + Esc 关闭 + Tab 键盘可达
//
// 已知 stub（Day 9+ 替代）：
//   - fetch /api/v1/chat/diagnose stubbed（前端 mock 响应，方便视觉验证）
//   - @-mention chip 仅高亮显示
//   - 启动修复按钮基于 ROOT_CAUSE_OBJECT sentinel 占位
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { X, Send, Sparkles } from 'lucide-react';
import { Card } from '@/components/ui/Card';
import { Chip } from '@/components/ui/Chip';
import { Button } from '@/components/ui/Button';
import { PageHeader } from '@/components/ui/PageHeader';
import { EmptyState } from '@/components/ui/EmptyState';
import {
  PhaseIndicator,
  ToolCallBlock,
  type TimelineToolCall,
} from '@/components/dpo';
import { useI18n } from '@/i18n/locale';
import { cn } from '@/lib/cn';

interface ChatMessage {
  id: string;
  role: 'user' | 'assistant';
  content: string;
  toolCalls?: TimelineToolCall[];
  rootCauseObject?: string;
  canPromote?: boolean;
  createdAt: string;
}

export interface ChatDrawerProps {
  incidentId: string;
  onClose?: () => void;
  variant?: 'drawer' | 'page';
}

export default function ChatDrawer(props: ChatDrawerProps) {
  const { incidentId, onClose, variant = 'drawer' } = props;
  const { tr } = useI18n();
  const navigate = useNavigate();
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);
  const scrollRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    setMessages([
      {
        id: 'sys-welcome',
        role: 'assistant',
        content: tr(
          '我是 sre-copilot。把告警上下文贴过来或 @agent <名> 切换专家；修复建议收敛后我会给出"启动修复"按钮。',
          'I am sre-copilot. Paste alert context or @agent <slug> to switch specialist. Once a remediation converges, the "启动修复" button appears.'
        ),
        createdAt: new Date().toISOString(),
      },
    ]);
  }, [tr]);

  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight });
  }, [messages]);

  const sendMessage = useCallback(async () => {
    const text = input.trim();
    if (!text) return;
    setBusy(true);
    const userMsg: ChatMessage = {
      id: 'u-' + Date.now(),
      role: 'user',
      content: text,
      createdAt: new Date().toISOString(),
    };
    setMessages((m) => [...m, userMsg]);
    setInput('');
    await new Promise((r) => setTimeout(r, 600));
    const assistantMsg: ChatMessage = {
      id: 'a-' + Date.now(),
      role: 'assistant',
      content: tr(
        '已收到。让我先用 KB 知识库检索…',
        'Got it. Let me search the KB knowledge base first…'
      ),
      toolCalls: [
        {
          name: 'kb_lookup',
          args: '{resource_type:"pg", symptom:"' + text.slice(0, 32) + '"}',
          result: '{hits: 3, top_score: 0.91}',
          status: 'success',
          latencyMs: 80,
        },
        {
          name: 'query_promql',
          args: '{expr:"pg_stat_activity_count"}',
          result: '{current: 18}',
          status: 'success',
          latencyMs: 220,
        },
      ],
      createdAt: new Date().toISOString(),
    };
    setMessages((m) => [...m, assistantMsg]);
    if (text.toLowerCase().includes('pg') || text.indexOf('长事务') >= 0) {
      const converge: ChatMessage = {
        id: 'a-' + (Date.now() + 1),
        role: 'assistant',
        content: tr(
          '根因收敛：ROOT_CAUSE_OBJECT:pg.long_running_txn。建议方案：pg.kill_session（risk=mutating）。可启动修复。',
          'Root cause converged: ROOT_CAUSE_OBJECT:pg.long_running_txn. Suggestion: pg.kill_session (risk=mutating). Ready to promote.'
        ),
        rootCauseObject: 'pg.long_running_txn',
        canPromote: true,
        createdAt: new Date().toISOString(),
      };
      setMessages((m) => [...m, converge]);
    }
    setBusy(false);
  }, [input, tr]);

  const handlePromote = useCallback(() => {
    navigate('/incidents/' + encodeURIComponent(incidentId) + '/loop');
  }, [incidentId, navigate]);

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        void sendMessage();
      } else if (e.key === 'Escape' && onClose) {
        e.preventDefault();
        onClose();
      }
    },
    [sendMessage, onClose]
  );

  const isPage = variant === 'page';
  const containerCls = isPage
    ? 'flex flex-col h-full max-w-3xl mx-auto px-6 py-5'
    : 'flex flex-col h-full';

  return (
    <div
      role={isPage ? 'main' : 'dialog'}
      aria-modal={isPage ? undefined : 'true'}
      aria-label={tr('诊断对话', 'Diagnostic Chat')}
      className={containerCls}
    >
      <div className="flex items-center justify-between mb-3">
        <div>
          <div className="text-[11px] uppercase tracking-wider text-zinc-500 mb-0.5">
            {tr('Incidents / Chat', 'Incidents / Chat')}
          </div>
          <h2 className="text-sm font-semibold text-zinc-100 flex items-center gap-2">
            <Sparkles size={14} className="text-indigo-500" />
            {tr('诊断对话', 'Diagnostic Chat')}
          </h2>
          <div className="mt-0.5 text-[11px] text-zinc-500 font-mono">
            incident_id: {incidentId}
          </div>
        </div>
        {onClose && (
          <Button variant="ghost" onClick={onClose} aria-label={tr('关闭', 'Close')}>
            <X size={14} />
          </Button>
        )}
      </div>

      <Card className="flex-1 overflow-hidden flex flex-col">
        <div ref={scrollRef} className="flex-1 overflow-y-auto p-3 space-y-3">
          {messages.length === 0 ? (
            <EmptyState
              title={tr('暂无消息', 'No messages')}
              hint={tr('输入问题开始对话', 'Type a question to start')}
            />
          ) : (
            messages.map((m) => (
              <MessageBubble key={m.id} message={m} onPromote={handlePromote} />
            ))
          )}
        </div>

        <div className="border-t border-zinc-800/60 p-2 flex items-end gap-2">
          <textarea
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={handleKeyDown}
            disabled={busy}
            placeholder={tr(
              '输入 @agent 或问题；Enter 发送，Shift+Enter 换行',
              'Type @agent or question; Enter sends, Shift+Enter newline'
            )}
            rows={2}
            aria-label={tr('消息输入', 'Message input')}
            className={cn(
              'flex-1 resize-none bg-zinc-900/60 border border-zinc-800/60',
              'rounded px-2 py-1.5 text-xs text-zinc-100 placeholder-zinc-500',
              'focus:outline-none focus:border-indigo-500/60'
            )}
          />
          <Button
            variant="primary"
            onClick={() => void sendMessage()}
            disabled={busy || !input.trim()}
            aria-label={tr('发送', 'Send')}
          >
            <Send size={14} />
          </Button>
        </div>
      </Card>
    </div>
  );
}

function MessageBubble({
  message,
  onPromote,
}: {
  message: ChatMessage;
  onPromote: () => void;
}) {
  const { tr } = useI18n();
  const isUser = message.role === 'user';
  const renderedContent = useMemo(() => {
    const m = /^(@\w+)\s*(.*)$/.exec(message.content);
    if (!m) return message.content;
    return (
      <span>
        <Chip tone="accent" className="mr-1.5 font-mono">
          {m[1]}
        </Chip>
        {m[2]}
      </span>
    );
  }, [message.content]);

  return (
    <div
      className={cn(
        'flex flex-col gap-1.5',
        isUser ? 'items-end' : 'items-start'
      )}
    >
      <div className="flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-zinc-500">
        <span>{isUser ? tr('用户', 'User') : tr('助手', 'Assistant')}</span>
        <span>·</span>
        <span className="font-mono">{fmtClock(message.createdAt)}</span>
      </div>
      <div
        className={cn(
          'max-w-[90%] rounded-md px-3 py-2 text-sm leading-relaxed',
          isUser
            ? 'bg-indigo-600/15 border border-indigo-500/30 text-zinc-100'
            : 'bg-zinc-900/60 border border-zinc-800/60 text-zinc-200'
        )}
      >
        {renderedContent}
      </div>

      {message.toolCalls && message.toolCalls.length > 0 && (
        <div className="max-w-[90%] w-full space-y-1">
          {message.toolCalls.map((tc, i) => (
            <ToolCallBlock
              key={message.id + '-tc-' + i}
              name={tc.name}
              args={tc.args}
              result={tc.result}
              status={tc.status}
              latencyMs={tc.latencyMs}
            />
          ))}
        </div>
      )}

      {message.canPromote && (
        <div className="max-w-[90%] flex items-center gap-2">
          <PhaseIndicator
            phase="correlated"
            status="success"
            duration="+0ms"
          />
          <Button variant="primary" onClick={onPromote}>
            {tr('启动修复', 'Promote to Loop')}
          </Button>
        </div>
      )}
    </div>
  );
}

function fmtClock(iso: string): string {
  try {
    const d = new Date(iso);
    return d.toTimeString().slice(0, 8);
  } catch {
    return '--:--:--';
  }
}

export function ChatDrawerPage() {
  const { id = '' } = useParams<{ id: string }>();
  const { tr } = useI18n();
  const navigate = useNavigate();
  return (
    <div className="flex flex-col h-full">
      <PageHeader
        title={tr('诊断对话', 'Diagnostic Chat')}
        subtitle={
          <span className="font-mono">incident_id: {id}</span>
        }
        actions={
          <Button
            variant="ghost"
            onClick={() => navigate('/incidents/' + encodeURIComponent(id) + '/loop')}
          >
            {tr('查看闭环', 'View Loop')}
          </Button>
        }
      />
      <div className="flex-1 min-h-0">
        <ChatDrawer
          incidentId={id}
          variant="page"
          onClose={() => navigate('/incidents/' + encodeURIComponent(id))}
        />
      </div>
    </div>
  );
}

export function ChatDrawerPanel({
  incidentId,
  onClose,
}: {
  incidentId: string;
  onClose: () => void;
}) {
  return (
    <div className="w-60 fixed right-0 top-0 bottom-0 z-30 border-l border-zinc-800/60 bg-zinc-950 p-2 shadow-2xl">
      <ChatDrawer incidentId={incidentId} variant="drawer" onClose={onClose} />
    </div>
  );
}
