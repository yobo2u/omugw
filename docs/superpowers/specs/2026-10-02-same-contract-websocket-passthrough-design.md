# A 包：同契约 WebSocket 直通与三条路径投放设计

日期：2026-10-02；修订：2026-10-03。

状态：用户在复审后要求「按评审意见修改并启动开发」。本版作为开发依据；真实
录制、路径兑现、发布分别需要对应证据或授权。原版保存在提交 `1183ed7`。
本地开发基线为 `1183ed7`（生产基线 `ee36d24`）。

依据：[ADR-0003](../../adr/0003-realtime-minimal-session-semantics.md)、
[最小会话契约](2026-09-28-realtime-minimal-session-contract.md)、
[WS fixture 设计](2026-09-29-websocket-fixture-design.md)。

## 1. 范围与顺序

剩余九条路径全部纳入；A 包三条同协议 WS、B 包四条 HTTP 异构、C 包双向
跨协议 Realtime。先按路径纵向交付 A 包：

1. S1：`dashscope.realtime → dashscope.ws.realtime`，`GET /api-ws/v1/realtime`。
2. S2：`openai.realtime → openai.realtime`，`GET /v1/realtime`，仅 GA。
3. S3：`dashscope.inference → dashscope.ws.inference`，`GET /api-ws/v1/inference`。

每条包括共享传输补齐、Provider、handler、观测、独立录制与回放、兑现。
先交付可离线验证的实现，随后取得录制授权和真实证据，最后才注册生产门并兑现。
共享代码由 S1 建立，后续路径复用；各自的协议约束有独立验收。

A 包不做协议翻译、重采样、通用 Realtime IR、工具执行、价格账单、WebRTC、
浏览器子协议鉴权或模型别名。不为直通路径制造一套 Canonical 重编码器。
OpenAI `call_id`、独立 `intent=transcription` 会话另案；会话内转写仍须观测。

## 2. 负载保全与矩阵

完整应用消息的 opcode、字节与单向顺序原样保全，包括未知字段及
missing/null/false/0/空集合。两段连接各自处理掩码、分片和 ping/pong。
只读窥探路由字段、包络、终态和 usage，不用 Canonical/Extensions 重建消息。
未知合法事件交给同契约上游处理；对应用参数的上游错误原样返回。

采用**整门投放**，不实现运行时半套逐事件能力投影：启动注册与拨上游之前，
矩阵必须批准该同协议门的整个当前可表达集合。部分兑现的 WS 门不可接入这个
直通 handler。跨协议路径不因 `MarkHomogeneous` 历史标记而进入本包。

整门投放仍按 `Redeem(ep, caps...)`、两份显式白名单、真实 fixture 和矩阵文档
执行。每项兑现能力要在证据表说明其承载字段、原样保全断言及关联真实轨迹；
允许一段轨迹覆盖多项，不要求每项重复付费。HTTP 少量 fixture 的先例不能作为
伪造 WS 能力证据的理由。未覆盖的门继续 PLANNED，不以 synthetic 回放转正。
这是传输路径保全承诺，不承诺某个上游模型支持所有业务能力。

当前 ExpressibleSet 是核对起点，不是永远正确的协议事实：例如 OpenAI GA 已有
`input_image`，不得沿用旧「不可表达」结论并同时声称全能力验证。S2 投放前做
契约/矩阵对账；需要的处置变更独立审阅，不在 S1 顺带改 B/C 路径。

## 3. 接口与配置

沿用平行接口：

```go
type StreamProvider interface {
    Kind() degrade.Provider
    Dial(ctx context.Context, req Request) (*ws.Conn, *http.Response, error)
}
```

`provider.Request` 使用已有 Target、Inbound、Header；Realtime 唯一 query 是
已核验并与 UpstreamModel 相等的 model，因此不预造通用 WS 参数袋。HTTP Call 不变。
Provider 只负责固定地址与干净握手，handler 持有下游升级和失败转移权。

不新增 `ws_contract`：代码中的明确协议定义固定本批支持的契约；配置 kind、
正式契约引用、测试轨迹共同声明它，不能靠模型名后缀猜测。OpenAI beta 已关闭，
握手携带 beta 信号直接拒绝，不静默改写为 GA。DashScope 模型族的 usage 差异
必须单独有证据；未知形状标 unavailable，不套 OpenAI 的计量解释。

新增 `websocket` 配置：

| 项 | 默认 | 约束 |
|---|---:|---|
| max_message_bytes | 32 MiB | >0，≤64 MiB，重组后的应用消息大小 |
| max_sessions | 128 | >0，≤4096，包含 pending 握手 |
| max_buffered_bytes | 256 MiB | >0，≤2 GiB，全进程 WS 受控 payload 分配预算 |

保留第三项：分块掩码把整条消息的写副本缩为工作区，不消除同时重组的多连接内存。
最低预算为 `2*M + min(M, 32 KiB)`（M 为消息上限），容纳双向各一条完整消息
及出站掩码工作区；“至少两倍”仅是下界，不是完整内存保证。分配/扩容前取得额度，
不够即明确终止该连接，禁止持有半条消息等待更多额度。多会话聚合、临时扩容的
新旧数组、控制帧及关闭原因仍真实计额，不保证任意128并发均满额，也不是 Go RSS。
临时扩容峰值及缓冲所有权必须说明。不得把128×32 MiB当小内存。
32 MiB 保留了 base64 膨胀及 JSON 开销；15 MiB 原始音频编码后约20 MiB。
WS 不额外解码音频检查 HTTP 内联限额，不限制整场会话的累计字节。
固定常量：去重记录4096、关联ID512字节、失败握手body64 KiB、握手头64 KiB。
请求头上限在 HTTP server；handler 不能宣称能在 net/http 解析前检查。

Provider 地址只接受无 userinfo/query/fragment/opaque 的 http/https/ws/wss。
出站固定加协议路径，保留部署前缀，防重复添加已完整包含的固定路径。
主机只来自配置；不跟随跨主机重定向，不代理任意 URL。

## 4. 鉴权和客户端兼容

仅请求头鉴权：Authorization 或 Api-Key，拒绝重复/多来源歧义，复用常量时间
校验。Origin 不是浏览器身份：官方 Python websocket-client 默认带 Origin，
已认证头客户端应可正常连接。Cookie 不鉴权、不转发。
浏览器的 `openai-insecure-api-key.*`、beta 子协议与其他不支持的子协议在 Dial 前
明确拒绝，不记录或回显 token；A 包不引入子协议鉴权或协商功能。

客户端可以提议 permessage-deflate：忽略提议、不选择扩展。上游未经我方提议
选择扩展/子协议则握手失败；收到 RSV/错误 mask 等非法帧时关闭。

出站 Authorization 始终由凭据池替换；User-Agent 使用固定网关值。
DashScope 的 `X-DashScope-WorkSpace`、`X-DashScope-DataInspection` 按 HTTP
直通先例白名单透传，值须单一、无控制字符。OpenAI Organization/Project 不转发，
客户端携带时仍可通过鉴权；`OpenAI-Safety-Identifier` 作为明确白名单保留。
未知非秘密可选头默认不转发，不从客户端租户值拼接目标主机。

OpenAI Node SDK 强制 wss：生产部署由反向代理终止 TLS，保留 HTTP/1.1 Upgrade，
禁用业务响应缓冲，代理空闲期限大于网关 idle。仅本地测试可以 ws；SDK/TLS
接入是 S2 验收项，不能用自家 Go 客户端通过替代它。

## 5. 握手、首字节与任务绑定

Realtime：鉴权、RFC 升级预检、唯一非空 model、路由、整门矩阵、凭据、Dial、
有界预读 session.created、Accept、relay。预读只确认已就绪，不确认客户端未来
配置被采纳；初始事件原字节保留并最先发下游。首个 error/close 不冒充就绪。

模型别名不支持：Realtime 在 Dial 前核对 model == target.UpstreamModel，跳过
不匹配候选；全部不匹配则明确拒绝。不能因 configured wildcard 放行任意模型。
同连接携带其他模型的命令交由固定同契约上游处理，不在网关翻译或切换 endpoint。

Inference 无握手 model：部署只允许一个 inference Provider，先拨它再升级；
首个 run-task 和每个后续任务按配置路由核对同 endpoint、真实模型名及矩阵。
**任务字段到101后才出现，失败在WS阶段返回 task-failed/1008，不能承诺HTTP拒绝。**
continue-task/flush 若有 model 同样核验，不改写；同连接串行任务需各自ID与终态。
task-started前不送 binary；finish-task不等于业务完成；task-finished后可复用，
task-failed原事件先送出后结束。二进制只属于唯一 active task，不猜任务归属。

RFC 预检覆盖 method GET、HTTP/1.1、Upgrade/Connection、唯一版本13、唯一有效
16字节 nonce、无body/transfer-encoding；没有 Hijacker 在触上游前失败。
Dial 校验101完整协商、唯一 Accept 摘要，非101 body有界读取并关闭原socket，
保留status与允许错误分类的信息。日志/本地错误不含原body、URL、key或close reason。

下游进入 Hijack/写101后即不可重试，即使 Accept 返回错误；使用独立承诺状态，
不是HTTP tracked.wrote。101前可对已明确可重试故障换凭据，非Retryable不换key，
可换同协议target。tried有限，每份key每次候选只试一次。下游101后绝不重拨。
关闭码不能单独判auth/quota；只有profile已核验的code+reason组合才可分类为
rate-limit，例如DashScope已知1011限流。未知错误保守非重试，不打印reason。

## 6. 生命周期与资源

connect 限TCP/TLS；first_byte是整个多候选握手及初始事件的共同预算，不能每次
重新计时；下游101写也受剩余预算约束。成功后转idle，HTTP total不套会话。
Go1.25 Hijack实现清空deadline，接口文档仍要求调用方管理；Accept明确设置
握手期限并在完成后清除，不依赖某版net/http恰好怎么做。

两方向固定数量worker，有界背压，无每消息goroutine、无音频累计队列。
定时ping间隔idle/2，读端自动pong，业务沉默不等于断线。写/ping/pong期限为
min(connect,idle)，只在写锁内设置；close礼貌收尾≤1秒。取消/错误关闭两侧并join。
读首事件必须服从握手ctx（含不停发ping也不能延长），不能用idle替代绝对期限。

受控读的消息及非空关闭原因均有显式所有权：`Message.Release()`与
`CloseError.Release()`在使用结束后归还额度；关闭原因的字符串副本也须分配前
预占，并在交接后持续计额。ctx竞争丢弃结果时由transport归还，不能要求调用方
释放未收到的对象。旧无Budget读接口不增加释放义务。

分块掩码保持4字节掩码跨块偏移，不原地修改调用方payload；短写不得报成功。
有效UTF-8文本、合法close与role mask在 transport守住；业务包络/usage只读处理。

| 情况 | 发往另一段的关闭 |
|---|---|
| 对端合法close | 同code/reason，尽力发送 |
| 对端空close（本地表示1005） | 空payload，绝不把1005编码上网 |
| 无close的EOF/上游断流 | 1011，固定安全reason |
| 本地策略拒绝 | 1008 |
| 消息超限 | 1009 |
| 进程退出 | 1001 |
| RFC错误 | 1002；文本非法UTF-8用1007 |

1006仅本地观测，禁止发送；CloseWithResult.sent不代表对端已收到。并发close时
第一个协调原因胜出，不能defer1000覆盖真实错误。完整上游终态已收到时先观测
再尽力转发，随后断开不抹掉已得到的usage。

registry涵盖pending/active，预占连接数在拨号前；封口与登记原子，close在锁外。
Built显式关闭WS：拒绝新登记、取消pending、关闭active、join；不能只调
http.Server.Shutdown。lease由单一协调者恰好结算一次，沿用generation保护。

## 7. 用量观测

按协议定义读取必要字段，记录键包括来源和response/item/task ID；JSON未知
字段不重写、不编造数字。上游完整消息到达就观测，不依赖下游是否写成功。
权威已结轮次不清零，取消仍有usage照实记，未结轮次unavailable。

Realtime response.done与输入转写分别计量；DashScope字段要有自己的官方/实录
依据，不能把OpenAI的同名字段直接当账单。暂未核验形状只上报unavailable，
生产投放前补齐相应证据。Inference duration/characters保留原单位，累计快照
取差额而非逐帧相加；口径无法确定则unavailable。

去重有界、不淘汰旧键重新计数；4096笔到限可见1008终止，已记录数据保留。
矛盾重复/负数/溢出记录诊断，不发布负增量或重复账。未核验usage也保全原事件。
指标标签限固定协议/来源/单位/fidelity，不用动态ID或model作标签。具体记录数
和不可知事件有计数，避免给token计数器加0却无法发现缺口。

## 8. 测试、真实录制和发布

离线回归：本地TCP对、不用net.Pipe。覆盖鉴权替换、Origin、扩展提议、RFC坏
握手、401/429、原字节与binary、大消息、分片、心跳、慢读、取消、close竞争、
pending/active shutdown、不同来源用量重复/断流、旧HTTP/SSE与矩阵不回归。
gateway测试可以构造明确的测试矩阵，不能修改默认Phase1来绕过生产投放门。

真实录制需另行确定模型/地域/版本、次数/时长/音频样本与费用；严禁裸make smoke。
录制器直连上游，以独立脚本取真实事件，候选独占落盘并审核；合成回放文件留在
测试目录，不进生产route目录。来源标签/SHA只证明自洽，不能认证云端真实性。
握手和消息内容都审查隐私；不从被测网关输出反生成预期，不伪造ack/terminal。

整门验收证据可跨轨迹复用，但逐项覆盖表及原样保全断言不可省。生产三处同改：
Mux接线＋真实fixture＋Redeem/两白名单/生成矩阵。没有真实证据仍为PLANNED。
`reconcileDoors`保持双向对账；测试装配与正式注册状态须清楚区分。

原则2.2的澄清作为独立本地文档提交及评审：同契约只读保全的证据不假称已经
验证C包转换；转换路径就绪时再按建模子集跑同组轨迹。正式原则PR和合并仍待
授权。在此前可开发离线代码，但不能声称原则采纳或生产投放完成。

## 9. 复审采纳与更正

采纳：Origin兼容、扩展提议不拒绝、头白名单、无别名、无beta/浏览器鉴权、
关闭码表、TLS部署说明、整门投放、路径纵切、固定小限额、分块掩码。

更正评审建议中的三点：保留32MiB（base64膨胀）；Inference模型校验在101后；
保留全局缓冲约束（分块写不能解决读端聚合内存）。整门批准不替代真实证据覆盖。
用户已指示启动开发，沿用原生子代理实施/独立复核；不再重复询问执行方式。
