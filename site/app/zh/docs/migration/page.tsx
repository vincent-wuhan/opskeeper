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

      <h2 id="step-1">第 1 步 · 执行 schema 迁移</h2>
      <CodeBlock language="bash" title="schema">
        {`opskeeper-migrate --from ongrid-lineage-1.x \\
  --target opskeeper-v2026.09.03 \\
  --dsn "$POSTGRES_DSN"`}
      </CodeBlock>

      <h2 id="step-2">第 2 步 · 翻译技能元数据</h2>
      <p>
        较老的 <code>skill_manifest.json</code> 在 dry-run 模式下被翻译为新的 <code>skill_meta.yaml</code> 格式。先检查再应用：
      </p>
      <CodeBlock language="bash" title="skill migration">
        {`# Dry-run
opskeeper migrate skills --dry-run --in ./skills-legacy

# Apply
opskeeper migrate skills --in ./skills-legacy --out ./skills-v2`}
      </CodeBlock>

      <h2 id="step-3">第 3 步 · 重新签发 HMAC 密钥</h2>
      <p>
        HMAC 链的根密钥用新密钥重新生成。旧密钥通过 <code>--hmac-rotate-grace</code> 参数保留 24 小时。
      </p>
      <CodeBlock language="bash" title="hmac">
        {`opskeeper audit rekey \\
  --new-secret "$OPSKEEPER_NEW_HMAC" \\
  --grace 24h`}
      </CodeBlock>

      <h2 id="step-4">第 4 步 · 校验</h2>
      <p>端到端走一遍新账本。Replay 应无中断。</p>
      <CodeBlock language="bash" title="verify">
        {`opskeeper audit replay --from GENESIS --to now`}
      </CodeBlock>

      <h2 id="rollback">回滚</h2>
      <p>
        迁移工具把每一步记录在 <code>migration_log</code> 里。用 <code>opskeeper migrate rollback</code> 加步骤 ID 可以撤销单步。如果 schema 还没晋升到 <code>v2026.09.03</code> 之后，可以完整回滚。
      </p>

      <h2 id="help">求助</h2>
      <p>
        在 <a href="https://github.com/louloulin/opskeeper/issues">github.com/louloulin/opskeeper/issues</a>{' '} 上开一个带 <code>migration</code> 标签的 issue，并附上 <code>opskeeper migrate doctor</code> 的输出。
      </p>
    </>
  );
}
