package opskeepersre

// toolSpec is one tool this package contributes to the agent.
//
// The table is data, not code: every entry is the same shape and every
// entry does the same thing, which is hand the call to the host. Nothing
// here interprets an argument or touches the machine, because everything
// that does lives in the edge, where it is permissioned, tested and — for
// the control-plane tools — reachable at all.
//
// The descriptions and schemas are copied from the host implementations
// rather than rewritten. A tool whose schema the model sees and whose
// schema the executor parses are two different objects, and a hand-edited
// copy of one is a lie the model finds out about at run time. The drift
// test in core/floor/pluginmanifest is what keeps the copy honest.
type toolSpec struct {
	// Name is what the model calls. It is also the name the host looks
	// up, so it is one string end to end rather than a translation
	// somewhere in the middle.
	Name string
	// Label is the human-readable name the console shows.
	Label string
	// Description is what the model reads to decide whether to call this.
	Description string
	// Parameters is the tool's JSON Schema, as a raw literal so a schema
	// change shows up in review as a schema change.
	Parameters string
}

// tools is the whole inventory, sorted by name.
//
// Sorted because this list is the review surface: a reviewer comparing two
// versions of this package is reading a diff, and a reordered list makes
// that diff lie.
var tools = []toolSpec{

	{
		Name:        "expand_topology",
		Label:       "expand_topology",
		Description: "Walk the topology graph outward from a node along failure-propagating relations. Use this to find what a component depends on and what depends on it.",
		Parameters: `{
  "type": "object",
  "properties": {
    "node_id": {
      "type": "integer",
      "description": "Topology node id to BFS from. Mutually exclusive with device_id; one of the two is required."
    },
    "device_id": {
      "type": "integer",
      "description": "Resolve a Device's linked node and BFS from there."
    },
    "depth": {
      "type": "integer",
      "description": "Max BFS hops. Default 2; cap 5.",
      "default": 2
    },
    "only_propagating": {
      "type": "boolean",
      "description": "When true (default), only walk relations whose semantics drives failure propagation. When false, walks every relation.",
      "default": true
    },
    "direction": {
      "type": "string",
      "enum": [
        "both",
        "downstream",
        "upstream"
      ],
      "description": "downstream = follow propagation direction; upstream = inverse. Default both.",
      "default": "both"
    }
  }
}`,
	},
	{
		Name:        "find_outlier_edges",
		Label:       "find_outlier_edges",
		Description: "Find fleet edges whose cpu/mem/disk is statistically unlike their peers. Use this to narrow a fleet-wide incident to the few hosts that actually differ.",
		Parameters: `{
  "type": "object",
  "properties": {
    "metric": {
      "type": "string",
      "enum": [
        "cpu",
        "mem",
        "disk"
      ],
      "description": "Which closed-set host metric to compare across the fleet."
    },
    "sigma": {
      "type": "number",
      "minimum": 0.5,
      "maximum": 10,
      "description": "z-score threshold (default 2). Edges with z > sigma are returned."
    }
  },
  "required": [
    "metric"
  ]
}`,
	},
	{
		Name:        "find_topology_node",
		Label:       "find_topology_node",
		Description: "Search the topology graph for nodes by name, optionally filtered by node type. Use this when you know roughly what something is called but not its id.",
		Parameters: `{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "Case-insensitive substring of Node.Name to match. Required."
    },
    "type": {
      "type": "string",
      "description": "Optional exact-match filter on Node.Type.",
      "examples": [
        "service",
        "cluster",
        "app",
        "device",
        "rack"
      ]
    },
    "limit": {
      "type": "integer",
      "description": "Cap on returned rows. Default 20, max 50.",
      "default": 20
    }
  },
  "required": [
    "name"
  ]
}`,
	},
	{
		Name:        "get_topology",
		Label:       "get_topology",
		Description: "Return high-level deployment topology: manager version, edge fleet size + online count, configured Prom/Loki/Tempo URLs, channel count, enabled rule count.",
		Parameters: `{
  "type": "object",
  "properties": {}
}`,
	},
	{
		Name:        "host_dmesg",
		Label:       "内核日志",
		Description: "读内核环形缓冲 (dmesg -T --level=err,warn). 诊断硬件错误 / 驱动异常 / OOM. 容器内可能读不到, 返回明确错误. NOT for: 应用日志 (tail_file) / 系统日志 (journalctl).",
		Parameters: `{
  "type": "object",
  "properties": {
    "levels": {
      "type": "string",
      "description": "日志级别, 逗号分隔 (默认 err,warn)",
      "default": "err,warn"
    },
    "since": {
      "type": "string",
      "description": "可选 RFC3339 起始时间, 只返回之后条目"
    },
    "max_lines": {
      "type": "integer",
      "description": "最大返回行数, 默认 200",
      "default": 200
    }
  }
}`,
	},
	{
		Name:        "host_file_inventory",
		Label:       "文件空间盘点",
		Description: "盘点一个目录树占用了多少空间：按大小排的目录小计 + 最大的文件. 诊断磁盘满 / 空间暴涨时定位到具体路径. NOT for: 按内容搜索 (host_grep_file).",
		Parameters: `{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "目录绝对路径"
    },
    "top_n": {
      "type": "integer",
      "description": "每张榜返回条数, 默认 20, 上限 100"
    },
    "max_depth": {
      "type": "integer",
      "description": "目录小计的最大深度, 默认 3, 上限 8"
    },
    "min_size_bytes": {
      "type": "integer",
      "description": "文件榜的最小字节数, 默认 0"
    }
  },
  "required": ["path"]
}`,
	},
	{
		Name:        "host_grep_file",
		Label:       "文件正则搜索",
		Description: "在文件 / 目录树里按正则搜索 (grep -nE). 返回匹配的 line_num + line + 命中数. NOT for: 大文件全量读取 (tail_file) / 二进制 (strings).",
		Parameters: `{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "文件 / 目录绝对路径"
    },
    "pattern": {
      "type": "string",
      "description": "正则表达式 (POSIX ERE)"
    },
    "ignore_case": {
      "type": "boolean",
      "description": "是否忽略大小写"
    },
    "max_matches": {
      "type": "integer",
      "description": "最大返回命中数, 默认 100",
      "default": 100
    }
  },
  "required": [
    "path",
    "pattern"
  ]
}`,
	},
	{
		Name:        "host_lsof",
		Label:       "打开文件/句柄查询",
		Description: "列出进程打开的文件 / 路径上的句柄 (lsof -p <pid> 或 lsof <path>). 诊断文件锁 / 句柄泄漏 / 删除但未释放的文件. NOT for: 进程列表 (probe_process).",
		Parameters: `{
  "type": "object",
  "properties": {
    "pid": {
      "type": "integer",
      "description": "目标进程 PID (与 path 二选一)"
    },
    "path": {
      "type": "string",
      "description": "目标路径 (与 pid 二选一)"
    }
  }
}`,
	},
	{
		Name:        "host_mtr",
		Label:       "路由+丢包综合探测",
		Description: "mtr -n -r -c 1 -w 输出 JSON; 报告每跳丢包率 + Avg/Best/Worst/StDev RTT. 诊断跨运营商丢包 / 中段路由黑洞. NOT for: 单点延迟 (probe_tcp) / 跳点定位 (traceroute).",
		Parameters: `{
  "type": "object",
  "properties": {
    "host": {
      "type": "string",
      "description": "目标主机"
    },
    "timeout_sec": {
      "type": "integer",
      "description": "总超时秒数",
      "default": 30
    }
  },
  "required": [
    "host"
  ]
}`,
	},
	{
		Name:        "host_netns_inspect",
		Label:       "网络命名空间探查",
		Description: "列出 /var/run/netns 下的所有 network namespace 并对每个 namespace 报告 IP 地址 / 路由 / 接口状态。填补 host_bash 不支持 `ip -n <ns>` 的盲区。仅 read-only。",
		Parameters: `{
  "type": "object",
  "properties": {
    "namespace": {
      "type": "string",
      "description": "可选：只查这一个 namespace 名（精确匹配）。留空 = 列全部。仅允许 [a-zA-Z0-9_.-]，最长 64 字符。"
    },
    "include_routes": {
      "type": "boolean",
      "description": "是否带回每个 ns 的路由表。默认 true。",
      "default": true
    },
    "include_links": {
      "type": "boolean",
      "description": "是否带回每个 ns 的 link 列表（含 MAC / state）。默认 false（多 ns 时数据量大）。"
    }
  }
}`,
	},
	{
		Name:        "host_probe_dns",
		Label:       "DNS 解析",
		Description: "DNS 解析目标 host，返回 A/AAAA 记录",
		Parameters: `{
  "type": "object",
  "properties": {
    "host": {
      "type": "string",
      "description": "要解析的主机名，例如 example.com"
    },
    "timeout_ms": {
      "type": "integer",
      "description": "解析超时（毫秒），默认 3000",
      "default": 3000
    }
  },
  "required": [
    "host"
  ]
}`,
	},
	{
		Name:        "host_probe_http",
		Label:       "HTTP 探测",
		Description: "对 URL 发起 HEAD/GET 请求，返回状态码 + 延迟 + 内容长度",
		Parameters: `{
  "type": "object",
  "properties": {
    "url": {
      "type": "string",
      "description": "完整 URL，例如 https://example.com/health"
    },
    "method": {
      "type": "string",
      "description": "HTTP 方法，默认 HEAD",
      "default": "HEAD"
    },
    "timeout_ms": {
      "type": "integer",
      "description": "请求超时（毫秒），默认 5000",
      "default": 5000
    }
  },
  "required": [
    "url"
  ]
}`,
	},
	{
		Name:        "host_probe_tcp",
		Label:       "TCP 连通性探测",
		Description: "对目标 host:port 发起 TCP 连接，返回连通状态 + 延迟",
		Parameters: `{
  "type": "object",
  "properties": {
    "target": {
      "type": "string",
      "description": "目标地址，host:port 形式，例如 google.com:443"
    },
    "timeout_ms": {
      "type": "integer",
      "description": "拨号超时（毫秒），默认 3000",
      "default": 3000
    }
  },
  "required": [
    "target"
  ]
}`,
	},
	{
		Name:        "host_read_journal",
		Label:       "Journald 日志读取",
		Description: "读 systemd-journald 日志（journalctl 包装），仅 Linux 支持",
		Parameters: `{
  "type": "object",
  "properties": {
    "unit": {
      "type": "string",
      "description": "systemd unit 名（可选），例如 opskeeper-edge"
    },
    "since": {
      "type": "string",
      "description": "回溯时长（journalctl --since 形式），默认 10m",
      "default": "10m"
    },
    "lines": {
      "type": "integer",
      "description": "最大行数，默认 200",
      "default": 200
    }
  }
}`,
	},
	{
		Name:        "host_sosreport",
		Label:       "系统取证打包",
		Description: "sos report --batch 收集系统诊断信息 (硬件 / 内核 / 网络 / 进程 / 配置). 输出 100MB+ tarball, 自动 spill 到 /var/tmp/. 容器内不可用. NOT for: 频繁调用 (重操作) / 单项数据 (各专项 skill).",
		Parameters: `{
  "type": "object",
  "properties": {
    "output_dir": {
      "type": "string",
      "description": "报告输出目录",
      "default": "/var/tmp"
    },
    "timeout_sec": {
      "type": "integer",
      "description": "总超时秒数, 默认 300 (5min)",
      "default": 300
    }
  }
}`,
	},
	{
		Name:        "host_strace",
		Label:       "进程系统调用追踪",
		Description: "跟踪指定 PID 的系统调用摘要 (strace -c -p <pid>). 报告 syscall / calls / errors / time. 需要 CAP_SYS_PTRACE. NOT for: 长时间跟踪 (>duration_sec) / 内核追踪 (bpftrace).",
		Parameters: `{
  "type": "object",
  "properties": {
    "pid": {
      "type": "integer",
      "description": "目标进程 PID"
    },
    "duration_sec": {
      "type": "integer",
      "description": "跟踪时长秒数, 默认 5",
      "default": 5
    },
    "events": {
      "type": "string",
      "description": "事件过滤, 如 'network,file,desc' (默认 all)"
    }
  },
  "required": [
    "pid"
  ]
}`,
	},
	{
		Name:        "host_tail_file",
		Label:       "文件尾部读取",
		Description: "读取文件最后 N 行（类似 tail -n）",
		Parameters: `{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "文件绝对路径，必须以 / 开头且不含 .."
    },
    "lines": {
      "type": "integer",
      "description": "返回行数，默认 100",
      "default": 100
    },
    "max_bytes": {
      "type": "integer",
      "description": "最多读取的尾部字节数，默认 1MiB",
      "default": 1048576
    }
  },
  "required": [
    "path"
  ]
}`,
	},
	{
		Name:        "host_top_processes",
		Label:       "节点进程排行",
		Description: "按 CPU 或内存列出本节点进程 (pid/命令/用户/百分比). 诊断 CPU 飙高 / 内存吃紧时定位到具体进程. NOT for: 打开文件查询 (host_lsof).",
		Parameters: `{
  "type": "object",
  "properties": {
    "sort_by": {
      "type": "string",
      "enum": ["cpu", "mem"],
      "description": "排序依据: cpu (默认) 或 mem"
    },
    "top_n": {
      "type": "integer",
      "description": "返回条数, 默认 20, 上限 100"
    },
    "interval_ms": {
      "type": "integer",
      "description": "两次采样间隔毫秒, 默认 500, 上限 5000; CPU 百分比是这个窗口内的速率"
    }
  }
}`,
	},
	{
		Name:        "host_traceroute",
		Label:       "网络路由追踪",
		Description: "追踪到目标 host 的网络路由跳点 (traceroute -n -w 2 -q 1). 诊断路由环路 / 高延迟跳点 / 跨运营商丢包. NOT for: ICMP 被防火墙阻断 (用 mtr 更鲁棒) / 单点延迟 (probe_tcp).",
		Parameters: `{
  "type": "object",
  "properties": {
    "host": {
      "type": "string",
      "description": "目标主机 IP 或域名"
    },
    "max_hops": {
      "type": "integer",
      "description": "最大跳数, 默认 30",
      "default": 30
    },
    "timeout_sec": {
      "type": "integer",
      "description": "总超时秒数, 默认 30",
      "default": 30
    }
  },
  "required": [
    "host"
  ]
}`,
	},
	{
		Name:        "query_alert_rules",
		Label:       "query_alert_rules",
		Description: "List OpsKeeper alert rules filtered by kind, enabled flag, or a name substring. Returns array of {id, rule_key, kind, name, scope_type, severity, enabled}.",
		Parameters: `{
  "type": "object",
  "properties": {
    "kind": {
      "type": "string",
      "description": "Filter by rule kind (metric_threshold | metric_anomaly | metric_forecast | metric_burn_rate | metric_raw | log_match | log_volume | trace_latency | trace_error_rate)."
    },
    "enabled": {
      "type": "boolean",
      "description": "Filter by enabled flag. Optional."
    },
    "name_contains": {
      "type": "string",
      "description": "Substring filter against rule name OR rule_key."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 500,
      "description": "Max rows returned (default 100)."
    }
  }
}`,
	},
}

// ToolNames returns the inventory in order, for the host-side drift check
// and for diagnostics.
func ToolNames() []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}
