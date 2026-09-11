import Link from 'next/link';
import { Section, SectionHeader } from '@/components/section';
import { Button } from '@/components/button';
import {
  Database,
  Server,
  ShoppingCart,
  Factory,
  Smartphone,
  ArrowRight,
  CheckCircle2,
} from 'lucide-react';

export const metadata = {
  title: '应用场景',
  description:
    'OpsKeeper 最能体现价值的场景：金融核心交易 / SaaS 多租户 / 零售 POS / 工厂 OT·IT / 手游后端。',
};

const cases = [
  {
    icon: Database,
    industry: '金融核心交易',
    family: 'PostgreSQL',
    headline: '凌晨三点的连接池风暴，MTTR 从 31 分钟降到 4 分钟。',
    problem:
      '交易数据库出现连接池耗尽，级联触发订单拒绝告警和交易所侧超时。On-call 必须在压力下关联 Loki 日志、Prometheus 曲线和一次近期上线的迁移。',
    howOpsKeeperHelps:
      'OpsKeeper 把 47 条原始告警归并成一个事件，定位到新副本上连接池 floor 配置不匹配的问题，并提交一份带明确爆炸半径的修复提案。审批人 90 秒内签字。验证器按四项指标白名单确认恢复。',
    outcome: 'MTTR：31 分钟 → 4 分钟。无需回滚交易所侧。',
    workflow: 'pg-connection-pool-exhaustion',
    severity: 'critical',
  },
  {
    icon: Server,
    industry: 'SaaS 多租户',
    family: 'PostgreSQL · Kubernetes',
    headline: '在客户发微博之前，把"吵闹邻居"拦住。',
    problem:
      '共享 Postgres 集群上一个租户拖累了副本延迟，所有租户的副本 replay lag 超过 60 秒。平台团队需要定位租户并把它的读流量切走，同时不影响其他 1200 个租户。',
    howOpsKeeperHelps:
      'OpsKeeper 通过 Qdrant 在历史模式上做向量召回，定位到唯一一个造成延迟飙升的租户，生成一份"只对该租户开启只读路由"的提案，并按 replay-lag 白名单验证恢复。整条闭环在到达审批关卡前都没有打扰人工。',
    outcome: '一个租户受影响 4 分钟，其余 1200 个租户 0 影响。',
    workflow: 'pg-replica-replay-lag',
    severity: 'warn',
  },
  {
    icon: ShoppingCart,
    industry: '零售 POS',
    family: '边缘部署',
    headline: '区域 POP 掉线，门店照常营业。',
    problem:
      '一个区域 POP 失去上行链路。本地 POS 终端靠边缘缓存继续工作，但中心端 OpsKeeper 控制平面失联。团队需要一个既能自治、又能产出干净审计轨迹的边缘。',
    howOpsKeeperHelps:
      'opskeeper-edge 守护进程在本地缓存证据，POP 恢复后自动与控制平面重连。HMAC 链式账本在中心端完整重放，审计零缺口、零人工对账。',
    outcome: '零数据丢失，零人工对账，19 家门店全程营业。',
    workflow: 'edge-resilience',
    severity: 'error',
  },
  {
    icon: Factory,
    industry: '制造 OT·IT',
    family: '磁盘 · 计算',
    headline: '坏 checkpoint 击穿车间 historian。',
    problem:
      'historian 数据库每晚的 checkpoint 与换班高峰的 IOPS 撞在一起，磁盘子系统饱和。产线遥测开始丢点。产线不能停，不允许人工介入。',
    howOpsKeeperHelps:
      'OpsKeeper 检测到磁盘饱和模式，定位到 checkpoint 时间窗口，并提议把 checkpoint 分散到两个副本。repairer 分阶段执行变更，verifier 按四项指标确认 IOPS 恢复到告警阈值以下，第二天早上复盘报告自动生成。',
    outcome: '产线在线率 100%，部署后再无磁盘告警。',
    workflow: 'pg-disk-io-saturation',
    severity: 'critical',
  },
  {
    icon: Smartphone,
    industry: '手游后端',
    family: '锁等待',
    headline: '一个长事务卡住了全球活动开服。',
    problem:
      '上次发布遗留的索引缺失导致开服期间锁等待超过 30 秒。玩家侧查询被卡，On-call 盯着一长串被阻塞的事务。',
    howOpsKeeperHelps:
      'investigator 把 blocker 追溯到某个批处理任务，reviewer 在明确爆炸半径的前提下签字取消，verifier 确认锁等待回到基线。复盘报告在恢复后 60 秒内推到团队 Slack。',
    outcome: '开服保住，复盘报告在恢复 60 秒内进 Slack。',
    workflow: 'pg-lock-wait-long-transaction',
    severity: 'error',
  },
];

export default function UseCasesZhPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            应用场景
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            闭环最能体现价值的地方。
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            五个来自 OpsKeeper 工作流目录的真实模式。每一个都作为可复现场景合入仓库，你可以端到端跑一遍。
          </p>
          <div className="mt-6 flex flex-wrap gap-3">
            <Button href="/zh/docs/getting-started">跑一遍演示</Button>
            <Button href="/zh/docs/workflow-catalog" variant="secondary">
              完整目录
            </Button>
          </div>
        </div>
      </Section>

      <Section className="py-10">
        <div className="grid gap-6">
          {cases.map((c) => (
            <article
              key={c.headline}
              className="rounded-2xl border border-white/10 bg-white/[0.02] p-6 md:p-8"
            >
              <div className="grid gap-8 md:grid-cols-12">
                <div className="md:col-span-1">
                  <span className="inline-flex h-10 w-10 items-center justify-center rounded-md border border-white/10 bg-white/5 text-accent-300">
                    <c.icon className="h-5 w-5" />
                  </span>
                </div>
                <div className="md:col-span-8">
                  <div className="flex flex-wrap items-center gap-2 text-xs">
                    <span className="rounded-md border border-white/10 bg-white/5 px-2 py-0.5 font-mono uppercase tracking-wider text-ink-200">
                      {c.industry}
                    </span>
                    <span className="text-ink-400">·</span>
                    <span className="font-mono text-ink-300">{c.family}</span>
                  </div>
                  <h2 className="mt-3 text-2xl font-semibold tracking-tight text-white">
                    {c.headline}
                  </h2>
                  <div className="mt-4 grid gap-4 md:grid-cols-2">
                    <div>
                      <div className="text-xs font-medium uppercase tracking-wider text-ink-400">
                        问题
                      </div>
                      <p className="mt-2 text-sm text-ink-300">{c.problem}</p>
                    </div>
                    <div>
                      <div className="text-xs font-medium uppercase tracking-wider text-ink-400">
                        OpsKeeper 如何解决
                      </div>
                      <p className="mt-2 text-sm text-ink-300">{c.howOpsKeeperHelps}</p>
                    </div>
                  </div>
                  <div className="mt-5 flex items-center gap-2 rounded-lg border border-accent-500/20 bg-accent-500/5 p-3 text-sm">
                    <CheckCircle2 className="h-4 w-4 flex-none text-accent-400" />
                    <span className="text-ink-100">{c.outcome}</span>
                  </div>
                </div>
                <div className="md:col-span-3">
                  <div className="rounded-xl border border-white/10 bg-white/[0.02] p-4">
                    <div className="text-xs font-medium uppercase tracking-wider text-ink-400">
                      工作流
                    </div>
                    <div className="mt-1 font-mono text-sm text-white">{c.workflow}</div>
                    <div className="mt-3 text-xs font-medium uppercase tracking-wider text-ink-400">
                      严重度
                    </div>
                    <div
                      className={
                        c.severity === 'critical'
                          ? 'mt-1 text-sm text-rose-500'
                          : c.severity === 'error'
                          ? 'mt-1 text-sm text-amber-400'
                          : 'mt-1 text-sm text-ink-200'
                      }
                    >
                      {c.severity}
                    </div>
                  </div>
                </div>
              </div>
            </article>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <div className="rounded-3xl border border-white/10 bg-gradient-to-br from-accent-500/15 via-ink-900 to-ink-950 p-10 md:p-14">
          <h2 className="max-w-2xl text-balance text-3xl font-semibold tracking-tight text-white sm:text-4xl">
            还有不在列表里的场景？
          </h2>
          <p className="mt-3 max-w-xl text-ink-200">
            在 <code className="text-accent-300">workflows/</code> 下加一个新工作流，再在{' '}
            <code className="text-accent-300">deploy/incident-events/</code> 配对应场景就能提 PR。
            目录里的四个 PG 场景都是这样贡献出来的。
          </p>
          <div className="mt-6 flex flex-wrap gap-3">
            <Button href="https://github.com/louloulin/opskeeper/blob/main/CONTRIBUTING.md" external>
              贡献指南
            </Button>
            <Link
              href="/zh/docs"
              className="inline-flex items-center gap-2 rounded-md border border-white/10 bg-white/5 px-4 py-2 text-sm font-medium text-ink-100 hover:bg-white/10"
            >
              阅读文档 <ArrowRight className="h-4 w-4" />
            </Link>
          </div>
        </div>
      </Section>
    </>
  );
}
