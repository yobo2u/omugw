# DashScope Native: `n` × Streaming / `incremental_output` Contract Research

**Date:** August 30, 2026
**Topic:** Official contract for combining `n>1` with streaming (`X-DashScope-SSE`) and
`incremental_output=true` on the DashScope Native text-generation endpoint
(`/api/v1/services/aigc/text-generation/generation`), with `qwen-plus` as the model under test.
**Trigger:** `qwen-plus` accepted `parameters.n=2` non-streaming (two candidates), but a real
incremental SSE call with `n=2` + `incremental_output=true` produced only one downstream candidate.

## Findings

### 1. The `n` parameter contract (current official docs)

Chinese primary source — DashScope API 参考 (help.aliyun.com, retrieved 2026-08-30):

> **n** `integer`（可选） 默认值为1
>
> 生成响应的个数，取值范围是`1-4`。对于需要生成多个响应的场景（如创意写作、广告文案等），可以设置较大的 n 值。
>
> 当前仅支持 [Qwen3（非思考模式）]、qwen-plus-character 模型，且在传入 tools 参数时固定为1。
>
> 设置较大的 n 值不会增加输入 Token 消耗，会增加输出 Token 的消耗。
>
> 通过HTTP调用时，请将 **n** 放入 **parameters** 对象中。

English primary source — DashScope API Reference (alibabacloud.com, "Last Updated: Aug 28, 2026"):

> **n** integer (Optional) The default value is 1.
>
> The number of responses to generate. The value range is **1-4**. For scenarios that require
> multiple responses to be generated, such as creative writing or ad copy, you can set a larger
> **n** value.
>
> Currently, only **Qwen3 (non-thinking mode)** models are supported. The value is fixed at 1
> if the **tools** parameter is passed.
>
> Setting a larger **n** value does not increase input token consumption but does increase
> output token consumption.
>
> When you call over HTTP, place **n** in the **parameters** object.

Contract points:
- Range `1-4`, default `1`; placed in the `parameters` object over HTTP.
- Supported models: zh page says "Qwen3（非思考模式）" **and** `qwen-plus-character`; the en page
  lists only "Qwen3 (non-thinking mode)" (the two language versions diverge slightly; zh is the
  more permissive statement).
- Fixed to `1` when `tools` is passed.
- Billing: larger `n` increases output tokens only, never input tokens.
- **The `n` entry says nothing about streaming, SSE, or `incremental_output` — no prohibition,
  no error description, no clamp statement.**

### 2. `qwen-plus` is inside the `n` support list

The deep-thinking doc (help.aliyun.com/zh/model-studio/deep-thinking) classifies `qwen-plus`:

> #### Qwen3
> - **商业版**
>   - **千问Plus系列**（混合思考模式，默认不开启思考模式）：qwen-plus、qwen-plus-latest、
>     qwen-plus-2025-04-28 及之后的快照版模型

`qwen-plus` is Qwen3 commercial edition, hybrid thinking, **thinking disabled by default** — i.e.
it runs in "Qwen3（非思考模式）" unless `enable_thinking=true` is set. This is consistent with the
observed acceptance of `n=2` in non-streaming calls.

### 3. Streaming is enabled by header, not by body, over HTTP

From the same DashScope API reference (`stream` parameter):

> **stream** `boolean`（可选）默认值为`false`
>
> 是否流式输出回复。参数值：
> - false：模型生成完所有内容后一次性返回结果。
> - true：边生成边输出，即每生成一部分内容就立即输出一个片段（chunk）。
>
> 该参数仅支持Python SDK。通过Java SDK实现流式输出请通过`streamCall`接口调用；
> **通过HTTP实现流式输出请在Header中指定`X-DashScope-SSE`为`enable`。**
>
> Qwen3商业版（思考模式）、Qwen3开源版、QwQ、QVQ只支持流式输出。

English:

> This parameter is supported only by the Python SDK. To implement streaming output with the
> Java SDK, call the `streamCall` interface. **To implement streaming output over HTTP, specify
> `X-DashScope-SSE` as `enable` in the header.**

So on the wire (HTTP), streaming = `X-DashScope-SSE: enable` header; the `stream` body field is a
Python-SDK-only concern. `qwen-plus` in non-thinking mode is **not** streaming-only.

### 4. `incremental_output` contract

> **incremental_output** `boolean`（可选）默认为`false`（Qwen3-Max、Qwen3-VL、Qwen3 开源版、
> QwQ、QVQ模型默认值为 `true`）
>
> 在流式输出模式下是否开启增量输出。推荐您优先设置为`true`。
>
> - false：每次输出为当前已经生成的整个序列，最后一次输出为生成的完整结果。
>   （示例：`I` / `I like` / `I like apple` / `I like apple.`）
> - true（推荐）：增量输出，即后续输出内容不包含已输出的内容。您需要实时地逐个读取这些片段以获得完整的结果。
>   （示例：`I` / `like` / `apple` / `.`）
>
> QwQ 模型与思考模式下的 Qwen3 模型只支持设置为 `true`。由于 Qwen3 商业版模型默认值为`false`，
> 您需要在思考模式下手动设置为 `true`。
>
> Qwen3 开源版模型不支持设置为 `false`。

Key points:
- Only meaningful "在流式输出模式下" (in streaming output mode).
- Default `false` for Qwen3 commercial edition (which includes `qwen-plus`); `true` is recommended.
- The dedicated streaming doc (help.aliyun.com/zh/model-studio/stream) restates it:
  > DashScope 协议支持增量与非增量式流式输出：
  > - **增量**（推荐）：每个数据块仅包含新生成的内容，设置`incremental_output`为`true`启动增量式流式输出。
  >   示例：["我爱","吃","苹果"]
  > - **非增量**：每个数据块都包含之前已生成的内容……设置`incremental_output`为`false`启动非增量式流式输出。
  >   示例：["我爱","我爱吃","我爱吃苹果"]
- **No statement anywhere ties `incremental_output` behavior to the value of `n`.**

### 5. `result_format` contract

> **result_format** `string`（可选）默认为`text`（Qwen3-Max、Qwen3-VL、QwQ 模型、Qwen3 开源模型
> （除了qwen3-next-80b-a3b-instruct）与 Qwen-Long 模型默认值为 message）
>
> 返回数据的格式。推荐您优先设置为`message`，可以更方便地进行多轮对话。
>
> 模型为千问VL/QVQ/Audio时，设置`text`不生效。
>
> Qwen3-Max、Qwen3-VL、思考模式下的 Qwen3 模型只能设置为`message`，由于 Qwen3 商业版模型默认值为
> `text`，您需要将其设置为`message`。

For `qwen-plus` (commercial, non-thinking): both `text` and `message` are legal; `message` is
recommended. `choices` only appears when `result_format=message` (see §6).

### 6. Documented SSE wire format (single-choice examples only)

The response-object section of the DashScope API reference is titled:

> ## chat响应对象（**流式与非流式输出格式一致**）

i.e. streaming and non-streaming responses share one shape. `output.choices` is documented as an
array ("模型的输出信息。当result_format为message时返回choices参数。").

The streaming doc gives the only official DashScope-native SSE example (`qwen-plus`,
`result_format=message`, `incremental_output=true`):

```
id:1
event:result
:HTTP_STATUS/200
data:{"output":{"choices":[{"message":{"content":"我是","role":"assistant"},"finish_reason":"null"}]},"usage":{"total_tokens":27,"output_tokens":1,"input_tokens":26,...},"request_id":"d30a9914-..."}

id:2
event:result
:HTTP_STATUS/200
data:{"output":{"choices":[{"message":{"content":"通义千","role":"assistant"},"finish_reason":"null"}]},...}
...
id:15
event:result
:HTTP_STATUS/200
data:{"output":{"choices":[{"message":{"content":"","role":"assistant"},"finish_reason":"stop"}]},...}
```

Wire-format facts from this example:
- Each SSE message: `id:` (chunk number), `event:result`, `:HTTP_STATUS/200`, `data:` JSON.
- Each `data` chunk carries `output.choices` as an array — **with exactly one element in every
  documented example**.
- Mid-stream `finish_reason` is the **string `"null"`** (not JSON `null`); the terminal chunk
  carries `"stop"`. (The 2024-07 archived legacy doc shows the same `"finish_reason": "null"`
  string pattern, so this is long-standing behavior.)
- **The documented native chunk carries no per-choice `index` field** (contrast: the OpenAI-
  compatible chunk examples on the same pages always show `"index":0`). If multiple candidates
  were interleaved one-element-per-chunk, the documented shape gives the client no way to
  attribute chunks to candidates; the alternative — every chunk containing all n choices — is
  never exemplified.
- Every official streaming code sample (Python/Java/curl, DashScope and OpenAI-compatible,
  including the 2024 archived legacy page) reads only `choices[0]`.

### 7. Official SDK passes `n` through with no streaming coupling

The official Python SDK (`dashscope/dashscope-sdk-python`, Apache-2.0, PyPI `dashscope` 1.27.2):

- `Generation.call(..., stream, incremental_output, ..., n, ...)` — `n` is an ordinary passthrough
  parameter; docstring: `n (int, optional): Number of responses to generate (1-4).`
  (dashscope/aigc/generation.py, main branch)
- No client-side validation or transformation couples `n` to `stream`/`incremental_output` for
  text generation. (Only `image_generation.py` contains an `incremental_output` merge workaround,
  unrelated to text `n`.)

So the observed single-candidate behavior, whatever its cause, is server-side; the SDK neither
documents nor performs any client-side clamp.

### 8. Legacy docs are gone and never documented it either

- The legacy DashScope reference (`help.aliyun.com/zh/dashscope/developer-reference/api-details`)
  now redirects to a 404 page; the product docs were folded into Model Studio.
- The last archived snapshot (web.archive.org, 2024-07-18) of that page contains **no `n`
  parameter at all** in its parameter table (top_p / temperature / presence_penalty / max_tokens /
  seed / stop ...), confirming `n` support statements exist only in the current Model Studio docs.

### 9. Net contract assessment (evidence vs. gap)

What IS officially documented:
1. `n ∈ [1,4]`, default 1, in `parameters`; supported models = Qwen3 (non-thinking mode) +
   qwen-plus-character; fixed 1 with `tools`; output-token billing scales with `n`.
2. `qwen-plus` (default non-thinking) is in that support list.
3. HTTP streaming = `X-DashScope-SSE: enable`; `incremental_output` (default false for
   qwen-plus, true recommended) controls delta vs. cumulative chunk content.
4. Streaming and non-streaming responses share one shape; `choices` is an array.

What is NOT documented anywhere in primary sources (contract gap):
- **No statement defines the behavior of `n>1` under streaming/`incremental_output` on the
  Native endpoint** — no promised multi-candidate SSE shape, no documented error, no documented
  silent clamp to one candidate, no supported/unsupported declaration.
- No official example shows more than one choice in any streaming chunk.
- The documented native chunk shape lacks a choice `index`, making interleaved multi-candidate
  streaming unattributable in the documented format.

## Implications for the gateway decision (facts above, inference here)

- `multi_candidate_stream` **cannot be claimed as an upstream-supported capability** on primary
  sources: the docs promise `n` only as a request parameter with a model list, and are silent on
  its streaming delivery.
- The observed "one candidate" SSE outcome is **undocumented either way**: it may be a server-side
  silent clamp (n→1 under SSE) or an interleaving the gateway translator failed to reassemble;
  the docs provide no contract to distinguish these. Under omugw's fail-closed principle this is
  exactly the "上游语义不确定" case: the capability should not be silently trusted.
- If the gateway wants certainty, the only evidence-grade options are: (a) treat `n>1 + stream`
  as REJECT/DEGRADE at the matrix level (docs give no promise to build on), or (b) capture a
  recorded fixture of the real SSE bytes for `n=2` and let the bytes decide — but note that even
  then the behavior is undocumented and may change without notice.

## Sources

- [DashScope API 参考（中文，主源）](https://help.aliyun.com/zh/model-studio/qwen-api-via-dashscope) — `n`/`stream`/`incremental_output`/`result_format` 定义、chat响应对象（流式与非流式输出格式一致）
- [DashScope API Reference (English)](https://www.alibabacloud.com/help/en/model-studio/qwen-api-via-dashscope) — Last Updated: Aug 28, 2026; English wording of the same parameters
- [流式输出（中文）](https://help.aliyun.com/zh/model-studio/stream) — SSE 工作原理、增量/非增量定义、DashScope 原生 SSE 响应示例（id/event/HTTP_STATUS/data、finish_reason 字符串 "null"）
- [深度思考（中文）](https://help.aliyun.com/zh/model-studio/deep-thinking) — qwen-plus 归类：Qwen3 商业版 千问Plus系列，混合思考模式，默认不开启思考模式
- [OpenAI兼容-Chat（中文）](https://help.aliyun.com/zh/model-studio/qwen-api-via-openai-chat-completions) — 交叉验证：兼容端 `n` 描述与 Native 端一致（仅 Qwen3 非思考模式、qwen-plus-character）
- [dashscope/dashscope-sdk-python](https://github.com/dashscope/dashscope-sdk-python/blob/main/dashscope/aigc/generation.py) — 官方 Python SDK：`n` 透传，无流式耦合校验
- [Legacy DashScope docs, last archived snapshot 2024-07-18](http://web.archive.org/web/20240718083828/https://help.aliyun.com/zh/dashscope/developer-reference/api-details) — 旧文档无 `n` 参数；流式示例同样只读 choices[0]、finish_reason 为字符串 "null"
