# GA 正式链路合成轨迹

`roundtrip.json` 是独立手写的 `synthetic-negative` 四点轨迹，含 60 个节点。
`TestOpenAIRealtimeConformanceReplay` 通过 `ReadWSFixture` 完整校验，再运行
测试专用整门矩阵下的正式 Mux → Handler → Provider。

- `ready.u/ready.c` 独立发送、逐字节交付；不能等下游 101 后才发 ready。
- tail 只移除 prelude 及相应因果边；所有实体 ID、空白、presence 与 payload
  都按原 bytes 匹配，无 bind/reference/ignore 规则。
- 覆盖文本、音频、工具调用往返、图像/detail、reasoning、错误后继续、cancelled
  与 truncate；未来字段不重编码。
- 生成用量为 132/121 与 cancelled 的 0/2 token；ASR 独立为 13/9 token、
  1.25 秒、0 秒及 duration 缺数值。权威总量为 145 输入 / 132 输出 token，
  不把秒数或明细加进总量。

图像和音频仅为占位字节，配置回显也为合成；这些数据只证明接线与负载保全，
不是 OpenAI 云端成功或逐能力兑现证据。生产门仍为 PLANNED。
