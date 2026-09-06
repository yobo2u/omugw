# Triage labels

五种角色与 GitHub 标签一一对应：

| 角色 | 标签 | 含义 |
|---|---|---|
| needs-triage | `needs-triage` | 尚未分类或验证的外部输入 |
| needs-info | `needs-info` | 缺少复现、范围或环境信息，等待报告者补充 |
| ready-for-agent | `ready-for-agent` | 已验证并写成可独立执行的 agent brief |
| ready-for-human | `ready-for-human` | 需要产品决定、凭据、权限或其他人类动作 |
| wontfix | `wontfix` | 已决定不进入实现队列 |

同一时刻只保留一个角色标签。角色变化时先移除旧标签，再添加新标签；产品类别、优先级
等其他标签不属于这套状态机，可以并存。
