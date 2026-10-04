# OpenAI Realtime S2：独立录制器与证据缺口

日期：2026-10-04。坐标：`openai.realtime → openai.realtime`，
`GET /v1/realtime`。**本次真实 OpenAI API 调用 0；无生产成功 fixture，仍 PLANNED。**
控制器已报告 `OMUGW_SMOKE_OPENAI_KEY`、`OPENAI_API_KEY` 均缺失。本任务未搜索密钥、
未调用其他供应商替代、未使用 S1 的八个已耗尽槽。

## 官方依据与适用边界

- [受版本控制的 GA 契约](2026-10-04-openai-realtime-ga-contract.md)：握手、15 项表达性、
  `max_output_tokens`、reasoning、parallel_tool_calls、嵌套 audio、计量单位。
- [官方客户端事件](https://developers.openai.com/api/reference/resources/realtime/client-events.md)：
  `session.update`、`conversation.item.create`、`response.create/cancel`、
  `input_audio_buffer.append/commit`、`conversation.item.truncate`。
- [官方服务端事件](https://developers.openai.com/api/reference/resources/realtime/server-events.md)：
  本任务于 2026-10-04 重新读取，确认 `conversation.item.added`、`response.output_*`、
  异步输入转写、实体索引及截断确认。日期是检索日期，不是页面发布日期。
- [A 包设计](../superpowers/specs/2026-10-02-same-contract-websocket-passthrough-design.md)：
  独立上游来源、四点预期、整门投放，合成回放不能兑现能力。

特别保留官方结构差别：`response.content_part.added/done.part.type` 参考列
`text/audio`；assistant item 的 `content[].type` 列 `output_text/output_audio`。
录制器只在观测副本关联它们，原字节不改写。若真实回显与本次契约不符，保留失败轨迹，
不猜测兼容映射。`session.finished` 不是 OpenAI 的业务终态。

## 工具入口与预算

实现全部位于 `tests/smoke/openairealtime_recording_*_test.go`；离线代码不依赖 smoke
标签、不导入 gateway/provider。收费入口单独在 `record_openairealtime_live_test.go`。

| 配置 | 约束 |
|---|---|
| `OMUGW_RECORD_OPENAI_REALTIME` | 仅精确 `1` 启用；普通 `OMUGW_SMOKE` 不启用 |
| 凭据 | `OMUGW_SMOKE_OPENAI_KEY` 或 `OPENAI_API_KEY`；同时存在且不同则失败，无值输出 |
| `OMUGW_SMOKE_WS_URL` | 精确 `wss://api.openai.com/v1/realtime`，无 query、fragment、代理地址 |
| `OMUGW_SMOKE_MODEL_REALTIME` | 显式精确 `gpt-realtime-2.1`；不切模型 |
| `OMUGW_RECORD_SCENARIO` | `text-tools-vision`、`audio-manual`、`vad-interrupt` 三选一 |
| `OMUGW_RECORD_OUTPUT` | 绝对路径，仓库 `.local/recordings/openairealtime/<batch>/<run>` |
| `OMUGW_RECORD_AUDIO_SAMPLE` | 两个音频场景必需的本地公开 PCM 元数据 JSON |

单次执行只拨号一次、一个会话。每批最多 8 个独占 `.attempt-N` 槽，失败也占槽，
删除 run 不回收槽；无自动重试、模型切换、batch 切换。录制目录 0700、文件 0600、
独占创建，拒绝根内链接与已有文件；这是本地单用户工作流，不宣称对抗任意并发恶意
文件系统替换。OpenAI 根独立于 DS 根。

- 单会话总期限 60 秒：业务最多 59 秒，关闭最多 1 秒且不超过原总截止。
- 每 response 最大输出 128 tokens，终态必须包含合法且不超限的 `output_tokens`；
  每会话最多 4 个不同 response，不自动重发。此限制不是金额计费上限。
- 输入与输出各最多 8 秒、24 kHz、mono、PCM s16le；音频场景必须逐字段确认输入格式、
  ASR 模型、输出格式及 voice。VAD 样本应含真实停顿；录制器不生成或补录人声。
- 单消息最多 1 MiB；原始 send + receive 合计最多 1 MiB，含 close code/reason；最多
  500 条原始记录。两个方向同时达到音频上限可能先触达总轨迹限额，照实失败。
- 四点派生副本与样本另受 testkit 默认限额校验（轨迹 4 MiB、文件 8 MiB 等），
  不提前把它们计入直连的 1 MiB 原始预算。

样本元数据示意（不是可执行真实样本；摘要必须替换为实际值）：

```json
{
  "file": "public.pcm",
  "origin": "https://example.org/public-audio",
  "license": "CC0-1.0",
  "public": true,
  "format": "pcm_s16le",
  "sample_rate": 24000,
  "channels": 1,
  "transcript": "公开测试。",
  "sha256": "实际文件的 SHA-256"
}
```

只读取本地有界普通文件，验证格式/字节数/摘要和显式公开来源声明；不下载来源 URL。
操作者负责来源、许可证与转写内容的真实性；非空 PCM 或非空转写不能替代内容核验。
不会代生成真人声音、上传隐私内容或挪用 DS 录音。

## 场景与证据规则

1. **text-tools-vision**：第一轮记住公开口令，第二轮独立复述；复述必须在工具结果前
   完成。第三轮同一个 response 返回完整 `test_color/test_shape` 各一次，先验证名称、
   item、非空唯一 call_id、完整参数集合，再依次回传关联结果。第四轮发送程序 PNG，
   `detail=high`，回答图片问题。工具值为绿色/三角形，图片为红圆/蓝方，避免工具结果
   冒充视觉识别。会话逐字段确认 `reasoning.effort=minimal`、并行开关及两个扁平工具定义。
2. **audio-manual**：确认 GA 24k 格式与 ASR、显式 `turn_detection=null` 后发送样本，
   真实 append/commit/committed 后显式 response.create。输出 delta 必须关联已打开的
   response/item/output_index/content_index；输入转写按 committed item 的 part 0 等待，
   可以晚于 response.done。转写文本仅去空白/标点后与元数据逐字比较，不做模糊匹配。
3. **vad-interrupt**：确认完整 server_vad 配置；真实 started/stopped/committed 同 item，
   时间戳合法且有序。收到活跃响应的关联音频才发送带 response_id 的 cancel，并要求
   同 response 的 cancelled。随后单独发送 truncate（已接收音频的 item/part，未播放，
   `audio_end_ms=0`），等待独立且相同坐标的 truncated；最后等齐输入 ASR。

所有场景都检查会话 id/model、响应唯一性、输出 item 和 part、真实终态及关闭。
当前录制器保守要求回显 model 精确等于请求名；官方可能返回 snapshot，这种情况将
留失败原始轨迹，需人工核对官方别名关系后独立修订，不能自动接受相似模型名前缀。
这比生产 Provider 的通用别名观测范围更窄。

候选重新核验整个原始脚本和后继证据，不信任保存的成功标志。每个原始方向派生一对
四点节点：`authored/upstream-accepted` 或 `recorded/golden`，后者不是云端 socket
抓包；本地 TCP 脚本标 `synthetic-negative`，各点 source=`synthetic`。
严格字节匹配包含原始实体 ID，不进行动态 ID 宽松归一。close 回应必须实际观察；
回放只取主动 close 与对侧接收一对，避免与 transport 自动回应竞争。
VAD 的候选 outcome=`interrupted`，cancelled 保留为原消息字面断言，不改成 completed。

失败优先独占写入 `recording.json`（以及输入样本）；只在全部证据和 testkit 验证通过后
写 `candidate.json`。丢配置、错误实体、finish-only、EOF、无 close 均不补造 ack/终态。
安全拒收的消息不落盘，只保留固定失败分类；不会通过改写负载伪装成原始录制。
头只保留 Upgrade/Connection，逐 JSON token 拒绝秘密字段、重复键和转义秘密，
也检查样本元数据及 close reason；不输出底层错误、鉴权头、错误 body 或密钥。

## 15 项逐项证据表

所有行的**真实证据均缺失、生产兑现均未进行**。下列“候选”规则只有在未来真正录制
成功且人工审核后才有真实意义；当前测试只是独立字面 TCP 脚本和完整 testkit 回放。

| 能力 | 官方承载 / 已实现的离线验证 | 候选 Coverage / 真实缺口 |
|---|---|---|
| text_generation | output_text delta、同 item/part 终态文本一致 | 可按关联序列列出；缺真实文本轨迹 |
| streaming | 关联非空文本 delta 与终态 | 只据文本序列；缺真实分片 |
| tool_calling | 扁平 tools、完整调用及 call_id 结果 ack | 完整工具往返后可列；缺真实调用 |
| parallel_tool_calls | 有效配置 true、同轮两个不同 call_id | 同轮集合后可列；缺真实并行结果 |
| reasoning | session.reasoning.effort=minimal 的实际回显 | 可列配置采纳，非思维链质量证明；缺真实回显 |
| vision_input | 独立 input_image item + 程序 PNG | 留空；需人工核验图片回答 |
| image_detail | 每图 high + item ack 原字节 | 留空；需核验真实采纳及逐图契约 |
| audio_input | 明确 PCM 输入、提交及 ASR 关联 | 留空；ASR 不证明生成模型听到了相同内容 |
| audio_output | 有效输出格式、关联 PCM delta | 留空；需独立试听、内容核验 |
| speech_synthesis | marin、audio modality、PCM 输出 | 留空；非空音频或 transcript 不等于合成内容一致 |
| speech_recognition | 有效转写配置、同 item/part completed | 仅元数据转写规范化精确一致才列；缺真实音频/转写核验 |
| stateful_conversation | 独立第二轮、工具结果前精确复述口令 | 可列；缺真实跨轮记忆 |
| realtime_session | created 身份及 updated 逐字段采纳 | 可列；缺真实有效配置与 close |
| realtime_server_vad | 有效配置、同 item started/stopped/committed | 可列；缺真实 VAD 时序 |
| realtime_interrupt_turns | 活跃音频 cancel→cancelled，再独立 truncate ack | 可列；缺真实取消/截断 |

Coverage 是指向原始关联序列的待审核账本，不是来源认证、生产 Redeem 或内容裁判。
整门仍需全部 15 项的真实证据映射；本文件不扩展为其余八条路径完成声明。

## 本次命令与后续真实运行条件

离线检查均显式禁 live，执行结果见 [S2 验收](2026-10-04-openai-realtime-s2-acceptance.md)。
本任务不启动真实运行。后续须先确认账户/项目、所在地域及出站地域适用性、
`gpt-realtime-2.1` 调用权限、收费目标/允许金额、公开音频来源与安全可用凭据。
凭据存在位不能替代以上地域/权限核验。确定一个固定 batch 后，每次只选一个场景，
即使失败也消耗槽；追加批次需要另行授权，不复用 S1 槽。

批准并安全注入凭据后，收费入口为 `go test -tags smoke ./tests/smoke
-run '^TestRecordOpenAIRealtime$' -count=1 -v`，同时显式设置上表全部必要变量。
密钥不放命令行、不在报告中输出。这个命令**本次未启用执行**。
