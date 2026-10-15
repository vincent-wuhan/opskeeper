import type { Approval } from '@/api/approvals';

// 审批签署解析与双签状态映射的单一来源:审批入口页与消息内嵌审批卡共用,
// 消除「一处已签署、一处待处理」的措辞漂移。纯函数,便于单测。

export interface Signer {
  user_id: number | string;
  role?: string;
  at?: string;
}

// 平台双签策略常量:危险操作需 2 位批准人(ADR-019 / 后端 dualsign_test.go)。
// 后端未按行暴露「所需签署人数」,故以策略常量呈现;known=false 时不显示人数。
export const DUAL_SIGN_REQUIRED = 2;

// parseSigners 宽容解析 Approval.signers(JSON 字符串数组)。
// 缺失 / 空串 / 不可解析 / 非数组 → { signers: [], known: false },不猜人数、不抛错。
export function parseSigners(json?: string): { signers: Signer[]; known: boolean } {
  if (!json) return { signers: [], known: false };
  try {
    const arr = JSON.parse(json);
    if (!Array.isArray(arr)) return { signers: [], known: false };
    return { signers: arr as Signer[], known: true };
  } catch {
    return { signers: [], known: false };
  }
}

export type DualSignState = 'unknown' | 'none' | 'partial' | 'decided';

// status 已离开 pending → decided;解析不可用 → unknown(中性,不阻塞);
// 可解析且零签署 → none;否则 partial。
export function dualSignState(a: Pick<Approval, 'status' | 'signers'>): DualSignState {
  if (a.status !== 'pending') return 'decided';
  const { signers, known } = parseSigners(a.signers);
  if (!known) return 'unknown';
  if (signers.length === 0) return 'none';
  return 'partial';
}

export interface SignerWording {
  zh: string;
  en: string;
}

// 文案表:zh/en 对集中于此,两处界面共用,由调用方经 useI18n().tr(w.zh, w.en) 渲染。
export function signerWording(state: DualSignState, count: number): SignerWording {
  switch (state) {
    case 'none':
      return {
        zh: '尚未签署 · 危险操作需 2 位批准人',
        en: 'Not yet signed · dangerous action needs 2 approvers',
      };
    case 'partial':
      return {
        zh: `${count} 人已签 / 需 ${DUAL_SIGN_REQUIRED} 人`,
        en: `${count} signed / ${DUAL_SIGN_REQUIRED} required`,
      };
    case 'unknown':
      return { zh: '签署状态未知', en: 'Signature status unknown' };
    default:
      return { zh: '', en: '' };
  }
}
