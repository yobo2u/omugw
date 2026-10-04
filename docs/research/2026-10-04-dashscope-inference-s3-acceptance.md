# S3 Inference 音频子阶段：条件验收与证据缺口

日期：2026-10-04。**本音频子阶段已在合并前通过离线条件验收；PR #19 合并后出现大消息关闭取证故障，其补丁已独立复核通过；随后控制器在 `1a554eb` 全库检查发现 Inference drain 的 task-failed 后 close EOF。当前共享关闭协调器修复已通过本地定向及相关包普通/race，仍待控制器独立复核、全库/SDK回归及修复 PR/远端 CI。真实Inference调用0次，整门仍未投放。**
范围仅 `audio/asr/recognition` 与 `audio/tts/SpeechSynthesizer`。
依据：[批准设计](../superpowers/specs/2026-10-04-dashscope-inference-s3-audio-design.md)、
[契约研究](2026-10-04-dashscope-inference-s3-contract.md)、[独立录制指南](2026-10-04-dashscope-inference-s3-recording.md)。

## 证据分层

| 维度 | 当前证据 | 仍缺什么 |
|---|---|---|
| 文档事实 | 握手无model；run→started；ASR duplex、TTS duplex、Sambert out；原单位及部分累计口径 | 文档不证明该key/地域/型号真实可用 |
| 合成正式链路 | 正式Build/Provider/Handler/Policy/Relay本地TCP、逐任务及全分支修复复审通过；含无ready预读、鉴权替换、双向字节、门闩、失败/关闭、共享预算、一次结算；合并前HEAD全库check/race通过，合并后关闭取证故障与本次回归见下节 | 本地对端不替代真实上游；修复 PR/CI 待验 |
| 独立录制器 | Task7本地脚本独立字面期望；六场景、单次持久Dial、预算、重复键/partial usage、SHA/原文对账、EOF/peer/local/silent分离 | 不含真实上游材料；smoke tag仅选择offline测试 |
| live支持 | **未测，调用0** | 显式北京workspace端点、非个人固定样本、完整计费依据/manifest及控制器单槽实验 |
| 原生保全 | 合成五份testkit fixture在正式链路四点严格回放；不是网关生成自己的预期 | 独立真实录制、内容审核与真实轨迹回放；未知字段保全不等于语义验证 |
| 计量 | 合成累计delta、终态/中断保留、计量单位隔离；独立录制器只识别wire快照、不推算费用 | 真实seconds/characters/tokens累计轨迹、服务端计费取整及最终费用；未证实型号仍unavailable |
| 投放 | 默认Build三扇WS门仍404，默认Inference矩阵501；无Redeem | 整门六能力证据、控制器最终关口与后续明确投放决策 |

Task6的五份机制样例在 `testdata/testkit/ws/inference-audio/`，来源为 `synthetic-negative`。
Task7没有增加 `testdata/fixtures/dashscope-inference/`，普通测试不要求这个待录目录存在。
真实来源标签、摘要和四点candidate也只构成自洽账本，须人工审核；101、价格表及合成成功不补真实支持。

## 六项能力逐项缺口

| 能力 | 文档/合成机制 | 真实支持 | 原生保全与计量缺口 |
|---|---|---|---|
| text_generation | multimodal-dialog有独立生成契约，本子阶段明确拒绝该型号与非audio三元组 | 未测，未实现该子契约 | 不能拿ASR转录、翻译或TTS回显顶账；独立状态机及fixture未完成 |
| streaming | duplex与out脚本/正式接线合成通过 | 未测 | 真实时序、尾消息与有界关闭尚缺 |
| audio_input | started后上行PCM、消息预算、字节保全合成通过 | 未测 | 固定公开样本与真实识别结果未核；传输字节不认证识别内容 |
| audio_output | TTS尾binary与finish后继续接收合成通过 | 未测 | 真实音频、格式与内容待核；非空binary不证明合成质量 |
| speech_synthesis | TTS duplex/out、字符及异常音色脚本合成通过 | 未测 | 精确型号/音色、真实characters累计及failed→close/EOF/silent |
| speech_recognition | ASR seconds/token快照、同连接两task、先完成后中断合成通过 | 未测 | 真实转录、累计用量和第二task中断证据；token费用硬上界仍缺 |

不缩小ExpressibleSet，不兑现任何S3生产能力；原S1/S2固定终态账与实验ledger保持独立。

## Task7的有界与证据验收

录制器仅依赖标准库、transport及testkit，不import被测gateway/Inference Inspector/Provider。
普通构建没有live拨号入口，实际云端Dial封装仅在smoke tag文件；专用开关还必须精确为1。
费用入口按本槽CostEvidence核验完整结构、精确身份和有理数/ceil算术；未知其他槽不阻断已证明槽。
来源摘录真实性/适用性仍由控制器核验；没有实际来源材料时拒绝该槽，正整数或verified布尔不能代替证据。
离线正例含显式SYNTHETIC来源载体，覆盖真实配置解析/reserve路径，不是云端证明或新增费用授权。

离线断言覆盖：固定六槽/10task/270秒/100分/45秒，固定音频/文本上界、独占目录与拒链接、
并发reserve唯一、失败占槽不退款、输出删除后不能二次Dial、样本摘要变更零Dial、
六场景因果、真实close与本地write分别记录、EOF与静默不补造peer close、
终态后抢先close竞争、关闭期间安全拒绝不能裁剪成candidate、原始与候选分开预算、raw/candidate逐行对账。
Fix round1另覆盖failed尾部model/task/streaming重申、重复failed的累计/presence冲突；矛盾raw保留而candidate unavailable。
1024记录满时真实本地peer不得收到额外close；关闭额度在BeginClose前检查，失败强制释放/join，预留不记作已发送。
Fix round2修复N1：完整manifest按含HTML转义的紧凑编码≤64KiB准入，recording摘要同编码且独立≤65KiB。
固定摘要开销≤984B（见录制指南的逐项证明）；六项完整合成费用材料在边界值下，失败握手及本地正常捕获均保存全部摘录、raw和摘要。
源文件虽小但HTML转义后超限的输入在reserve/Dial前拒绝；最长时间/整数/Outcome等固定字段包络亦有保存断言。

Task7初次提交的历史输出（485e195；不代表fix round1复核）：

```text
$ go test ./tests/smoke -run '^TestInferenceRecorderOffline' -count=1
ok github.com/yobo2u/omugw/tests/smoke 6.545s
$ go test -race ./tests/smoke -run '^TestInferenceRecorderOffline' -count=1
ok github.com/yobo2u/omugw/tests/smoke 8.889s
$ go test -tags=smoke ./tests/smoke -run '^TestInferenceRecorderOffline' -count=1
ok github.com/yobo2u/omugw/tests/smoke 6.963s
$ go vet ./tests/smoke
（无输出，exit 0）
$ git diff --check
（无输出，exit 0）
```

这些检查仅代表Task7，**不代替下面的全分支与SDK关口**。录制器没有使用真实key、公网请求或既有实验ledger。

Fix round1最后一次代码修改后的历史验证（I1/I2/I3现已独立复审标为ADDRESSED）：

```text
$ go test ./tests/smoke -run '^TestInferenceRecorderOffline' -count=1
ok github.com/yobo2u/omugw/tests/smoke 17.915s
$ go test -race ./tests/smoke -run '^TestInferenceRecorderOffline' -count=1
ok github.com/yobo2u/omugw/tests/smoke 22.559s
$ go test -tags=smoke ./tests/smoke -run '^TestInferenceRecorderOffline' -count=1
ok github.com/yobo2u/omugw/tests/smoke 19.102s
$ go vet ./tests/smoke
（无输出，exit 0）
$ git diff --check
（无输出，exit 0）
```

费用测试包括本槽可达且其他token槽未知、两种ceil范围、2^53边界、缺证/假verified/错绑/不够费用/量与费溢出、
补证明不重置批次、Dial前证据封存与共享分币预算不可提高。成功数据均为SYNTHETIC，不认证真实计费材料。

Fix round2最终定向验证：Metadata/MetadataBounds及Evidence中raw对账、终态篡改与分开预算、合法raw超candidate预算、凭据不落盘四例，
普通`go test`通过（1.483s），`-race`通过（3.099s）；`go test -tags=smoke ./tests/smoke -run '^$' -count=1`
通过（0.986s，未执行测试），`go vet -tags=smoke ./tests/smoke`无输出/exit 0。N1已由控制器同范围独立复审判定PASS（合并前事实）。

## 控制器最终关口

控制器在固定 `fb57dcc` 实际运行并核对全部输出，以下均退出0：

| 检查 | 结果 |
|---|---|
| `make check` | fmt/vet/全库普通/矩阵及生成文档同步通过；gateway 11.266s |
| `make test-race` | 全库通过；gateway 14.503s、transport/ws 6.780s |
| `make test-sdk` | SDK本地WSS/TLS通过，3.952s |
| SDK同一筛选 `-race -tags=sdk -count=1` | 通过，5.014s |
| 录制器 `^TestInferenceRecorderOffline` 普通 / race / smoke-tag | 分别8.829s / 11.093s / 8.867s，全部通过 |
| `go vet -tags=smoke ./tests/smoke` | 通过 |

本地Node为`v26.8.1`；`npm ls --offline`确认`openai@7.27.0`、`ws@8.21.0`与锁定版本一致。
执行环境显式移除上游API Key及录制/冒烟开关；只选择离线测试，没有运行裸`make smoke`或真实录制入口。
部分未变包的Go结果来自有效缓存，SDK与录制器定向均`-count=1`；不把缓存标为重新实跑。
逐命令完整输出保存在本阶段交接目录的`final-fb57dcc-*.log`及`final-fb57dcc-checks.json`。

跨Task接口/所有权扫描已完成。全分支终审在`fb57dcc`发现：policy先封口、后向唯一终止器认领
业务失败，另一方向内部`context.Canceled`可抢占结果，导致`task-failed`丢失和误1001。
真实policy×relay的failed及Expire屏障反例已复现；现有通过的套件没有覆盖这个窗口。
最终修复`8042408`使用独立内部封口哨兵；relay只退出该worker，由原End/写错owner认领，
真实context取消/shutdown仍照常。四类屏障（failed/Expire/reject/started写失败）实际RED→GREEN；
另补G内尾消息/真实close、真实取消先赢、共享期限与资源归零反例。消息工厂的opcode准入和
活动拒绝测试断言两项Minor也已修复。独立复审判定I1/M1/M2全部ADDRESSED，无新问题。

控制器在最终代码`8042408`重新执行必要回归，全部退出0：

| 检查 | 新HEAD结果 |
|---|---|
| `make check` | 通过；gateway 11.423s、transport/ws 6.133s、录制器包10.594s |
| `make test-race` | 通过；gateway 14.558s、transport/ws 6.808s、录制器包13.131s |
| `make test-sdk` | 通过，3.944s |
| SDK同一筛选 `-race -tags=sdk -count=1` | 通过，5.024s |

录制器源码未被最终fix修改，先前专门smoke-tag/定向检查记录继续有效；上表全库普通/race也
重新覆盖其与共享transport的组合。未将未变包的缓存结果写成重新执行。新日志为
`final-8042408-*.log`与`final-8042408-checks.json`。
本文件记录本地验收时点；PR、远端CI及合并证据以关联工作项的阶段收尾评论为准。
真实费用/端点/样本仍须落实，现有缺口如实保留；当前本地通过不构成整门投放证据。

## PR #19 合并后故障与修复时点

PR #19 已合并为 `4268fe3`。main CI `37207997625` 的 Ubuntu `make test-race`
在 `TestOpenAIRealtimeBinaryLargeAndTotal` 失败：超限负载发送报 broken pipe，随后
消息级读取未取得预期1009。此前本地和PR通过不能覆盖本次真实失败。

在该基线的新分支 `fix/ws-oversize-close-evidence-20261004` 上，本地TCP屏障已复现：
先排入真实ping，超限帧头触发网关关闭后再发送负载，完整发送与自动pong分别报写错误。
二者不是同一缓存错误；独立帧读取仍取得 `1009 / message too large`，relay已join且预算归零。
根因是测试把拒绝后的消息级读取当作独立关闭证据；未发现该窗口中网关漏发close。

本次仅修改测试：健康大消息/HTTP total回环保留，另连接只发1 MiB+1的掩码长度头，
要求网关在没有负载时拒绝；固定三秒期限内被动越过控制心跳，严格核对实际1009与原因，
不接受EOF/broken pipe替代。永久屏障回归固定失败发送窗口，原读取断言实际RED，新帧断言GREEN。
永久例在网关关闭完成后半关闭测试端写侧，排除各内核暂收剩余负载的差异；初始根因探针未作此半关闭。
自然退出后检查registry/预算归零；生产的超限预分配拒绝、共享关闭预算与worker join保持原实现。

本次环境为 `go1.25.0 darwin/arm64`，下面的测试命令均使用 `-count=1`，完整日志与RED证据位于
阶段交接目录的 `postmerge-*.log`，分析见 `postmerge-fix-report.md`：

| 检查 | 本次本地结果 |
|---|---|
| 指定大消息例与新增屏障例普通 / race | 通过，1.332s / 1.925s |
| `go test ./internal/gateway ./internal/transport/ws -count=1` | 通过，11.407s / 5.715s |
| 同两包 `-race -count=1` | 通过，14.536s / 6.915s |
| `go vet ./internal/gateway ./internal/transport/ws` | 无输出，exit 0 |

本次修复PR、远端Ubuntu CI和控制器独立复核仍待验；此表不宣称合并后全库/SDK或远端CI通过。

## `1a554eb` 收尾检查：drain 的本地错误后 close EOF

控制器 `make check` 在 `TestInferenceHandlerLifecycle/绝对阶段超时/drain`
的 `inferenceLocalFailure` 成功后，于下一条严格 close 断言读到 EOF（0.17s）。
原配置 Connect=80ms、FirstByte=Idle=160ms、Total=300ms；本次保留原配置及
`1011 / task_drain_timeout`，也保留三个阶段的持续心跳与绝对期限断言。

源码与真实 TCP 屏障证明另一处生产竞争：`closeWSConnections` 调用
`CloseWithResult`，后者遇到 heartbeat 的写锁会立即强拆。屏障先交付本地
`Gateway.TaskDrainTimeout`，再固定 Ping 持锁，旧代码在尚余约79ms关闭预算时
直接 EOF；这不是测试端自动 pong 读错。隔离反例没有传输 idle，故其强拆不能归因
于 idle 与业务 timer 同时到期。原控制器日志未记录栈与帧，本证据不伪称恢复了那次调度。

最小修复让共享协调器改用已有 `BeginClose(code, reason, deadline)` 并调用 finish：
在相同绝对期限内等锁、发送并物理释放；真正阻塞写仍由原守卫到期强拆。
W/B/G/Dall 及只收紧规则、本地/原文先交付、peer与业务分槽、Cancel/Stop顺序保持。
新增 `TestWSTerminationLocalFailureHeartbeatClose` 的 released/stalled 两例：前者
解除门闩后被动核对 Ping 与精确1011，后者核对到期强拆；二者都 join 工作者并检查预算0。
20ms观察窗口只检验剩余B内不得提前强拆，不充当关闭预算或新增调度容差。

最终测试版本回退生产修复后再次实测 RED：released 为“剩余79.54ms即强拆”与EOF，
stalled 为“剩余79.94ms即强拆”。恢复修复后的新结果（均 `-count=1`）：

| 检查 | 结果 |
|---|---|
| 新屏障例 + 原三个阶段超时例普通 / race | 通过，0.968s / 2.062s |
| gateway + transport/ws 普通 | 通过，13.756s / 5.731s |
| gateway + transport/ws race | 通过，16.954s / 6.949s |

完整命令、RED/GREEN输出、后续check结果及提交记录见交接目录的
`postmerge-drain-report.md` 与 `postmerge-drain-*.log`。改变共享生产关闭路径的代价是
争锁时可等待至既有B而非立即退出；控制器须独立重做全库/SDK回归，远端Ubuntu仍待验。

## 未完成项与暂停

multimodal-dialog、听悟、text_generation独立承载、其余型号精确计量、Paraformer/Fun-ASR累计语义、
Qwen3.1 TTS中间/终态重叠口径、真实全部六槽材料及生产整门投放均未完成。
网关120秒draining政策可能截断大文本积压，短录制场景不覆盖此生产边界。

**本音频子阶段已完成合并前实现、独立复核与控制器本地验收；PR #19 合并后仍须完成上述同阶段故障复核及修复PR/CI收尾，再暂停。**
整个S3、A包和三门生产投放仍未完成，不自动进入下一子契约，也不因条件验收修改Redeem或生产路由。

## 执行裁决（按作出顺序）

1. **复用隔离工作树、切S3独立分支后连续实施。** S2先独立远端验收，S3包含其合并祖先；
   错误代价是同步合并祖先及回退设计修订，不改其他工作树中的既有工作。
2. **本阶段只完成ASR/TTS子契约，保留六项矩阵声明与PLANNED。** 同URL还有多模态交互、
   听悟等不同状态机；不以删掉text_generation降低门槛。错误代价是后续扩展再设计和整门重新对账。
3. **按自审七任务计划执行；非正常合法close保全原码，同时记安全失败。** 避免原草案恒nil
   分类把1011误记成功。错误代价是分类/接口兼容回归，由协议、共享关闭和正式链路三层验证。
4. **Task1内存问题修复扩至共享JSON字段扫描器。** 转义键比较不逐键堆分配，保持原语义，
   避免另造scanner。错误代价是S1/S2解析回归，已有转义、重复、UTF-8/代理项及普通/race覆盖。
5. **录制费用门禁采用本槽结构化证据与精确算术。** 完整材料可授权本槽，未知槽拒绝且
   不阻断其他已证明槽；扩充manifest证据载体，不接受裸正整数或verified布尔替代。
   工具核结构、身份与计算，控制器核官方来源语义；当前未取得真实完整材料，调用仍0。
   错误代价是证据格式维护与重新审查，不能把算术自洽当实际计费证明。
6. **内部封口不参与首次生命周期原因认领。** 用独立信号区别真正context取消，仍由原End/
   写失败owner认领；修复已实证的跨模块窗口。错误代价是遗漏producer可能无主等待或吞取消，
   因而逐一核对全部封口路径，补真实取消/停机、期限和join反例，复审与新HEAD回归均通过。
