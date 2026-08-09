# omugw — Universal AI Gateway

多协议双向转换的 AI 网关数据面。不把 OpenAI 格式当作内部总线，而是在
**入站协议**与**出站 Provider** 之间建立可控的转换层。

> **状态：Phase 1 开发中（M0）。尚不可用于生产。**

## 这个项目和别人不一样的地方

只有两点，其余能力「够用即可，不追求超越」：

1. **DashScope Native 作为一级协议**。rerank、视频生成、多模态 embedding、
   `run-task` 语音流——这些在 OpenAI 兼容层里根本表达不出来。
2. **协议双向转换 + 凭据池的组合**。OpenAI Realtime 客户端可零改动驱动
   Qwen-Omni-Realtime；订阅凭据池作为可插拔模块接入。

## 核心设计原则

完整版见 [docs/architecture/principles.md](docs/architecture/principles.md)。三条最重要的：

- **转换是有损的，损失必须显式。** 维护代码化的[降级矩阵](docs/degradation-matrix.md)，
  未注册的 `(入站协议, 出站 Provider, 能力)` 组合一律显式报错，绝不静默丢字段。
- **同源走快通道。** 入站协议族 == 出站 Provider 族时字节级透传，只改写鉴权，
  不进 Canonical——保住 TTFT，绕开绝大多数转换 bug。
- **流式 failover 只在首字节之前有效。** 首字节发出后上游失败一律不重试
  （会重复内容），发送协议对应的终止错误事件收尾，并将 usage 标记为不可用。

## 仓库边界

| 仓库 | 职责 | 状态 |
|---|---|---|
| **omugw** | 协议网关（数据面），Apache-2.0 Clean Core | 本仓库 |
| omsub | 订阅账号池，独立进程，独立承担 ToS 风险 | 未开始 |
| omapi | 控制面、计费、Admin UI | 未开始 |

omugw 的 core **不包含**任何 OAuth 刷新、账号封禁冷却、客户端指纹伪装逻辑。
这些全部在 omsub，通过标准 Provider 接口接入。

## Phase 1 范围

### ✅ 在范围内

| 类别 | 内容 |
|---|---|
| 入站协议 | OpenAI Chat Completions、OpenAI Responses（**无状态**）、OpenAI Realtime |
| 出站 Provider | OpenAI 兼容、Anthropic Messages、DashScope Compatible、**DashScope Native（全量）** |
| DashScope Native | generation / multimodal / embedding / rerank / image / video / speech / realtime |
| 传输 | HTTP、SSE、WebSocket（`/api-ws/v1/inference` 与 `/api-ws/v1/realtime` 两类）、异步 Job |
| 多模态 | DashScope（优先级 1，全量）、OpenAI（优先级 2：image / audio / realtime） |
| 治理 | API Key 鉴权、内存态凭据池、首字节前 failover、冷却、结构化审计 |

### ❌ 不在 Phase 1

| 排除项 | 去向 |
|---|---|
| Anthropic Messages **入站** | Phase 2 文本协议轴第一位 |
| Gemini 文本协议 | Phase 2 |
| Gemini 多模态 + Live API | Phase 2 多模态轴第一位（优先级 3） |
| Anthropic 多模态 | 优先级 4，最后（无 realtime API，仅 vision 输入） |
| WebRTC 传输 | 只做 WebSocket |
| Responses 有状态（`store=true`） | 需自建 conversation store 子系统 |
| 控制面 / Admin UI / 计费 | 归 omapi |
| 订阅账号池实现 | 归 omsub |
| 多副本共享状态（Redis） | Phase 1 内存态凭据池，单实例正确 |
| 跨 Provider 多模态资源搬运 | 显式 REJECT，不做自动搬运 |

多模态 / 实时能力的 Provider 优先级：**DashScope > OpenAI > Gemini > Anthropic**。
注意这条轴与文本协议轴独立——Anthropic 在多模态轴排最后，但 Anthropic Messages
入站在文本轴上是 Phase 2 第一优先。

## 开发

```bash
make test       # 单元测试 + fixture 矩阵（离线，无需 API Key）
make matrix     # 降级矩阵完整性断言
make licenses   # 依赖许可证扫描 + SBOM 生成
make smoke      # 端到端冒烟（需真实 API Key，CI 默认跳过）
```

## License

Apache-2.0。见 [LICENSE](LICENSE)、[NOTICE](NOTICE)、
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
