import { Section } from '@/components/section';

export const metadata = {
  title: '更新日志',
  description: 'OpsKeeper 的发布、特性、破坏性变更。',
};

type Entry = {
  date: string;
  version: string;
  title: string;
  highlights: string[];
  tag: 'release' | 'hotfix' | 'rc';
};

const entries: Entry[] = [
  {
    date: '2026-09-03',
    version: 'v2026.09.03',
    title: 'Release · opskeeper-v2026.09.03',
    tag: 'release',
    highlights: [
      'Manager 风格派发 + 显式 L0–L3 安全级别',
      'append-only loop_event_log（DB 强制）+ SHA256 链式提案审计',
      '插件 stdio MCP server，W3C traceparent 直通',
      '四个可复现的 PostgreSQL 事件场景',
      'OpsKeeper TeamHarness v0：17 个 MCP 工具，Bearer + HMAC 鉴权',
    ],
  },
  {
    date: '2026-08-21',
    version: 'v2026.08.21',
    title: 'Release · opskeeper-v2026.08.21',
    tag: 'release',
    highlights: [
      '技能注册中心热加载（Nacos Config，30 秒轮询）',
      '离线安装场景下 skill_meta.yaml 的本地降级',
      'Web Console：events / workflows / approvals / audit 页面',
      'Repairer 窄域执行器，带 payload 哈希匹配',
    ],
  },
  {
    date: '2026-08-04',
    version: 'hotfix-2026.08.04',
    title: 'Hotfix · hotfix-2026.08.04',
    tag: 'hotfix',
    highlights: [
      '修复：指标 label 轮转时 recovery.verify 白名单出现竞争',
      '修复：critic 在部分证据链下的 needs_correction 误报',
    ],
  },
  {
    date: '2026-07-18',
    version: 'v2026.07.18',
    title: 'Release · opskeeper-v2026.07.18',
    tag: 'release',
    highlights: [
      'agentteams-plugin-installer v1 提供 5 个扩展点',
      'Qdrant 支持候选决策证据保留',
      '复盘 Reporter 基于预计算的 ReportFacts 撰写',
      '为 CI 提供合成事件 fixture（deploy/incident-events）',
    ],
  },
  {
    date: '2026-07-02',
    version: '2026.07.02-rc.1',
    title: 'Release candidate · 2026.07.02-rc.1',
    tag: 'rc',
    highlights: [
      'Manager 派发表中 L0–L3 安全级别首版',
      'Investigator max_turns 预算（默认 40）',
      '闭环编排器骨架（loopbiz 包）',
    ],
  },
];

function Tag({ tag }: { tag: Entry['tag'] }) {
  const styles =
    tag === 'release'
      ? 'border-accent-500/30 bg-accent-500/10 text-accent-300'
      : tag === 'hotfix'
      ? 'border-rose-500/30 bg-rose-500/10 text-rose-500'
      : 'border-white/15 bg-white/5 text-ink-200';
  const label = tag === 'release' ? 'Release' : tag === 'hotfix' ? 'Hotfix' : 'RC';
  return (
    <span className={`rounded-md border px-2 py-0.5 font-mono text-[11px] uppercase tracking-wider ${styles}`}>
      {label}
    </span>
  );
}

export default function ChangelogZhPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            更新日志
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            什么在什么时候变了，为什么变。
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            每条发布记录都和仓库里的 <code className="text-accent-300">CHANGELOG.md</code> /{' '}
            <code className="text-accent-300">RELEASE_VERSION.json</code> 一一对应。要看完整 commit 历史，去 GitHub Releases。
          </p>
        </div>
      </Section>

      <Section className="pb-24">
        <div className="overflow-hidden rounded-2xl border border-white/10">
          <table className="w-full text-left">
            <thead className="bg-white/[0.03] text-ink-200">
              <tr>
                <th className="w-32 px-4 py-3 text-sm font-medium">日期</th>
                <th className="w-44 px-4 py-3 text-sm font-medium">版本</th>
                <th className="px-4 py-3 text-sm font-medium">变更要点</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-white/5">
              {entries.map((e) => (
                <tr key={e.version} className="align-top bg-white/[0.01] hover:bg-white/[0.04]">
                  <td className="px-4 py-4 font-mono text-xs text-ink-300">{e.date}</td>
                  <td className="px-4 py-4">
                    <div className="flex items-center gap-2">
                      <Tag tag={e.tag} />
                      <span className="font-mono text-xs text-white">{e.version}</span>
                    </div>
                    <div className="mt-2 text-xs text-ink-300">{e.title}</div>
                  </td>
                  <td className="px-4 py-4 text-sm text-ink-200">
                    <ul className="space-y-1.5">
                      {e.highlights.map((h) => (
                        <li key={h} className="flex items-start gap-2">
                          <span className="mt-1.5 h-1.5 w-1.5 flex-none rounded-full bg-accent-400" />
                          <span>{h}</span>
                        </li>
                      ))}
                    </ul>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Section>
    </>
  );
}
