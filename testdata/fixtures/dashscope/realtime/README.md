# DashScope Realtime 真实轨迹归档

审核日期：2026-10-04。来源为北京官方端点
`wss://dashscope.aliyuncs.com/api-ws/v1/realtime` 的独立直连录制；批次
`.local/recordings/dsrealtime/batch-20261004` 已消耗 **8/8 次尝试**，包括失败尝试。
本次仅归档和离线回放，新增真实调用 **0**；不得重跑命令或换 batch 绕过上限。

## 文件与不可变摘要

三个顶层 JSON 是原 `candidate.json` 的原字节副本；`recordings/` 中同名文件是原
`recording.json` 的原字节副本。源目录分别为文件同名 run，未改写 Note、ID、版本或历史元数据。
`text-tools-v3` 的候选由第八次完整录制离线恢复，并非第九次云端调用。

| 文件 | 字节数 | SHA-256 |
|---|---:|---|
| `tts-commit-v3.json` | 435661 | `41eab869cd2456ce2244e887867306bf336c07ae403378842d5a291748f3e429` |
| `recordings/tts-commit-v3.json` | 148625 | `43edba97b4dce9cd37bc37bed556c8438e36e1987a03e1c4e91d5f09b8b15eeb` |
| `tts-server-commit-v2.json` | 497882 | `a6140efd3485acda1d0b3dbbc0d6b9c6036f17bcddd92cc27c593efd68f80f52` |
| `recordings/tts-server-commit-v2.json` | 171228 | `7b8fb6cd5defab14e76a5149d8bec4d796c87b26aa72e366e97879a27c579c71` |
| `text-tools-v3.json` | 128738 | `2155931e0220da70a2693dbb45bb0fcf63a92fbd9eef6d516dfc7b41c7f277a2` |
| `recordings/text-tools-v3.json` | 25857 | `971d3b8752fbb04313b3b2b4f918daf11eb1b4ff73765fc63d0f791f2fa55c6f` |

| 轨迹 | 开始时间（北京时间） | 请求及回显模型 | 消息 / 四点节点 / response 终态 |
|---|---|---|---|
| commit | 2026-10-04 13:22:52 | `qwen3-tts-flash-realtime` | 23 / 48 / 1 |
| server_commit | 2026-10-04 13:41:15 | `qwen3-tts-flash-realtime` | 22 / 46 / 1 |
| text-tools | 2026-10-04 13:41:30 | `qwen3.5-omni-flash-realtime` | 50 / 102 / 3 |

文件内时间保持原始 UTC。`official-raw-2026-10-04` 是参考文档日期标签；模型是请求别名，
回显也是上述别名，**不代表云端固定版本快照**。`provenance.source_sha256` 是 testkit
契约摘要，与此处整个文件的 SHA-256 不同；都仅提供自洽核对，不能单独认证来源。

## 源审查状态与来源边界

已审阅三份录制的全部解码 JSON 消息、握手元数据和关闭记录：

- 请求仅含公开短句“请描述图片中的颜色和形状。”、公开测试指令与口令“蓝色方块”、
  两个固定工具 `test_color` / `test_shape`（参数 `{}`，结果“蓝色”/“方块”）。未执行外部工具。
- TTS 使用系统音色 Cherry；文本模型回显默认 Tina，但该会话只有文本输出。
  源内容未发现凭据、个人信息、私人录音或业务数据；音频未试听、未调用 ASR，
  **不能声称语音内容人工核验通过**。输入短句与原生 PCM 的取得、保全已验证。
- 原录制只保留 `connection=Upgrade`、`upgrade=websocket` 响应头；候选握手不含凭据。
  服务端 event/session/response/item/call/conversation ID 原样保留，不作脱敏重编码。
- 每条原消息映射到候选两个点，opcode 与 payload 完全相同：23/22/50 条原消息对应
  46/44/100 个消息节点，双点 payload 总字节数为 218504 / 252558 / 29952。
  `client.send=authored`，`upstream.receive=upstream-accepted` 是由真实后继见证支持的
  请求预期；不是云端抓包。`upstream.send=recorded`，`client.receive=golden` 是同契约保全预期。
- 三份原录制最后均实际记录 send close1000、receive close1000，reason 为空。
  候选各只投影一组跨端 close（2 节点），不重复编排自动回应；完整回应留在 recordings。
- 候选原 Note 的“待审核”措辞保留历史原状，本 README 独立登记本次源审查；
  源审查通过与逐能力语义验收是不同结论。

## 音频样本

TTS 候选均内嵌 `samples["output.pcm_s16le.16000.mono"]`，不需要另存重复 PCM。
已将全部原始 audio.delta 严格 base64 解码并按顺序拼接，同时核对内嵌样本及源 `audio.pcm`。

| 轨迹 | 分片 | PCM 字节 | PCM SHA-256 | 源 audio-sample.json SHA-256（各 277 字节） |
|---|---:|---:|---|---|
| commit | 8 | 78294 | `b063ca7ae3af8ab46a3c06dc83e404eb2cd11dbdfc16afe9a4d33e346aea46a7` | `1188a8525622c6353e85a80dd6a5d1d8e60b3a07f48061f837af5040657e5ce6` |
| server_commit | 9 | 91016 | `6d08efc191f77a5a51d365de428a0f1ef1f59e8b96f5f27c15e755e4fef5e936` | `598bc849607ec23e4522981820dbd7329865925c27f2023a5f093f4751d78afb` |

样本账本的 transcript 是输入短句，不是 ASR 结果。样本可供之后内容识别；本批不另调服务。

## handler 离线验收及身份交接

`internal/gateway/ws_recorded_conformance_test.go` 加载并验证完整原 fixture，然后用显式
测试整门矩阵和 `buildWithWS(..., true)` 装配正式 Mux → WSHandler → provider → 本地
WSReplayUpstream。预期始终来自独立原录制，不经过被测 gateway 生成。

1. 从首两节点检测唯一 session.created 发送/接收对及同一个 session bind/reference，
   不硬编码合成 ready 节点。Dial 等下游 101 时，上游先送原首事件；下游先核对 opcode 和全部字节。
2. 仅在测试内深拷贝 tail，删除已验证 prelude、相关 after/coverage 引用。清除所有
   FieldRules，使用更严格的 `bytes` 匹配；所有 ID、字段和 JSON 空白均保留原录制字面值。
   因此首事件的 session 身份不能在后续 updated 中重新绑定成别的 ID。
3. Outcome.Terminal.Symbol 改为原始终态消息的 response.id，保留原 Node/IDPointer/State。
   三个 text response 与各自 created/delta/done/工具引用都锁到同一原始字面身份；
   `previous_item_id` 同样严格。测试负例确认改 session/response ID 会失败。
4. 仅重算内存 tail digest 并重新 Validate，归档文件及原 fixture 不变。
   ReplayWS 在真实连接上核验全部剩余节点、收发因果、转发字节和终态/close；随后等待
   handler 自然返回，再检查 shutdown、上游 Close/Err、全部 worker join 与共享 budget=0。
5. 独立字面计量：commit 输入/输出 token 8/31、音频输出 31、字符 25；auto 为 8/36、36、25；
   text 为 1165/49、3 笔权威 token 记录、无字符记录。两种单位不相加；不以 Inspect 算预期。

这是固定已录制轨迹的离线保全证据，不是实时 gateway 对云 smoke，也不证明任意调度或模型别名未来行为。
生产 Build 仍不开门，Phase1 仍为 PLANNED；未添加 routes、Redeem 或兑现白名单。
十五项逐项缺口和八次尝试见 [研究账本](../../../../docs/research/2026-10-04-dashscope-realtime-evidence.md)。
