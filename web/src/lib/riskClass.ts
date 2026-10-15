// Risk-class labels for approvals.
//
// The value is the producer's declaration parsed by the manager into
// domain.ToolClass — `read` / `write` / `destructive`, or empty when the
// producer declared nothing (which the gate treats as destructive, see
// core/manager/biz/approval/usecase.go). Until now three surfaces printed
// the raw token next to a Chinese 「风险等级」 label, so the column read
// "风险等级: destructive".
import type { ChipTone } from '@/components/ui/Chip';

type Label = { zh: string; en: string };

const RISK_LABELS: Record<string, Label> = {
  read: { zh: '只读', en: 'read-only' },
  write: { zh: '会改状态', en: 'mutating' },
  destructive: { zh: '破坏性', en: 'destructive' },
};

const UNDECLARED: Label = { zh: '未声明', en: 'undeclared' };

export function riskClassLabel(
  value: string | undefined,
  tr: (zh: string, en: string) => string,
): string {
  if (!value) return tr(UNDECLARED.zh, UNDECLARED.en);
  const l = RISK_LABELS[value];
  return l ? tr(l.zh, l.en) : value;
}

/** Chip tone for the risk class — destructive is the only red one. */
export function riskClassTone(value: string | undefined): ChipTone {
  if (value === 'destructive') return 'danger';
  if (value === 'write') return 'warning';
  return 'default';
}

/** Blast-radius tokens the gate emits (cluster > host > single target). */
const BLAST_LABELS: Record<string, Label> = {
  cluster: { zh: '全集群', en: 'cluster-wide' },
  cluster_wide: { zh: '全集群', en: 'cluster-wide' },
  host: { zh: '单机', en: 'one host' },
  node: { zh: '单机', en: 'one host' },
  service: { zh: '单服务', en: 'one service' },
  process: { zh: '单进程', en: 'one process' },
  file: { zh: '单文件', en: 'one file' },
};

export function blastRadiusLabel(
  value: string | undefined,
  tr: (zh: string, en: string) => string,
): string {
  if (!value) return tr('未声明', 'undeclared');
  const l = BLAST_LABELS[value];
  return l ? tr(l.zh, l.en) : value;
}
