# S1 DashScope Realtime 接线阶段验收

日期：2026-10-04。当前代码至 `d15f60f`；本批基线 `2704fd7`。
任务级独立复核全部通过，最终跨层复核 `2704fd7..0a0976f` 也通过：无Critical/Important/Minor，
结论为可合并的S1接线阶段条件验收。记录描述代码与证据，不表示整门投放。

## 已完成与实际边界

- 配置限额、StreamProvider、鉴权与握手协调、整门测试装配、双向原字节relay、用量账本和关停。
- 只读token/字符分单位计量；同ID终态去重，断开不清零已有权威值；受控内存与关闭原因归还。
- 直连录制器默认关闭；北京、每次一会话、60秒、八次不可回收尝试槽、非个人测试内容。
- 本批八次尝试全部用完：五份失败保留；两种TTS和三轮文本双工具取得三份完整真实轨迹。
- 三份原始recording与candidate原字节归档，固定调度下经正式Mux/Handler/Provider离线回放；
  鉴权替换、所有应用消息/终态/关闭、双单位指标、worker退出和预算归零均有断言。
- `make check`通过；控制器在`d15f60f`执行全库`make test-race`通过。smoke标签离线测试另行通过，
  默认与最终检查均未开启live。

归档：[README及摘要](../../testdata/fixtures/dashscope/realtime/README.md)。
逐节点证据和八次调用详情：[证据账本](2026-10-04-dashscope-realtime-evidence.md)。
用量口径：[usage契约](2026-10-04-dashscope-realtime-usage-contract.md)。

## 未完成的投放前置

`Build=false`，矩阵未Redeem，路径仍PLANNED。真实轨迹位于fixtures，不在routes。

- `audio_input`、`speech_recognition`、`vision_input`、`realtime_image_input`、
  `realtime_server_vad`、`realtime_interrupt_turns`没有本批完整真实闭环。
- `stateful_conversation`未证明：第二轮未在工具结果前复述口令，第三轮答案可能来自工具结果。
- `speech_synthesis`已有短句→PCM/终态，但尚未试听或识别内容，不能写成人工验收通过。
- manual模式的null请求在回显中被省略；录制器保守拒绝确认，未把missing与null全局等同。
- 未执行真实gateway冒烟；没有将本地实现称为上线。S2当前环境缺OpenAI凭据，Anthropic两条
  后续路径也未发现可用环境凭据；只查存在位，没有输出密钥。

这些缺口阻止整门兑现，不妨碍复用已离线验收的传输与生命周期代码继续后续路径。

## 执行裁决与代价（依发生顺序）

1. 完整授权作为已展示方案的执行批准，采用北京8×60秒、短合成素材与10元保守目标。
   实际约束是请求数/时长/输出量，不保证异步账单实时封顶；错误代价为有限费用及重录。
2. TTS字符用量独立指标，不伪造0token或扩张Canonical；依据官方字符计量形状。
   错误代价为计量接口返工。
3. gateway回放先核验唯一session.created，再驱动tail，避免握手/driver互等。
   原fixture不变；错误代价为测试harness返工。
4. TTS仅允许已实录Chinese→chinese的窄回显比较，文本场景不请求不需要的VAD配置。
   音频格式仍严格确认；错误代价为保守失败及有限重录。
5. TTS完成后主动取得真实close回应；同时出现且合法的字符与token分组校验、分单位发布。
   不由计量推算账单；错误代价为录制/观测返工。
6. 增加窄transport BeginClose接口，共享关闭帧发送权及绝对收尾期限，接受普通/race和独立复核。
   错误代价为传输局部回归；一秒是I/O预算，不承诺任意调度下零尾延迟的硬实时墙钟。
7. 自动提交以finish前真实同实体非空音频证明自动启动，允许finish后完成；文本作者省略可选item.id，
   严格核验串行pending的实际ack。错误代价为录制见证返工，不能洗白finish-only或错实体。
8. 已完成真实轨迹先归档fixtures、正式handler测试装配回放；prelude后tail以原录制字面ID严格匹配。
   错误代价为测试/归档返工，不代表生产转正。
9. 最低预算补入双完整消息之外的 `min(M,32KiB)` 掩码工作区，配置与实际分配共用计算。
   错误代价为旧极限配置需多配最多32KiB；默认值不变，聚合和临时扩容仍实际计额。

## PR阶段追加修复

PR：[S1接线与真实轨迹离线验收](https://github.com/yobo2u/omugw/pull/16)。

- 首次远端CI `37182264303` 的race暴露取消回调竞争：读者先观察取消并取得CAS后，
  遗漏物理关闭。`008cbef`补齐读者负责的abort；真实TCP闩锁在普通/race下先失败再通过。
  没有延长等待、跳过测试或把平台差异当结论。独立差异复核通过。
- 外部复核两项P2在 `d7c27c3`修复并独立复核通过：未完成分片后的合法close仍保留线上码/原因，
  本地按中断结算而非成功；最低预算包含真实掩码工作区。七个受影响包普通/race及make check通过。
- 新提交的远端CI以PR实际检查结果为准；首次失败记录不被本地通过覆盖。

## 后续交接

补足剩余能力需要新的明确有界录制方案；本批八槽不得回收或换batch绕过。
原则修订已独立评审；最终审查确认生产关闭状态、真实证据与离线实现边界一致。
远端发布状态以PR及CI实际结果为准。
本批ignored协调工件先保留，防止后续会话丢失待补证据与审查交接。
