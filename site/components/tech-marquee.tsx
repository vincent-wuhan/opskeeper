import {
  Database,
  Brain,
  Container,
  Activity,
  Server,
  Network,
  Boxes,
  Lock,
  FileCode2,
  ShieldCheck,
  GitBranch,
  Boxes as ContainerIcon,
} from 'lucide-react';

type Logo = { name: string; icon: React.ComponentType<{ className?: string }> };

const stack: Logo[] = [
  { name: 'PostgreSQL', icon: Database },
  { name: 'Qdrant', icon: Brain },
  { name: 'Kubernetes', icon: Container },
  { name: 'Prometheus', icon: Activity },
  { name: 'Loki', icon: Server },
  { name: 'Tempo', icon: Network },
  { name: 'Grafana', icon: Boxes },
  { name: 'OpenTelemetry', icon: Network },
  { name: 'Nacos', icon: GitBranch },
  { name: 'MCP', icon: FileCode2 },
  { name: 'HMAC', icon: Lock },
  { name: 'W3C Traceparent', icon: ShieldCheck },
];

export function TechMarquee({ label }: { label?: string }) {
  const items = [...stack, ...stack]; // duplicate for seamless loop
  return (
    <section className="border-y border-white/5 bg-ink-950/40 py-8">
      <div className="mx-auto max-w-7xl px-4 sm:px-6 lg:px-8">
        {label && (
          <div className="mb-6 text-center text-xs font-medium uppercase tracking-wider text-ink-400">
            {label}
          </div>
        )}
        <div
          className="relative overflow-hidden"
          style={{
            maskImage:
              'linear-gradient(to right, transparent, black 10%, black 90%, transparent)',
            WebkitMaskImage:
              'linear-gradient(to right, transparent, black 10%, black 90%, transparent)',
          }}
        >
          <div className="flex w-max animate-marquee gap-12 py-2">
            {items.map((it, i) => (
              <div
                key={`${it.name}-${i}`}
                className="flex shrink-0 items-center gap-2 text-ink-300"
              >
                <it.icon className="h-5 w-5 text-accent-300" />
                <span className="whitespace-nowrap text-sm font-medium text-ink-200">
                  {it.name}
                </span>
              </div>
            ))}
          </div>
        </div>
      </div>
    </section>
  );
}
