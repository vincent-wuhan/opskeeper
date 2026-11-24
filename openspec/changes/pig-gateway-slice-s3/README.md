# pig-gateway-slice-s3

pig-runtime-adapter 任务 3.1–3.5（治理工具网关集成）的执行切片：PiG 工具身份
翻译（消费 S2 对账输出）、分发前七条策略强制（含 incident/resource/read-only
三条硬闸）、调用持久化（新表 runtimemgmt_pig_tool_calls，参数只落哈希）、七类
失败闭合矩阵与 proposal-only 接缝。裁决：接入 Manager decorators 面（方案 A），
不造第三个网关。基线 main@f771c1bd。
