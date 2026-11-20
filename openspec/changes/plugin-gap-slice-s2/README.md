# plugin-gap-slice-s2

插件业务能力恢复方案 4.8 的切片 S-2：readonly 包 0.3.0——把 `query_incidents`、
`get_incident_detail`、`query_devices` 三个只读控制面工具写进
`opskeeper-sre-readonly` 的 manifest 与工具清单（方案 §5.2 / §5.4，零 profile
scope 增量），并同步 persona 的使用边界说明。

服务面在 S-1 已就位：三个工具经 BaseTool 上呼桥可服务，闸门
`cmd/opskeeper/plugin_gap_gate_test.go` 在 manifest 加名后自动纳入对账。
