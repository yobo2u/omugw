# internal/gateway

整条链路唯一汇合的地方。每一层都在自己的包里单独测过，这里只负责按正确顺序接起来，
以及一件别处做不了的事——**跟踪下游首字节**，因为只有它知道客户端到底看到了什么。

## STRUCTURE

| 文件 | 职责 |
|---|---|
| `build.go` | 从 `config.Config` 装配凭据池、Provider、Router、Handler、Mux。Mux 注册先落三个精确端点，再挂 DashScope Native 命名空间的两条兜底：`POST /api/v1/` 返回协议化 501，不带方法的 `/api/v1/` 返回框架 404——未投放端点在进 Handler 主链路之前就被拦下 |
| `handler.go` | `serve()` 主链路 + `dispatch()` 两层 failover + `tracked` 首字节跟踪 |
| `relay.go` | `relayJSON` / `relayStream`：回写响应、抽取 usage、流中断收尾 |
| `auth.go` | 常量时间 API Key 校验，产出 `Caller` |
| `conformance_test.go` | 路径级端到端 fixture 回放 + golden 断言 |

## REQUEST PATH

未投放的 DashScope Native 端点不会进入这条链路——它们在 Mux 层就被 `build.go:139`
注册的 `POST /api/v1/` 前缀兜底拦下，直接返回 DashScope 扁平信封的 501
（精确注册的文本生成端点凭最长前缀匹配优先命中自己的 Handler）。同命名空间下的
非 POST 请求由 `build.go:133` 那条不带方法的兜底接住，返回框架 404。

```
ServeHTTP (handler.go:175)  ← tracked 包装 ResponseWriter
  └ serve (:191)
      Authenticate (auth.go:33)        → auth_failed
      readBody + openairesponses.Decode → bad_request
      InlineBytes vs Limits.MaxInlineBytes
      Router.Resolve(model)            → 候选 targets
      Matrix.BestOutbound(...)         → kind + Verdict（501 / 422 在此产生）
      └ dispatch (:259)
          for target:                  ← 换上游
            for credential:            ← 换凭据（仅 Retryable）
              Provider.Call → 成功则 Lease.Succeed，进 relay，此后不得重试
```

## CONVENTIONS

- 路由与矩阵**分工不互相包含**：Router 只给候选，Matrix 才做能力裁决。
- `Build()` 遇到「找不到」一律返回错误让启动失败——配置引用在 `config.Validate`
  已经校验过，这里再撞上只可能是代码写错。
- 未实现的出站协议族在 `Build()` 阶段就拒绝，不等请求打进来。
- Native 命名空间兜底依赖 `net/http.ServeMux` 的最长前缀匹配：精确注册的
  `TextGenerationPath` 优先命中，其余 `POST /api/v1/*` 才落到 501 兜底，非 POST
  再落到不带方法的 404 兜底。兜底的 501 走 `dashscopewire` 错误信封，与上游错误
  同形——改端点注册前先想清楚匹配优先级。
- 不带方法的 `/api/v1/` 兜底不能省：只注册 `POST` 兜底时，ServeMux 会对同路径的
  GET 答 405，等于宣称一个不存在的端点存在。
- `Deps.Now` 可注入，测试用它控制时间。
- 出参 `(outcome, outbound, err)` 中的 `outcome` 直接进 metrics 标签，
  新增分类要同步看 `obs.Metrics`。

## ANTI-PATTERNS

- **不要**在 `tracked.wrote == true` 之后重试、failover 或改状态码（`handler.go:425`）。此后上游失败
  只能写协议对应的终止错误事件，并把 usage 标成 `FidelityUnavailable`。
- **不要**对非 `Retryable` 错误换凭据——直接换下一个上游。
- **不要**用 `==`/`bytes.Equal` 比对 API Key，必须 `subtle.ConstantTimeCompare`。
- **不要**在没配 `models` 时报错——那是合法形态（只提供 `/healthz`）。
- **不要**把矩阵裁决的两类错误混为一谈：`ClassNotImplemented` → 501（路径还没建好
  或能力还没投放），其余 → 422（能力不支持）。
