import { CodeBlock } from '@/components/code-block';

export const metadata = { title: '部署' };

export default function DeploymentZhPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          运维
        </div>
        <h1>部署</h1>
        <p>
          OpsKeeper 支持 standalone、高可用和 Kubernetes 部署。本页介绍典型拓扑；详细清单在仓库的 <code>deploy/</code> 下。
        </p>
      </header>

      <h2 id="standalone">Standalone</h2>
      <p>单主机安装，适合开发或小团队。一个二进制、一个 PostgreSQL、一个 Qdrant 实例。</p>
      <CodeBlock language="bash" title="standalone">
        {`# 1. 配置
cp .env.example .env
$EDITOR .env   # 配置 POSTGRES_DSN、QDRANT_URL、OPSKEEPER_PLUGIN_HMAC

# 2. 启动
docker compose up -d

# 3. 校验
opskeeper health
opskeeper incident list --limit 5`}
      </CodeBlock>

      <h2 id="ha">高可用</h2>
      <p>
        在 TCP 负载均衡器后面跑多个 OpsKeeper 控制平面实例。编排器通过 MySQL <code>GET_LOCK</code> 风格的咨询锁做 per-incident 串行化，因此 leader 是隐式的、不需要选举。
      </p>
      <CodeBlock language="text" title="ha topology">
        {`              ┌──────────────┐
              │  client / web │
              └──────┬───────┘
                     ▼
            ┌──────────────────┐
            │  TCP 负载均衡器    │
            └──────┬───────────┘
                   ▼
   ┌───────────┬──┴──┬───────────┐
   ▼           ▼     ▼           ▼
opskeeper-1 opskeeper-2 opskeeper-3 opskeeper-N
   └───────────┴──┬──┴───────────┘
                  ▼
        ┌──────────────────────┐
        │  PostgreSQL 主库      │
        │   + 只读副本           │
        └──────────────────────┘
                  ▼
        ┌──────────────────────┐
        │  Qdrant 集群          │
        └──────────────────────┘`}
      </CodeBlock>
      <p>
        标准的 Compose 和 Kubernetes 清单，以及备份/恢复 runbook，见 <code>docs/deployment/ha.md</code>。
      </p>

      <h2 id="kubernetes">Kubernetes</h2>
      <p>
        Kubernetes Operator 在 2027 Q1 路线图上。当前推荐方式是用 <code>deploy/k8s/</code> 下的 Helm chart：
      </p>
      <CodeBlock language="bash" title="kubernetes">
        {`helm upgrade --install opskeeper deploy/k8s/charts/opskeeper \\
  --namespace opskeeper --create-namespace \\
  --set persistence.postgres.size=200Gi \\
  --set persistence.qdrant.size=100Gi`}
      </CodeBlock>

      <h2 id="edge">边缘安装</h2>
      <p>
        <code>opskeeper-edge</code> 二进制在边缘侧（车间、零售 POS、区域 POP）跑 Worker，通过插件 stdio MCP 通道把证据转发到中心控制平面。
      </p>
      <CodeBlock language="bash" title="edge">
        {`# 配置一个新的边缘节点
opskeeper-edge provision \\
  --endpoint https://opskeeper.internal.example.com \\
  --name factory-floor-7

# 跑边缘守护进程
opskeeper-edge serve --config /etc/opskeeper/edge.yaml`}
      </CodeBlock>

      <h2 id="upgrade">升级</h2>
      <p>按你的拓扑对应的升级 runbook 走。一般流程：</p>
      <ol>
        <li>把活跃事件排到 <code>verified</code> 或 <code>postmortem</code>。</li>
        <li>应用新镜像 / 新二进制。</li>
        <li>逐个滚动升级控制平面实例。</li>
        <li>重放最近的账本事件，确认没有事件丢失。</li>
      </ol>

      <h2 id="external-deps">外部依赖</h2>
      <p>
        必需和可选服务的完整表格（含最低版本、不可用时的故障模式）见 <code>docs/deployment/external-deps.md</code>。
      </p>
    </>
  );
}
