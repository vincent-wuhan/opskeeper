import { CodeBlock } from '@/components/code-block';

export const metadata = { title: '迁移' };

export default function MigrationZhPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          团队协作
        </div>
        <h1>迁移</h1>
        <p>
          从更早的 OnGrid 系安装迁移到当前 OpsKeeper。权威指南是 <code>docs/migration/opskeeper-user-transition.md</code>。
        </p>
      </header>

      <h2 id="before-you-start">开始之前</h2>
      <ul>
        <li>阅读 <code>docs/PROVENANCE.md</code> 中的兼容性映射。</li>
        <li>阅读 <code>docs/BRAND_GOVERNANCE.md</code> 中的命名政策。</li>
        <li>运行任何迁移前先备份现有 PostgreSQL 数据库。</li>
      </ul>

      <h2 id="step-1">第 1 步 · 从源系统导出快照</h2>
      <p>
        自服务迁移工具是 <code>opskeeper-migrate</code>（<code>cmd/opskeeper-migrate</code>）。
        支持 9 类实体，写入幂等、可回滚、有限速、多租户隔离。
      </p>
      <CodeBlock language="bash" title="export">
        {`go build -o opskeeper-migrate ./cmd/opskeeper-migrate

opskeeper-migrate export \\
  --source "$OPSKEEPER_OLD_URL" \\
  --output ./snapshot.json`}
      </CodeBlock>

      <h2 id="step-2">第 2 步 · 导入 OpsKeeper</h2>
      <CodeBlock language="bash" title="import">
        {`opskeeper-migrate import \\
  --source ./snapshot.json \\
  --target "$OPSKEEPER_NEW_URL" \\
  --tenant-mapping "$TENANT_MAP"`}
      </CodeBlock>

      <h2 id="step-3">第 3 步 · 校验源与目标</h2>
      <p>
        <code>verify</code> 子命令按实体对比导出快照与目标系统并报告偏差（可输出 HTML 报告）。
        切流之前应零偏差。
      </p>
      <CodeBlock language="bash" title="verify">
        {`opskeeper-migrate verify \\
  --source ./snapshot.json \\
  --target "$OPSKEEPER_NEW_URL" \\
  --tenant-mapping "$TENANT_MAP" \\
  --report ./verify-report.html`}
      </CodeBlock>

      <h2 id="rollback">回滚</h2>
      <p>
        每次导入都会留档。用导入时生成的 rollback snapshot 撤销一次迁移：
      </p>
      <CodeBlock language="bash" title="rollback">
        {`opskeeper-migrate rollback \\
  --rollback-snapshot ./rollback-20260911.json \\
  --target "$OPSKEEPER_NEW_URL"`}
      </CodeBlock>
      <p>
        随时可用 <code>opskeeper-migrate list-entities</code> 列出支持的实体类型。
      </p>

      <h2 id="help">求助</h2>
      <p>
        在 <a href="https://github.com/louloulin/opskeeper/issues">github.com/louloulin/opskeeper/issues</a>{' '} 上开一个带 <code>migration</code> 标签的 issue，并附上 <code>opskeeper-migrate --help</code> 的输出。
      </p>
    </>
  );
}
