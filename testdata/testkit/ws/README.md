# WS 合成机制样例

这些文件全部为人工设计的 `synthetic-negative`，**不是上游能力证据**，从未调用云端或真实凭据。`valid/` 只表示 schema 自洽、能在本地 TCP 桥上取得消息与关闭证据，不表示真实协议已实现；`invalid/` 是必须拒绝的字面轨迹。二者分目录，避免目录 reader 因读到负例而掩盖正例遗漏。

| 文件 | 手工固定的预期 / 防止的假绿 |
|---|---|
| `valid/hello.json` | hello 四点、`hello-1/done` 业务接收终态、客户端 1000 → 上游接收 1000 |
| `valid/tool-id.json` | 绑定 `tool/call` 到 `call-7`，后续 tool.result 必须引用同一 ID；转发字节不可擅改 |
| `valid/binary.json` | 双向 opcode=2、字节 `00 ff 10`（base64 `AP8Q`）；样本与账本摘要必须一致 |
| `valid/two-tasks.json` | 一对连接内 task-a/task-b 独立绑定、各自 done 接收终态，最后才关闭 |
| `invalid/bad-source.json` | synthetic-negative 中伪贴 recorded 来源，必须拒绝 |
| `invalid/bad-reference.json` | reference 没有在先绑定，必须拒绝 |
| `invalid/bad-sample-hash.json` | 样本摘要与字节不符，必须拒绝 |
| `invalid/cyclic.json` | 两 close 的 after 成环，必须拒绝 |

JSON 消息原文、节点、因果、预期实体与终态先手写固定；base64 只是编码这些已指定字节，不从 ReplayWS 输出生成答案。`00 ff 10` 的 SHA-256 独立用 Python 标准库计算并固定为 `2da45f2cd1f9c8e69a67abf7a6b26c282533d0a7686787a9533265418680d4d2`。它不是音频，不能据此证明 DSP、采样率或重采样保真。

`internal/testkit/ws_integration_test.go` 用显式预算的 reader 加载并实际回放，另在桥侧注入错工具 ID、未声明的键序/空白改写与二进制字节变化。期望发令次序和终态独立写成字面量；此处链式 after 每次只让一个发送节点就绪，不能推广为任意相同 seed 必有相同完整网络序列。

`internal/degrade/ws_fixture_gate_test.go` 仅把正例复制到临时目录与本地 Route 验证旧 `request.path` 门禁；不会把这些文件放进真实 `testdata/routes/`，不会 Redeem 产品能力。路径门禁不校验真实来源，摘要也不能识别手工伪造的相同哈希；真实记录及人工来源审核仍由后续录制计划负责。
