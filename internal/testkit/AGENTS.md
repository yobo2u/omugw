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

## TESTDATA LAYOUT

```
testdata/
├── fixtures/<provider>/*.json          # 按 provider 归档的上游交互录制
└── routes/<in>__<out>/                 # 目录名由 degrade.FixtureDir() 决定
    ├── *.json                          # 路径级端到端用例；有损格子的举证以能力名命名
    └── golden/*.txt                    # 期望输出
```

## CONVENTIONS

- 每条 fixture 必须写 `Note`，说明它覆盖的是什么场景（如「工具参数跨分片切断」）。
  没有它，半年后没人知道这条奇怪的 fixture 为什么长这样。
- `SSEBody.Frames` 定义回放时的 **Write 边界**：一次 Write 塞三个事件、或把一个事件
  切成两次 Write，都能暴露缓冲逻辑的 bug。为空则逐事件写出。
- 回放服务器每帧后必须 flush，才能真实复现上游的分片节奏。
- 每条经 `Redeem()` 登记投放的路径都必须在 `testdata/routes/` 下有覆盖其全部
  `DEGRADE` 与 `EMULATE` 格子的用例——有损格子的文件名即能力名，这就是举证
  （ADR-0001）。兑现了哪项能力，哪项能力就得有跑得通的 fixture 作证；
  fixture 是投放证据，不是装饰品。

## ANTI-PATTERNS

- **不要**放宽 `secretHeaders` 脱敏名单——录制脚本打真实上游，凭据必然出现在请求头
  里，这份名单是 fixture 不泄密的唯一保障。宁可多列不可漏列。
- **不要**让回放服务器对未录制的请求返回 404；必须 `onMiss` 让测试失败，
  否则「fixture 没命中」会伪装成「上游返回了 404」。
- **不要**无脑跑 `make golden-update` 就提交；重写后必须人工审阅 diff。
