# OpenAI Realtime S2 条件验收交接

日期：2026-10-04；Task 8 基线 `6764aff`。**离线实现可验收，真实证据阻塞，生产门仍为空。**
真实 OpenAI 调用 0，真实 DS/Anthropic 等替代调用 0。凭据仍缺失；未构造生产成功 fixture。

## 当前实现与投放分开记录

| 范围 | 当前事实 |
|---|---|
| 正式 GA Provider/profile/Build | 前置任务已实现，测试矩阵能显式开门；生产 Build 仍传空门，Mux 未注册 WS URL 返回 404；矩阵未兑现门仍为 PLANNED/501 |
| GA 表达性 | 已完成 15 项对账，设计 PASS 不等于兑现；见 GA 契约 §7 |
| 用量 | 前置任务已完成 response/ASR、tokens/duration、item/part、有界账本、未知与已结分离 |
| 官方 Node SDK | 本地 Node 26.8.1、远端 Node 24.21.0 的 WSS/TLS/正式装配普通与 race 均通过；云端与生产代理未验 |
| 传输回归 | `6764aff` 包含 Accept 缓存前缀和接管后 HTTP reader 取消隔离；保留 EOF→1011、主动 shutdown→1001 |
| 独立录制器 | Task 8 三场景、固定单拨号预算、安全失败落盘、原始轨迹派生候选与完整回放 |
| 真实证据 / 整门兑现 | 缺凭据、地域/权限/费用确认、逐能力实录和内容审核；仍 PLANNED，未改 Redeem/白名单/生产 routes |
| 最终独立审查 / 阶段 PR | `5eaf1e2..e7522fe` 获 Spec PASS（限 S2 离线）；M1–M5 至 `0a533a5` 全部独立复核通过；阶段 PR 已合并至 `ddb72c1` |

**S1 历史状态校正**：PR #16、#17 均已合并，当时 main=`5eaf1e2`，本分支通过
`8ee9710` 已包含该 main。不能继续用“尚未合并”描述 S1。S1 的 8 个录制槽已耗尽，
本次未增加、回收或复用。A/B/C 九条路径的整体完成状态不由 S2 离线检查替代。

## Task 8 验证范围

- 配置默认关闭、凭据冲突、安全头/字段与转义秘密、公开样本源/摘要、链接边界、独占文件、
  删除 run 不回收八槽、输入八秒、输出 token/response/总字节/记录数边界。
- 三种独立字面 TCP 上游脚本成功；全候选经 testkit 有界加载、四点字节/实体/终态与
  实际 TCP 回放。独立录制/候选构造不导入 gateway，合成样例不进入生产 routes。
- 负例：丢配置、错会话/模型/采样率、关闭 ASR、错工具名/唯一 call_id、未复述记忆、
  错输出 item/part、错 ASR item/part、未取消、错 truncate、非法 VAD 时间、finish-only、
  转义秘密、缺对侧 close、篡改原始轨迹；均失败落盘且无成功 candidate。
- 不相关但非空转写只保留原始事实，不进入 speech_recognition Coverage；音频内容、视觉
  与 detail 等未核验项留空。详细 15 项表、预算、官方依据、真实缺口与接口见
  [证据页](2026-10-04-openai-realtime-evidence.md)。

## 检查命令

所有命令均附以下前缀，防止继承任意已有收费开关：

```sh
env OMUGW_RECORD_OPENAI_REALTIME=0 OMUGW_RECORD_DSREALTIME=0 OMUGW_SMOKE=0
```

该前缀后分别执行：

```sh
go test ./tests/smoke -run '^TestOpenAIRealtimeRecorderOffline' -count=1 -v
go test -race ./tests/smoke -run '^TestOpenAIRealtimeRecorderOffline' -count=1
go test -tags smoke ./tests/smoke -run '^(TestOpenAIRealtimeRecorderOffline|TestRecordOpenAIRealtime)' -count=1 -v
go vet -tags smoke ./tests/smoke
```

Task 8 当时结果：普通定向测试 `ok .../tests/smoke 2.274s`，race `ok .../tests/smoke
4.033s`，smoke 定向测试 `ok .../tests/smoke 2.533s`，其中
`TestRecordOpenAIRealtime` 明确 `SKIP`；smoke vet 无输出、退出码 0。完整命令及实际
输出摘录归入任务交接报告。此段保留 Task 8 历史结果；后续全分支检查与审查见下节。

## 最终审查与控制器验收补记（2026-10-04）

- 固定审查范围 `5eaf1e2..e7522fe`；独立报告位于
  `.superpowers/sdd/2026-10-04-openai-realtime-s2/final-review.md`。结论为
  Spec PASS（限 S2 离线、需文档纠正）、Quality Approved with minor fixes；
  M1–M5 分别涉及 Provider 失败收尾、候选样本元数据、ignore、Inspect policy 和阶段状态。
- 控制器已在 **`e7522fe` 代码**完成六项检查，均 **PASS / exit 0**：`make check`、
  `make test-race`、SDK 普通、SDK race、显式关闭 live 的录制器 smoke、smoke vet。
  证据为同工作目录的 `final-check.log`、`final-race.log`、`final-sdk.log`、
  `final-sdk-race.log`、`final-recorder-smoke.log`、`final-smoke-vet.log` 及
  `progress.md` 控制器退出码记录。这些是控制器既有结果，不是本次 fix 的重新运行。
- 最终修复 `0a533a5` 的 Provider/录制器普通与 race、smoke/vet、ignore/文档静态核验
  通过；`e7522fe..0a533a5` 独立复核确认五项全部 ADDRESSED，无新增 Critical/Important。
- [阶段 PR](https://github.com/yobo2u/omugw/pull/18) 已合并，merge=`ddb72c1`。
  [PR CI 37194561346](https://github.com/yobo2u/omugw/actions/runs/37194561346) 的实际 HEAD
  为 `0a533a5`：全库普通/race、矩阵、许可证及 SDK 普通/race 均通过；SDK 日志明确
  `node: v24.21.0`，普通 3.974s、race 5.065s。审批与安全检查通过，无新增行内意见。
  合并后 main 的独立运行另以实际结果记录，不用 PR 检查替代。
- C 包历史 `MarkHomogeneous` 及生成说明仍属 C 接线前债务；A 的同契约证据不授权
  跨协议字节透传。S3 与 A/B/C 九条路径整体交付不在此次离线验收结论内。

## 保留的真实缺口

1. `gpt-realtime-2.1` 的账户、模型权限、地域和费用目标尚未核验；两种 OpenAI 凭据缺失。
2. 当前严格录制脚本没有真实模型行为证据。若有效回显丢字段、改为 snapshot、图片 ack
   省略内容或模型没有在限额内返回完整双工具，保留失败，不自动放宽、补造或重试。
3. 来源声明及 SHA 只能验证本地样本自洽，不能认证公开许可或识别音频内容；需要人工
   溯源/试听/对照。输出 PCM 可从关联原始 delta 提取核验，不能混合不同 item/part。
4. 生产代理部署、真实费用、原生整门 15 项证据及余下路径开发仍待后续。只有真实证据、
   独立审核、矩阵白名单/fixture/生产接线全部满足后才能投放。
