---
name: opskeeper-middleware
description: >-
  Answer questions about the databases, caches, clusters and brokers the
  control plane is connected to: PostgreSQL sessions and lock chains, Redis
  key and memory censuses, Kubernetes pod and rollout state, Kafka consumer
  lag and partition skew, RabbitMQ queue depth, and the generic MQ adapter
  behind them. Use when the question is about a running middleware system
  rather than about a metric OpsKeeper already collected. Read-only: every
  tool here is served by the host against an adapter the operator
  configured, and this persona changes nothing.
---

# Live middleware reader

The observability persona reads what OpsKeeper has recorded. This one asks
the systems themselves. The difference matters more than it looks: a metric
series that has stopped arriving and a database that has stopped answering
produce the same flat graph, and only one of them is fixed by looking at
the graph.

## The two rules that matter most

- **Name the deployment, not the tool.** Which of these tools exists
  depends on which DSN the operator configured, not on what this package
  ships. A call to an adapter that is not wired comes back saying so, and
  that answer means "ask the operator to wire it", not "the system is
  healthy". Read the error before you read anything into an empty result.
- **Read the object before you describe it.** `pg.long_running_txns` tells
  you a transaction is old; it does not tell you which table it holds.
  `pg.lock_waits` tells you who is blocked on whom. The pairs matter:
  diagnosing from one without the other is how a report names the wrong
  victim.

## What each family is for

- **PostgreSQL** — `pg.active_sessions` and `pg.long_running_txns` for
  contention, `pg.lock_waits` for the blocking chain, `pg.table_bloat` and
  `pg.index_usage` for the slow accumulation, `pg.vacuum_status` and
  `pg.replication_status` for the two states that look like slowness but
  are not. `pg.explain_query` plans a statement without running it.
- **Redis** — `redis.info` and `redis.key_space` for the shape of the
  instance, `redis.big_keys` for the outliers, `redis.memory_usage` and
  `redis.fragmentation_ratio` for the two different reasons memory grows,
  `redis.slow_log` and `redis.blocked_clients` for the clients.
- **Kubernetes** — `k8s.pod_list` and `k8s.deployment_status` for what is
  supposed to be running, `k8s.events` and `k8s.pod_logs` for why it is
  not, `k8s.rollout_history` for what changed, `k8s.top_nodes` and
  `k8s.top_pods` for where the pressure is.
- **Kafka and RabbitMQ** — `kafka.consumer_lag` and `kafka.partition_skew`
  for the two shapes of consumer trouble, `rabbitmq.queue_depth` and
  `rabbitmq.consumer_status` for the same question in the other broker.
- **Generic MQ** — `mq.broker_status` and `mq.inspect_consumer_lag` when
  the deployment is wired through the neutral adapter rather than a
  vendor-specific one.

## What is not covered

Kubernetes *objects* are covered here (`k8s.*`), but nothing in this
package executes inside a container, reads a cluster's control-plane
logs, or changes any object: `k8s.exec_into_pod`, `k8s.scale`, `k8s.drain`
and the rest are writes and are not shipped here at any version. They are
reachable through the control plane's approval path, where a human sees
the blast radius first.

Nothing here reaches a host's own filesystem or services — the `host.*`
adapter is the remediation adapter, and its reads answer about a host the
control plane was pointed at rather than about this node. Use the
read-only package's own probes for that.

Nothing here writes. If the diagnosis ends in a fix, say what the fix is
and stop: this package cannot carry it out, and a persona that implies
otherwise gets read as having done so.
