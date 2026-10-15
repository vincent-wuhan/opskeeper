// ReportHostedView — 报表只读渲染壳(共享,design §1.1/§2)。
// 包裹既有 ReportContentView:props 只收 ReportContentT,不触发取数。
// 卡内预览默认 max-h-[480px] 内部滚动(design §1.1、tasks 4.4);
// 独立报表页不限高:maxHeight 传 'none'。
import { ReportContentView } from './ReportContent';
import type { ReportContent as ReportContentT } from '@/api/reports';

const DEFAULT_MAX_HEIGHT = 480;

export function ReportHostedView({ content, maxHeight }: { content: ReportContentT; maxHeight?: number | 'none' }) {
  if (maxHeight === 'none') return <ReportContentView content={content} />;
  const px = maxHeight ?? DEFAULT_MAX_HEIGHT;
  return (
    <div className="overflow-y-auto pr-1" style={{ maxHeight: px }}>
      <ReportContentView content={content} />
    </div>
  );
}
