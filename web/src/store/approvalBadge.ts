// Pending-approval count for the sidebar 审批中心 pill. Mirrors
// incidentBadge.ts: one 30 s poll while the app is mounted, shared by
// every consumer (expanded nav item today, collapsed rail later), with
// the handle kept in state so test code (and HMR) can reset it.
//
// Why the same 30 s cadence: it is already the app's badge heartbeat —
// 告警 red dot and 审批 red dot arriving on different cadences would
// read as two unrelated systems.
//
// Gates, in order of cheapness:
//   1. no token → stop polling (same reason as incidentBadge: no 401 storm)
//   2. role !== 'admin' → don't call at all. Every handler under
//      /v1/approvals (list/count/get/approve/reject) is behind
//      requireAdmin, so a non-admin poll is a guaranteed 403 — and the
//      catch below is silent, which would hide the whole storm. This is
//      the store-side half of the guard; the sidebar also hides the
//      whole 审批 group from non-admins (the UI-side half). Both stay.
import { create } from 'zustand';
import { approvalsPendingCount } from '@/api/approvals';
import { useAuth } from './auth';

type State = {
  pending: number;
  // The poll handle is kept so test code (and HMR) can reset it.
  _timer: number | null;
  start(): void;
  stop(): void;
  refresh(): Promise<void>;
};

const POLL_INTERVAL_MS = 30_000;

export const useApprovalBadge = create<State>((set, get) => ({
  pending: 0,
  _timer: null,
  refresh: async () => {
    const auth = useAuth.getState();
    if (!auth.token || auth.role !== 'admin') return;
    try {
      const { pending } = await approvalsPendingCount();
      set({ pending: pending ?? 0 });
    } catch {
      // Silent — the badge is best-effort chrome. A missing red dot must
      // never break the shell, and the 审批中心 page itself surfaces the
      // real error if the user clicks through.
    }
  },
  start: () => {
    if (get()._timer != null) return;
    void get().refresh();
    const id = window.setInterval(() => {
      void get().refresh();
    }, POLL_INTERVAL_MS);
    set({ _timer: id });
  },
  stop: () => {
    const id = get()._timer;
    if (id != null) {
      window.clearInterval(id);
      set({ _timer: null });
    }
  },
}));
