# internal/gateway

整条链路唯一汇合的地方。每一层都在自己的包里单独测过，这里只负责按正确顺序接起来，
以及一件别处做不了的事——**跟踪下游首字节**，因为只有它知道客户端到底看到了什么。

## STRUCTURE

| 文件 | 职责 |
|---|---|
| `build.go` | 从 `config.Config` 装配凭据池、Provider、Router、Handler、Mux |
| `handler.go` | `serve()` 主链路 + `dispatch()` 两层 failover + `tracked` 首字节跟踪 |
| `relay.go` | `relayJSON` / `relayStream`：回写响应、抽取 usage、流中断收尾 |
| `auth.go` | 常量时间 API Key 校验，产出 `Caller` |
| `conformance_test.go` | 路径级端到端 fixture 回放 + golden 断言 |

## REQUEST PATH

```
ServeHTTP (handler.go:58)  ← tracked 包装 ResponseWriter
  └ serve (:74)
      Authenticate (auth.go:33)        → auth_failed
      readBody + openairesponses.Decode → bad_request
      InlineBytes vs Limits.MaxInlineBytes
      Router.Resolve(model)            → 候选 targets
      Matrix.BestOutbound(...)         → kind + Verdict（501 / 422 在此产生）
      └ dispatch (:142)
          for target:                  ← 换上游
            for credential:            ← 换凭据（仅 Retryable）
              Provider.Call → 成功则 Lease.Succeed，进 relay，此后不得重试
```

## CONVENTIONS

- 路由与矩阵**分工不互相包含**：Router 只给候选，Matrix 才做能力裁决。
- `Build()` 遇到「找不到」一律返回错误让启动失败——配置引用在 `config.Validate`
  已经校验过，这里再撞上只可能是代码写错。
- 未实现的出站协议族在 `Build()` 阶段就拒绝，不等请求打进来。
- `Deps.Now` 可注入，测试用它控制时间。
- 出参 `(outcome, outbound, err)` 中的 `outcome` 直接进 metrics 标签，
  新增分类要同步看 `obs.Metrics`。

## ANTI-PATTERNS

- **不要**在 `tracked.wrote == true` 之后重试、failover 或改状态码（`handler.go:196`）。此后上游失败
  只能写协议对应的终止错误事件，并把 usage 标成 `FidelityUnavailable`。
- **不要**对非 `Retryable` 错误换凭据——直接换下一个上游。
- **不要**用 `==`/`bytes.Equal` 比对 API Key，必须 `subtle.ConstantTimeCompare`。
- **不要**在没配 `models` 时报错——那是合法形态（只提供 `/healthz`）。
- **不要**把矩阵裁决的两类错误混为一谈：`ClassNotImplemented` → 501（路径还没建好），
  其余 → 422（能力不支持）。
