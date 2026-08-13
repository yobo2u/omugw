# DashScope Native 转正收口设计

## 背景

`dashscope.native -> dashscope.native` 已作为同源快通道转正，但当前声明与投放面存在
粒度错位：矩阵把入站协议可表达的 18 项能力全部标为可用，HTTP Mux 实际只注册文本
生成端点。其余 Native 端点因此返回 Go 默认 404，既绕过矩阵的显式 501，也不符合
DashScope 的错误信封。

错误映射还有一个独立缺口：上游 `{code,message,request_id}` 被解成
`canonical.Error` 后，`request_id` 暂存在 `Param`，重新编码时没有写回，导致同源路径
丢失排障凭据。

## 决策

### 1. 将设计处置与当前兑现范围分开

路径规则继续描述协议设计：DashScope Native 同源路径对 18 项可表达能力均为
`PASSTHROUGH`。在路径上增加独立的“当前已兑现能力集合”，供运行时裁决和
`AvailableScore()` 使用。

文本生成端点本期只兑现已被请求解码器、fixture 与响应链路验证的能力。其他能力仍保留
设计处置，但当前不可用，不能再被运行时当成已经上线。

这不是把能力改成 `REJECT`：能力与协议之间没有结构性不兼容，只是实现尚未投放，故错误
应为 `501 Not Implemented`，而不是 `422 Unsupported`。

### 2. 为 Native 端点提供协议化兜底

保留文本生成端点的精确注册；另外注册 DashScope Native HTTP 命名空间的 POST 兜底。
未投放端点进入 Native handler 的错误编码边界，返回 DashScope 平铺错误信封和 501，
不得落到 `http.ServeMux` 的纯文本 404。

兜底不把请求发到上游，也不猜测它属于哪项能力。端点投放是能力裁决之前的入口约束，
错误信息应说明该 Native 端点仍在规划中。

### 3. 独立保留上游 request ID

在 `canonical.Error` 增加专用 `UpstreamRequestID` 字段。DashScope 错误解码将
`request_id` 写入该字段，编码时再写回 `Envelope.RequestID`。不再借用 `Param`，避免未来
异构路径把 request ID 误编码成“出错参数名”。

### 4. 加强转正证据

测试固定三个公共 seam：

1. `Built.Mux`：未投放的 Native POST 端点返回 501、JSON、DashScope 平铺信封，且不上游。
2. `dashscopewire.DecodeError -> EncodeError`：`request_id` 往返不丢失。
3. Native 路径级 conformance：fixture 的请求头驱动入站请求，并断言上游实际收到正确
   路径、SSE/租户头以及除模型改写外保持的请求字段。

另外补普通上游错误和流内中断的 Native 错误形状测试，防止错误编码器注入以后被回退。

## 非目标

- 本次不转正 multimodal、embedding、rerank、image、video 或 speech 端点。
- 不为未知 Native 端点猜测能力。
- 不改变协议的可表达性声明或设计处置。
- 不引入向后兼容分支；当前尚未发布的矩阵可用范围直接修正。

## 完成标准

- 文本生成路径保持可用，其他 Native POST 端点返回协议化 501。
- 运行时不会把未兑现能力视为可用，生成矩阵文档与代码同步。
- DashScope 上游错误的 `request_id` 能往返到客户端。
- 新测试经回归注入可证明会在旧实现上失败。
- `make check`、相关 LSP 诊断和真实 HTTP 行为验证全部通过。
