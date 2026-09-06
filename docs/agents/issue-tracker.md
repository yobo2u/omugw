# Issue tracker

本仓库使用 [GitHub Issues](https://github.com/yobo2u/omugw/issues) 管理工作项。
所有命令显式带 `--repo yobo2u/omugw`，防止从错误的工作目录操作其他仓库。
外部 PR 默认不进入 triage 队列。

## 常用操作

```bash
gh issue create --repo yobo2u/omugw --title "<标题>" --body-file <正文文件> --label <标签>
gh issue list --repo yobo2u/omugw --state open
gh issue view --repo yobo2u/omugw <编号> --comments
gh issue comment --repo yobo2u/omugw <编号> --body-file <评论文件>
gh issue close --repo yobo2u/omugw <编号> --reason completed
```

创建、编辑、关闭 Issue 都是共享外部副作用；执行前必须有用户授权。读取与查询不需要
额外确认。

## Wayfinding operations

Wayfinder 地图使用 `wayfinder:map` 标签。每张决策票都是地图的原生 sub-issue，且
恰有一个类型标签：`wayfinder:research`、`wayfinder:prototype`、
`wayfinder:grilling` 或 `wayfinder:task`。

```bash
# 先创建地图，再以它为父 Issue 创建决策票。
gh issue create --repo yobo2u/omugw --title "<地图名>" --body-file <map.md> --label wayfinder:map
gh issue create --repo yobo2u/omugw --parent <地图编号> --title "<决策名>" \
  --body-file <ticket.md> --label wayfinder:<类型>

# 先创建全部票，再在第二遍接原生阻塞边：左边被右边阻塞。
gh issue edit --repo yobo2u/omugw <被阻塞票编号> --add-blocked-by <阻塞票编号>

# 领取必须先于工作；assignee 就是 claim。
gh issue edit --repo yobo2u/omugw <票编号> --add-assignee @me

# 查看原生父子与阻塞关系。
gh issue view --repo yobo2u/omugw <编号> \
  --json number,title,state,assignees,parent,subIssues,subIssuesSummary,blockedBy,blocking,url
```

Frontier 是指定地图下仍 open、无 assignee、且没有 open blocker 的原生子 Issue：

```bash
MAP=<地图编号>
gh issue list --repo yobo2u/omugw --state open --limit 200 \
  --json number,title,assignees,parent,blockedBy,labels,url |
  jq --argjson map "$MAP" '.[] | select(
    .parent.number == $map and
    (.assignees | length) == 0 and
    ([.blockedBy[] | select(.state == "OPEN")] | length) == 0
  )'
```

解决一张票时，把答案写成 resolution comment，关闭该票，并在地图的
`Decisions so far` 中只追加「票名链接 + 一行结论」。详细决定只存在票里，不在地图
复制第二份。创建或更新票时，在人类可读文字中始终用票名链接，不用裸编号代称。

## Triage operations

外部问题先加 `needs-triage`。信息不足转 `needs-info`；已形成 agent-ready brief 转
`ready-for-agent`；必须由人处理转 `ready-for-human`；确认不做则转 `wontfix`。
完整角色映射见 `docs/agents/triage-labels.md`。
