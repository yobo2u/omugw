# OpenAI Chat → DashScope Native 音频能力部分投放设计

**日期**：2026-08-30  
**状态**：待书面复核  
**范围**：修订 `openai.chat → dashscope.native` 波次 4 的证据门与兑现集合

## 背景

原计划要求先录制 13 份真实 fixture，再在 `/v1/chat/completions` 门兑现 9 项能力，
其中包括 `audio_input`。当前北京地域凭据的 `qwen-audio-turbo` 免费额度已经耗尽，
上游返回 `403 Throttling.AllocationQuota`。阿里云官方文档同时明确：

- Qwen-Audio 当前仅供免费体验，额度耗尽后不可调用且不支持付费；
- 生产替代 Qwen-Omni 仅支持 OpenAI 兼容方式调用；
- Qwen-Omni 必须流式调用，不能使用现有 DashScope Native 非流式契约。

因此，不存在可通过“只替换模型名”得到的付费 Native 音频证据。把 Qwen-Omni 的
Compatible SSE 响应写成 Native fixture，会同时伪造端点、请求体与响应协议，违反
ADR-0001 的“先有真实证据，后兑现能力”。

官方依据：

- [音频理解（Qwen-Audio）](https://help.aliyun.com/zh/model-studio/audio-language-model)
- [非实时（Qwen-Omni）](https://help.aliyun.com/zh/model-studio/qwen-omni)

## 决策

本期按能力粒度部分投放 `openai.chat → dashscope.native`：

1. `audio_input` 的设计处置继续保持 `PASS`。Chat 入站与 DashScope Native
   多模态端点都能表达音频输入，协议能力并未消失。
2. `/v1/chat/completions` 门本期不兑现 `CapAudioInput`。没有真实 fixture 就不宣称
   当前可用。
3. 路径仍可转正，但门保持 `Gated()`，明确表示仍有设计上可交付、当前未投放的格子。
4. `audio_input` 请求由矩阵在触达上游前返回 501 `not_implemented`。它不是 422
   `unsupported`：客户端请求没有错，只是该能力尚未投放。
5. 不为 Qwen-Omni 增加 Native 特例，不更改 `dashscope.compatible` 路径，也不把
   Compatible 证据用于 Native 兑现。

## 兑现集合与分数

本期在 `EndpointOpenAIChat` 精确兑现 8 项：

1. `text_generation`
2. `streaming`
3. `tool_calling`
4. `parallel_tool_calls`
5. `structured_output`
6. `reasoning`
7. `vision_input`
8. `web_search`

设计处置不变，仍为 6 PASS + 3 DEGRADE + 2 REJECT，设计分仍是
`(6 + 3×0.5) / 11 = 7.5/11 ≈ 0.682`。

当前可用集合是 5 PASS + 3 DEGRADE，门可用分为
`(5 + 3×0.5) / 11 = 6.5/11 ≈ 0.591`。未兑现的 `audio_input` 仍计入分母，不能从
路径的可表达能力中抹掉。`dashscope.compatible` 的可用分保持 `8/11 ≈ 0.727`，
因此现有出站偏好顺序不变。

## 真实证据门

严格 fixture 名单从 13 份收窄为恰好 12 份：

- `basic.json`
- `streaming.json`
- `tool_calling.json`
- `vision_input.json`
- `reasoning.json`
- `parallel_tool_calls.json`
- `structured_output.json`
- `web_search.json`
- `combined.json`
- `multi_candidate_nonstream.json`
- `multi_candidate_stream.json`
- `parallel_tool_calls_default.json`

`audio_input.json` 不在本期名单中。严格校验仍需双向对账，不能接受缺文件、多文件、
非 fixture 条目、未脱敏凭据、非法 SSE frame 或不完整 upstream 断言。

录制仍逐项 fail-fast。`combined.json` 必须由一个真实模型在同一次请求中证明 vision、
tools、web search 与 structured output 可组合；如果没有单一真实模型支撑，任务 17
再次停止并回到设计阶段，不得拼接不同模型的证据。

## 运行时行为

路径转正后：

- 只使用上述 8 项已兑现能力的请求可进入 Provider；
- 含 `audio_input` 的请求在矩阵裁决阶段返回 501；
- 501 响应不得触达上游，测试必须断言上游调用次数为零；
- `file_input` 与 `audio_output` 继续按设计返回 422，不受本次修订影响。

已经实现的 Native 音频编解码代码保留。删除它会把“当前缺少投放证据”误写成
“协议无法表达”，并增加将来恢复投放时的无关返工。

## 实施影响

### 任务 17

- 从真实录制用例与严格 fixture 名单中移除 `audio_input`；
- 录制并验证剩余 12 份 fixture；
- 不发起 Qwen-Omni Native 试探调用。

### 任务 18

- `Redeem(EndpointOpenAIChat, ...)` 只列 8 项，不含 `CapAudioInput`；
- 显式路径白名单照常加入该路径；
- 显式能力白名单登记同一组 8 项；
- 精确兑现测试断言 8 项、`AvailableScore() == 6.5/11` 且 `Gated() == true`；
- 重新生成并人工审阅降级矩阵文档。

### 任务 19

- conformance 回放覆盖全部 12 份 fixture；
- 新增 `audio_input` 未投放负例：返回 501，错误分类为 `not_implemented`，上游零调用；
- `file_input`、`audio_output` 的既有 422 负例保持不变；
- golden 数量与 12 份 fixture 双向一致。

任务 18 开始后仍须连续完成任务 19，ADR-0001 窗口不变：真实证据先落地，随后兑现，
立即补齐回放并关闭窗口。

## 验证门

实现完成至少通过：

1. 严格 12-file fixture 校验；
2. 8 项兑现集合、分数与 `Gated()` 聚焦测试；
3. `audio_input` 501 且上游零调用负例；
4. 全部 conformance/golden 回放；
5. `make matrix-update` 后的人工 diff 审阅；
6. `make check`；
7. 有真实凭据时执行剩余真实 smoke，且不得调用已知额度耗尽的音频模型。

## 后续恢复条件

只有同时满足以下条件，才能在未来兑现 `CapAudioInput`：

1. 存在真实可调用、契约兼容的 DashScope Native 音频模型；
2. 请求确实经过 `/api/v1/services/aigc/multimodal-generation/generation`；
3. 录制得到真实、脱敏且严格校验通过的 `audio_input.json`；
4. conformance 回放验证上游请求映射、下游响应与 usage；
5. 同一变更同步更新 `Redeem`、显式能力白名单、可用分测试与矩阵文档。

Qwen-Omni 的 OpenAI-compatible 强制流式契约若要支持，应另立路径设计；它不满足上述
Native 恢复条件。
