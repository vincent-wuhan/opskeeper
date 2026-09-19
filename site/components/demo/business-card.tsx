import type { BusinessSection, BusinessSnapshot } from '@/lib/demo-types';
import type { DemoLocale } from '@/lib/demo-locale';
import { formatBeijingClock } from '@/lib/time-format';
import { cn } from '@/lib/utils';

type BusinessCardProps = {
  section: BusinessSection;
  locale: DemoLocale;
  snapshot?: BusinessSnapshot;
  errorCode?: string;
  loading: boolean;
};

const sectionCopy: Record<DemoLocale, Record<BusinessSection, { title: string; label: string }>> = {
  zh: {
    orders: { title: '订单查询', label: '已支付订单' },
    inventory: { title: '库存查询', label: '最近仓库' },
    audit: { title: '审计查询', label: '审计事件' },
  },
  en: {
    orders: { title: 'Order query', label: 'Paid orders' },
    inventory: { title: 'Inventory query', label: 'Recent warehouse' },
    audit: { title: 'Audit query', label: 'Audit events' },
  },
};

function formatSnapshot(section: BusinessSection, snapshot: BusinessSnapshot, locale: DemoLocale) {
  if (section === 'orders') {
    return {
      value: locale === 'zh' ? `${snapshot.value} 笔` : `${snapshot.value} orders`,
      detail: locale === 'zh'
        ? `金额合计 ${(Number(snapshot.detail || '0') / 100).toFixed(2)} 元`
        : `Total amount ¥${(Number(snapshot.detail || '0') / 100).toFixed(2)}`,
    };
  }
  if (section === 'inventory') {
    return { value: snapshot.value, detail: snapshot.detail };
  }
  return {
    value: locale === 'zh' ? `${snapshot.value} 条` : `${snapshot.value} events`,
    detail: locale === 'zh'
      ? snapshot.detail ? `最新事件 ${snapshot.detail}` : '暂无最新事件'
      : snapshot.detail ? `Latest event ${snapshot.detail}` : 'No latest event',
  };
}

export function BusinessCard({
  section,
  locale,
  snapshot,
  errorCode,
  loading,
}: BusinessCardProps) {
  const degraded = Boolean(errorCode);
  const timedOut = errorCode === 'query_timeout' || errorCode === 'pool_exhausted';
  const translate = (zh: string, en: string) => locale === 'en' ? en : zh;
  const status = timedOut
    ? translate('查询超时', 'Query timeout')
    : degraded
      ? translate('降级', 'Degraded')
      : snapshot
        ? translate('正常', 'Healthy')
        : translate('查询中', 'Querying');
  const formatted = snapshot ? formatSnapshot(section, snapshot, locale) : undefined;

  return (
    <article className="flex h-full flex-col rounded-xl border border-white/10 bg-white/[0.03] p-5">
      <div className="flex items-start justify-between gap-3">
        <div>
          <p className="font-mono text-xs uppercase tracking-wider text-ink-400">
            {sectionCopy[locale][section].label}
          </p>
          <h3 className="mt-2 text-lg font-semibold text-white">
            {sectionCopy[locale][section].title}
          </h3>
        </div>
        <span
          className={cn(
            'inline-flex shrink-0 items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium',
            !degraded && snapshot && 'border-accent-500/30 bg-accent-500/10 text-accent-200',
            timedOut && 'border-amber-400/30 bg-amber-400/10 text-amber-200',
            degraded && !timedOut && 'border-rose-500/30 bg-rose-500/10 text-rose-200',
            !snapshot && !degraded && 'border-white/10 bg-white/5 text-ink-300',
          )}
          role="status"
          aria-live="polite"
        >
          <span
            className={cn(
              'h-1.5 w-1.5 rounded-full',
              !degraded && snapshot && 'bg-accent-400',
              timedOut && 'bg-amber-300',
              degraded && !timedOut && 'bg-rose-400',
              !snapshot && !degraded && 'bg-ink-400',
            )}
          />
          {status}
        </span>
      </div>

      <div className="mt-5 min-h-[74px] flex-1" aria-busy={loading}>
        {formatted ? (
          <>
            <div className="text-2xl font-semibold text-white">
              {formatted.value}
            </div>
            <p className="mt-2 text-sm text-ink-300">
              {degraded ? translate('上次成功：', 'Last success: ') : ''}
              {formatted.detail}
            </p>
          </>
        ) : (
          <p className="text-sm leading-relaxed text-ink-400">
            {errorCode === 'demo_scenario_not_started'
              ? translate('等待演示场景初始化后开始真实查询。', 'Real queries start after scenario initialization.')
              : translate('暂无成功查询结果；本页不展示模拟成功数据。', 'No successful query yet; this page never shows simulated success data.')}
          </p>
        )}
      </div>

      <footer className="mt-4 flex items-center justify-between gap-3 border-t border-white/5 pt-3 text-xs text-ink-400">
        <span className="tabular-nums">
          {snapshot ? translate(`延迟 ${snapshot.latency_ms} ms`, `Latency ${snapshot.latency_ms} ms`) : translate('延迟待测', 'Latency pending')}
        </span>
        <time className="tabular-nums" dateTime={snapshot?.generated_at}>
          {snapshot ? formatBeijingClock(snapshot.generated_at) : locale === 'zh' ? '--:--:-- 北京时间' : '--:--:-- Beijing time'}
        </time>
      </footer>
    </article>
  );
}
