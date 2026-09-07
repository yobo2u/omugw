# WebSocket 传输层设计

**日期**：2026-09-06
**状态**：待确认
**范围**：为 omugw 建立 WebSocket 传输层，并投放第一条 realtime 路径。

## 1. 问题

`internal/transport/` 下只有 `httpx` 与 `sse`。降级矩阵里 5 条路径全部卡在这个缺口上：

| 路径 | 设计分 |
|---|---|
| `openai.realtime → openai.realtime` | 1.000 |
| `openai.realtime → dashscope.ws.realtime` | 0.917 |
| `dashscope.realtime → dashscope.ws.realtime` | 1.000 |
| `dashscope.realtime → openai.realtime` | 0.733 |
| `dashscope.inference → dashscope.ws.inference` | 1.000 |

其中 `openai.realtime → dashscope.ws.realtime` 是 README 列的**两大差异化特性之一**
（「OpenAI Realtime 客户端零改动驱动 Qwen-Omni-Realtime」），目前完全没有落地。

矩阵层的声明**已经齐备**——处置、可表达性、同源标记、重采样降级都写好了。
缺的只是传输层实现。

## 2. 实测证据（本设计的事实基础）

文档不足以定案，因此对真实服务做了探针。以下是实测结果，不是文档转述。

### 2.1 决定性发现：`session.updated` 不等于配置被采纳

| 探针 | 模型 | 发送 | 服务端回显 | 判读 |
|---|---|---|---|---|
| 1 | `qwen3.5-omni-flash-realtime` | 新式 `audio.input.format.sample_rate=24000` | `{"input":{"format":{"sample_rate":24000,"type":"pcm"}}}` | **真正接受 24kHz** |
| A | `qwen3-omni-flash-realtime` | 同上 | `input_audio_format: "pcm16"` | **静默降级为 legacy** |
| C | `qwen3.5-omni-flash-realtime` | 旧式 `input_audio_format: "pcm16"` | `input_audio_format: "pcm16"` | legacy 仍受支持 |

探针 A 是整个调研里最重要的一条：**中间代模型收到新式字段后照样回 `session.updated`，
但回显的是 legacy 字段**。也就是说「服务端没报错」根本不能证明它按 24kHz 收音。
唯一可靠的判据是**检查回显结构**：

- 回显里有 `audio.input.format.sample_rate` → 新式生效，按该值直通
- 回显里只有 `input_audio_format` → 降级为 legacy，必须按 16kHz 重采样

这条陷阱在三份官方文档里都没有提及。若不实测而直接信任 `session.updated`，
网关会把 24kHz 的音频喂给一个按 16kHz 解析的上游——听感上是变速的乱码，
而且**不会报任何错**。

### 2.2 采样率约束

- **OpenAI Realtime**：`pcm16` 固定 24kHz（另支持 g711 8kHz）
- **DashScope 新式**（仅 `qwen3.5-omni-*-realtime`）：8000/16000/24000/48000 四档可配
- **DashScope 旧式**：输入只认 16kHz，输出 24kHz

因此重采样**不是路径级恒定必要**，而是**模型级条件性**的。

### 2.3 其他实测事实

- 自实现的 WebSocket 客户端**已打通真实服务**：三次探针全部握手成功（101），
  `Sec-WebSocket-Accept` 校验通过，掩码帧被正确接收。零依赖方案得证。
- 上游会因容量限制主动关连接：close code **1011**，
  reason `To many requests. Your requests are being throttled...`。
  这不是 4xx，网关必须把 1011 也映射成可重试的上游错误。

## 3. 设计

### 3.1 包结构

```
internal/transport/ws/
├── frame.go       # RFC 6455 帧编解码：纯函数，可穷举测试
├── frame_test.go  # 三种长度分支 / 掩码 / 分片 / 非法帧
├── conn.go        # Conn：帧收发 + ping/pong + close 握手 + 空闲超时
├── accept.go      # 服务端：http.Hijacker 接管 + 101 响应
└── dial.go        # 客户端：拨号 + 握手 + Accept 校验
```

只用标准库：`crypto/sha1`、`crypto/rand`、`encoding/base64`、`encoding/binary`、
`net/http`、`crypto/tls`。**不新增任何直接依赖**（原型已验证可行）。

### 3.2 为什么帧编解码要独立成纯函数

RFC 6455 的坑集中在三处：三种负载长度分支（<126 / 16 位 / 64 位）、
客户端强制掩码、分片续帧。这些都是**输入到输出的纯映射**，
适合用穷举表驱动测试锁死，与网络无关。把它们和连接生命周期混在一起，
就只能靠起真实连接来测，慢且不稳。

### 3.3 连接生命周期归 `Conn`

`Conn` 负责 ping/pong 保活、读写超时、close 握手。这部分无法纯函数化，
用 `net.Pipe()` 做离线测试。

**空闲超时沿用 `httpx` 的语义**：两帧之间超过 `idle` 判定上游挂死。
realtime 场景下这个判据比总超时有意义得多——一个 30 分钟的语音会话是正常的，
而 60 秒没有任何帧就是异常。

### 3.4 首字节规则在 WebSocket 下的对应

原则 2.4 说「下游首字节之后不得重试」。WebSocket 的对应是：

**向下游发出 101 之后，不得再 failover。**

101 一旦发出，客户端就认为握手成功并开始发帧。此时换上游意味着要么丢掉
客户端已发的帧，要么在新连接上重放——两者都会让客户端看到不一致的会话状态。

因此 failover 只在**上游握手完成之前**有效：网关先连上游，成功后才向下游回 101。

### 3.5 采样率协商（本设计的核心机制）

按 §2.1 的证据，网关不能盲信 `session.updated`。流程：

```
1. 客户端发 session.update（OpenAI 线格式，隐含 24kHz）
2. 网关改写成 DashScope 新式字段，声明 sample_rate: 24000
3. 上游回 session.updated → 网关检查回显结构：
   ├─ 有 audio.input.format.sample_rate == 24000
   │    → 协商成功，音频帧字节级透传，零重采样
   └─ 只有 input_audio_format（legacy 回显）
        → 协商失败，启用 24k→16k 重采样
4. 网关把回显改写回 OpenAI 线格式转给客户端
```

**重采样必须先低通再抽取。** 直接每 3 取 2 会产生混叠失真，
那比丢高频严重得多——高频丢失只是听感变闷，混叠是引入原本不存在的假频率。

### 3.6 矩阵改动：两个方案，需要你定

现状是 `MarkHomogeneous()` + `Degrade(CapAudioInput)`。表面看矛盾——
同源快通道的定义是字节级透传不进 Canonical，而重采样必须解码重编码。

**但自检发现，撤掉 `MarkHomogeneous()` 的代价比预想的大**：

- `docs/architecture/principles.md` 原则 2.2 把这条路径**点名写进了快通道覆盖范围**
  （「以及 **`OpenAI Realtime → DashScope Realtime`**（两者事件模型基本一致，
  绝大多数事件原样转发）」）。
- 该原则的强制机制表就指向 `TestRealtimeFastPathIsHomogeneous`。
- `IsHomogeneous()` 在选路时**永远优先于**全局 `OutboundPreference`。撤掉它会
  改变选路行为，不只是改一个标记。

也就是说这不是「改个测试」，而是**动一条写进原则文档的架构结论**。

#### 方案 A：保留 `MarkHomogeneous()`（推荐）

论据是实测的：**在 qwen3.5-omni 系列上，这条路径确实是字节级透传**——
协商成功后音频帧原样转发，零解码零重编码。原则 2.2 说的「绝大多数事件原样
转发」在新代模型上完全成立。

重采样只在**旧模型协商失败时**才发生，那是降级分支，不是主路径。
`Degrade(CapAudioInput)` 已经把这个分支如实登记了——矩阵的处置本就是
路径级的最坏情况声明。

按此方案，改动只有一处：**改写降级说明**，把实测发现写进 `Note`
（现在只提「高频信息丢失」，没提真正的风险是静默降级与混叠）。
`TestRealtimeFastPathIsHomogeneous` 断言 `Note` 含「16 kHz」，改写时需保留该字样。

#### 方案 B：撤掉 `MarkHomogeneous()`，归类为 wire-compatible

论据是严格性：只要存在需要重编码的分支，就不该叫同源。
与 `openai.chat → dashscope.compatible` 同一类。

代价：改 `principles.md` 原则 2.2 的覆盖范围表述、改 `TestRealtimeFastPathIsHomogeneous`、
改选路行为（失去快通道优先级）、README 相应表述也要跟。

#### 我的建议

选 **A**。实测证据支持「主路径确实是字节透传」，而原则 2.2 的表述
（「绝大多数事件原样转发」）本就为少数例外留了余地。为一个只在旧模型上
触发的降级分支推翻一条架构原则，代价与收益不成比例。

**已知的不精确**（无论选哪个方案都存在）：矩阵不支持「按模型条件性处置」，
所以 qwen3.5 上会多报一个降级头。这是保守方向的误差（声明降级、实际无损），
不违反 fail-closed。要精确表达需要给矩阵加条件处置能力，超出本次范围。

## 4. 范围

WebSocket 传输层 + **一条**路径：`openai.realtime → dashscope.ws.realtime`。

理由：这条是 README 的招牌特性，且是四条 realtime 路径里唯一需要跨协议改写的
（其余三条要么同族、要么是完全不同的 run-task 指令流）。传输层建好之后，
其余路径是增量工作。

**不在本次范围**：
- 另外 3 条 realtime 路径与 1 条 inference 路径
- WebRTC（Phase 1 明确排除）
- `dashscope.inference` 的 run-task 指令流（协议完全不同，值得单独一轮）

## 5. 测试

| 层 | 内容 | 是否需要凭据 |
|---|---|---|
| `frame.go` | 三种长度分支、掩码往返、分片、非法帧拒绝 | 否 |
| `conn.go` | ping/pong、close 握手、空闲超时（`net.Pipe`） | 否 |
| `accept/dial` | 握手成功、Accept 校验失败、非 WS 请求拒绝 | 否 |
| 网关层 | 101 前 failover、101 后不 failover、协商成功/失败两条分支 | 否（假上游） |
| smoke | 真实 DashScope 会话协商 | 是 |

离线部分全部不需要凭据，与仓库现有约定一致。

## 6. 决策与落地状态

### 已决定并落地

1. **范围**：本轮只做传输层。`internal/transport/ws` 已交付
   （`frame` / `conn` / `handshake` 三层，42 条离线测试 + 2 条真实 smoke）。
2. **矩阵**：取**方案 A**——保留 `MarkHomogeneous()`，只改写降级说明。
   理由见 §3.6：实测证明主路径（qwen3.5）确实是字节透传，而撤掉标记要动
   `principles.md` 原则 2.2 与选路行为，代价与收益不成比例。
   已落地于 `internal/degrade/rules_phase1.go`，`docs/degradation-matrix.md`
   由 `make matrix-update` 同步。

### 尚未开工

3. **重采样**：24k→16k（含低通防混叠）尚未实现。它属于协议层而非传输层，
   与网关接线一起做更合适——单独实现一个没有调用方的重采样器，无法验证
   它在真实事件流里的位置是否正确。
4. **网关接线**：`openai.realtime → dashscope.ws.realtime` 这条路径的
   handler 注册、协商检测（§3.5）与 failover 边界（§3.4）都还没写。
   矩阵仍是 `PLANNED`，打过去返回 501。

即：**传输层已通，路径未通。** 下一轮的工作是把 §3.5 的协商流程与 §3.4 的
101 前 failover 规则实现出来，并按 ADR-0001 用 fixture 兑现能力。
