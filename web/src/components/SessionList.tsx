// SessionList — the sidebar's 对话 group, extracted from Sidebar.tsx so the
// row's visual language can evolve without touching the ~900-line nav file.
//
// Task 11 is a *visual* upgrade, not a rewrite: double-click rename, the
// inline editor, the explicit Pencil/Trash2 buttons and the hover-reveal
// padding all carried over unchanged. What changed is what's inside the row
// — an AgentAvatar(32) + localized persona name above a truncated title,
// replacing the old "title + AgentBadge chip" pairing.
//
// Slicing (5 vs 10 rows), the empty state and the expand/collapse toggle
// stay in Sidebar, so this component only ever renders what it's handed.
import { useEffect, useRef, useState } from 'react';
import { NavLink, useLocation } from 'react-router-dom';
import { Pencil, Trash2 } from 'lucide-react';
import { AgentAvatar } from './AgentAvatar';
import { personaLabel } from './AgentBadge';
import { useI18n } from '@/i18n/locale';
import { cn } from '@/lib/cn';
import { renameSession, type ChatSession } from '@/api/chat';
import { invalidateChatSessions } from '@/store/chatSessions';
import { useAgents, avatarFor } from '@/store/agents';

export function SessionList({
  sessions,
  onDelete,
}: {
  sessions: ChatSession[];
  /** Receives the row's session — Sidebar owns the delete modal + its state. */
  onDelete: (session: ChatSession) => void;
}) {
  return (
    <ul className="space-y-0.5">
      {sessions.map((s, index) => (
        <SessionRow key={s.id} session={s} index={index} onDelete={onDelete} />
      ))}
    </ul>
  );
}

function SessionRow({
  session,
  index,
  onDelete,
}: {
  session: ChatSession;
  index: number;
  onDelete: (session: ChatSession) => void;
}) {
  const { tr } = useI18n();
  const location = useLocation();
  const byName = useAgents((s) => s.byName);
  const fallbackTitle = tr(`会话 ${index + 1}`, `Session ${index + 1}`);
  const displayTitle = session.title || fallbackTitle;
  // personaLabel is handed the reactive `tr` (not the module-level one) so
  // flipping the language repaints these rows without a reload.
  const persona = personaLabel(session.agent_id, tr);
  const [renaming, setRenaming] = useState(false);
  const [draft, setDraft] = useState(session.title || '');
  const [saving, setSaving] = useState(false);
  const inputRef = useRef<HTMLInputElement | null>(null);

  // NavLink only hands `isActive` to its className callback, which renders
  // *inside* the row — the <li> above it can't read it. Re-deriving it from
  // the location is equivalent here: a session row's `to` is always the
  // bare `/chat/<id>` path with no query or trailing segment, which is the
  // one case where NavLink's prefix match collapses to path equality. The
  // className callback below still uses NavLink's own isActive for the
  // styling, so the highlight can't drift from the attribute.
  const isActive = location.pathname === `/chat/${session.id}`;

  // When external session title changes (e.g. another tab renamed it
  // and invalidateChatSessions refetched) sync the draft so the next
  // edit starts from the latest value rather than a stale string.
  useEffect(() => {
    if (!renaming) setDraft(session.title || '');
  }, [session.title, renaming]);

  const enterRename = () => {
    setDraft(session.title || '');
    setRenaming(true);
    // focus + select on next tick so the input is mounted.
    setTimeout(() => inputRef.current?.select(), 0);
  };

  const cancelRename = () => {
    setRenaming(false);
    setDraft(session.title || '');
  };

  const commit = async () => {
    const t = draft.trim();
    if (t === '' || t === (session.title || '')) {
      cancelRename();
      return;
    }
    setSaving(true);
    try {
      await renameSession(session.id, t);
      invalidateChatSessions();
      setRenaming(false);
    } catch {
      // Keep editor open on failure so the user can retry.
    } finally {
      setSaving(false);
    }
  };

  return (
    <li data-session-id={session.id} data-active={isActive ? 'true' : 'false'}>
      {renaming ? (
        <div className="group relative">
          <div className="flex items-center gap-1.5 rounded-rk-md bg-zinc-800/80 py-1 pl-2 pr-7">
            <input
              ref={inputRef}
              value={draft}
              disabled={saving}
              onChange={(e) => setDraft(e.target.value)}
              onBlur={() => void commit()}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault();
                  void commit();
                } else if (e.key === 'Escape') {
                  e.preventDefault();
                  cancelRename();
                }
              }}
              className="w-full bg-transparent text-[13px] text-zinc-100 outline-none placeholder:text-zinc-600"
              placeholder={fallbackTitle}
              maxLength={256}
            />
          </div>
        </div>
      ) : (
        <div className="group relative">
          <NavLink
            to={`/chat/${session.id}`}
            title={displayTitle}
            onDoubleClick={(e) => {
              e.preventDefault();
              e.stopPropagation();
              enterRename();
            }}
            className={({ isActive: linkActive }) =>
              cn(
                'flex items-center gap-2 rounded-rk-md py-1.5 pl-2 pr-12 text-[13px] transition-colors',
                'hover:bg-zinc-800/60 hover:text-zinc-100',
                linkActive && 'bg-zinc-800/80 text-zinc-100'
              )
            }
          >
            <AgentAvatar agentId={session.agent_id} size={32} avatar={avatarFor(byName, session.agent_id)} />
            <span className="min-w-0 flex-1">
              {persona && (
                <span className="block truncate text-[11px] leading-tight text-zinc-500">{persona}</span>
              )}
              <span className="block truncate">{displayTitle}</span>
            </span>
          </NavLink>
          <button
            type="button"
            aria-label={tr('重命名会话', 'Rename session')}
            title={tr('双击会话名也可重命名', 'Double-click the title to rename')}
            onClick={(e) => {
              e.preventDefault();
              e.stopPropagation();
              enterRename();
            }}
            className={cn(
              'absolute right-7 top-1/2 -translate-y-1/2 rounded p-1 text-zinc-600 transition-opacity',
              'opacity-0 hover:bg-zinc-800 hover:text-zinc-200 focus:opacity-100 group-hover:opacity-100'
            )}
          >
            <Pencil size={12} />
          </button>
          <button
            type="button"
            aria-label={tr('删除会话', 'Delete session')}
            onClick={(e) => {
              e.preventDefault();
              e.stopPropagation();
              onDelete(session);
            }}
            className={cn(
              'absolute right-1 top-1/2 -translate-y-1/2 rounded p-1 text-zinc-600 transition-opacity',
              'opacity-0 hover:bg-red-900/30 hover:text-red-300 focus:opacity-100 group-hover:opacity-100'
            )}
          >
            <Trash2 size={12} />
          </button>
        </div>
      )}
    </li>
  );
}
