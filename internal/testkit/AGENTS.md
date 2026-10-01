# internal/testkit

一致性测试的基座。存在的理由：(入站协议 × 出站 Provider × 能力) 的组合规模随每加
一个协议而相乘，人工写断言维护不住，打真实上游又慢又贵还不稳定。做法是**真实响应
录制一次、脱敏入库、此后全部离线回放**——CI 不需要任何 API Key。

## STRUCTURE

| 文件 | 内容 |
|---|---|
| `fixture.go` | `Fixture`/`Request`/`Response`/`SSEBody`、脱敏名单、回放 `Server`/`Handler` |
| `golden.go` | `Golden` / `GoldenJSON` / `AssertJSONEqual`，支持 `-update` |
| `sse.go` | SSE 帧读写辅助 |
| `ws_fixture.go` / `ws_validate.go` | WS 会话 schema、预算、四点因果与来源账本；不证明实际云端支持 |
| `ws_io.go` | 显式预算的 `ReadWSFixture` / `ReadWSFixtureDir`；结构与消息严格 JSON 校验 |
| `ws_handshake.go` | 握手脱敏、query 多重值与安全子协议匹配，不伪造协商选择 |
| `ws_match.go` | `WSMatcher` 的完整消息、实体绑定与引用；`ForwardedFrom` 只豁免显式标量改写 |
| `ws_schedule.go` | `WSSchedule` 的 after + 点内 FIFO 调度，发送完成不能代替接收证据 |
| `ws_upstream.go` | `WSReplayUpstream` 本地单连接握手端、首次错误槽与 socket 所有权 |
| `ws_replay.go` | `ReplayWS` 双端实际收发、业务终态与可观测关闭、取消后 join 工作者 |
| `ws_integration_test.go` | 合成 WS 全链路机制负例及所有旧 HTTP/SSE 路径的原 golden 回归 |

## TESTDATA LAYOUT

```
testdata/
├── fixtures/<provider>/*.json          # 按 provider 归档的上游交互录制
├── testkit/ws/{valid,invalid}/*.json     # synthetic-negative 机制样例，绝不是投放证据
└── routes/<in>__<out>/                 # 目录名由 degrade.FixtureDir() 决定
    ├── *.json                          # 路径级端到端用例；有损格子的举证以能力名命名
    └── golden/*.txt                    # 期望输出
```

## CONVENTIONS

- 每条 fixture 必须写 `Note`，说明它覆盖的是什么场景（如「工具参数跨分片切断」）。
  没有它，半年后没人知道这条奇怪的 fixture 为什么长这样。
- **HTTP/SSE 保持旧门槛**：异构路径（含协议兼容路径）的 fixture 必须携带 method/path/body 的 `upstream` 预期断言；一致性测试会校验该预期，防止 provider 转换逻辑发错端点或丢字段导致测试伪绿。
- **WS 单独分支**：`response.ws` 与 body/SSE/HTTP `upstream` 互斥；上下游握手为 GET，`request.path` 不带 query，握手无 body。独立上游握手预期在 `response.ws.upstream`，完整消息预期在四点 nodes，绝不能用 WS 无 body 例外放宽 HTTP/SSE 的 `upstream.body`。
- WS 必须用 `ReadWSFixture(path, limits)` / `ReadWSFixtureDir(path, limits)`，不能借 `Load` / `LoadDir` / HTTP `Handler` 绕过 WS 专用验证。默认文件 8 MiB、目录 64 MiB、消息 1 MiB、轨迹 4 MiB、节点 4096、after 8192、绑定 2048、规则 8192、JSON 64 层、回放 5 秒；注入预算全部为正。在 `Dial` / `Accept` 时就设置 `MaxPayload=limits.MessageBytes`，不能读入大消息后才校验。
- `SSEBody.Frames` 定义回放时的 **Write 边界**：一次 Write 塞三个事件、或把一个事件
  切成两次 Write，都能暴露缓冲逻辑的 bug。为空则逐事件写出。
- 回放服务器每帧后必须 flush，才能真实复现上游的分片节奏。
- 每条经 `Redeem()` 登记投放的路径都必须在 `testdata/routes/` 下有覆盖其全部
  `DEGRADE` 与 `EMULATE` 格子的用例——有损格子的文件名即能力名，这就是举证
  （ADR-0001）。`PASSTHROUGH` 格子没有这种一一对应：代码只查目录里有用例，
  「这项能力真的跑通了」由改 `TestRedeemedCapabilitiesAreExplicit` 白名单的人
  担保。所以兑现一项能力之前，自己去把它的 fixture 写出来跑通。

## WS 证据与所有权边界

- 来源 `synthetic-negative` 的 hello、工具 ID、二进制及同连接两 task 样例均手写，只证明加载、匹配、关联、因果、回放与门禁机制；非法样例用于拒绝测试。它们位于 `testdata/testkit/ws/`，不进 `routes/`，不改变 Phase1 矩阵、`Redeem` 或任何投放白名单。
- `recorded` 的来源角色、时间、版本、模型、上游契约摘要和样本 SHA-256 是**自洽账本**，不是来源认证。加载器能拒绝缺失、角色错配和摘要篡改，不能识别一个人手工伪造的完整相同哈希。真实录制、独立编写上游请求、内容脱敏与人工审核仍属于后续录制计划；101/200、来源标签或 Coverage 不能证明参数采纳、同契约或云互通。
- `WSSchedule` 的相同 seed 只固定**相同就绪集合/反馈历史下的纯选择策略**；完整 `SendOrder` 还依赖读与 write-result 反馈，不承诺完整网络序列或网关内部执行顺序确定。链式样例可固定字面发令预期，不能把该性质推广到所有并发图。
- `WSReplayUpstream` 只拥有单次握手与 socket 兜底释放，不运行消息 driver；`Connection` 只转交一次使用权，httptest 不负责劫持连接。整合 owner 必须在驱动返回后调用 `Close()` 并检查其结果与 `Err()`，迟到重复/错配握手不能被 driver Done 掩盖。最终检查前调用方应停止并 join 自己的请求生产者，不能声称已检查未来尚未到达的请求。
- `ReplayWS` 只有初始化成功才接管两个 socket，返回前关闭并 join 自己的固定工作者；初始化失败仍由调用方释放端点。completed 必须同时有真实客户端业务终态与对侧 close 接收；failed/interrupted 不得混成 completed，raw TCP EOF 不能当有效 close。
- 传输层的最小 `CloseWithResult(code, reason) (sent, err)` 只证明**本次关闭帧完整写入成功**，不证明对端收到。幂等 nil、被动自动关闭或写锁占用均不能补造发送，仍须独立实际对端接收证据；不增加 Abort 或状态预检查来绕过生命周期竞争。
- 全部机制测试使用本地 TCP/httptest，不用 `net.Pipe`、真实凭据或 smoke。本阶段没有 P2 录制器、P3 生产 WS 接线、音频宽松匹配或能力投放，不因此宣告 Phase1 已完成。

## ANTI-PATTERNS

- **不要**放宽 `secretHeaders` 脱敏名单——录制脚本打真实上游，凭据必然出现在请求头
  里，这份名单是 fixture 不泄密的唯一保障。宁可多列不可漏列。
- **不要**让回放服务器对未录制的请求返回 404；必须 `onMiss` 让测试失败，
  否则「fixture 没命中」会伪装成「上游返回了 404」。
- **不要**无脑跑 `make golden-update` 就提交；重写后必须人工审阅 diff。
