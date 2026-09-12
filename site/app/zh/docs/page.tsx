import Link from 'next/link';
import { CodeBlock } from '@/components/code-block';
import { ArrowRight } from 'lucide-react';

export const metadata = {
  title: '文档',
  description: 'OpsKeeper 简介：闭环、Worker 角色、安全边界。',
};

export default function DocsIntroZhPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          简介
        </div>
        <h1>欢迎使用 OpsKeeper</h1>
        <p>
          OpsKeeper 是授权可控、全程可审计的多智能体运维事件响应平台。本文档会带你了解如何运行它、如何扩展它，以及闭环是如何把变更动作关在提案、人工审批和审计留痕范围内的。
        </p>
        <div className="not-prose mt-6 flex flex-wrap gap-3">
          <Link
            href="/zh/docs/getting-started"
            className="inline-flex items-center gap-2 rounded-md bg-white px-3 py-1.5 text-sm font-medium text-ink-950 hover:bg-ink-100"
          >
            快速开始 <ArrowRight className="h-4 w-4" />
          </Link>
          <Link
            href="/zh/docs/architecture"
            className="inline-flex items-center gap-2 rounded-md border border-white/10 bg-white/5 px-3 py-1.5 text-sm text-ink-100 hover:bg-white/10"
          >
            阅读架构
          </Link>
        </div>
      </header>

      <h2 id="what-is-opskeeper">OpsKeeper 是什么</h2>
      <p>
        OpsKeeper 把<strong>告警接入</strong>、<strong>证据采集</strong>、
        <strong>根因分析</strong>、<strong>人工审批</strong>、<strong>窄域授权恢复</strong>、
        <strong>独立验证</strong>、<strong>事后复盘</strong>串成同一条闭环。
        每个阶段都是带护栏的显式状态跃迁，每次跃迁都是 append-only 账本上的一笔事件。
      </p>

      <h2 id="who-it-is-for">面向谁</h2>
      <ul>
        <li><strong>SRE / DevOps 团队</strong> —— 想要智能体驱动的、有可验证审计的事件响应。</li>
        <li><strong>平台团队</strong> —— 在开源内核之上搭建内部事件响应产品。</li>
        <li><strong>安全团队</strong> —— 要求可变更动作经过授权、限定范围、可重放。</li>
      </ul>

      <h2 id="how-to-read-these-docs">怎么读这份文档</h2>
      <p>
        如果你在评估 OpsKeeper，从 <Link href="/zh/docs/getting-started">快速开始</Link> 和{' '}
        <Link href="/zh/docs/architecture">架构</Link> 页开始。如果你在生产环境运行它，{' '}
        <Link href="/zh/docs/operations">运维手册</Link> 是 day-2 参考。如果你要扩展它，直接看{' '}
        <Link href="/zh/docs/plugins">插件</Link>。
      </p>

      <h2 id="the-closed-loop-in-one-snippet">闭环一段话讲清</h2>
      <p>每一个事件都流过相同的八个阶段，控制平面在护栏未满足前拒绝往下走：</p>
      <CodeBlock language="text" title="closed-loop phases">
        {`detected → correlated → investigated → critiqued
     → approved → recovered → verified → postmortem`}
      </CodeBlock>

      <h2 id="principles">原则</h2>
      <ol>
        <li><strong>诊断只读。</strong>只读工具始终可用。</li>
        <li><strong>恢复才写。</strong>可变更工具必须先有提案。</li>
        <li><strong>由人审批。</strong>审批与一份具体的资源、payload 哈希绑定。</li>
        <li><strong>独立 Worker 验证。</strong>&ldquo;行动者&rdquo;和&ldquo;裁判者&rdquo;不是同一个角色。</li>
        <li><strong>每次跃迁都可重放。</strong>账本是 HMAC 链式的。</li>
      </ol>

      <h2 id="next">下一步</h2>
      <p>
        继续看 <Link href="/zh/docs/getting-started">快速开始</Link> 在本地跑一遍闭环，或跳到{' '}
        <Link href="/zh/docs/architecture">架构</Link> 看数据平面。
      </p>
    </>
  );
}
