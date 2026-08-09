# Third-Party Notices

本文件列出 omugw 直接依赖的第三方组件及其许可证。完整的传递依赖清单由
`make licenses` 生成到 `SBOM.spdx.json`，并在 CI 中强制校验。

## 直接依赖

| 组件 | 版本 | 许可证 | 用途 |
|---|---|---|---|
| `gopkg.in/yaml.v3` | v3.0.1 | MIT + Apache-2.0（双授权） | 配置文件解析 |
| `github.com/google/go-cmp` | v0.7.0 | BSD-3-Clause | 测试断言（golden file 比对） |
| `github.com/prometheus/client_golang` | v1.22.0 | Apache-2.0 | metrics 暴露 |

## 计划中的依赖（M6 / M7）

| 组件 | 许可证 | 用途 |
|---|---|---|
| `github.com/gorilla/websocket` | BSD-2-Clause | WebSocket 传输层 |

## 许可证策略

- Core 采用 Apache-2.0。
- **MIT 来源的代码保留原始 copyright 与 permission notice**，不改写成
  `SPDX-License-Identifier: Apache-2.0`。
- **LGPL / AGPL 来源的代码不进入 Core。** 若需要其能力，采用独立进程边界或
  依据公开协议文档独立实现。
- 参考 LiteLLM 时须做**文件路径级**许可证审计：其 `enterprise/` 目录适用
  单独的 Enterprise License，不可按 MIT 处理。
- **不阅读 sub2api / QuantumNous/new-api 的源码。** 不读即不需要 clean-room
  流程，这是成本最低的合规姿态。仅参考其公开文档与交互行为。

## 允许合理复用的上游（MIT / Apache-2.0）

参考实现来源须在 `docs/provenance.yaml` 中逐模块登记 `Implementation-Type`
（`original` / `derived` / `clean-room`）与来源 commit。
