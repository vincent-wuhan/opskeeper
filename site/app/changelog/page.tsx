import { Section } from '@/components/section';

export const metadata = {
  title: 'Changelog',
  description: 'Releases, features, and breaking changes for OpsKeeper.',
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
      'Manager-style dispatch with explicit L0–L3 safety levels',
      'Append-only loop_event_log (DB-enforced) + SHA256-chained proposal audit',
      'Plugin stdio MCP server with W3C traceparent passthrough',
      'Four reproducible PostgreSQL incident scenarios',
      'OpsKeeper TeamHarness v0: 17 MCP tools, Bearer + HMAC auth',
    ],
  },
  {
    date: '2026-08-21',
    version: 'v2026.08.21',
    title: 'Release · opskeeper-v2026.08.21',
    tag: 'release',
    highlights: [
      'Skill registry hot reload (Nacos Config, 30s polling)',
      'Local fallback for skill_meta.yaml in air-gapped installs',
      'Web console: incidents, workflows, approvals, audit pages',
      'Repairer narrow-scope executor with hash-matched payloads',
    ],
  },
  {
    date: '2026-08-04',
    version: 'v2026.08.04',
    title: 'Hotfix · hotfix-2026.08.04',
    tag: 'hotfix',
    highlights: [
      'Fix: race in recovery.verify allowlist when metric labels rotate',
      'Fix: critic needed_correction false-positive on partial evidence chains',
    ],
  },
  {
    date: '2026-07-18',
    version: 'v2026.07.18',
    title: 'Release · opskeeper-v2026.07.18',
    tag: 'release',
    highlights: [
      'agentteams-plugin-installer v1 with five extension points',
      'Qdrant-backed candidate-decision evidence retention',
      'Postmortem reporter writes from pre-computed ReportFacts',
      'Synthetic incident fixtures for CI (deploy/incident-events)',
    ],
  },
  {
    date: '2026-07-02',
    version: 'v2026.07.02-rc.1',
    title: 'Release candidate · 2026.07.02-rc.1',
    tag: 'rc',
    highlights: [
      'First cut of the L0–L3 safety levels in the manager dispatch table',
      'Investigator max_turns budget (default 40)',
      'Closed-loop orchestrator skeleton (loopbiz package)',
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

export default function ChangelogPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            Changelog
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            What changed, when, and why.
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            Each release entry mirrors what landed in <code className="text-accent-300">CHANGELOG.md</code>{' '}
            and <code className="text-accent-300">RELEASE_VERSION.json</code> in the repo. For
            the full commit-by-commit history, see the GitHub releases page.
          </p>
        </div>
      </Section>

      <Section className="pb-24">
        <div className="overflow-hidden rounded-2xl border border-white/10">
          <table className="w-full text-left">
            <thead className="bg-white/[0.03] text-ink-200">
              <tr>
                <th className="w-32 px-4 py-3 text-sm font-medium">Date</th>
                <th className="w-44 px-4 py-3 text-sm font-medium">Version</th>
                <th className="px-4 py-3 text-sm font-medium">Highlights</th>
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
