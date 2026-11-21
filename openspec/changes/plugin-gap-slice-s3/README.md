# plugin-gap-slice-s3

插件业务能力恢复方案 4.8 的切片 S-3：observability 包 0.3.0——把 `query_knowledge`
（含 `knowledge.read` scope 增量与 finance/SaaS 两 profile 的 Granted 同步）与
`get_host_processes` 写进 `opskeeper-sre-observability` 的 manifest 与生成链
（方案 §5.1 / §5.3），并按 §4 的措辞要求为"已声明但依赖缺失"的就地降级分句
（7.2），不提前实现其他 S-3 项。

生成链走 `OPSKEEPER_UPDATE_TOOLSET=1 go test ./core/manager/biz/aiops/tools/ -run Toolset`
→ `scripts/sync-pig-ops.sh`；`promptguard.KindSource` 对 `query_knowledge` 的
标注已在表内，零增量。
