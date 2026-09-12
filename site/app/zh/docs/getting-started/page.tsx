import Link from 'next/link';
import { CodeBlock } from '@/components/code-block';

export const metadata = { title: '快速开始' };

export default function GettingStartedZhPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          快速上手
        </div>
        <h1>快速开始</h1>
        <p>
          本指南带你本地跑起 OpsKeeper 的闭环演示场景。需要 Docker、Go 1.25+、Node.js 20+ 和 pnpm 9+ —— Makefile 会帮你检查这些依赖。
        </p>
      </header>

      <h2 id="prerequisites">前置依赖</h2>
      <ul>
        <li>Docker 24+ 含 Compose 插件</li>
        <li>Go 1.25+（见 <code>.tool-versions</code>）</li>
        <li>Node.js 20+ 与 pnpm 9+</li>
        <li>Python 3.11+（仅插件测试需要）</li>
        <li>zip / tar / 标准 POSIX 工具</li>
      </ul>

      <h2 id="install">1. 安装</h2>
      <p>克隆仓库，在仓库根目录启动本地环境：</p>
      <CodeBlock language="bash" title="bootstrap">
        {`git clone https://github.com/vincent-wuhan/opskeeper.git
cd opskeeper
cp deploy/demo.env.example .env

# 构建并启动整套演示环境（首次构建需要几分钟）
docker compose up -d --build
docker compose ps                      # 等待所有服务 healthy

# 校验控制平面健康（opskeeper 的 HTTP 端口是 8080）
curl -fsS http://localhost:8080/healthz`}
      </CodeBlock>

      <h2 id="replay-a-scenario" className="scroll-mt-24">2. 写入场景</h2>
      <p>
        <code>deploy/incident-events/</code> 下有四个可复现的 PostgreSQL 场景，用 incident-seed
        工具把它们写入事件记忆库：
      </p>
      <CodeBlock language="bash" title="seed">
        {`# 先校验数据集（不写数据库）
go run ./cmd/incident-seed -dry-run -dir deploy/incident-events

# 把 4 个场景全部写入事件记忆库
go run ./cmd/incident-seed \\
  -dsn "postgres://opskeeper:opskeeper@localhost:5432/opskeeper?sslmode=disable" \\
  -dir deploy/incident-events`}
      </CodeBlock>
      <p>
        写入的事件覆盖连接池耗尽、磁盘 I/O 饱和、锁等待 / 长事务、副本 replay 延迟。通过事件
        API 查看指标、召回日志与 runbook：
      </p>
      <CodeBlock language="bash" title="inspect">
        {`# 控制平面提供的事件指标 + runbook
curl -fsS "http://localhost:8080/v1/incidents/metrics" | jq .
curl -fsS "http://localhost:8080/v1/incidents/runbooks" | jq .`}
      </CodeBlock>

      <h2 id="open-the-web-console" className="scroll-mt-24">3. 打开 Web Console</h2>
      <p>
        OpsKeeper 的 Web Console 是一个 React + Vite SPA，位于 <code>web/</code>，通过 <code>/api/v1</code> 与控制平面通信。
      </p>
      <CodeBlock language="bash" title="web">
        {`cd web
pnpm install --frozen-lockfile
pnpm dev
# → http://localhost:5173`}
      </CodeBlock>

      <h2 id="install-a-worker-plugin">4. 安装一个 Worker 插件</h2>
      <p>
        把 TeamHarness MCP 代理塞进任何讲 stdio MCP 的 Worker。它暴露 17 个工具，使用 Bearer + HMAC
        鉴权，全部通过环境变量配置：
      </p>
      <CodeBlock language="bash" title="plugin">
        {`cd plugins/opskeeper-teamharness

OPSKEEPER_BACKEND_URL=http://localhost:8080 \\
OPSKEEPER_GATEWAY_KEY="$GATEWAY_KEY" \\
OPSKEEPER_TENANT_ID=default \\
python3 mcp/server.py`}
      </CodeBlock>
      <p>
        AgentTeams Dashboard 插件包请运行 <code>make build-plugins</code>，然后在 Dashboard 里安装生成的
        zip（热部署：<code>qwenpaw plugin install &lt;path&gt; --force</code>）。
      </p>

      <h2 id="demos" className="scroll-mt-24">演示</h2>
      <p>
        如果想先体验公网环境，可以从{' '}
        <Link href="/zh/demo">在线演示</Link> 进入 OpsKeeper 控制台、AgentTeams
        Dashboard 和 AgentTeams Element 房间，查看端到端演示使用的三个联通入口。
      </p>
      <p>
        写入场景后，跑一遍验证 Harness，确认 Manager、critic、verifier 都正确接入。每个场景都是带
        runner 容器的独立 compose 栈：
      </p>
      <CodeBlock language="bash" title="verify">
        {`# 在插件目录下执行 —— alert_storm / rca_loop / recovery_verify
bash plugins/opskeeper-teamharness/eval/scenarios/alert_storm/run.sh
bash plugins/opskeeper-teamharness/eval/scenarios/rca_loop/run.sh
bash plugins/opskeeper-teamharness/eval/scenarios/recovery_verify/run.sh`}
      </CodeBlock>

      <h2 id="next">下一步</h2>
      <p>
        阅读 <Link href="/zh/docs/architecture">架构</Link> 文档，了解数据平面、Manager 派发表和安全边界。
      </p>
    </>
  );
}
