# OpenAI Realtime 官方 Node SDK 离线验收

要求 Node **>=22**（CI 固定 Node 24）、npm 与 Go 1.25。在仓库根运行：

```sh
npm --prefix tests/sdk/openai-realtime ci --ignore-scripts
make test-sdk
go test -race -tags=sdk ./internal/gateway -run '^TestOpenAIRealtimeNodeSDK' -count=1
```

`test-sdk` 不自动安装依赖。Node、SDK 或客户端缺失直接失败；普通 `go test ./...`
与 `make check` 不启用 `sdk` 标签，不要求 Node，也不安装 npm 包。

## 链路与证据

```text
openai/realtime/ws 的 OpenAIRealtimeWS
  → wss://localhost:<随机端口>/v1/realtime?model=real-model
  → 临时 CA 签发证书的 HTTP/1.1 TLS 反向代理
  → buildWithWS 的正式 Mux / GA handler / Provider / usage observer
  → 独立本地合成上游
```

SDK 接受的 `baseURL` 为 `https://localhost:<端口>/v1`，由 SDK 自行构造 wss 地址。
CA 只通过本次连接 `options.ca` 传入，`rejectUnauthorized=true`；检查底层 TLS
socket 的 `authorized`。另一独立 CA 的负例必须产生明确证书信任错误，且代理
HTTP handler、网关与上游均未收到请求。证书及私钥仅在内存中生成，使用 localhost
SAN 与真实链校验。子进程环境不继承真实凭据、代理或 Node TLS 绕过配置。

- 假入站凭据由正式凭据池替换；Safety-Identifier 来自 SDK `options.headers`；
  Organization/Project 在 TLS 入口可见，但在上游必须消失。验证固定 UA、路径、
  model、额外头清洗及不协商扩展/子协议。
- 复用 `testdata/testkit/ws/openai-ga/roundtrip.json` 的独立合成事件，共 **10 条
  SDK 请求、19 条上游事件**。Go 只规整客户端 fixture 的 JSON 空白，以匹配
  SDK 自身 `send()` 的 `JSON.stringify`；上游收到的每条请求都逐字节核验。
  `socket.prependListener('message')` 在 SDK 解析前核验全部上游原字节和文本类型，
  SDK `event/error` 分发另行核验，绝不从网关结果生成期望。
- 在 socket `open` 回调中、尚未收到首条 `session.created` 时立即 SDK
  `session.update`，验证提前 pipeline。覆盖 GA 文本、音频、转写 token/seconds、
  工具、图像、reasoning、未知字段、错误后继续、cancel、truncate 与 close。
- 客户端 `sdk-close` 原因必须抵达上游；SDK 收到的被动 close ACK 按 transport
  契约为 `1000` 空原因，两段关闭分别核验。
- 业务沉默时正常自动 pong，至少接收 6 次心跳、跨过 HTTP total 后正常关闭；
  关闭 autoPong 时验证有限清理。后者允许既有 HTTP 请求取消与 relay 空闲错误
  仲裁产生的 `1001/空原因` 或 `1011/downstream connection failed`；有限关闭与心跳
  写锁竞争时，transport 直接中断 TCP，可见 `1006/空原因`（上游可见 EOF）。只在
  这个主动停回 pong 的负例接受异常断连，且要求连接至少存活 700ms、5 秒内退出，
  防止连接刚建立就断开冒充空闲清理；正常 close 仍严格核验码与原因。
  两种模式的未结 response 都只记 unavailable，不以心跳补造终态或零用量。
- 子进程自然退出并 `Wait`，上游、反代与 gateway handler 全部 join 后检查注册表
  为空及共享预算为 0；Node 10 秒 watchdog 与 Go 15 秒子进程期限兜底失败清理。

本验收使用测试矩阵显式开门，生产 `Build` 仍传空门列表。测试证明 SDK/TLS
**离线集成**，不是 OpenAI 云端成功实录、逐能力兑现或生产代理部署验收。

## 锁包与许可证（2026-10-04）

实际执行 `npm ci --ignore-scripts`，逐项检查 lockfile、安装后 package.json 与
LICENSE 正文；`npm ls --all` 核对安装树。锁文件含两个外部包，无额外传递安装包：

| 包 | 固定版本 | 许可证 | 本地证据 |
|---|---|---|---|
| openai | 7.27.0 | Apache-2.0 | `node_modules/openai/{package.json,LICENSE}` |
| ws | 8.21.0 | MIT | `node_modules/ws/{package.json,LICENSE}` |

lockfile v3 的 `resolved` 均为公开 `registry.npmjs.org`，并锁定 SHA-512 integrity。
OpenAI 的 AWS/Smithy、undici、zod，以及 ws 的 bufferutil/utf-8-validate 均为未安装的
optional peers；OpenAI 的 ws peer 由这里的固定直接依赖满足。锁文件内无
forbidden/restricted/unknown 许可证。升级时须重新核查**整个安装树**，不能沿用此结论。
这些 npm 包仅在此测试目录使用，不进入 Go 模块或生产二进制。

契约来源：

- [受版本控制的 GA 契约与官方来源](../../../docs/research/2026-10-04-openai-realtime-ga-contract.md)
- [A 包设计 §4](../../../docs/superpowers/specs/2026-10-02-same-contract-websocket-passthrough-design.md#4-鉴权和客户端兼容)
- 官方 SDK 固定提交 `11b9283f2a22737e273ccc1593d01af5cf584a0b` 的
  [ws.ts](https://github.com/openai/openai-node/blob/11b9283f2a22737e273ccc1593d01af5cf584a0b/src/realtime/ws.ts)
  与 [internal-base.ts](https://github.com/openai/openai-node/blob/11b9283f2a22737e273ccc1593d01af5cf584a0b/src/realtime/internal-base.ts)。
