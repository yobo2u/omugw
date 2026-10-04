# Task 6 自动流收尾与服务端 item 身份修正报告

## 状态与范围

- 状态：离线实现、回归、完整 candidate 回放与自审通过，交回控制器。
- 基线：`62bb8f5e0c1f14b2285db8c83879ff5dfe6d46fb`；当前分支
  `feat/ws-passthrough-s1-20261003`。启动时已执行 fetch / worktree list / status -sb；
  当前 HEAD 与 brief 相符，无 behind。其他工作树为 main、production-store、progress-audit、
  realtime-session-contract 的独立既有分支，本任务沿用控制器分支。
- 改动仅限 `tests/smoke/*dsrealtime*`、evidence 研究文档和本报告。
- 控制器已有的 `docs/superpowers/plans/2026-10-04-dashscope-realtime-production-wiring.md`
  未提交修改从开工就存在，未改写、未暂存。未改控制器 ledger、矩阵、routes 或其他任务文件；
  evidence 表仅同步自动提交的验收顺序，没有改变能力状态。
- 本批 **6/8** 由控制器管理；本次没有真实调用、没有读取凭据、没有派子代理。

## 实录核验

仅只读解码指定 `recording.json`，严格 base64 解码两条音频，并在验证后重算 SHA-256。

```text
tts-server-commit failure=read_failed_or_raw_eof confirmed=True
response_id=resp_QioPULXAcyZPaOHfCkJqk decoded_bytes=9788
response_id=resp_QioPULXAcyZPaOHfCkJqk decoded_bytes=9786
tts-server-commit bae6fb047a6957689519c09eb577c264d73bacdaac902d89bce69e7860698ac4 UNCHANGED
text-tools-v2 da1b0c6477c4f62202ecce03aeae623f9f7aac70e49aa9a4b85d8e5adea15951 UNCHANGED
```

文本实录请求 `record_user_1`，ack 为 `item_Ji44D7s85jVY5djzY5naZ`；
message/user/input_text 与固定文本一致，新增 object/status。原始 ack 字面副本用于离线
TCP 回归，candidate 检查其原字节及真实 ID bind。官方本地 raw
`model-api-reference/omni-realtime-api/client-events.md:511-525` 明确 item.id 可选，
call_id/output 必选，工具结果只省略可选 ID。

## 实现与证据边界

1. `dsAutomaticCommitProgress` 统一解释前缀与完整链：确认 server_commit、append、自动
   created、同 ID 合法非空音频之后才允许 finish。done 可先于或晚于 finish，但必须同 ID、
   completed，先于 session.finished；后继见证和 Coverage 还要求正常 receive close。
   显式 commit、finish-only、提前 finish/终结、跨 ID、无效音频、重复 finish、缺 done /
   finished / close、异常 close 都拒绝。
2. `dsDriver.tts` 在自动提交场景等待首个合法音频后发送一次 finish，继续收齐终态与
   finished；物理 close 使用原有握手。显式 commit 场景仍等待 done 再 finish。
3. 作者的两条文本消息和两条工具结果省略可选 item.id。`dsItemAcks` 同时用于驱动与候选，
   串行维护唯一 pending，匹配 type/role/content 或 type/call_id/output，绑定非空且未复用
   的服务端 ID。错 ack、重复 ack、重叠 pending 不可推进；显式 ID 仍严格匹配。
   item/content 内新增服务器字段按 subset 核对，数组长度和顺序保持严格，通用比较与
   gateway 消息均未改动。
4. 回归保留实录事件形状，TTS PCM 缩为合成数据；完成尾部、后续工具轮次以及关闭来自
   独立本地 TCP 脚本。原失败录制未补尾、未转换成成功来源，预期没有通过 gateway 生成。
5. `dsReplayCandidate` 对成功路径执行 `NewWSReplayUpstream` + `ReplayWS` 完整离线回放。
   还检查发送次数（正常 4 个 item create / 3 个 response create）、真实 ack 字节、ID bind，
   并对工具 ack 的 call_id/output/type 分别篡改确认 candidate 拒绝。

## TDD 与实际命令输出

### RED：先改测试，尚未改驱动/见证

```bash
OMUGW_RECORD_DSREALTIME=0 OMUGW_SMOKE=0 go test -tags=smoke ./tests/smoke -run '^TestDSRealtimeRecorderOffline(ServerCommitEvidence|TTSConversation|ObservedStreamingFinish|ServerItemIdentity|ItemWitnessPending)$' -count=1
```

退出码 1，实际输出节选：

```text
--- FAIL: TestDSRealtimeRecorderOfflineServerCommitEvidence/finish_before_done
    自动提交Coverage必须先有同响应音频，再finish，并取得同ID终态/finished/正常close
--- FAIL: TestDSRealtimeRecorderOfflineObservedStreamingFinish/streaming
    已自动启动的流没有经finish完成: failure=timeout_or_cancelled bytes=8
--- FAIL: TestDSRealtimeRecorderOfflineServerItemIdentity/server_id
    作者仍指定可选item.id，无法接受实录的服务端生成ID
    错误ack推进或重复提交: counts=map[:1 conversation.item.create:1 session.update:1]
--- FAIL: TestDSRealtimeRecorderOfflineItemWitnessPending/overlapping_pending
    candidate err=<nil>; want valid=false
--- FAIL: TestDSRealtimeRecorderOfflineTTSConversation/tts-server-commit
    ws: 空闲超时
FAIL github.com/yobo2u/omugw/tests/smoke 15.923s
```

其余 RED 同时揭示 finish-only / 跨 ID / 缺 close 等轨迹仍能取得旧的宽松后继见证，
以及旧 item 见证允许缺 ID、复用 ID、重复 ack、错 ack 后再取正确 ack、ack 前推进响应。
首次 RED 的脚本继续执行还出现工具结果关联错误；随后让测试脚本遇连接失败即停止，
防止这些派生错误掩盖真正根因。

窄实现后原定向命令退出码 0：

```text
ok github.com/yobo2u/omugw/tests/smoke 0.904s
```

### 最终检查（均退出码 0）

以下测试命令均显式设置 `OMUGW_RECORD_DSREALTIME=0 OMUGW_SMOKE=0`：

| 命令 | 实际结果 |
|---|---|
| `go test -tags=smoke ./tests/smoke -run '^TestDSRealtimeRecorderOffline' -count=1` | `ok github.com/yobo2u/omugw/tests/smoke 2.715s` |
| `go test -race -tags=smoke ./tests/smoke -run '^TestDSRealtimeRecorderOffline' -count=1` | `ok github.com/yobo2u/omugw/tests/smoke 3.650s`，无竞态报告 |
| `go vet -tags=smoke ./tests/smoke` | 无输出 |
| `make check` | fmt-check、`go vet ./...`、`go test ./...`、matrix 通过；全仓 Go 测试输出为 cached，matrix 两项 PASS |
| `go test -tags=smoke ./tests/smoke -run '^TestRecordDSRealtime$' -count=1 -v` | `未显式开启独立 Realtime 录制`；`--- SKIP: TestRecordDSRealtime (0.00s)`；`PASS` |
| `GIT_MASTER=1 git diff --check` | 无输出 |

## 自审与交接 / concerns

- 已逐项检查新证据链的正负例、pending 生命周期、原字节保留、正常关闭要求和提交范围。
  原有物理 close / 59 秒业务与 1 秒收尾预算 / 八次槽位 / 北京 / 输出限额 / 隐私回归继续通过。
- 无已知离线检查失败。上游是否在本次 finish 后给出完整同 ID done/finished/正常 close，
  以及省略 ID 后真实双工具闭环能否完成，仍需控制器后续实录；本报告不把合成成功升级为
  真实能力证据。当前两份失败实录继续原样保留。
- 控制器若继续，只能使用本批剩余至多两次额度和新的 run 目录；这里没有发起任何重试。
  后续先检查完整尾部与原始 ack，再更新控制器自己的 ledger/能力状态。

## Review Important 追加修正：工具集合必须完整预检

FixBASE：`1eb1f4c`。开工重新完成 fetch / worktree list / status -sb，HEAD 符合基线；
原有 plan 修改仍由控制器持有。本次仅改录制器相关 smoke 文件并追加本报告，未 live、
未读取凭据、未派代理、未修改录制/ledger/plan/矩阵；6/8 额度继续由控制器管理。

### 根因与修正

旧循环只有 `seen[name]`，且边校验边发送。`test_color` 与 `test_shape` 共用 `call_a`
时会发送 `call_a=蓝色` 和 `call_a=方块`，继而发起第三轮；第二项或尾部非法也可能已发送
第一份甚至两份结果。

- 驱动先调用 `dsValidatedToolResults` 校验整个 completed response.done.output，成功后
  才进入发送循环：两个预期名称各一次、call_id 非空且唯一、工具为 function_call 对象、
  arguments 为可解析的 JSON 对象字符串。缺项、重复、未知工具、错误类型/参数均返回
  `invalid_tool_set`，保证第一次 function_call_output 前失败。允许同轮合法文本消息。
- candidate 结果见证和工具 Coverage 共用同一集合校验；见证另外验证结果属于最近一轮
  有效集合、内容映射正确且不重复，再核对真实 item ack。自洽 ack 不能洗白历史错误集合。
- 永久反例覆盖同 call_id 不同名称，以及第二项/尾部多种损坏，逐一断言 **零结果发送**、
  response.create 仍只有前两次。合法双工具及伴随文本输出保持完整 candidate 回放通过。
  另测历史错误集合的见证/Coverage/candidate 拒绝，及外来 call_id、错结果、重复结果。

### RED（实现前，退出码 1）

```bash
OMUGW_RECORD_DSREALTIME=0 OMUGW_SMOKE=0 go test -tags=smoke ./tests/smoke -run '^TestDSRealtimeRecorderOfflineToolSet' -count=1
```

实际输出节选：

```text
--- FAIL: TestDSRealtimeRecorderOfflineToolSetPreflight/same_call_id_different_names
    非法工具集合已部分发送: results=[call_a=蓝色 call_a=方块] response.create=3
    failure=""; want invalid_tool_set
--- FAIL: TestDSRealtimeRecorderOfflineToolSetPreflight/duplicate_name
    非法工具集合已部分发送: results=[call_a=蓝色] response.create=2
    failure="unexpected_tool_call"; want invalid_tool_set
--- FAIL: TestDSRealtimeRecorderOfflineToolSetEvidence/same_call_id_different_names
    工具结果见证未验证完整工具集合
    工具Coverage与完整集合校验不一致: map[realtime_session:true tool_calling:true]
    candidate err=<nil>; want valid=false
FAIL github.com/yobo2u/omugw/tests/smoke 0.941s
```

### GREEN / 检查（均退出码 0）

以下命令均显式设置 `OMUGW_RECORD_DSREALTIME=0 OMUGW_SMOKE=0`：

| 命令 | 实际输出 |
|---|---|
| `go test -tags=smoke ./tests/smoke -run '^TestDSRealtimeRecorderOfflineTool(Set\|Result)' -count=1` | `ok github.com/yobo2u/omugw/tests/smoke 0.901s` |
| `go test -tags=smoke ./tests/smoke -run '^TestDSRealtimeRecorderOffline' -count=1` | `ok github.com/yobo2u/omugw/tests/smoke 1.720s` |
| `go test -race -tags=smoke ./tests/smoke -run '^TestDSRealtimeRecorderOffline' -count=1` | `ok github.com/yobo2u/omugw/tests/smoke 3.342s`，无竞态报告 |
| `go vet -tags=smoke ./tests/smoke` | 无输出 |
| `make check` | fmt-check / vet / 全仓测试 / matrix 均通过，全仓 Go 测试为 cached |

`GIT_MASTER=1 git diff --check` 无输出。自审确认第一次结果发送位于完整校验之后，驱动、
候选见证和 Coverage 没有独立的宽松工具集合规则。无已知离线阻断；真实闭环仍交控制器验证。
