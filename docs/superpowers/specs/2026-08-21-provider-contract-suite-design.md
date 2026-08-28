# 出站适配器共享契约套件设计

**日期**：2026-08-21
**状态**：已批准，待实现
**前序约束**：原则 2.2（同源快通道）、ADR-0001（声明必须由 fixture 兑现）

## 背景

`internal/provider` 下已有两个出站适配器：`passthrough`（同源直通，同时服务
`openai.compat` 与 `dashscope.native` 两个协议族）与 `dashscopecompat`
（wire-compatible 异构）。Phase 1 还要接入 Anthropic Messages 出站，以及
`openai.realtime`、`dashscope.ws.realtime`、`dashscope.ws.inference` 三条
WebSocket 出站。

两个适配器的测试各自重写了同一批不变量：网关凭据覆盖客户端 `Authorization`、
客户端头不转发、`Accept` 随流式、模型改写、`Path` 覆盖与缺省回退、base_url
前缀保留、非 2xx 解成 `*canonical.Error`、缺 model 判 `bad_request`。

问题不在重复本身，而在**遗漏是隐形的**：`passthrough` 有
`TestOversizedErrorBodyIsCapped` 而 `dashscopecompat` 没有；`dashscopecompat` 有
`TestMissingUpstreamModelIsInternal` 而 `passthrough` 没有。两处的生产代码其实
都正确，缺的只是断言——但没有任何机制会告诉你缺了。第三个适配器接进来时，
会继续按作者当天想到什么写什么，漏掉哪一条谁都不知道。

这不是「测试写得不够勤」，而是缺一个把契约本身表达出来的地方。契约散落在各包的
测试函数里，就没有单一位置可以回答「一个出站适配器必须满足什么」。

## 决策

新建 `internal/provider/providertest` 包，提供**声明式一致性套件**：各适配器的
测试包填一个 `Subject` 声明自己的差异，套件跑固定的不变量清单。

### 为什么不是另外两种做法

**不给 `provider.Provider` 加 `Descriptor()` 让套件读它。** 那会把测试契约推进
生产接口，而且是自证的——适配器自己声明「我只转发这三个头」，套件就只查这三个；
当实现与声明一起错时，套件完全看不见。契约必须由测试侧独立声明，才能与实现互为
对照。

**不用出站请求 golden 比对。** golden 覆盖面最广，但它只记录「发生了什么」，
不记录「防的是什么」。`Authorization` 泄露给上游这种安全回归，在 golden 里只是
一行 diff，`make golden-update` 会把它顺手盖掉。本仓库的注释与断言一贯写防的是
什么（见 `TestClientAuthorizationIsNotForwarded` 的注释），golden 与这个取向冲突。

## 不变量清单

分两层。这个分层不是为了整齐，而是为了将来 WebSocket 适配器接入时，不必重写
与传输无关的那部分。

### 传输无关族

对任何出站适配器成立，不涉及 HTTP 概念：

1. **身份**：`Kind()` 等于 `Subject` 声明的协议族。
2. **凭据隔离**：客户端 `Authorization` 绝不到达上游；上游收到的是网关凭据。
   这是安全约束，不是风格问题——转发它等于把密钥泄露给一个没有理由知道它的第三方。
3. **装配错误归网关**：`Target.UpstreamModel` 为空时返回 `ClassInternal`。
   判成 `bad_request` 会误导客户端去改一个它没写错的请求。
4. **客户端错误归客户端**：请求体不是 JSON 对象、或缺 `model` 时返回
   `ClassBadRequest`。

### HTTP 传输族

5. **请求形状**：method 为 POST，`Content-Type: application/json`。
6. **流式信号落位**：`Accept` 为 `text/event-stream`（流式）或 `application/json`
   （非流式）；另有 `Subject.StreamHeaders` 声明的额外头（DashScope Native 的
   `X-DashScope-SSE: enable`）。流式信号丢了，上游会按非流式返回，整条流的语义变掉。
7. **模型改写**：上游收到 `Target.UpstreamModel`，其余字段语义不变。按语义比对，
   不做字节比对——重新序列化会改键序，钉死字节等于钉死 `encoding/json` 的实现细节。
8. **路径来源**：`Request.Path` 优先，留空退回适配器默认路径。
9. **base_url 前缀保留**：base_url 带非版本路径前缀时，端点路径追加在前缀之后。
10. **错误解码**：非 2xx 返回 `*canonical.Error`，`Class` / `Retryable` /
    `RetryAfter` / `UpstreamStatus` 均正确；不可解析的超大错误体不让分类退化——
    状态码本身就是可靠信号。

第 10 条拆成两个具体断言，避免实现者自己猜边界：

- **429 + `Retry-After: 7`**，体为 `Subject.RateLimitEnvelope`。断言
  `Class == ClassRateLimit`、`Retryable == true`、`RetryAfter == 7s`、
  `UpstreamStatus == 429`。`Retryable` 为真的语义是「换一个凭据或 Provider 可能
  成功」，不是「上游临时故障」。
- **500 + 1 MiB 垃圾体**（不可解析）。断言
  `Class == ClassUpstreamUnavailable` 且 `UpstreamStatus == 500`。

**不断言 `len(Message)` 的上限。** 实测过：不可解析的体走的是「解析失败 → 回退
`http.StatusText(status)`」这条路，`Message` 恒为 21 字节（`"Internal Server Error"`），
无论读取上限是 64 KiB、1 MiB 还是根本没有上限，长度断言都通过——它抓不到任何东西。

现有的 `passthrough.TestOversizedErrorBodyIsCapped` 就是这样一个一直在空转的断言。
读取上限属于 wire 层实现细节，该由 `openaiwire` / `dashscopewire` 自己的测试守住；
适配器这一层真正的契约是「垃圾进来，分类不许乱」。

`Retry-After` 用固定时钟解析，由 `Deps.Now` 注入——HTTP-date 形式的
`Retry-After` 依赖当前时间，不注入会让测试在跨秒边界随机失败。

### 头转发用严格闭集

第 2 条的断言强度定为：**上游收到的头 = 网关自造头 ∪ `Subject.ForwardedHeaders`，
此外一个都没有**。

不用「注入几个客户端头再断言它们没出现」的抽样式黑名单：抽样只能证明想到的那几个
没漏，证明不了没想到的那些。新适配器悄悄多转一个客户端头，抽样抓不到，闭集能。

代价是要显式忽略传输层自动附加的头。忽略清单固定为：

| 头 | 来源 |
|---|---|
| `Host` | `net/http` 从 URL 推导 |
| `User-Agent` | `net/http` 默认填充 |
| `Content-Length` | `net/http` 按 body 长度计算 |
| `Accept-Encoding` | `net/http` Transport 自动协商 |

这四个由 `net/http` 而非适配器决定，断言它们等于断言标准库的行为。清单写死在套件
里并注明理由；适配器自造的头（`Content-Type`、`Accept`、`Authorization`、
`StreamHeaders` 声明的头）**不在**忽略之列，它们由前几条不变量正面断言。

`Subject.ForwardedHeaders` **零值即最严**——留空表示一个客户端头都不许转发。
要放宽必须显式列出来。默认宽松会让「忘记声明」和「确实不转发」不可区分。

套件构造闭集断言的方式：注入一组**必定不该出现**的客户端头（含一个假
`Authorization`、一个自定义头、以及其他 `Subject` 的转发白名单里出现过但本
`Subject` 未声明的头），再断言上游实际收到的头集合减去忽略清单后，恰好等于
网关自造头 ∪ `ForwardedHeaders`。

## API 形状

```go
package providertest

// Subject 是一个待测适配器的声明。
//
// 用具名结构体而不是位置参数：将来加第七、第八项声明时，不必回到每个调用点
// 数位置。理由与 dashscopecompat 测试里 callInput 的收敛一致。
type Subject struct {
	Name string
	Kind degrade.Provider

	// New 构造适配器。套件按传输族注入不同的 driver。
	New func(t *testing.T, deps Deps) provider.Provider

	// DefaultPath 是请求未带 Path 时应落到的端点。
	DefaultPath string

	// ForwardedHeaders 是允许原样带给上游的客户端头。
	// 零值即最严：留空表示一个客户端头都不许转发。
	ForwardedHeaders []string

	// StreamHeaders 是流式时除 Accept 之外必须落位的额外头。
	StreamHeaders map[string]string

	// ValidBody 是该协议下一份合法的请求体，含 model 字段。
	ValidBody string

	// RateLimitEnvelope 是上游 429 的错误信封样例。
	// 各协议族信封不同形（DashScope 扁平 {code,message}，OpenAI 系嵌套），
	// 由 Subject 提供代表性样例，套件只断言解码结果。
	RateLimitEnvelope string
}

// Deps 是套件注入给适配器构造函数的依赖。
//
// 存在这一层间接而不是直接传 *httpx.Client：套件要控制 httptest 服务器与
// 固定时钟，且将来 WebSocket driver 接入时只扩展 Deps，不改任何调用点。
type Deps struct {
	HTTPClient *httpx.Client
	Now        func() time.Time
}

// Run 跑全部不变量族。
func Run(t *testing.T, s Subject)
```

调用点（各适配器的 `_test.go`）：

```go
func TestProviderContract(t *testing.T) {
	providertest.Run(t, providertest.Subject{
		Name: "dashscopecompat",
		Kind: degrade.ProviderDashScopeCompatible,
		New: func(t *testing.T, d providertest.Deps) provider.Provider {
			return New(d.HTTPClient, d.Now)
		},
		DefaultPath:       ChatCompletionsPath,
		ValidBody:         `{"model":"logical","messages":[{"role":"user","content":"hi"}]}`,
		RateLimitEnvelope: `{"error":{"type":"rate_limit_error","message":"slow down"}}`,
	})
}
```

`passthrough` 跑两次 `Run`。它一个类型服务两个协议族，两族的契约差异是真实存在的，
合成一个带变体的 `Subject` 只会让声明多一层嵌套。

**必填项缺失即失败**：`Kind`、`New`、`ValidBody`、`RateLimitEnvelope` 任一为零值时
`t.Fatal` 并指出缺哪一项。不静默跳过对应的族——跳过等于把「没跑」伪装成「跑过了」。

### 三个 Subject 的取值

实现时照此填写，不需要再从各适配器反推：

| 字段 | passthrough / openai.compat | passthrough / dashscope.native | dashscopecompat |
|---|---|---|---|
| `Kind` | `ProviderOpenAICompat` | `ProviderDashScopeNative` | `ProviderDashScopeCompatible` |
| `New` | `New(kind, path, d.HTTPClient, d.Now)` | 同左 | `New(d.HTTPClient, d.Now)` |
| `DefaultPath` | `/v1/responses` | `/api/v1/services/aigc/text-generation/generation` | `ChatCompletionsPath` |
| `ForwardedHeaders` | 空 | `X-DashScope-WorkSpace`、`X-DashScope-DataInspection`、`X-DashScope-Async` | 空 |
| `StreamHeaders` | 空 | `X-DashScope-SSE: enable` | 空 |
| `ValidBody` | `{"model":"logical","input":"hi"}` | `{"model":"logical","input":{"messages":[{"role":"user","content":"x"}]}}` | `{"model":"logical","messages":[{"role":"user","content":"hi"}]}` |
| `RateLimitEnvelope` | OpenAI 嵌套信封 | DashScope 扁平信封 | OpenAI 嵌套信封 |

`ValidBody` 三者不同形不是笔误：DashScope Native 的请求体是
`input.messages` 嵌套结构，OpenAI Responses 是顶层 `input`，Chat 是顶层
`messages`。套件只要求它是合法 JSON 对象且含 `model`，其余形状由 `Subject` 自带——
这正是把请求体做成声明而非套件内置常量的原因。

两份错误信封样例：

- OpenAI 嵌套：`{"error":{"type":"rate_limit_error","message":"slow down"}}`
- DashScope 扁平：`{"code":"Throttling.RateQuota","message":"slow down","request_id":"req-1"}`

DashScope 的 `Throttling.` 前缀是 `dashscopewire.classify` 判定
`ClassRateLimit` 的依据；换成别的 code 会让第 10 条不变量断言到错误的分类上。

## 现有测试的去留

套件只收对**所有**适配器成立的不变量。现有测试按是否被套件覆盖分三类处理。

删除范围定为**只删纯重复的**。判据是：旧测试的断言内容能否被 `Subject` 声明完整
表达。能，就是纯重复；不能——它带有套件看不见的**协议特有取值**——则保留。

### 保留：单个适配器的独有语义

| 测试 | 所属 | 理由 |
|---|---|---|
| `TestUnknownFieldsSurvive` | passthrough | 同源直通独有：保住网关没建模的字段 |
| `TestIdenticalModelIsByteExact` | passthrough | 同名时零改动转发，异构适配器不适用 |
| `TestOfficialBaseURLDoesNotRepeatVersion` | dashscopecompat | 官方 base_url 版本段去重，DashScope 独有 |
| web_search_options 映射 | dashscopecompat | 定点修补语义 |

### 保留：协议特有取值

套件断言的是「`ForwardedHeaders` 里声明的头都到了」，它不知道也不该知道那三个
DashScope 头**具体叫什么、丢了会怎样**。那份知识留在原包才有归属。

| 测试 | 套件替代不了的部分 |
|---|---|
| `TestDashScopeTenantHeadersForwarded` | 三个头的具体名字 + 「丢了会落到错误子租户」这条理由 |
| `TestDashScopeStreamingSetsSSEHeader` | `X-DashScope-SSE: enable` 的具体取值——声明写错了套件照样通过 |
| `TestOversizedErrorBodyIsCapped` | 见下 |

这三项与套件是**互补**而非重复：套件守形状，它们守取值。同一契约两处断言之所以
危险，是因为两处断的是同一件事；这里不是。

`TestOversizedErrorBodyIsCapped` 保留但**必须改**：它的 `len(cerr.Message) > 128<<10`
断言恒为假，一直在空转（理由见「不变量清单」第 10 条）。改成断言分类不退化，
或下沉到 wire 层去真正验证读取上限——两条路选一条，别留一个假装在测的测试。

### 删除：纯重复（18 个）

- passthrough：`TestClientAuthorizationIsNotForwarded`、
  `TestAcceptHeaderMatchesStreaming`、`TestModelIsRewritten`、
  `TestPathIsAppendedToBaseURL`、`TestBaseURLPathPrefixIsPreserved`、
  `TestUpstreamErrorBecomesCanonical`、`TestMissingModelIsRejected`、
  `TestNonObjectBodyIsRejected`、`TestRequestPathOverridesDefault`、
  `TestDefaultPathUsedWhenRequestPathEmpty`
- dashscopecompat：`TestKindIsDashScopeCompatible`、`TestRequestShape`、
  `TestDefaultPathWhenRequestPathEmpty`、`TestBaseURLPathPrefixIsPreserved`、
  `TestUpstreamErrorDecoded`、`TestMissingModelRejected`、
  `TestInvalidJSONRejected`、`TestMissingUpstreamModelIsInternal`

**删除前必须逐条确认套件确实覆盖了同一件事**，不能按名字相近就删。删除是本次工作
的最后一步，在套件全绿之后做，且单独成一个提交——这样 review 时能一眼看出「删掉的
断言在套件里对应哪一条」。

### 搬运：注释里「防的是什么」

被删测试上的注释是这个仓库最有价值的部分（如 `TestClientAuthorizationIsNotForwarded`
解释了转发客户端 `Authorization` 等于泄露密钥给没有理由知道它的第三方）。这些理由
**搬进套件对应的不变量注释里**，不随测试一起删掉。套件的注释密度应当不低于被它
替代的那些测试。

## WebSocket 预留

Phase 1 范围内有三条 WebSocket 出站（`openai.realtime`、`dashscope.ws.realtime`、
`dashscope.ws.inference`），但**今天没有任何 WS 适配器**。

因此预留只做到结构可扩展，不写任何 WS 类型、WS 断言或 WS driver：

- 传输无关族的四条不引用 `httptest`，将来 WS driver 接入后可直接复用。
- `Deps` 是结构体，扩展字段不破坏现有调用点。
- HTTP 传输族绑定 HTTP driver，WS 接入时新增独立的 WS 族。

为不存在的接口提前设计有猜错的风险。这里的取舍是：只让**已经写出来的东西**具备
扩展位，不预先定义扩展的内容。

## 与既有设施的边界

`internal/testkit` 是路径级端到端 fixture 与 golden，回答「这条转换路径整体对不对」，
且是 ADR-0001 的转正门槛。本套件是适配器级单元契约，回答「这个出站适配器守不守
规矩」。两者不重叠，也不互相替代：fixture 通过不代表适配器没把客户端密钥转给上游
（fixture 里根本没有恶意客户端头）。

本套件不参与转正门槛，不改动降级矩阵，不新增能力声明。矩阵仍是语义损失的唯一权威。

## 验收

- `make check` 通过（fmt-check + vet + test + matrix）。
- `make test-race` 通过。
- `passthrough` 两次 `Run` 与 `dashscopecompat` 一次 `Run` 全绿。
- 严格闭集断言若暴露真实行为差异，在本批工作内修复；两个适配器结束时均全绿，
  不留 skip、不留红灯。
- 旧测试删除 18 个、保留 7 个，删除量与「纯重复」清单一一对应。
- `passthrough.TestOversizedErrorBodyIsCapped` 的空转断言已改（断分类不退化，
  或下沉到 wire 层真正验证读取上限），不留假装在测的测试。

预期不会有生产代码改动：两处覆盖不对称（`dashscopecompat` 缺错误体上限断言、
`passthrough` 缺 UpstreamModel 分类断言）经核对**实现均已正确**，缺的只是断言。

其中「错误体上限」这一项经实测另有发现：`passthrough` 那条断言本身恒为真，
从未真正验证过上限。这比「另一个包漏了一条断言」更隐蔽——它看起来有覆盖，
实际没有。正是本套件要消除的那类隐形缺口。

若严格闭集断言暴露出真实的头转发差异，属本批修复范围；修复生产代码时只做最小改动，
不顺手重构周边。

## 提交边界

本次产出与工作区中已有的技能配置初始化（`AGENTS.md` 的 `## Agent skills` 一节与
`docs/agents/` 三份文档）**不混在同一提交**。

本次工作自身拆成三个提交，顺序即实施顺序：

1. `providertest` 套件本体 + 三个 `Subject` 接入（此时新旧测试并存，全绿）。
2. 严格闭集暴露的生产代码修复（若无差异则此提交不存在）。
3. 删除被套件覆盖的旧测试 + 注释搬运。

第 3 步单独成提交，是为了让 review 能直接对照「删掉的断言 ↔ 套件里的哪一条」。
把删除混进第 1 步会让 diff 同时出现大量新增与删除，那个对照关系就看不见了。

## 来源与许可证

共享 provider 契约测试基类的**思路**参考 LiteLLM，固定提交
[`e17988f`](https://github.com/BerriAI/litellm/tree/e17988f4fe7891bd39335b7e2a4b405bbe7b5ddc)。

概念借鉴，无源码复制：本套件的不变量清单来自本仓库现有两个适配器测试的交集，
API 形状按本仓库的 Go 惯例独立设计。不读取、不参考 LiteLLM 的 `enterprise/` 目录。

实现完成后在 `docs/provenance.yaml` 登记该固定提交与「概念借鉴，无源码复制」的
边界说明。
