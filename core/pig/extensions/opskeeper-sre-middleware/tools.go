package opskeepermiddleware

// GENERATED FILE — do not edit.
//
// Produced from the live registration of the tools in
// core/manager/middleware/adapter by core/manager/middleware/toolset's tests. The
// adapter is the authority: it is what executes the call and parses the
// arguments, so its description and its argument map are the ones that have
// to be correct, and this file is the copy the node's agent reads.
//
// Regenerate with:
//
//	OPSKEEPER_UPDATE_TOOLSET=1 go test ./core/manager/middleware/toolset/ -run Toolset
//
// then run scripts/sync-pig-ops.sh to copy the extension into the package.
// TestToolsetMatchesTheAdapters fails if this file and the adapters
// disagree, so editing it by hand fails a test rather than shipping a menu
// the executor cannot parse.
//
// Only L0 and L1 tools appear here. The writes the same adapters offer are
// reachable through the control plane's approval path and deliberately not
// through this one.

// toolSpec is one tool this package contributes to the agent.
//
// The table is data, not code: every entry is the same shape and every
// entry does the same thing, which is hand the call to the host. Nothing
// here interprets an argument or touches a system, because everything that
// does lives in the control plane, where it is permissioned, covered and
// audited.
type toolSpec struct {
	// Name is what the model calls. It is also the name the host looks
	// up, so it is one string end to end.
	Name string
	// Label is the human-readable name the console shows.
	Label string
	// Description is what the model reads to decide whether to call this.
	Description string
	// Parameters is the tool's JSON Schema, derived from the adapter's own
	// argument map. The adapter is what parses the call, so its idea of which
	// argument is required is the one that decides whether a call works.
	Parameters string
}

// tools is the whole inventory, sorted by name.
var tools = []toolSpec{

	{
		Name:        "git.find_runtime_link",
		Label:       "git.find_runtime_link",
		Description: "运行时符号 → commit + file:line 反查（集成 git-artifact Linker，4 类符号）",
		Parameters: `{
  "allOf": [
    {
      "if": {
        "properties": {
          "symbol_type": {
            "const": "pg_query"
          }
        }
      },
      "then": {
        "properties": {
          "input": {
            "properties": {
              "database": {
                "type": "string"
              },
              "query": {
                "type": "string"
              }
            },
            "required": [
              "query"
            ],
            "type": "object"
          }
        }
      }
    },
    {
      "if": {
        "properties": {
          "symbol_type": {
            "const": "redis_cmd"
          }
        }
      },
      "then": {
        "properties": {
          "input": {
            "properties": {
              "cmd": {
                "type": "string"
              },
              "key": {
                "type": "string"
              }
            },
            "required": [
              "cmd"
            ],
            "type": "object"
          }
        }
      }
    },
    {
      "if": {
        "properties": {
          "symbol_type": {
            "const": "k8s_image"
          }
        }
      },
      "then": {
        "properties": {
          "input": {
            "properties": {
              "image": {
                "type": "string"
              },
              "tag": {
                "type": "string"
              }
            },
            "required": [
              "image"
            ],
            "type": "object"
          }
        }
      }
    },
    {
      "if": {
        "properties": {
          "symbol_type": {
            "const": "http_route"
          }
        }
      },
      "then": {
        "properties": {
          "input": {
            "properties": {
              "handler": {
                "type": "string"
              },
              "method": {
                "type": "string"
              },
              "path": {
                "type": "string"
              }
            },
            "required": [
              "method",
              "path"
            ],
            "type": "object"
          }
        }
      }
    }
  ],
  "properties": {
    "input": {
      "description": "The symbol itself. Its fields depend on symbol_type; see the branches below.",
      "type": "object"
    },
    "symbol_type": {
      "description": "Which kind of runtime symbol to look up. It selects the shape of the input object.",
      "enum": [
        "pg_query",
        "redis_cmd",
        "k8s_image",
        "http_route"
      ],
      "type": "string"
    }
  },
  "required": [
    "symbol_type",
    "input"
  ],
  "type": "object"
}`,
	},

	{
		Name:        "k8s.cluster_info",
		Label:       "k8s.cluster_info",
		Description: "集群版本 + 节点数 + API server 地址",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "k8s.connect",
		Label:       "k8s.connect",
		Description: "建立集群连接（kubeconfig / in-cluster / endpoint，经 secretbox 解密）",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "k8s.deployment_status",
		Label:       "k8s.deployment_status",
		Description: "Deployment 期望/就绪/可用副本对比",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    },
    "name": {
      "type": "string"
    },
    "namespace": {
      "type": "string"
    },
    "unavailable": {
      "type": "boolean"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "k8s.describe_pod",
		Label:       "k8s.describe_pod",
		Description: "生成 Pod 详细诊断报告（状态 + 容器 + 最近事件）",
		Parameters: `{
  "properties": {
    "namespace": {
      "type": "string"
    },
    "pod": {
      "type": "string"
    }
  },
  "required": [
    "pod"
  ],
  "type": "object"
}`,
	},

	{
		Name:        "k8s.events",
		Label:       "k8s.events",
		Description: "K8s events（默认只看 Warning，最新优先）",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    },
    "namespace": {
      "type": "string"
    },
    "warnings_only": {
      "type": "boolean"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "k8s.node_list",
		Label:       "k8s.node_list",
		Description: "列出所有 Node + 状态 + 容量",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    },
    "unhealthy": {
      "type": "boolean"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "k8s.pod_list",
		Label:       "k8s.pod_list",
		Description: "列出 Pod（可过滤 namespace / 标签 / 仅异常）",
		Parameters: `{
  "properties": {
    "label_selector": {
      "type": "string"
    },
    "limit": {
      "type": "integer"
    },
    "namespace": {
      "type": "string"
    },
    "unhealthy": {
      "type": "boolean"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "k8s.pod_logs",
		Label:       "k8s.pod_logs",
		Description: "Pod 日志（tail N / previous）",
		Parameters: `{
  "properties": {
    "container": {
      "type": "string"
    },
    "namespace": {
      "type": "string"
    },
    "pod": {
      "type": "string"
    },
    "previous": {
      "type": "boolean"
    },
    "tail_lines": {
      "type": "integer"
    }
  },
  "required": [
    "pod"
  ],
  "type": "object"
}`,
	},

	{
		Name:        "k8s.pvc_list",
		Label:       "k8s.pvc_list",
		Description: "列出 PersistentVolumeClaim：申请容量 / 实际容量 / 绑定状态 / StorageClass。注意 API 不上报文件系统用量，用量要靠 k8s.pvc_usage 测量",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    },
    "namespace": {
      "type": "string"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "k8s.pvc_usage",
		Label:       "k8s.pvc_usage",
		Description: "测量 PVC 文件系统真实用量（找到挂载该 claim 的 Running Pod，在其挂载点执行 df）。测不到时返回 measured=false 与原因，不返回 0",
		Parameters: `{
  "properties": {
    "namespace": {
      "type": "string"
    },
    "pvc": {
      "type": "string"
    }
  },
  "required": [
    "pvc"
  ],
  "type": "object"
}`,
	},

	{
		Name:        "k8s.rollout_history",
		Label:       "k8s.rollout_history",
		Description: "Deployment 的 ReplicaSet 修订历史",
		Parameters: `{
  "properties": {
    "deployment": {
      "type": "string"
    },
    "limit": {
      "type": "integer"
    },
    "namespace": {
      "type": "string"
    }
  },
  "required": [
    "deployment"
  ],
  "type": "object"
}`,
	},

	{
		Name:        "k8s.rollout_status",
		Label:       "k8s.rollout_status",
		Description: "rollout 进展与阻塞原因；给 deployment 看单个，不给则列出全部未完成 rollout",
		Parameters: `{
  "properties": {
    "deployment": {
      "type": "string"
    },
    "limit": {
      "type": "integer"
    },
    "namespace": {
      "type": "string"
    },
    "only_stuck": {
      "type": "boolean"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "k8s.top_nodes",
		Label:       "k8s.top_nodes",
		Description: "Node 资源使用（需 metrics-server）",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "k8s.top_pods",
		Label:       "k8s.top_pods",
		Description: "每个容器的用量对其自身 limit 的占比（需 metrics-server），含 OOMKilled 证据（current/last terminated reason）与重启次数。无 limit 的容器单独标出——那不是安全",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    },
    "namespace": {
      "type": "string"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "kafka.broker_skew",
		Label:       "kafka.broker_skew",
		Description: "broker 节点 + controller + 各分区 leader 分布",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "kafka.consumer_lag",
		Label:       "kafka.consumer_lag",
		Description: "consumer group 逐分区滞后（OffsetFetch 对比 ListOffsets）",
		Parameters: `{
  "properties": {
    "group": {
      "type": "string"
    },
    "limit": {
      "type": "integer"
    },
    "queue": {
      "type": "string"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "kafka.partition_skew",
		Label:       "kafka.partition_skew",
		Description: "分区负载分布：每个分区未消费记录数与其占比，加上副本同步情况。Kafka 管理协议不提供分区级吞吐率，这里不是速率测量",
		Parameters: `{
  "properties": {
    "group": {
      "type": "string"
    },
    "limit": {
      "type": "integer"
    },
    "topic": {
      "type": "string"
    }
  },
  "required": [
    "topic"
  ],
  "type": "object"
}`,
	},

	{
		Name:        "kafka.rebalance_history",
		Label:       "kafka.rebalance_history",
		Description: "consumer group 成员变更历史（由连续采样的 DescribeGroups 得出；窗口从 OpsKeeper 首次观测该组开始，不含更早的变更）",
		Parameters: `{
  "properties": {
    "group": {
      "type": "string"
    },
    "limit": {
      "type": "integer"
    },
    "since_minutes": {
      "type": "integer"
    }
  },
  "required": [
    "group"
  ],
  "type": "object"
}`,
	},

	{
		Name:        "kafka.topic_list",
		Label:       "kafka.topic_list",
		Description: "列出所有 topic 及其分区与 leader",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    },
    "queue": {
      "type": "string"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "mq.broker_status",
		Label:       "mq.broker_status",
		Description: "broker 节点 / 分区分布",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "mq.connect",
		Label:       "mq.connect",
		Description: "连接 broker（amqp:// 或 kafka://，凭据经 secretbox 解密）",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "mq.inspect_consumer_lag",
		Label:       "mq.inspect_consumer_lag",
		Description: "消费滞后：RabbitMQ 按 queue 消费者速率，Kafka 按 consumer group 逐分区计算（Kafka 需 group；缺失时报告全部 group）",
		Parameters: `{
  "properties": {
    "group": {
      "type": "string"
    },
    "limit": {
      "type": "integer"
    },
    "queue": {
      "type": "string"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "mq.queue_list",
		Label:       "mq.queue_list",
		Description: "列出 queue / topic 及其堆积量",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    },
    "prefix": {
      "type": "string"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "pg.active_sessions",
		Label:       "pg.active_sessions",
		Description: "列出当前活跃会话",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "pg.connect",
		Label:       "pg.connect",
		Description: "建立 PG 连接（DSN 经 secretbox 解密）",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "pg.explain_query",
		Label:       "pg.explain_query",
		Description: "EXPLAIN（不带 ANALYZE，不执行语句）",
		Parameters: `{
  "properties": {
    "query": {
      "type": "string"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "pg.index_usage",
		Label:       "pg.index_usage",
		Description: "索引使用率（未使用索引优先）",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "pg.list_databases",
		Label:       "pg.list_databases",
		Description: "列出所有数据库（含大小与连接数）",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "pg.list_schemas",
		Label:       "pg.list_schemas",
		Description: "列出 schema（含表数量）",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "pg.list_tables",
		Label:       "pg.list_tables",
		Description: "列出表（含大小估算）",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "pg.lock_waits",
		Label:       "pg.lock_waits",
		Description: "锁等待链（等待者 → 阻塞者）",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "pg.long_running_txns",
		Label:       "pg.long_running_txns",
		Description: "列出长事务（阈值可配，默认 30s）",
		Parameters: `{
  "properties": {
    "min_age_seconds": {
      "type": "integer"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "pg.replication_status",
		Label:       "pg.replication_status",
		Description: "流复制状态：每个 standby 的 state / sync_state / 未应用 WAL 字节数与秒数。空结果=本实例没有连着的 standby（可能它自己就是 standby），不等于零延迟；replay_lsn 为 NULL 表示位置未知而非零",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "pg.slow_log",
		Label:       "pg.slow_log",
		Description: "慢查询（pg_stat_statements）",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    },
    "min_ms": {
      "type": "integer"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "pg.table_bloat",
		Label:       "pg.table_bloat",
		Description: "表膨胀估算（死元组占比）",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "pg.top_queries_by_calls",
		Label:       "pg.top_queries_by_calls",
		Description: "按调用次数排序 Top N 慢查询",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "pg.top_queries_by_time",
		Label:       "pg.top_queries_by_time",
		Description: "按总耗时排序 Top N 慢查询",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "pg.vacuum_status",
		Label:       "pg.vacuum_status",
		Description: "正在进行的 vacuum 进度",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "rabbitmq.cluster_info",
		Label:       "rabbitmq.cluster_info",
		Description: "cluster 拓扑 + 节点（内存 / 磁盘 / fd / 运行时长）",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "rabbitmq.consumer_status",
		Label:       "rabbitmq.consumer_status",
		Description: "消费者覆盖情况：按每条消息需要多少个消费者排序，0 消费者的 queue 单独标出。深度大但有消费者是健康的，深度大且无人消费才是事故",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    },
    "prefix": {
      "type": "string"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "rabbitmq.queue_depth",
		Label:       "rabbitmq.queue_depth",
		Description: "queue 堆积深度（就绪 / 未确认 / 每消费者分摊），按最深优先",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    },
    "prefix": {
      "type": "string"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "rabbitmq.queue_list",
		Label:       "rabbitmq.queue_list",
		Description: "列出所有 queue + 状态 + 消费者数",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    },
    "prefix": {
      "type": "string"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "redis.big_keys",
		Label:       "redis.big_keys",
		Description: "TOP N 大 key（SCAN 采样 + MEMORY USAGE）",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    },
    "scan_limit": {
      "type": "integer"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "redis.blocked_clients",
		Label:       "redis.blocked_clients",
		Description: "阻塞客户端",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "redis.client_list",
		Label:       "redis.client_list",
		Description: "客户端连接列表",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "redis.cluster_info",
		Label:       "redis.cluster_info",
		Description: "cluster 拓扑信息",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "redis.config_get",
		Label:       "redis.config_get",
		Description: "读取配置项",
		Parameters: `{
  "properties": {
    "parameter": {
      "type": "string"
    }
  },
  "required": [
    "parameter"
  ],
  "type": "object"
}`,
	},

	{
		Name:        "redis.connect",
		Label:       "redis.connect",
		Description: "建立 Redis 连接（DSN 经 secretbox 解密）",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "redis.dbsize",
		Label:       "redis.dbsize",
		Description: "当前 db 的 key 数量",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "redis.fragmentation_ratio",
		Label:       "redis.fragmentation_ratio",
		Description: "内存碎片率",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "redis.hot_keys",
		Label:       "redis.hot_keys",
		Description: "TOP N 热 key（SCAN 采样 + OBJECT FREQ，需 LFU 淘汰策略）",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    },
    "scan_limit": {
      "type": "integer"
    }
  },
  "type": "object"
}`,
	},

	{
		Name:        "redis.info",
		Label:       "redis.info",
		Description: "server info（分段解析）",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "redis.key_space",
		Label:       "redis.key_space",
		Description: "各 db 的 key 分布",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "redis.memory_usage",
		Label:       "redis.memory_usage",
		Description: "整体内存占用与分配器统计",
		Parameters: `{
  "properties": {},
  "type": "object"
}`,
	},

	{
		Name:        "redis.slow_log",
		Label:       "redis.slow_log",
		Description: "Redis 慢日志",
		Parameters: `{
  "properties": {
    "limit": {
      "type": "integer"
    }
  },
  "type": "object"
}`,
	},
}

// ToolNames returns the inventory in order, for the host-side drift
// check and for diagnostics.
func ToolNames() []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}
