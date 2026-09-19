'use client';

import { useCallback, useEffect, useMemo, useState } from 'react';
import { usePathname } from 'next/navigation';
import { BusinessCard } from '@/components/demo/business-card';
import { PreviewDecisionCard } from '@/components/demo/preview-decision-card';
import { StageRail } from '@/components/demo/stage-rail';
import { CodeBlock } from '@/components/code-block';
import { Section } from '@/components/section';
import {
  BUSINESS_SECTIONS,
  type BusinessSection,
  type BusinessSnapshot,
  type ScenarioStatus,
  type WorkflowStage,
} from '@/lib/demo-types';
import type { DemoLocale } from '@/lib/demo-locale';
import { DEMO_URLS } from '@/lib/demos';
import { cn } from '@/lib/utils';

type SnapshotState = {
  snapshot?: BusinessSnapshot;
  errorCode?: string;
  loading: boolean;
};

type DemoLink = {
  label: string;
  description: string;
  href: string;
  emphasis?: 'primary' | 'secondary';
};


const activeStages: WorkflowStage[] = [
  'starting',
  'awaiting_alert',
  'alert_correlated',
  'diagnosis_dispatched',
  'preview_ready',
  'awaiting_approval',
  'repair_dispatched',
  'verifying',
  'recovered',
];

const stageCopy: Record<DemoLocale, Partial<Record<WorkflowStage, string>>> = {
  zh: {
    starting: '场景启动中',
    awaiting_alert: '等待 Prometheus 告警',
    alert_correlated: '告警已关联',
    diagnosis_dispatched: '诊断已派发',
    preview_ready: '预演证据已就绪',
    awaiting_approval: '等待人工审批',
    repair_dispatched: '修复已受控派发',
    verifying: '独立验证中',
    recovered: '业务已恢复',
    closed: '事故档案已关闭',
    start_failed: '场景启动失败，可重试',
  },
  en: {
    starting: 'Scenario starting',
    awaiting_alert: 'Waiting for Prometheus alert',
    alert_correlated: 'Alert correlated',
    diagnosis_dispatched: 'Diagnosis dispatched',
    preview_ready: 'Preview evidence ready',
    awaiting_approval: 'Awaiting approval',
    repair_dispatched: 'Repair dispatched',
    verifying: 'Independent verification',
    recovered: 'Business recovered',
    closed: 'Incident archived',
    start_failed: 'Scenario failed; retry available',
  },
};

const demoLinks: Record<DemoLocale, DemoLink[]> = {
  zh: [
    {
      label: '监控看板',
      description: '主看板：确认连接池 4/4、等待队列与查询错误率',
      href: 'https://teams.yueming.xin/grafana/d/opskeeper-pgpool-live/?orgId=1&from=now-15m&to=now&refresh=5s',
      emphasis: 'primary',
    },
    {
      label: 'Manager 状态',
      description: '次看板：查看 Manager 健康、HTTP 延迟与内部资源',
      href: 'https://teams.yueming.xin/grafana/d/opskeeper-manager-internals/opskeeper-manager-internals?orgId=1&from=now-1h&to=now&refresh=30s',
      emphasis: 'secondary',
    },
    {
      label: 'Element 房间',
      description: '观察 Manager 主持诊断、预演与修复协同',
      href: 'https://rooms.yueming.xin/#/room/#benyue-lumos-ops:matrix-local.agentteams.io:18080',
    },
    {
      label: 'AgentTeams Dashboard',
      description: '查看任务看板与 OpsKeeper Runtime',
      href: 'https://teams.yueming.xin/#plugin-route:opskeeper-teamharness/home',
    },
    {
      label: 'OpsKeeper 后台',
      description: '切换为演示普通用户，查看事故、审批、审计与只读配置',
      href: DEMO_URLS.opskeeper,
    },
    {
      label: '事故档案',
      description: '关闭后回看完整证据链与 A/B 对比表',
      href: 'https://teams.yueming.xin/#plugin-route:opskeeper-teamharness/archive',
    },
    {
      label: '预演报告',
      description: '打开 preview-pg 深链核对完整指标',
      href: 'https://opskeeper.yueming.xin/preview/',
    },
  ],
  en: [
    {
      label: 'Monitoring',
      description: 'Primary board: verify pool 4/4, wait queue, and query errors',
      href: 'https://teams.yueming.xin/grafana/d/opskeeper-pgpool-live/?orgId=1&from=now-15m&to=now&refresh=5s',
      emphasis: 'primary',
    },
    {
      label: 'Manager status',
      description: 'Secondary board: Manager health, HTTP latency, and internals',
      href: 'https://teams.yueming.xin/grafana/d/opskeeper-manager-internals/opskeeper-manager-internals?orgId=1&from=now-1h&to=now&refresh=30s',
      emphasis: 'secondary',
    },
    {
      label: 'Element room',
      description: 'Watch Manager coordinate diagnosis, preview, and repair',
      href: 'https://rooms.yueming.xin/#/room/#benyue-lumos-ops:matrix-local.agentteams.io:18080',
    },
    {
      label: 'AgentTeams Dashboard',
      description: 'Inspect the task board and OpsKeeper runtime',
      href: 'https://teams.yueming.xin/#plugin-route:opskeeper-teamharness/home',
    },
    {
      label: 'OpsKeeper console',
      description: 'Switch to the standard demo user to review incidents, approvals, audits, and read-only settings',
      href: DEMO_URLS.opskeeper,
    },
    {
      label: 'Incident archive',
      description: 'Review the full evidence chain and A/B comparison after closure',
      href: 'https://teams.yueming.xin/#plugin-route:opskeeper-teamharness/archive',
    },
    {
      label: 'Preview report',
      description: 'Open the preview-pg deep link to verify all metrics',
      href: 'https://opskeeper.yueming.xin/preview/',
    },
  ],
};

async function fetchJson<T>(path: string, init?: RequestInit): Promise<T> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 2_000);
  try {
    const response = await fetch(path, {
      ...init,
      cache: 'no-store',
      signal: controller.signal,
    });
    const payload = await response.json().catch(() => null);
    if (!response.ok) {
      const code = payload && typeof payload === 'object' && 'error_code' in payload
        ? String(payload.error_code)
        : 'request_failed';
      const error = new Error(code) as Error & { code: string };
      error.code = code;
      throw error;
    }
    return payload as T;
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') {
      const error = new Error('query_timeout') as Error & { code: string };
      error.code = 'query_timeout';
      throw error;
    }
    throw error;
  } finally {
    clearTimeout(timeout);
  }
}

function useBusinessSnapshot(section: BusinessSection) {
  const [state, setState] = useState<SnapshotState>({ loading: true });

  const load = useCallback(async () => {
    setState((previous) => ({ ...previous, loading: true }));
    try {
      const snapshot = await fetchJson<BusinessSnapshot>(
        `/api/demo/business/${section}`,
      );
      setState({ snapshot, loading: false });
    } catch (error) {
      const code = error instanceof Error && 'code' in error
        ? String((error as Error & { code?: string }).code)
        : 'request_failed';
      setState((previous) => ({ ...previous, errorCode: code, loading: false }));
    }
  }, [section]);

  useEffect(() => {
    let active = true;
    void load().finally(() => {
      if (!active) setState((previous) => ({ ...previous, loading: false }));
    });
    const interval = setInterval(() => void load(), 3_000);
    return () => {
      active = false;
      clearInterval(interval);
    };
  }, [load]);

  return state;
}

export default function LiveIncidentPage() {
  const pathname = usePathname() || '/';
  const locale: DemoLocale = pathname === '/en/live-incident'
    ? 'en'
    : 'zh';
  const translate = useCallback((zh: string, en: string) => locale === 'en' ? en : zh, [locale]);
  const orders = useBusinessSnapshot('orders');
  const inventory = useBusinessSnapshot('inventory');
  const audit = useBusinessSnapshot('audit');
  const [scenario, setScenario] = useState<ScenarioStatus | null>(null);
  const [scenarioErrorCode, setScenarioErrorCode] = useState<string>();
  const [starting, setStarting] = useState(false);
  const [startErrorCode, setStartErrorCode] = useState<string>();

  const loadScenario = useCallback(async () => {
    try {
      const status = await fetchJson<ScenarioStatus>('/api/demo/scenario');
      setScenario(status);
      setScenarioErrorCode(undefined);
    } catch (error) {
      const code = error instanceof Error && 'code' in error
        ? String((error as Error & { code?: string }).code)
        : 'request_failed';
      if (code === 'demo_scenario_not_started') {
        setScenario(null);
        setScenarioErrorCode(undefined);
      } else {
        setScenarioErrorCode(code);
      }
    }
  }, []);

  useEffect(() => {
    void loadScenario();
    const interval = setInterval(() => void loadScenario(), 3_000);
    return () => clearInterval(interval);
  }, [loadScenario]);

  const startScenario = useCallback(async () => {
    setStarting(true);
    setStartErrorCode(undefined);
    try {
      const status = await fetchJson<ScenarioStatus>('/api/demo/scenario', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-Opskeeper-Demo-Action': 'start',
        },
        body: JSON.stringify({}),
      });
      setScenario(status);
      setScenarioErrorCode(undefined);
    } catch (error) {
      setStartErrorCode(error instanceof Error && 'code' in error
        ? String((error as Error & { code?: string }).code)
        : 'request_failed');
    } finally {
      setStarting(false);
    }
  }, []);

  const snapshotStates = useMemo(
    () => [orders, inventory, audit],
    [orders, inventory, audit],
  );
  const successfulSnapshots = snapshotStates
    .map((state) => state.snapshot)
    .filter(Boolean);
  const healthyCount = snapshotStates.filter((state) => state.snapshot && !state.errorCode).length;
  const degradedCount = BUSINESS_SECTIONS.length - healthyCount;
  const averageLatency = successfulSnapshots.length
    ? Math.round(
        successfulSnapshots.reduce((total, snapshot) => total + snapshot!.latency_ms, 0) /
          successfulSnapshots.length,
      )
    : undefined;
  const active = scenario ? activeStages.includes(scenario.status) : false;
  const serviceStatus = scenarioErrorCode
    ? translate('异常', 'Error')
    : scenario ? stageCopy[locale][scenario.status] ?? translate('运行中', 'Running') : translate('待初始化', 'Not initialized');

  return (
    <Section className="py-12 md:py-16">
      <header className="rounded-2xl border border-white/10 bg-gradient-to-b from-white/[0.06] to-transparent p-6 sm:p-8">
        <div className="flex flex-wrap items-start justify-between gap-6">
          <div className="max-w-2xl">
            <p className="font-mono text-xs uppercase tracking-wider text-accent-300">
              {translate('决赛演示 · PostgreSQL 连接池耗尽', 'Final Demo · PostgreSQL pool exhaustion')}
            </p>
            <h1 className="mt-3 text-balance text-3xl font-semibold tracking-tight text-white sm:text-4xl">
              {translate('业务体感与事故闭环控制台', 'Business Impact & Incident Console')}
            </h1>
            <p className="mt-3 text-pretty text-sm leading-relaxed text-ink-300 sm:text-base">
              {locale === 'zh'
                ? '一个 PostgreSQL 应用连接池耗尽案例，串联真实业务查询、监控告警、多智能体诊断、受控预演、人工审批、修复执行与独立验证。页面静态骨架始终可用，数据库卡片真实降级。'
                : 'A PostgreSQL application pool-exhaustion case connects real business queries, alerts, multi-agent diagnosis, controlled preview, human approval, repair, and independent verification. The static shell stays available while database cards degrade truthfully.'}
            </p>
          </div>
          <div className="rounded-xl border border-white/10 bg-white/[0.04] p-4 text-sm">
            <p className="text-xs uppercase tracking-wide text-ink-400">{translate('环境状态', 'Environment')}</p>
            <p className={cn(
              'mt-2 flex items-center gap-2 font-medium',
              scenarioErrorCode ? 'text-rose-200' : active ? 'text-amber-200' : 'text-accent-200',
            )}>
              <span className={cn(
                'h-2 w-2 rounded-full',
                scenarioErrorCode ? 'bg-rose-400' : active ? 'bg-amber-300' : 'bg-accent-400',
              )} />
              {serviceStatus}
            </p>
            <p className="mt-2 font-mono text-xs text-ink-400">
              {scenario ? `incident ${scenario.incident_id}` : translate('incident 待创建', 'incident pending')}
            </p>
          </div>
        </div>

        <div className="mt-8 grid gap-4 lg:grid-cols-[minmax(280px,360px)_1fr]">
          <div className="rounded-xl border border-white/10 bg-ink-900/60 p-5">
            <h2 className="text-lg font-semibold text-white">{translate('一键故障注入', 'One-click fault injection')}</h2>
            <p className="mt-2 text-sm leading-relaxed text-ink-300">
              {translate(
                '请求先进入 Manager 创建事故，再由 Manager 调用受控 fixture；页面不持有数据库或 fixture 凭据。',
                'The request first asks Manager to create an incident; Manager then invokes the controlled fixture. This page holds no database or fixture credentials.',
              )}
            </p>
            <button
              type="button"
              onClick={() => void startScenario()}
              disabled={starting || active}
              className="mt-5 inline-flex w-full items-center justify-center rounded-md bg-white px-4 py-2.5 text-sm font-semibold text-ink-950 transition-colors hover:bg-ink-100 disabled:cursor-not-allowed disabled:bg-white/20 disabled:text-ink-300"
              aria-describedby="injection-readback"
            >
              {starting ? translate('正在注入…', 'Injecting…') : active ? translate('场景运行中', 'Scenario running') : translate('注入连接池耗尽', 'Inject pool exhaustion')}
            </button>
            {startErrorCode && (
              <p role="alert" className="mt-3 text-xs text-rose-200">
                {translate('启动失败：', 'Start failed: ')}{startErrorCode}
              </p>
            )}
          </div>

          <dl id="injection-readback" className="grid gap-3 rounded-xl border border-white/10 bg-white/[0.03] p-5 sm:grid-cols-3">
            <div>
              <dt className="text-xs text-ink-400">{translate('持续时间', 'Duration')}</dt>
              <dd className="mt-1 font-mono text-sm text-white tabular-nums">{translate('90 秒（TTL 兜底）', '90 seconds (TTL safety net)')}</dd>
            </div>
            <div>
              <dt className="text-xs text-ink-400">{translate('允许目标', 'Allowed target')}</dt>
              <dd className="mt-1 font-mono text-sm text-white">pg:pool-fixture</dd>
            </div>
            <div>
              <dt className="text-xs text-ink-400">{translate('目标指纹', 'Target fingerprint')}</dt>
              <dd className="mt-1 break-all font-mono text-xs text-ink-200">0123456789abcdef0123456789abcdef</dd>
            </div>
          </dl>
        </div>
      </header>

      <section className="mt-8" aria-labelledby="business-title">
        <div className="flex flex-wrap items-end justify-between gap-3">
          <div>
            <h2 id="business-title" className="text-2xl font-semibold text-white">{translate('真实业务查询', 'Real business queries')}</h2>
            <p className="mt-2 text-sm text-ink-300">{translate('三张卡片独立轮询，共用同一个被施加压力的应用连接池。', 'Three cards poll independently through the same stressed application pool.')}</p>
          </div>
          <p className="text-xs text-ink-400">{translate('每 3 秒轮询 · 2 秒客户端超时', '3-second polling · 2-second client timeout')}</p>
        </div>
        <div className="mt-5 grid gap-4 md:grid-cols-3">
          <BusinessCard section="orders" locale={locale} {...orders} />
          <BusinessCard section="inventory" locale={locale} {...inventory} />
          <BusinessCard section="audit" locale={locale} {...audit} />
        </div>
      </section>

      <section className="mt-8" aria-labelledby="impact-title">
        <h2 id="impact-title" className="text-2xl font-semibold text-white">{translate('业务影响', 'Business impact')}</h2>
        <div className="mt-5 grid gap-4 sm:grid-cols-3">
          {[
            { label: translate('正常卡片', 'Healthy cards'), value: `${healthyCount}/3` },
            { label: translate('降级卡片', 'Degraded cards'), value: `${degradedCount}/3` },
            { label: translate('上次成功平均延迟', 'Avg last-success latency'), value: averageLatency === undefined ? translate('待测', 'Pending') : `${averageLatency} ms` },
          ].map((metric) => (
            <div key={metric.label} className="rounded-xl border border-white/10 bg-white/[0.03] p-5">
              <p className="text-sm text-ink-400">{metric.label}</p>
              <p className="mt-3 text-2xl font-semibold text-white tabular-nums">{metric.value}</p>
            </div>
          ))}
        </div>
      </section>

      <div className="mt-8 grid gap-6 xl:grid-cols-[1.15fr_1fr]">
        <StageRail status={scenario?.status} locale={locale} />
        <PreviewDecisionCard decision={scenario?.preview_decision ?? null} locale={locale} />
      </div>

      <section className="mt-8" aria-labelledby="links-title">
        <h2 id="links-title" className="text-2xl font-semibold text-white">{translate('演示切换入口', 'Demo switch entries')}</h2>
        <div className="mt-5 grid gap-3 md:grid-cols-2 xl:grid-cols-3">
          {demoLinks[locale].map((link) => (
            <a
              key={link.href}
              href={link.href}
              target="_blank"
              rel="noreferrer noopener"
              className={cn(
                'group rounded-xl border p-5 transition-colors',
                link.emphasis === 'primary'
                  ? 'border-accent-500/50 bg-accent-500/10 hover:border-accent-500/70 hover:bg-accent-500/15'
                  : link.emphasis === 'secondary'
                    ? 'border-white/20 bg-white/[0.05] hover:border-white/30 hover:bg-white/[0.07]'
                    : 'border-white/10 bg-white/[0.03] hover:border-accent-500/40 hover:bg-white/[0.05]',
              )}
            >
              <p className="text-base font-semibold text-white">{link.label}</p>
              <p className="mt-2 text-sm text-ink-300">{link.description}</p>
              <p className="mt-3 break-all font-mono text-xs text-ink-500">{link.href}</p>
            </a>
          ))}
        </div>
      </section>

      <section className="mt-8" aria-labelledby="manual-title">
        <h2 id="manual-title" className="text-2xl font-semibold text-white">{translate('人工动作提示词', 'Human action prompts')}</h2>
        <p className="mt-2 max-w-3xl text-sm text-ink-300">
          {translate(
            '以下仅保留真正需要人工介入的两个节点；故障注入、诊断、预演和验证由 Manager 与 Worker 协同推进。',
            'Only two nodes require human intervention. Injection, diagnosis, preview, and verification are coordinated by Manager and Workers.',
          )}
        </p>
        <div className="mt-5 grid gap-4 xl:grid-cols-2">
          <CodeBlock title={translate('仅在 awaiting_approval 使用', 'Use only at awaiting_approval')} language="text">
            {locale === 'zh'
              ? `请人工审批 incident ${scenario?.incident_id ?? '<incident_id>'} 的 Candidate A 修复方案。请逐项核对 candidate_id、execution_id、目标指纹、影响范围、参数与有效期；预演 PASS 只代表具备审批资格，不等于人工批准。`
              : `Approve Candidate A for incident ${scenario?.incident_id ?? '<incident_id>'}. Verify candidate_id, execution_id, target fingerprint, scope, parameters, and expiry. Preview PASS only grants approval eligibility; it is not human approval.`}
          </CodeBlock>
          <CodeBlock title={translate('仅在 recovered 后使用', 'Use only after recovered')} language="text">
            {locale === 'zh'
              ? `请对 incident ${scenario?.incident_id ?? '<incident_id>'} 执行档案关闭：确认业务查询、连接池指标和独立验证结果，然后归档完整证据链与 A/B 预演对比表。`
              : `Close incident ${scenario?.incident_id ?? '<incident_id>'}: confirm business queries, pool metrics, and independent verification, then archive the evidence chain and A/B preview comparison.`}
          </CodeBlock>
        </div>
      </section>
    </Section>
  );
}
