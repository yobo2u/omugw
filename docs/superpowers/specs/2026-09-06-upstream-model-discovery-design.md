# 上游模型清单发现（Upstream Model Discovery）设计

**日期**：2026-09-06
**状态**：已确认，待实施
**范围**：为 omugw 增加「上游提供商模型清单」的自动发现能力，并扩充
`config.example.yaml` 的提供商与模型示例。

## 1. 问题

仓库里没有任何硬编码的模型目录。运行时能调用哪些模型，完全由部署者的
`config.yaml` 的 `models[]` 决定；`Router.Models()` 只把**精确匹配**规则的
名字列出来（前缀与 `*` 匹配的是无穷集合，列不出来）。

于是「更新上游提供商的模型清单」这件事今天只有一条路：人去翻上游文档，手抄
进 YAML。上游发新模型时，网关既不知道，也没有任何地方能问。

同时还有一个更小但更硬的缺口：**网关根本没有 `GET /v1/models` 这扇门**。
`Router.Resolve` 的错误消息刻意不列模型，并告诉调用方「想知道有什么模型，走
`/v1/models` 那条受鉴权保护的路」——而那条路当前会落到框架 404。

## 2. 目标与非目标

### 目标

1. 网关提供受鉴权的 `GET /v1/models`，返回 OpenAI list 信封。
2. 该清单在**显式配置的模型**之外，可以补充**上游发现到的模型**。
3. 发现按 Provider 协议族走各自的**官方**枚举接口，无官方接口的协议族显式跳过。
4. `config.example.yaml` 展示三类已实现 Provider 与代表性模型。

### 非目标（明确不做）

1. **发现结果不创建路由。** 发现到的模型不可被请求。请求侧仍然只认
   `models[]`。这是本设计最重要的边界，理由见 §4.1。
2. 不做「写回 YAML」。配置文件是人的，网关不改它。
3. 不抓取上游的静态文档页。只调官方 API。
4. 不做 DashScope 的 `permissions` / `quotas` 控制面对接（可后续单独提）。

## 3. 上游契约（已核实，见 `docs/research/`）

| Provider 协议族 | 官方枚举接口 | 分页 | 结论 |
|---|---|---|---|
| `openai.compat` | `GET {base_url}/v1/models` | 无分页参数，一次返回全量 | 支持发现 |
| `dashscope.native` | `GET {base_url}/api/v1/models` | `page_no` / `page_size`，默认 20 | 支持发现，必须翻页 |
| `dashscope.compatible` | 无 | — | **显式跳过**，不探测 |

依据：

- `docs/research/openai-list-models-contract.md`
- `docs/research/dashscope-native-model-discovery.md`
- `docs/research/dashscope-compatible-get-v1-models.md`

三条关键事实直接决定实现形状：

1. OpenAI 的 `data[]` 项必有 `id` / `object` / `created` / `owned_by`，
   可选 `shutdown_date`；**没有分页字段**，不要发 `limit` / `after`。
2. DashScope Native 是**自有信封**：`{code, message, success, output:{total,
   page_no, page_size, models[]}, request_id}`。模型 ID 在
   `output.models[].model`，**没有** `owned_by` / `created` / `shutdown_date`。
3. DashScope Compatible 的 `GET /compatible-mode/v1/models` 在官方文档中不存在。
   即便线上探测「碰巧能通」，也不得作为契约实现——所以这一族一律跳过。

## 4. 设计

### 4.1 发现只充实清单，绝不影响选路（核心约束）

发现结果**只**进入 `GET /v1/models` 的响应体。`Router` 完全不知道它的存在。

防的是什么：

- 上游目录 ≠ 本网关可调用集合。DashScope 官方文档明说
  `GET /api/v1/models` 返回的是「平台上可用的模型」，不是「本 workspace 有权
  调用的模型」；OpenAI 的 list 也不带「这把 key 能否调用」的位。
- 若发现即建路由，一次上游目录变更就能让网关无声地开始把请求发去一个
  没人配置过、没有降级矩阵审视过的目的地。那正是本仓库整套矩阵设计要防的
  「不可见的行为漂移」。
- 因此：发现失败的最坏后果是「清单少了几行」，而不是「请求去错了地方」。

### 4.2 数据流

```
启动
 └─ Build(cfg, ...)
     ├─ 装配 pools / providers / router（现状不变）
     ├─ 若 discovery.enabled：
     │    ├─ 为每个 provider 按 kind 选探测器（compatible → 跳过）
     │    ├─ 起 Registry：启动时同步拉一轮，随后按 refresh_interval 后台刷新
     │    └─ 失败：保留上一次成功快照；无快照则该 endpoint 为空
     └─ mux.Handle("GET /v1/models", modelsHandler)

请求 GET /v1/models
 └─ Authenticator.Authenticate（与主链路同一把网关 Key）
     └─ 合并：Router.Models()（配置精确名）+ Registry.Snapshot()（发现名）
         └─ 去重（配置优先）→ 按 id 字典序 → OpenAI list 信封
```

### 4.3 配置

新增顶层 `discovery` 块，**默认关闭**：

```yaml
discovery:
  enabled: false
  refresh_interval: 30m
  timeout: 10s
```

- `enabled` 默认 `false`：发现会拿真实凭据打上游，默认开启等于替部署者做了
  一个会花钱、会产生外部流量的决定。
- `refresh_interval` 与 `timeout` 仅在 `enabled: true` 时校验（必须为正，且
  `timeout < refresh_interval`——否则一轮还没超时下一轮就开始了）。
- `discovery` 不参与 `auth/credentials/providers/models` 那条「要么全配要么
  全不配」的四件套判定：它是可选增强，不是网关工作的必要条件。

### 4.4 新包 `internal/discovery`

职责边界：**只负责「从上游取回一组模型 ID」**。不碰路由、不碰矩阵、不碰
凭据冷却语义。

```go
// Model 是发现到的一个上游模型。字段取三家的交集 + 来源标注。
type Model struct {
    ID       string // 上游模型 ID（OpenAI: data[].id；Native: output.models[].model）
    Endpoint string // 来自哪个配置的 provider endpoint
    OwnedBy  string // OpenAI 有；Native 用 provider 字段兜底，可能为空
}

// Prober 是一个协议族的枚举实现。
// client 随调用传入：探测必须走网关统一的 httpx.Client，否则拿不到独立的
// connect 一层，一个建连挂住的上游要拖满整个发现超时才失败。
type Prober interface {
    Probe(ctx context.Context, client *httpx.Client, baseURL, secret string) ([]Model, error)
}

// Registry 持有快照，供 /v1/models 读取。
type Registry struct{ /* mu + map[endpoint][]Model + 后台刷新 */ }
```

实现要点：

- **凭据**：向 `credential.Pool` 借一次 Lease。成功 `Succeed()`，失败
  `Fail(err)`——直接复用既有的按错误类型冷却，不另造一套。
- **HTTP**：复用 `httpx.Client`（四层超时）。发现的 `timeout` 通过
  `context.WithTimeout` 叠加，因为目录 GET 不该按生成请求的尺度等下去
  （OpenAI SDK 默认 10 分钟是给生成用的）。
- **错误信封**：OpenAI 走 `openaiwire.DecodeError`，DashScope Native 走
  `dashscopewire.DecodeError`。不新写分类逻辑。
- **分页**：Native 循环 `page_no`，直到累计条数 ≥ `output.total` 或本页为空。
  设一个硬上限（页数上限）防上游 `total` 撒谎导致死循环。
- **`dashscope.compatible`**：探测器查表查不到 → 跳过并记一条 info 日志，
  **不是错误**。理由写进注释：官方无此接口。
- **失败保留旧快照**：刷新失败只记 warn，不清空。清空会让一次上游抖动直接
  表现为「客户端看到的模型清单突然少了一半」。

### 4.5 `GET /v1/models` 处理器

- 复用 `Authenticator`：未鉴权返回 OpenAI 错误信封的 401，与主链路一致。
- 输出：
  ```json
  {"object":"list","data":[{"id":"...","object":"model","created":0,"owned_by":"..."}]}
  ```
  `created` 对发现不到时间的来源填 0（Native 无此字段），不编造时间戳。
- 去重键是 `id`；配置项优先于发现项（配置是权威，发现是补充）。
- 排序按 `id` 字典序，保证响应稳定可 diff。
- **仅在 `len(cfg.Models) > 0` 时注册**：只提供 `/healthz` 的合法形态不应
  凭空多出一扇需要鉴权的门。

### 4.6 `config.example.yaml`

补三类 Provider（`openai.compat` / `dashscope.compatible` / `dashscope.native`）
与代表性模型示例，并加 `discovery` 段。

约束：示例文件中新增的凭据引用若写成 `${DASHSCOPE_API_KEY}`，会让只设了
`OPENAI_API_KEY` 的人**启动失败**（`config.Load` 对未定义变量是硬失败，这是
刻意设计）。因此新增的 DashScope 两段以**注释形态**给出，默认可直接启动的
最小闭环仍是 OpenAI 那一组。

## 5. 错误处理

| 场景 | 行为 |
|---|---|
| `discovery.enabled: false` | 不起后台任务；`/v1/models` 只返回配置模型 |
| 启动首轮探测失败 | 记 warn，网关照常启动（发现是增强，不是启动条件） |
| 刷新失败 | 记 warn，保留上一次成功快照 |
| 某 endpoint 401/403 | 记 warn 并 `Lease.Fail`（走既有冷却），其余 endpoint 不受影响 |
| `dashscope.compatible` | 记 info「无官方枚举接口，跳过」，非错误 |
| 未鉴权访问 `/v1/models` | OpenAI 信封 401 |

日志中**绝不出现** `credential.Secret`。

## 6. 测试

| 层 | 内容 |
|---|---|
| `internal/config` | `discovery` 默认关闭；启用时 `timeout < refresh_interval` 校验；非正数拒绝 |
| `internal/discovery` | `httptest` 造 OpenAI 与 Native 两种上游；Native 多页翻页；上游 500 时保留旧快照；compatible 被跳过 |
| `internal/gateway` | `/v1/models` 鉴权失败 401；成功返回合并去重且有序的 list；仅健康检查形态下该门不存在 |
| 手动 QA | 起真实二进制 + mock 上游，`curl` 实际访问 `/v1/models` |

全部离线，CI 不需要任何真实 API Key——与仓库现有 `make test` 的约定一致。

## 7. 对现有约束的影响

- **降级矩阵**：不受影响。`/v1/models` 是网关自身的管理面读接口，不是一条
  「入站协议 × 出站 Provider × 能力」的转换路径，不进矩阵，也不需要
  `Redeem`。它不转发请求体，也不产生任何能力损失。
- **启动期门对账**（`reconcileDoors`）：只对账 `doors` 里登记的转换端点。
  `/v1/models` 不加入 `doors`，因此不会被要求在矩阵里兑现。
- **首字节规则**：不适用，该接口无上游流式转发。
