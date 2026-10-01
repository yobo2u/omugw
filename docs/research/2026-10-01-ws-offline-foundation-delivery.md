# WS 离线基座分期交付状态（2026-10-01）

本状态对应 [P1 实施计划](../superpowers/plans/2026-10-01-websocket-fixture-offline-foundation.md)、
[设计](../superpowers/specs/2026-09-29-websocket-fixture-design.md) 与
[验收清单](../superpowers/specs/2026-09-29-websocket-fixture-acceptance.md)。
日期之前的“尚未编码/未实现”是执行前历史，不应让后续工作者重做已经交付的基座。

## 当前交付与尚待复核

P1 八项本地任务已实现并有永久离线回归；整分支审查发现的统一最终修复已进入
交付波次，**尚待控制器最终 scoped 复核**。不是全部 WS 举证方案完成，更不是
任何路径/能力投放；未来阶段不能从 8/8 任务推算项目完成率。

| P1 任务 | 已实现的软件边界 |
|---|---|
| 1 schema / 门归属 | HTTP/SSE/WS 互斥，三扇 WS 门只登记归属，不注册生产 Mux |
| 2 加载 / 来源账本 | 有界严格 JSON、因果图、摘要/样本一致性；普通文件预检与 descriptor 复核 |
| 3 握手 | 去敏快照、精确 path/query/header 比较；占位仅证明存在，不认证真实凭据 |
| 4 匹配 | 完整消息、presence、限定 ID、原子 bind/reference、显式标量改写与来源负载断言 |
| 5 调度 | after + 点内 FIFO，pending/in-flight/completed 纯选择策略 |
| 6 本地上游 | 真实本机 TCP 单连接握手、首次 Err、Close/所有权与取消 |
| 7 双端回放 | 固定 reader/writer、有界回收、真实终态/关闭发送与接收、拒绝 partial-close 截尾 |
| 8 整合 | 手写 synthetic hello/tool-ID/binary/two-task、旧 HTTP/SSE fixture 与原 golden 回归 |

最终修复统一静态/运行字段纪律、提前拒绝不可执行发送，v1 `ForwardedFrom` 只用于
message；close 上声明此字段明确拒绝。无来源声明的两侧关闭原因可独立声明。
`CloseError.IncompleteMessage` 只证明关闭时仍有未完成分片，不改变原 code/reason、
自动回应或 Error 文本，也不把所有分片中关闭称为 RFC 错误；回放不能用它批准
完整结局。`CloseWithResult` 仍只证明本次帧写完，对端接收另行举证。

## seed 与证据的准确口径

同 seed 在**同反馈历史、同就绪集合**下固定纯选择策略；至少两种 seed 覆盖多就绪
发送选择，节点归一后的消息/结局核对同一语义断言。实际读与 write-result 反馈
可能竞争，**完整 SendOrder、网络到达序列、网关内部执行顺序不保证一致**。不为
制造确定性停止合法 reader 竞争，也不声称穷举所有调度。

## 未交付范围

- **P2 真实录制器**：直连上游、双 opt-in、费用/录制预算、样本授权、消息内隐私审核、
  候选独占落盘与真正来源证据，均留后续计划。标签/SHA-256 是自洽账本，不是来源认证。
- **P3 生产网关与投放**：StreamProvider/handler、凭据替换、101 重试边界、版本与配置
  采纳、VAD、cancel/truncate、usage 去重及逐端点能力兑现，均未交付。
- **DSP / 音频**：参考重采样器、容差、ASR/VAD/可懂音频证据未交付。合成二进制
  只证明负载/机制，不证明云端互通或音频语义。

未修改矩阵处置、Redeem、投放白名单、MarkHomogeneous、生产 Mux、旧 routes/golden
或依赖。七扇 known 门表示准确协议归属，**不等于开了七门**。

## 资源边界与保留观察

Darwin/Linux 普通文件读取先 Lstat，再用 O_NONBLOCK/O_NOFOLLOW 打开，最后 Fstat
与 SameFile 复核，防末级路径在预检后换成 FIFO/symlink/另一文件。其它平台 fallback
只有预拒特殊文件/链接与 descriptor 复核，不承诺完整抗恶意文件系统竞态；同 inode
内容并发修改、祖先路径与整个目录的隔离不属于本工具的沙箱保证。

文件字节预算不是堆内存预算：严格 JSON 先建通用树，再查 schema，会发生累计分配
放大。审查的约 128 KiB 未知数组约 13.4 MB **累计分配**观察不是峰值存活内存、
OOM 或无限分配证明；此非阻断项保留，未借修复波次重写 streaming schema。

这些交付边界只适用于本地离线工具。真实录制、生产语义与投放必须在后续各阶段
重新取得对应证据，不能用当前全绿替代。
