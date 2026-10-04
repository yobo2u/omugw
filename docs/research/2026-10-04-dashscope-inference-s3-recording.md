# S3 音频子契约：独立有界录制指南

日期：2026-10-04。状态：独立有界录制器；**本阶段真实调用0次，当前缺真实前置材料**。
依据：[audio-design §8.1](../superpowers/specs/2026-10-04-dashscope-inference-s3-audio-design.md#81-控制器独立有界实验方案)、
[最新真实前置事实](2026-10-04-dashscope-inference-s3-contract.md#真实证据与实验前置核对)。

## 当前停止条件

- 已知 DS key 存在，不代表已确定北京 workspace 端点。本次没有提供显式端点、完整manifest或可复核的费用上界。
- 两个 Qwen3.1 token ASR 尚缺服务端最坏输入/输出token上限依据；短音频、45秒客户端截止、轨迹字节上限、价格低及免费额度均不能补此缺口。
- `inferenceValidate`按**本槽**结构化证据校验身份与精确费用算术；缺本槽证据时在读取key、reserve和Dial前拒绝。仅填正整数WorstCaseTokens不能授权。
- 已证明的非token槽可独立通过，未知token槽不阻断它。控制器补齐并核验本槽材料即可经现有入口使用，无须再改行为代码；本次未提供这些真实材料，仍未发起调用。
- 工具核验结构、绑定、摘录摘要和算术，**不自动认证官方文档语义真假或适用性**。离线价格/上限/摘录均显式SYNTHETIC，不能复制成真实费用授权。

长期维护点：精确六槽模型/音色表、经核实的费用依据、独立场景解码器。新增模型、换地域或变更输入必须重新评审，不从可用模型目录或后缀猜测。

## 六槽固定输入

批次只能是 `s3-audio-20261004`，表内每槽只允许一次物理Dial。失败、401、超时、写盘失败均不退款、不自动重试。

| 槽 | 场景 / 精确model | voice | task数 | 本槽输入上界 / 取证目的 |
|---|---|---|---|---|
| 1 | ASR两task / `qwen-audio-3.0-asr-flash-streaming` | 空 | 2 | 各≤3秒，共≤6秒；seconds、串行新ID |
| 2 | TTS两task / `qwen-audio-3.0-tts-flash` | `longanlingxi` | 2 | 各≤40字符，共≤80；continue/finish后尾音、characters |
| 3 | out / `sambert-zhichu-v1` | 空 | 1 | ≤40字符；run包含完整文本，无continue/finish |
| 4 | ASR / `qwen-audio-3.1-asr-flash-message` | 空 | 1 | ≤3秒；token三字段，兼容duration不是第二份计费 |
| 5 | ASR / `qwen-audio-3.1-asr-flash-streaming` | 空 | 2 | 各≤3秒；首task完成，第二task首个有效真实usage后主动中断 |
| 6 | 无效音色 / `qwen-audio-3.0-tts-flash` | `omugw-invalid-voice-s3` | 1 | ≤40字符；原生failed及peer close/EOF/静默 |

表内计划合计9 task，代码仍检查批次上限10 task、270秒、100分、上传音频18秒、TTS240字符。
每槽预占45秒、该槽全部task及WorstCaseFen；六个唯一槽的总和不得超批次上限。一次只指定一槽，由控制器审核后决定下一槽。

输入为同一份非敏感 PCM16LE / 16000Hz / mono `.pcm`，每task至多96000字节，完整16位采样。
SamplePath必须为无链接的规范绝对路径；预检、reserve、capture重复核对实际文件SHA与长度，发送使用已核验字节。
文件扩展名、SHA与长度不能证明内容非个人或真实编码参数，控制器必须核对这两项，不允许暗换样本。

TTS固定两段 `这是网关测试。` 与 `请确认声音清晰。`；duplex分别continue，out在run内连接成整句。
握手无model/query；先run再started，上行binary/continue只在started之后，下一新ID只在前task-finished之后。
槽6若先收到started，则发送同样固定文本；若立即failed，不再追加输入。

## manifest字段与校验

manifest为至多64KiB的普通JSON文件，六项Slots恰好齐全；未知字段、重复键（含JSON转义等价）、未知模型和地域不符全部拒绝。
文件原始字节及解码后`json.Marshal`紧凑编码（含HTML转义）**各≤65,536B**；不能用未转义的`<>&`规避编码预算。
超限在配置/reserve/Dial前拒绝，不消耗槽。保存摘要采用同一紧凑编码口径。

| 字段 | 约束 |
|---|---|
| Batch / Region | `s3-audio-20261004` / `cn-beijing` |
| Endpoint | 显式 `wss://<workspace>.cn-beijing.maas.aliyuncs.com/api-ws/v1/inference`，不接受公共域名、端口、query或fallback |
| SamplePath / SampleSHA256 | 上述绝对PCM路径 / 64位小写SHA256；全槽固定 |
| PriceSource | `https://help.aliyun.com/zh/model-studio/model-pricing` |
| PriceCheckedAt | RFC3339且不在未来；批次初始价格参考日。本槽新鲜度由CostEvidence.CheckedAt校验 |
| WorstCaseFen | 全批，至少覆盖六槽费用之和且≤100 |
| Slots | 固定 `[6]`，每项Slot、Model、Voice、MaxTasks、MaxInputAudioSeconds、MaxInputCharacters、WorstCaseTokens、WorstCaseFen |
| CostEvidence | 可省略或只列已核验槽；至多6项，Slot不重复。当前指定槽必须有完整条目 |

上限数值为int64，MaxTasks严格等于表内数量；音频/字符只允许对应单位、足以覆盖实际固定输入且不超过表内硬界。
被选择token槽的WorstCaseTokens必须为正且覆盖两路最大量乘task数；未选择token槽0仅表示未知，不是通过费用检查。
其他槽为0（不适用）。每槽WorstCaseFen至少1分。预算数字不能代替下述证据，也不代表人民币实时账单。

### 本槽CostEvidence载体

每项字段如下，名称与JSON严格解码一致；未列的`Verified`等字段直接拒绝，不提供人工布尔放行口。

| 字段 | 含义/约束 |
|---|---|
| Slot / Region / Model / Voice / SampleSHA256 / MaxTasks | 与选中槽、固定北京地域及批次样本精确相等 |
| PriceSource / CheckedAt | 固定官方价格页；RFC3339，在预检过去24小时内且不在未来 |
| Unit | 槽1=`seconds`；2/3/6=`characters`；4/5=`tokens` |
| Rounding | `ceil_quantity_per_task_then_slot_fen` 或 `ceil_quantity_per_task_then_task_fen` |
| RoundingSource | 取整规则的来源载体，必须覆盖下述Quantum和分币处理语义 |
| OutputUnbilled | seconds/characters必须有输出不计费来源；tokens不允许此字段 |
| Components | seconds/characters恰好一项input；tokens恰好input/output各一项，不能遗漏一边或重复方向 |

每个Component含 `Direction`、`MaxPerTask`、`PriceFenPerUnit`、`Quantum`、`MaxBasis`、`PriceSource`、`MaximumSource`。
三个数值对象均为 `{Numerator: int64, Denominator: int64}`，两值严格正；单位由Unit给定，价格一律为**分/原单位**，不使用float。
seconds/characters的MaxBasis=`bounded_input`，MaxPerTask精确等于该槽MaxInputAudioSeconds或MaxInputCharacters除以MaxTasks，
按整个允许输入上限计算。tokens的MaxBasis=`server_limit`，两路均需官方服务最大量依据；短音频/关闭时长/响应大小不能替代。
characters/tokens的最大量和Quantum必须为整数；token两路最大量乘task数须无溢出且≤本槽WorstCaseTokens，零上限拒绝。

每份Source均含 `URL`、`Excerpt`、`SHA256`：URL仅接受无query/fragment的 `https://help.aliyun.com/zh/model-studio/…`，
价格及输出不收费来源必须是PriceSource；Excerpt非空且≤2048字节，SHA256为其UTF-8原文字节的摘要。
URL≤1024字节。控制器须核对摘录是否真实、完整、适用于该地域/型号/计量单位；摘要只查文本一致，不能认证其语义。
本指南不填写未知的真实token量、取整规则或单槽费用证明。

精确计算：每个component先取 `ceil(MaxPerTask / Quantum) × Quantum × PriceFenPerUnit`，相加得每task费用。
第一种Rounding计算 `ceil(每task费用 × MaxTasks)`；第二种计算 `ceil(每task费用) × MaxTasks`。
所有中间乘加使用big.Rat/big.Int；最终必须可表示为int64且≥1分、≤本槽WorstCaseFen及全批预算。
不能把“金额小于一分”舍成0，也不能以浮点误差吞掉超过整数分的尾数。若官方计费规则不能用这两种已声明规则表达，当前槽拒绝。

本流程是控制器核验来源后的可复核费用上界估算，不是云账单硬限；工具不会联网查价或为提交者声明的真假背书。

## 持久性、输出与证据

唯一reserve根是仓库下 `.local/ws-recordings/s3-audio-20261004/`；输出只能为其 `slot-1` 至 `slot-6`。
该精确根被.gitignore忽略，`testdata/fixtures/dashscope-inference/`仍可跟踪，只能由控制器审核真实材料后添加。

- `manifest.json`：冻结批次身份和共享预算投影；不含CostEvidence，token最大量归零作为非授权占位。不能改模型、样本、人民币/task/墙钟/音频/字符预算。
- `reserve-N.json`：独占、Sync文件与父目录之后才允许后续操作；含完整选中Slot（包括已证token最大量）、45秒、manifest投影摘要、具体CostEvidence及CalculatedFen。
- 未占槽的费用证据与token最大量可补齐；已占槽证据不能换摘录/摘要/数量，Dial前重新计算并逐字节核对reserve。补其他槽证明不使已占槽失效，不恢复任何次数。
- `dial-N`：实际调用前第二道持久一次性认领。删除/改名输出不复活reserve或Dial；失败不删除这两个文件。
- `slot-N/records.jsonl`：每次应用写完成或ReadOwnedMessage交付后，按原字节、opcode、相对时间及payload SHA逐条刷盘。
- `slot-N/recording.json`：紧凑JSON，独立上限**65KiB（66,560B）**；保留完整六槽费用来源/摘录及绝对Started/Ended、业务Outcome、HTTP状态、原始文件SHA、failed与peer-close时刻、PassiveEnd/TransportEnd等；不含key、响应头或错误body。
- `slot-N/candidate.json` 或 `candidate-unavailable.txt`：完整候选通过独立脚本复核、WSContractDigest、ValidateWSSession、ReadWSFixture后才存在。

目录0700、文件0600、exclusive-create，逐级拒绝链接并复核打开的目录/文件身份；Go os.Root限制路径逃逸。
同一用户主动删除整个私有批次账本仍能破坏本地约束：这不是外部不可篡改的账单系统，控制器不得这样重置批次。
只操作S3根，原S1/S2工具、八槽ledger与计量账本均独立保留。

摘要预算证明：manifest清空SamplePath/Endpoint只会缩小，其他路径均不序列化；路径/摘录的HTML转义已计入64KiB准入额度。
摘要19字段的键/标点共223B；9个固定ASCII/hex字符串（Outcome/Scenario/Model/Voice/SampleSHA256/TransportEnd/PassiveEnd/Failure/RawSHA256）
各≤64B，加引号共≤594B；两个RFC3339Nano时间含引号≤74B，四个整数含符号≤80B，Records/Sample两个null共8B，bool≤5B。
因此额外≤`223+594+74+80+8+5=984B`，总长≤`65,536+984=66,520B < 66,560B`。固定预留1KiB，禁止再用缩进扩张；
此界同时覆盖正常和握手失败摘要。来源摘录完整保留，raw独立预算不受此变更影响。

消息≤1MiB、记录≤1024、响应binary≤8MiB/连接、原始payload及编码JSONL均≤16MiB/连接。
输入/发送/复制/编码前检查相应预算；网络reader从握手起限制单消息大小，原始内存只保留有界记录和一个在途reader消息。
这些是载荷/文件预算，不是对整个Go进程RSS的声明。原始超限或安全拒绝会污染证据完整性，不能裁剪后生成候选。
不含秘密的重复键消息可以原样保存为失败材料，但严格场景解码拒绝其成为候选证据。

候选另受默认 **8MiB文件 / 4MiB轨迹 / 1MiB消息** 限制；四点复制也计入预算。超限只保留完整私有raw，不能删事件或修改字节凑数。
保存前逐行比对内存记录与已刷盘raw，防止candidate被贴到另一条证据上。SHA是自洽校验，不认证来源真实性。

## 关闭与计量边界

整会话同一45秒绝对期限覆盖握手、应用读写和关闭；父ctx更早则取更早。单次写≤1秒，无重拨，固定reader与transport守卫退出均join。
槽6failed后被动观察至一秒（或peer close/EOF），再用剩余至多一秒主动收尾；两段都受原45秒截止约束。
这个一秒取证窗口不是网关的G/B政策。

`PeerClose=true`仅来自实际CloseError；本地BeginClose只有sent=true才记send，自动回复不补造发送。
BeginClose前为本地close和最坏peer close预检两条记录与编码额度；发送后继续保留peer-close额度。
不足时报告`close_budget_exhausted`/`local_budget_abort`，直接取消reader、强制释放并join，不发送一条无法记入raw的主动close，也不把预留写成send。
本地完整写出不证明云端收到；终态后peer抢先关闭导致BeginClose失败时，仍消费reader的真实接收证据。
EOF保持EOF；被动一秒内无终止为silent，随后本地主动关闭得到的peer回应不能改写先前的silent事实。
时间是transport向录制器交付时的观测时间，包含transport自动回应处理，不是原始网络帧到达时间。

候选的upstream-accepted来自匹配started/usage/terminal建立的契约预期，不冒充云端socket抓包。
local close只生成client.send；peer close单列recorded upstream.send与golden client.receive。
schema v1无法如实表达failed+1000、纯EOF或只有本地close的完整失败关闭对时，保留raw、不给候选。
只有实际failed→peer-close观测间隔落在网关G内，才可能成为其保全原码的相关证据，仍需控制器审核。

独立decoder只读约定payload.usage；token三字段齐全且total=input+output，拒绝partial、重复、溢出、负数、回退或单位冲突。
正常阶段及failed后的全部尾文本共用绑定检查，核对实际失败task ID；终态usage冻结后，同值重复可保全，增长/回退/presence改变均不能成为候选。
矛盾尾部仍保存原始字节，save输出candidate-unavailable，不能靠通用testkit结构校验冒充协议证据。
槽5必须见第二task真实有效快照才主动中断；首task终态和第二task快照原样保留，interrupted候选不伪造第二终态。
不从音频长度推算usage，不把价格表当wire用量，不给candidate填写已验证能力Coverage。

## 控制器单槽命令与停机规则

**当前真实端点、样本与费用依据仍缺，不能直接执行。** 控制器核验并提供该槽完整CostEvidence后，可使用现有入口；未知其他槽不会阻断它：

```bash
# 仓库根执行；key由既有安全环境提供，不打印、不写进命令或manifest。
OMUGW_RECORD_DS_INFERENCE=1 \
OMUGW_DS_INFERENCE_SLOT=1 \
OMUGW_DS_INFERENCE_MANIFEST='/绝对路径/经审核manifest.json' \
OMUGW_DS_INFERENCE_OUTPUT="$PWD/.local/ws-recordings/s3-audio-20261004/slot-1" \
go test -tags=smoke ./tests/smoke -run '^TestRecordDashScopeInference$' -count=1
```

下一槽须同步精确改变SLOT与slot-N，不能运行循环或批量补跑。预检缺项、费用余量不足、真实模型不支持、
failed/EOF/超时、原始记录拒绝、候选未生成及样本/输出异常时停止，由控制器审核材料；不得换模型/地域、重置账本或重试当前槽。

开发与CI只选离线测试（即便编译smoke tag也不选live入口）：

```bash
go test ./tests/smoke -run '^TestInferenceRecorderOffline' -count=1
go test -race ./tests/smoke -run '^TestInferenceRecorderOffline' -count=1
go test -tags=smoke ./tests/smoke -run '^TestInferenceRecorderOffline' -count=1
```
