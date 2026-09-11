# 来源说明（Provenance）

本包（`workbuddy2api/internal/apicompat`）的代码**移植自第三方项目**，不是本项目原创。

| 项 | 值 |
|---|---|
| 来源仓库 | `https://github.com/Wei-Shaw/sub2api` |
| 本地来源 | `D:/sub2api-custom`（fork: `706412584/sub2api`） |
| 来源 commit | `d89e533424ea58cb286c05e957f0cc0e54949ae6`（2026-09-09，v0.1.190-fork.5） |
| 原路径 | `backend/internal/pkg/apicompat/` |
| 许可证 | **LGPL-3.0**（见同目录 `LICENSE`） |
| 移植范围 | 该目录下全部 14 个非测试 `.go` 文件，**逐字复制、未作修改** |
| 移植日期 | 2026-09-12 |

## 本目录内文件的性质划分

| 文件 | 性质 |
|---|---|
| 上述 14 个 `.go` 文件 | **LGPL-3.0**，移植自上游 |
| `LICENSE` | 上游 LGPL-3.0 全文副本 |
| `PROVENANCE.md`（本文件） | 本项目原创（MIT），仅为署名与合规说明 |
| `smoke_test.go` | **本项目原创（MIT）**，非移植内容；用于验证移植后的纯函数与流式状态机在 `workbuddy2api` 内可用 |

## 为什么是 LGPL

上游仓库于 2026-04-19 由提交 `23def40bc`（`chore: change license from MIT to LGPL v3.0`）
把许可证从 MIT 改为 LGPL-3.0。本包所移植的文件大多创建或修改于该时点**之后**，
因此适用 LGPL-3.0。

## 使用与分发注意

- 本包代码为 LGPL-3.0，与项目其余部分的 MIT 许可**不同**。
- 若分发包含本包的作品，需遵守 LGPL-3.0 的义务（提供对应源码、保留许可声明等）。
- 本包**只依赖 Go 标准库**，未引入任何第三方依赖，因此不牵涉额外的传递依赖许可。

## 移植时未包含的内容

上游同目录下的测试文件（`*_test.go`）未移植——它们依赖 `testify` 与 `gjson`，
本项目不在测试中引入新依赖。本包的验证通过 `workbuddy2api` 侧的适配层测试完成。
