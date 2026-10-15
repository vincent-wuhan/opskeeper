import { request } from './client';

// Approvals (propose-confirm inbox) API — /v1/approvals/* (HLD-017).
// A dangerous action proposed by an agent (or a flow approval node) waits
// here for a human to approve (→ executes) or reject. Admin-only.

export interface Approval {
  id: string;
  kind: string;
  title: string;
  summary: string;
  payload: string;
  source: string;
  session_id?: string;
  status: 'pending' | 'approved' | 'rejected' | 'executed' | 'failed';
  // Producer's own statement of how far the action reaches (approval
  // model.go:59-79). Present on rows written since 2d58a31; absent on older
  // rows, hence optional. GET /v1/approvals/{id} serialises the whole model
  // row, so these already reach the browser.
  risk_class?: string;
  blast_radius?: string;
  target?: string;
  // JSON string of Signer[] (user_id/role/at); present once signatures exist.
  // A pending row with one signer means dual-sign is still waiting for another.
  signers?: string;
  proposed_by: number;
  approved_by?: number;
  reason?: string;
  result?: string;
  created_at: string;
  decided_at?: string;
  executed_at?: string;
}

export function listApprovals(status?: string) {
  const qs = status ? `?status=${encodeURIComponent(status)}` : '';
  return request<{ items: Approval[] }>('GET', `/approvals${qs}`);
}

export function approvalsPendingCount() {
  return request<{ pending: number }>('GET', '/approvals/count');
}

export function getApproval(id: string) {
  return request<Approval>('GET', `/approvals/${id}`);
}

export function approveApproval(id: string) {
  return request<Approval>('POST', `/approvals/${id}/approve`);
}

export function rejectApproval(id: string, reason: string) {
  return request<{ ok: boolean }>('POST', `/approvals/${id}/reject`, { reason });
}
