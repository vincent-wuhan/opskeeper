// 插件市场 — the 2.0 plugin surface: get a package in, and find out where
// it can run.
//
// It is two cards and they are not related, which is worth saying up front
// because a page called "marketplace" invites the assumption that it is one
// workflow. It is not:
//
//   1. 导入 converts a legacy container into a PiG package. The result sits
//      on the manager's disk. Nothing is installed, no node is touched.
//   2. 兼容矩阵 answers "which of my nodes could host this", before anyone
//      starts a release. It reads versions, it changes nothing.
//
// The release console that actually puts code on hosts lives at
// /admin/plugins and is not duplicated here. A second copy of the wave
// controls would be a second thing to keep correct while somebody is
// watching a canary, and this page has no business being where that
// decision is made.
//
// What this page refuses to do is the thing that would make it dangerous.
// A successful import is NOT rendered as a finished package. The converter
// refuses to guess a tool list, a safety level, scopes or a blast radius —
// a legacy container simply does not contain them — so an import that
// renders as "imported ✓" is telling an operator that a package is ready
// when the most important part of it is still blank. The decisions are
// therefore the largest thing on the card, and the counts are the smallest.
import { useCallback, useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { AlertTriangle, FileUp, Loader2, Package, Rocket, Upload } from 'lucide-react';

import { ApiError } from '@/api/client';
import { importPack, type ImportResponse } from '@/api/pluginImport';
import {
  getCompatibility,
  getNodeInstalled,
  type CompatibilityMatrix,
  type CompatibilityVerdict,
  type InstalledOnNode,
} from '@/api/pluginReleases';
import { listEdges, type Edge } from '@/api/edges';
import { Button, Card, Chip, PageHeader } from '@/components/ui';
import { cn } from '@/lib/cn';
import { useI18n } from '@/i18n/locale';
import { usePermissions } from '@/store/me';

export default function PluginMarketplacePage() {
  const { tr } = useI18n();
  const { isAdmin } = usePermissions();

  return (
    <main className="flex h-full flex-col">
      <PageHeader
        title={tr('插件市场', 'Plugin Marketplace')}
        subtitle={tr(
          '把存量插件容器转换成 PiG 包，并在发布前看清哪些节点带得动',
          'Convert legacy containers into PiG packages, and see which nodes can host them before a release'
        )}
        actions={
          <Link to="/admin/plugins">
            <Button variant="ghost">
              <Rocket className="h-3.5 w-3.5" />
              {tr('发布控制台', 'Release console')}
            </Button>
          </Link>
        }
      />
      <div className="grid flex-1 grid-cols-1 items-start gap-4 overflow-auto p-6 xl:grid-cols-2">
        <ImportCard isAdmin={isAdmin} />
        <CompatibilityCard isAdmin={isAdmin} />
        <NodeInventoryCard isAdmin={isAdmin} />
      </div>
    </main>
  );
}

// ---------------------------------------------------------------------------
// Import
// ---------------------------------------------------------------------------

function ImportCard({ isAdmin }: { isAdmin: boolean }) {
  const { tr } = useI18n();
  const [file, setFile] = useState<File | null>(null);
  const [busy, setBusy] = useState(false);
  const [report, setReport] = useState<ImportResponse | null>(null);
  const [error, setError] = useState<string | null>(null);

  const run = useCallback(async () => {
    if (!file) return;
    setBusy(true);
    setError(null);
    setReport(null);
    try {
      setReport(await importPack(file));
    } catch (err) {
      setError(describeImportFailure(err, tr));
    } finally {
      setBusy(false);
    }
  }, [file, tr]);

  return (
    <Card className="space-y-4">
      <div>
        <h2 className="text-sm font-semibold text-zinc-100">
          {tr('导入存量插件容器', 'Import a legacy container')}
        </h2>
        <p className="mt-1 text-xs text-zinc-500">
          {tr(
            '支持 .claude-plugin/plugin.json、openclaw.plugin.json 与裸 skills.sh 目录。转换结果写在管理面磁盘上，不会装到任何节点。',
            'Accepts .claude-plugin/plugin.json, openclaw.plugin.json and bare skills.sh drops. The result is written to the manager\'s disk and installed on no node.'
          )}
        </p>
      </div>

      <label className="flex cursor-pointer items-center gap-2 rounded-lg border border-dashed border-zinc-700 px-3 py-6 text-xs text-zinc-400 transition-colors hover:border-zinc-600">
        <FileUp className="h-4 w-4" />
        <span className="truncate">
          {file ? file.name : tr('选择 zip / tar.gz 归档…', 'Choose a zip / tar.gz archive…')}
        </span>
        <input
          type="file"
          className="hidden"
          accept=".zip,.tar,.gz,.tgz"
          disabled={!isAdmin}
          onChange={(e) => {
            setFile(e.target.files?.[0] ?? null);
            setReport(null);
            setError(null);
          }}
        />
      </label>

      <Button variant="primary" onClick={run} disabled={!file || busy || !isAdmin}>
        {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Upload className="h-3.5 w-3.5" />}
        {tr('转换', 'Convert')}
      </Button>
      {!isAdmin ? (
        <p className="text-[11px] text-zinc-500">
          {tr('只有管理员可以导入——导入会改变机队能运行什么。', 'Admin only — an import changes what the fleet can run.')}
        </p>
      ) : null}

      {error ? <ErrorNote text={error} /> : null}
      {report ? <ImportResult report={report} /> : null}
    </Card>
  );
}

function ImportResult({ report }: { report: ImportResponse }) {
  const { tr } = useI18n();
  return (
    <div className="space-y-3 border-t border-zinc-800/60 pt-3">
      <div className="flex flex-wrap items-center gap-1.5">
        <Chip tone="accent" dense>
          <Package className="mr-1 inline h-3 w-3" />
          {report.name}
        </Chip>
        {report.version ? <Chip dense>{report.version}</Chip> : null}
        <Chip dense>{tr('容器格式', 'container')}: {report.kind}</Chip>
      </div>

      {report.description ? (
        <p className="text-xs text-zinc-400">{report.description}</p>
      ) : null}

      {/* The counts are inventory, not review. They are one line and
          visually quiet on purpose: a report that led with "12 skills, 3
          extensions" would read as a finished package, and it is not one. */}
      <div className="flex flex-wrap gap-1.5 text-[11px]">
        <Chip dense>{tr(`${report.skills.length} 技能`, `${report.skills.length} skill(s)`)}</Chip>
        <Chip dense>{tr(`${report.agents.length} 助理`, `${report.agents.length} agent(s)`)}</Chip>
        <Chip dense>{tr(`${report.prompts} 提示词`, `${report.prompts} prompt(s)`)}</Chip>
        <Chip dense>{tr(`${report.mcp} MCP`, `${report.mcp} MCP`)}</Chip>
        <Chip dense>{tr(`${report.extensions} 扩展`, `${report.extensions} extension(s)`)}</Chip>
        {/* The last two classes are counted rather than omitted for the
            same reason the first five are: an uncounted class is a class
            nobody can tell is missing. Both were dropped by an earlier
            converter without an error, and the report read identically
            either way. */}
        <Chip dense>{tr(`${report.themes} 主题`, `${report.themes} theme(s)`)}</Chip>
        <Chip dense>
          {tr(
            `${report.agent_environments} 运行环境`,
            `${report.agent_environments} agent environment(s)`
          )}
        </Chip>
      </div>

      {/* A container that selected its resources by manifest is the one
          case where the converted package serves a different SET than the
          one that was uploaded, so it is called out here rather than left
          to be found in the decisions list further down. */}
      {report.source_manifest?.declares_resources ? (
        <p className="text-[11px] text-amber-600/90">
          {tr(
            `源容器的 package.json 用清单声明了 ${(
              report.source_manifest.classes ?? []
            ).join('、')}；转换后的包没有清单，改为按约定目录发现，因此会多出一些资源。`,
            `The source declared ${
              (report.source_manifest.classes ?? []).join(', ') ||
              'its resources'
            } by manifest. The converted package has no manifest and discovers by convention, so it will serve a superset.`
          )}
        </p>
      ) : null}

      {report.skills.length > 0 ? (
        <ul className="space-y-0.5 text-[11px] text-zinc-500">
          {report.skills.map((s) => (
            <li key={s} className="truncate font-mono">
              {s}
            </li>
          ))}
        </ul>
      ) : null}

      {/* The decisions are the product of this route. A legacy container
          carries no tool list, no safety level, no scopes and no blast
          radius, and the converter refuses to invent any of them — so this
          list is never empty for a real container, and an empty one would
          mean the container DID carry governance, which is worth noticing
          rather than celebrating. */}
      <div className="rounded-lg border border-amber-500/30 bg-amber-500/5 p-3">
        <div className="text-xs font-medium text-amber-200">
          {tr(
            `还需要人工决定 ${report.decisions.length} 项`,
            `${report.decisions.length} decision(s) still to make`
          )}
        </div>
        {report.decisions.length === 0 ? (
          <p className="mt-1 text-[11px] text-zinc-400">
            {tr(
              '这个容器自带了治理声明——值得确认一下它是不是真的。',
              'This container carried its own governance — worth confirming that is true.'
            )}
          </p>
        ) : (
          <ul className="mt-2 space-y-2">
            {report.decisions.map((d) => (
              <li key={d.field} className="text-[11px] leading-relaxed">
                <div className="font-mono text-amber-300">{d.field}</div>
                <div className="text-zinc-200">{d.question}</div>
                {/* `why` is the one that tells a reviewer whether the
                    converter or the vendor has to think, so it is rendered
                    rather than hidden behind a tooltip. */}
                <div className="text-zinc-500">{d.why}</div>
              </li>
            ))}
          </ul>
        )}
      </div>

      {report.warnings.length > 0 ? (
        <div className="rounded-lg border border-zinc-700 bg-zinc-950/40 p-2.5">
          <div className="text-[11px] font-medium text-zinc-300">
            {tr('加载器告警', 'Loader warnings')}
          </div>
          <ul className="mt-1 space-y-0.5 text-[11px] text-zinc-500">
            {report.warnings.map((w, i) => (
              <li key={`${w.path}-${i}`}>
                <span className="font-mono">{w.path || w.code}</span>: {w.reason}
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      <p className="text-[11px] text-zinc-500">
        {tr('已写入：', 'Written to: ')}
        <span className="font-mono text-zinc-400">{report.dest}</span>
        <br />
        {tr(
          '回答上面这些问题并复核之后，才能通过发布通道下发；导入本身不会安装任何东西。',
          'Answer the questions above and review it, then publish through the release routes. An import installs nothing.'
        )}
      </p>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Compatibility matrix
// ---------------------------------------------------------------------------

function CompatibilityCard({ isAdmin }: { isAdmin: boolean }) {
  const { tr } = useI18n();
  const [name, setName] = useState('');
  const [version, setVersion] = useState('');
  const [minEdge, setMinEdge] = useState('');
  const [minPig, setMinPig] = useState('');
  const [busy, setBusy] = useState(false);
  const [matrix, setMatrix] = useState<CompatibilityMatrix | null>(null);
  const [error, setError] = useState<string | null>(null);

  const ask = useCallback(async () => {
    if (!name.trim()) return;
    setBusy(true);
    setError(null);
    setMatrix(null);
    try {
      setMatrix(
        await getCompatibility(name.trim(), {
          version: version.trim() || undefined,
          min_edge_version: minEdge.trim() || undefined,
          min_pig_version: minPig.trim() || undefined,
        })
      );
    } catch (err) {
      setError(describeCompatFailure(err, tr));
    } finally {
      setBusy(false);
    }
  }, [minEdge, minPig, name, tr, version]);

  return (
    <Card className="space-y-4">
      <div>
        <h2 className="text-sm font-semibold text-zinc-100">
          {tr('兼容矩阵', 'Compatibility matrix')}
        </h2>
        <p className="mt-1 text-xs text-zinc-500">
          {tr(
            '发布前先看哪些节点带得动：两条轴分别是节点 edge 版本与它跑的 PiG 版本。',
            'Before a release, see which nodes can carry it: two axes, the node\'s edge build and the PiG it runs.'
          )}
        </p>
      </div>

      <div className="grid grid-cols-2 gap-2">
        <Field label={tr('包名', 'Package')} value={name} onChange={setName} placeholder="opskeeper-sre-repair" />
        <Field label={tr('版本', 'Version')} value={version} onChange={setVersion} placeholder="1.4.0" />
        <Field label="min_edge_version" value={minEdge} onChange={setMinEdge} placeholder="0.7.0" mono />
        <Field label="min_pig_version" value={minPig} onChange={setMinPig} placeholder="0.3.0" mono />
      </div>

      <Button variant="primary" onClick={ask} disabled={!name.trim() || busy || !isAdmin}>
        {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : null}
        {/* Named for what it checks rather than for the verb. Two cards on
            one page both ending in a bare "Check" leaves an operator
            guessing which one they just pressed, and a test has to reach
            for an index to disambiguate — both are the same defect seen
            from two sides. */}
        {tr('检查兼容性', 'Check compatibility')}
      </Button>

      {error ? <ErrorNote text={error} /> : null}
      {matrix ? <MatrixView matrix={matrix} /> : null}
    </Card>
  );
}

function Field({
  label,
  value,
  onChange,
  placeholder,
  mono,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  mono?: boolean;
}) {
  return (
    <label className="block">
      <span className="mb-1 block text-[11px] text-zinc-500">{label}</span>
      <input
        className={cn(
          'w-full rounded-md border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-xs text-zinc-100 placeholder:text-zinc-600',
          mono && 'font-mono'
        )}
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
      />
    </label>
  );
}

function MatrixView({ matrix }: { matrix: CompatibilityMatrix }) {
  const { tr } = useI18n();
  return (
    <div className="space-y-3 border-t border-zinc-800/60 pt-3">
      <div className="flex flex-wrap items-center gap-1.5 text-[11px]">
        <span className="text-zinc-400">{matrix.plugin}</span>
        {matrix.version ? <Chip dense>{matrix.version}</Chip> : null}
        {matrix.min_edge_version ? (
          <Chip dense>min_edge {matrix.min_edge_version}</Chip>
        ) : null}
        {matrix.min_pig_version ? (
          <Chip dense>min_pig {matrix.min_pig_version}</Chip>
        ) : null}
      </div>

      <VerdictTable
        title={tr(`可承载 (${matrix.hostable.length})`, `Hostable (${matrix.hostable.length})`)}
        rows={matrix.hostable}
        emptyText={tr('没有任何节点满足这个要求。', 'No node satisfies this requirement.')}
      />
      <VerdictTable
        title={tr(`被拒 (${matrix.refused.length})`, `Refused (${matrix.refused.length})`)}
        rows={matrix.refused}
        emptyText={tr('没有被拒的节点。', 'Nothing was refused.')}
        tone="danger"
      />
    </div>
  );
}

function VerdictTable({
  title,
  rows,
  emptyText,
  tone,
}: {
  title: string;
  rows: CompatibilityVerdict[];
  emptyText: string;
  tone?: 'danger';
}) {
  const { tr } = useI18n();
  return (
    <div>
      <div className="mb-1.5 text-xs font-medium text-zinc-300">{title}</div>
      {rows.length === 0 ? (
        <p className="text-[11px] text-zinc-500">{emptyText}</p>
      ) : (
        <ul className="space-y-1">
          {rows.map((v) => (
            <li
              key={v.node_id}
              className="rounded-md border border-zinc-800/60 bg-zinc-950/40 px-2.5 py-1.5"
            >
              <div className="flex items-center justify-between gap-2">
                <span className="truncate text-xs text-zinc-200">
                  {v.name || `#${v.node_id}`}
                </span>
                <Chip tone={tone ?? 'success'} dense>
                  {v.hostable
                    ? tr('可承载', 'hostable')
                    : stepLabel(v.step, tr)}
                </Chip>
              </div>
              <div className="mt-0.5 flex flex-wrap gap-2 text-[10px] text-zinc-500">
                <span>edge {v.edge_version || '—'}</span>
                <span>pig {v.pig_version || '—'}</span>
              </div>
              {/* The refusal sentence is shown exactly as the node would
                  refuse with. Rewriting it here would create a second
                  wording to keep in step with the node's, and the two
                  would drift the first time either was edited. */}
              {v.reason ? <div className="mt-1 text-[11px] text-zinc-400">{v.reason}</div> : null}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function stepLabel(step: CompatibilityVerdict['step'], tr: (zh: string, en: string) => string): string {
  if (step === 'version') return tr('edge 版本过低', 'edge too old');
  if (step === 'agent_version') return tr('PiG 版本过低', 'PiG too old');
  return tr('被拒', 'refused');
}

// ---------------------------------------------------------------------------
// Shared bits
// ---------------------------------------------------------------------------

function ErrorNote({ text }: { text: string }) {
  return (
    <div className="flex items-start gap-2 rounded-md border border-red-500/40 bg-red-500/10 px-3 py-2 text-xs text-red-200">
      <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
      <span>{text}</span>
    </div>
  );
}

/** describeImportFailure separates "this deployment cannot import" from
 *  "this archive is not a container".
 *
 *  The 503 is the one an operator can fix — the manager was never given an
 *  import root — and rendering it as a bad upload would send them to
 *  re-zip a file that was fine. The 409 is equally specific and equally
 *  mis-actionable if flattened: the route refuses to overwrite an existing
 *  package precisely so a second import cannot destroy review work, so the
 *  operator's next move is to look at what is already there, not to retry. */
function describeImportFailure(err: unknown, tr: (zh: string, en: string) => string): string {
  if (err instanceof ApiError && err.code === 'not-wired') {
    return tr(
      '这个管理面没有配置导入目录（OPSKEEPER_PLUGIN_IMPORT_DIR），与上传的文件无关。',
      'This manager has no import directory configured (OPSKEEPER_PLUGIN_IMPORT_DIR). The file is not the problem.'
    );
  }
  if (err instanceof ApiError && err.status === 409) {
    return tr(
      '同名包已经存在。导入不会覆盖任何东西——先去看看已有的那个。',
      'A package of that name already exists. An import replaces nothing — go and look at the one that is there.'
    );
  }
  if (err instanceof ApiError && err.status === 400) {
    return tr('这个归档里没有可识别的插件容器。', 'That archive contains no recognisable plugin container.');
  }
  return (err as Error).message || tr('导入失败', 'import failed');
}

/** describeCompatFailure keeps "the control plane cannot tell" distinct
 *  from "nothing is compatible".
 *
 *  A manager with no version snapshot has no idea which nodes can host
 *  what. That has to stay an error on screen: an empty matrix would read
 *  as a fleet that is uniformly too old, and the operator's response —
 *  upgrade everything — would be a fleet-wide change made on a sentence
 *  the control plane never actually said. */
function describeCompatFailure(err: unknown, tr: (zh: string, en: string) => string): string {
  if (err instanceof ApiError && err.code === 'not-wired') {
    return tr(
      '管理面还没有拿到节点版本快照，所以无法判断兼容性——这不等于没有节点能装。',
      'The manager has no node version snapshot, so it cannot answer. That is not the same as no node being able to host it.'
    );
  }
  return (err as Error).message || tr('查询失败', 'compatibility check failed');
}

// ---------------------------------------------------------------------------
// Node inventory
// ---------------------------------------------------------------------------

/** NodeInventoryCard answers "what is this host actually running".
 *
 *  Until this existed, the only window onto a node's package set was a
 *  release in flight — which is exactly when nobody is asking. An operator
 *  who wanted to know whether a host had picked up last week's package had
 *  no way to find out short of starting a release, which is a terrible way
 *  to ask a question.
 *
 *  What this card is careful about is the difference between the three
 *  answers the endpoint can give. They are not variations on "no
 *  packages": a node that runs nothing is a fact about the node, a node
 *  the control plane cannot reach is a fact about the control plane, and a
 *  node that did not answer is a fact about that host. Collapsing them into
 *  an empty list would tell an operator that a host is clean when the truth
 *  is that nobody asked it.
 */
function NodeInventoryCard({ isAdmin }: { isAdmin: boolean }) {
  const { tr } = useI18n();
  const [edges, setEdges] = useState<Edge[]>([]);
  const [edgeId, setEdgeId] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<InstalledOnNode | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    listEdges()
      .then((r) => {
        if (!alive) return;
        setEdges(r.items ?? []);
        setEdgeId((cur) => cur ?? r.items?.[0]?.id ?? null);
      })
      .catch(() => undefined);
    return () => {
      alive = false;
    };
  }, []);

  const ask = useCallback(async () => {
    if (edgeId == null) return;
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      setResult(await getNodeInstalled(edgeId));
    } catch (err) {
      setError(describeInventoryFailure(err, tr));
    } finally {
      setBusy(false);
    }
  }, [edgeId, tr]);

  return (
    <Card className="space-y-4 xl:col-span-2">
      <div>
        <h2 className="text-sm font-semibold text-zinc-100">
          {tr('节点上装了什么', 'What a node is running')}
        </h2>
        <p className="mt-1 text-xs text-zinc-500">
          {tr(
            '直接向节点查询它当前激活的包。节点不回答与管理面够不着是两回事，页面会分开说。',
            'Ask the node directly what it has active. "The node did not answer" and "the manager cannot reach it" are different facts, and this page says which one you have.'
          )}
        </p>
      </div>

      <div className="flex flex-wrap items-end gap-2">
        <label className="block">
          <span className="mb-1 block text-[11px] text-zinc-500">{tr('节点', 'Node')}</span>
          <select
            className="rounded-md border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-xs text-zinc-300"
            value={edgeId ?? ''}
            onChange={(e) => {
              const v = e.target.value ? Number(e.target.value) : null;
              setEdgeId(v);
              setResult(null);
              setError(null);
            }}
          >
            <option value="">{tr('选择节点…', 'Select a node…')}</option>
            {edges.map((e) => (
              <option key={e.id} value={e.id}>
                {e.name}
              </option>
            ))}
          </select>
        </label>
        <Button variant="primary" onClick={ask} disabled={edgeId == null || busy || !isAdmin}>
          {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : null}
          {tr('查看节点已装', 'Check node')}
        </Button>
      </div>

      {error ? <ErrorNote text={error} /> : null}
      {result ? (
        result.packages.length === 0 ? (
          <p className="text-xs text-zinc-400">
            {tr(
              `节点 #${result.edge_id} 回答了：它没有装任何插件包。`,
              `Node #${result.edge_id} answered: it has no plugin packages installed.`
            )}
          </p>
        ) : (
          <ul className="space-y-1.5">
            {result.packages.map((p) => (
              <li
                key={`${p.name}@${p.version}`}
                className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-zinc-800/60 bg-zinc-950/40 px-2.5 py-1.5"
              >
                <span className="truncate text-xs text-zinc-200">{p.name}</span>
                <span className="flex items-center gap-1.5">
                  <Chip dense>{p.version || '—'}</Chip>
                  {/* The digest is the node's own tree digest, and its
                      whole purpose is to be compared against the one a
                      release meant to ship. Truncating it to fit the row
                      would defeat that: an operator could not tell a match
                      from a mismatch on the parts they can see. So it is
                      shown whole and allowed to wrap. */}
                  {p.digest ? (
                    <span className="max-w-[22rem] break-all font-mono text-[10px] text-zinc-500">
                      {p.digest}
                    </span>
                  ) : null}
                </span>
              </li>
            ))}
          </ul>
        )
      ) : null}
    </Card>
  );
}

/** describeInventoryFailure keeps the three answers apart.
 *
 *  503 means this manager was never given a tunnel, so it cannot tell you
 *  anything about any node — that is a configuration fact, and rendering it
 *  as "no packages" would be a lie about every host at once. 502 means this
 *  one node did not answer, which is a fact about that host. Neither of
 *  them is an empty list, and both are things an operator can act on in
 *  completely different places. */
function describeInventoryFailure(err: unknown, tr: (zh: string, en: string) => string): string {
  if (err instanceof ApiError && (err.status === 502 || err.code === 'node_unreachable')) {
    return tr(
      '这台节点没有回答。它可能正在重启、agent 版本过旧，或者隧道那一端断了——这不是「它没有装插件」。',
      'This node did not answer. It may be restarting, running an older agent, or the far end of the tunnel is down — which is not the same as it having nothing installed.'
    );
  }
  if (err instanceof ApiError && err.status === 503) {
    return tr(
      '这个管理面没有可用的隧道，因此无法询问任何节点——这不等于所有节点都是空的。',
      'This manager has no usable tunnel, so it cannot ask any node. That does not mean every node is empty.'
    );
  }
  if (err instanceof ApiError && err.status === 403) {
    return tr('节点上装了什么属于治理信息，只有管理员可以查看。', 'What a node runs is governance information; admin only.');
  }
  return (err as Error).message || tr('查询失败', 'inventory check failed');
}
