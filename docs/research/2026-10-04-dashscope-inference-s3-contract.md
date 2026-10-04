# S3 DashScope Inference 契约核对

日期：2026-10-04。状态：官方文档研究，尚未实施 S3，也没有真实 Inference 调用。
前置 S2 阶段 PR 为 [OpenAI Realtime GA 接线](https://github.com/yobo2u/omugw/pull/18)。
依据：[A 包已批准设计](../superpowers/specs/2026-10-02-same-contract-websocket-passthrough-design.md)。

## 来源与证据层次

首次核对由只读预研完成，2026-10-04 读取当前官方页面；下表是文档事实，不是实录证明。

| 家族 | 官方来源 |
|---|---|
| 总览 | [WS 接入](https://help.aliyun.com/zh/model-studio/realtime-websocket-overview) |
| Paraformer | [接入](https://help.aliyun.com/zh/model-studio/websocket-for-paraformer-real-time-service)、[客户端](https://help.aliyun.com/zh/model-studio/paraformer-client-events)、[服务端](https://help.aliyun.com/zh/model-studio/paraformer-server-events) |
| Fun-ASR | [客户端](https://help.aliyun.com/zh/model-studio/fun-asr-client-events)、[服务端](https://help.aliyun.com/zh/model-studio/fun-asr-server-events) |
| Qwen ASR Streaming | [接入](https://help.aliyun.com/zh/model-studio/qwen-audio-asr-streaming-websocket-api)、[客户端](https://help.aliyun.com/zh/model-studio/qwen-audio-asr-streaming-client-events)、[服务端](https://help.aliyun.com/zh/model-studio/qwen-audio-asr-streaming-server-events) |
| Qwen ASR Message | [接入](https://help.aliyun.com/zh/model-studio/qwen-asr-message-websocket-api)、[客户端](https://help.aliyun.com/zh/model-studio/qwen-asr-message-client-events)、[服务端](https://help.aliyun.com/zh/model-studio/qwen-asr-message-server-events) |
| CosyVoice | [接入](https://help.aliyun.com/zh/model-studio/cosyvoice-websocket-api)、[客户端](https://help.aliyun.com/zh/model-studio/cosyvoice-client-events)、[服务端](https://help.aliyun.com/zh/model-studio/cosyvoice-server-events) |
| Qwen TTS | [接入](https://help.aliyun.com/zh/model-studio/qwen-audio-tts-websocket-api)、[客户端](https://help.aliyun.com/zh/model-studio/qwen-audio-tts-client-events)、[服务端](https://help.aliyun.com/zh/model-studio/qwen-audio-tts-server-events) |
| Sambert | [接入](https://help.aliyun.com/zh/model-studio/sambert-websocket-api)、[客户端](https://help.aliyun.com/zh/model-studio/sambert-client-events)、[服务端](https://help.aliyun.com/zh/model-studio/sambert-server-events) |
| Gummy | [实时识别与翻译](https://help.aliyun.com/zh/model-studio/real-time-websocket-api) |
| 错误/地域 | [错误码](https://help.aliyun.com/zh/model-studio/error-code)、[ASR 模型地域](https://help.aliyun.com/zh/model-studio/real-time-speech-recognition-user-guide)、[TTS 模型地域](https://help.aliyun.com/zh/model-studio/realtime-tts-user-guide) |

## 握手与任务

- 路径 `/api-ws/v1/inference`，握手无 model；真实模型在 `run-task.payload.model`。
  通常为 workspace 的北京/新加坡域名，各型号地域需独立核对；Gummy 页面仍明列公共北京域名。
- header 包含 `action/task_id/streaming`；payload 路由为 `task_group/task/function/model`。
  ASR 为 `audio/asr/recognition`，TTS 为 `audio/tts/SpeechSynthesizer`。
- 两段 101 完成后才有 run-task，再有 task-started；升级前等待 task-started 会死锁。
  网关不可重试边界始终为下游 Accept，不随任务启动移动。
- ASR duplex 上行 binary 在 task-started 后发送；部分模型的 continue-task 用于 context，
  不是音频。CosyVoice/Qwen TTS duplex 通过 continue-task 文本并下行 binary。
- Sambert 明确 streaming=out，所有文本在 run-task.input.text，无 continue/finish 前提，
  服务端自行 task-finished。不得把全部 Inference 写死为 duplex。
- finish-task 后仍接收尾音/结果至 task-finished；正常结束可再用新 ID，未文档化并发多 task。
- task-failed 的 code/message 在 header；保留原消息后结束。各族未统一规定真实 close
  时刻/码，不能从错误码列表中 Realtime 的例子猜 Inference 鉴权/额度，也不能在101后重拨。

## 用量原单位与可信边界

只读取约定的 `payload.usage`，不递归搜 output 同名字段，不以时间戳或音频长度代计费。

| 契约 | 字段/位置 | 已知口径 |
|---|---|---|
| Paraformer/Fun-ASR | result-generated.duration；非句末 null，终态示例 null | 秒；“当前任务计费时长”，是否句级或累计尚需补证 |
| Qwen 3.0 ASR Streaming | result-generated/task-finished.duration | 明确累计秒 |
| Qwen 3.1 ASR Streaming | 同两事件 input_tokens/output_tokens/total_tokens | 明确累计 token，total=input+output |
| Qwen 3.1 ASR Message | 同上并保留 duration | token 累计；duration 是兼容时长，不可误当额外计费单位 |
| CosyVoice/Qwen 3.0 TTS | sentence-end 和 task-finished.characters | 明确截至当前累计字符 |
| Qwen 3.1 TTS | 同上 token 三字段 | “本次请求”，中途句级与终态重叠口径尚需核实 |
| Sambert | result-generated/task-finished.characters | 明确累计字符，不要求 sentence-end 子事件 |
| Gummy | 示例无 usage 或 null | 无可确认 wire 计量口径，价格表不能替代 |

累计数据 6→13→13 只应累计13，不产生三笔请求；字段 missing/null 不等于零，冲突/回退
不允许负增量。未知模型或契约不按后缀猜口径。已取得权威快照保留，不能被后续断开清零。

## 相对旧研究的更正

[旧研究](2026-09-06-dashscope-inference-run-task-websocket-contract.md) 没有 live 证据。
其中 task-started 之前可重试、全端点 duplex、ASR仅秒/TTS仅字符等结论不能继续作为新实现
依据。旧 SDK 的 flush/heartbeat 行为不授权网关代加业务输入；未知合法消息的保全不证明
其业务语义已验证。

## 待设计闭合与真实证据

现矩阵列 text_generation/streaming/audio_input/audio_output/speech_synthesis/speech_recognition
六项，实施前须审清 text_generation 是否确有独立承载，而不是把转录或TTS回显混称通用文本
生成。任务等待/收尾时限、累计指标的记录计数、未知动作的关联保全边界应在 S3 计划明确。

真实验证需独立批次、固定地域/型号/样本与次数时长费用上限。必须包含正常两 task、先完成
再故障、task-failed及实际关闭、duplex ASR/TTS与Sambert out、不同单位累计。原 S1 八次批次
已耗尽不复用。合成 testkit 不替代 live；生产三扇 WS 门继续保持关闭，S3 无 Redeem。
