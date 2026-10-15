import { Link } from 'react-router-dom';
import {
  Server, AlertTriangle, Bell, FileText, BookOpen, Compass, PanelRightOpen, PanelRightClose,
} from 'lucide-react';
import { cn } from '@/lib/cn';
import { EmptyState } from '@/components/ui';
import { useI18n, tr as trInline } from '@/i18n/locale';
import { useContextPanel } from '@/store/contextPanel';
import {
  mentionRoute,
  type SessionContext,
  type MentionRef,
  type KnowledgeRef,
} from '@/lib/sessionContext';
import type { MentionType } from '@/api/chat';

const TYPE_ORDER: MentionType[] = ['device', 'incident', 'rule', 'file'];

function mentionIcon(type: MentionType) {
  const cls = 'shrink-0 text-zinc-400';
  switch (type) {
    case 'device': return <Server size={13} className={cls} />;
    case 'incident': return <AlertTriangle size={13} className={cls} />;
    case 'rule': return <Bell size={13} className={cls} />;
    case 'file': return <FileText size={13} className={cls} />;
  }
}

function typeLabel(type: MentionType): string {
  switch (type) {
    case 'device': return trInline('设备', 'Device');
    case 'incident': return trInline('事件', 'Incident');
    case 'rule': return trInline('规则', 'Rule');
    case 'file': return trInline('日志文件', 'Log file');
  }
}

function SourceCaption({ index }: { index: number }) {
  const { tr } = useI18n();
  return (
    <span className="shrink-0 text-[10px] text-zinc-600">
      {tr(`来源 · 第 ${index + 1} 条`, `Source · msg ${index + 1}`)}
    </span>
  );
}

// ContextPanel 只读呈现本会话的上下文依据,数据由调用方经 deriveSessionContext
// 反推后传入;面板自身不发请求、不提供编辑/删除。
export function ContextPanel({ context }: { context: SessionContext }) {
  const { tr } = useI18n();
  const expanded = useContextPanel((s) => s.expanded);
  const toggle = useContextPanel((s) => s.toggle);
  const empty = context.mentions.length === 0 && context.knowledgeRefs.length === 0;

  const groups = TYPE_ORDER
    .map((type) => ({ type, refs: context.mentions.filter((m) => m.type === type) }))
    .filter((g) => g.refs.length > 0);

  return (
    <aside
      data-testid="context-panel"
      aria-label={tr('会话上下文', 'Session context')}
      className={cn(
        'flex shrink-0 flex-col border-l border-zinc-800 bg-zinc-950/60 transition-[width]',
        expanded ? 'w-72' : 'w-11',
      )}
    >
      <div className="flex items-center justify-between gap-2 border-b border-zinc-800 px-2 py-2">
        {expanded && (
          <span className="flex min-w-0 items-center gap-1.5 text-xs font-medium text-zinc-300">
            <Compass size={14} className="shrink-0 text-zinc-500" />
            <span className="truncate">{tr('会话上下文', 'Session context')}</span>
          </span>
        )}
        <button
          type="button"
          aria-expanded={expanded}
          aria-label={expanded ? tr('收起上下文面板', 'Collapse context panel') : tr('展开上下文面板', 'Expand context panel')}
          onClick={toggle}
          className={cn(
            'rounded-rk-sm p-1 text-zinc-500 hover:bg-zinc-800 hover:text-zinc-200',
            !expanded && 'mx-auto',
          )}
        >
          {expanded ? <PanelRightClose size={15} /> : <PanelRightOpen size={15} />}
        </button>
      </div>

      {expanded && (
        <div className="flex-1 space-y-4 overflow-y-auto px-3 py-3">
          {empty ? (
            <EmptyState
              icon={Compass}
              title={tr('本会话还没有可反推的上下文', 'No derivable context yet')}
              hint={tr('在消息里 @ 运维对象,或让 Agent 查知识库,依据会出现在这里。', 'Mention objects with @, or ask the agent to search the knowledge base — evidence shows up here.')}
              className="flex h-40 flex-col items-center justify-center gap-2 px-2 text-center"
            />
          ) : (
            <>
              {groups.length > 0 && (
                <section data-testid="context-mentions" className="space-y-1.5">
                  <h4 className="text-[11px] uppercase tracking-wide text-zinc-500">
                    {tr('@提及对象', 'Mentioned objects')}
                  </h4>
                  {groups.map((g) => (
                    <div key={g.type} className="space-y-1">
                      {g.refs.map((m: MentionRef) => (
                        <Link
                          key={`${m.type}:${m.id}`}
                          to={mentionRoute(m.type, m.id)}
                          data-message-id={m.messageId}
                          data-source-index={m.sourceIndex}
                          className="flex items-center gap-2 rounded-rk-sm px-2 py-1 text-xs text-zinc-200 hover:bg-zinc-900"
                        >
                          {mentionIcon(m.type)}
                          <span className="truncate">{m.label}</span>
                          <span className="ml-auto shrink-0 text-[10px] text-zinc-600">{typeLabel(m.type)}</span>
                          <SourceCaption index={m.sourceIndex} />
                        </Link>
                      ))}
                    </div>
                  ))}
                </section>
              )}

              {context.knowledgeRefs.length > 0 && (
                <section data-testid="context-knowledge" className="space-y-1.5">
                  <h4 className="text-[11px] uppercase tracking-wide text-zinc-500">
                    {tr('知识引用', 'Knowledge references')}
                  </h4>
                  {context.knowledgeRefs.map((k: KnowledgeRef, i: number) => (
                    <div
                      key={`${k.name}-${i}`}
                      data-message-id={k.messageId}
                      data-source-index={k.sourceIndex}
                      className="flex items-center gap-2 rounded-rk-sm px-2 py-1 text-xs text-zinc-200"
                    >
                      <BookOpen size={13} className="shrink-0 text-zinc-400" />
                      <span className="truncate">{k.name}</span>
                      <span className="ml-auto shrink-0 text-[10px] text-zinc-500">{k.status}</span>
                      {typeof k.durationMs === 'number' && (
                        <span className="shrink-0 text-[10px] text-zinc-600">{k.durationMs}ms</span>
                      )}
                      <SourceCaption index={k.sourceIndex} />
                    </div>
                  ))}
                </section>
              )}
            </>
          )}
        </div>
      )}
    </aside>
  );
}
