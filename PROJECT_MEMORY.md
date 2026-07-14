# SpecRelay 项目记忆

> 本文件用于后续会话快速恢复项目背景、关键约束和当前交接状态。不要在此文件中写入数据库密码、访问令牌或其他密钥。

## 项目定位与架构

- **SpecRelay** 是一个本地优先的桌面端需求、计划与任务编排工具。
- 前端：React + TypeScript（`frontend/`）。
- 后端：Go（`backend/`），以宿主机进程运行。
- 桌面端：Tauri（`desktop/`），打包时将 Go 后端作为 sidecar；Windows sidecar 使用 `-H=windowsgui`，避免出现 CMD 黑窗。
- 数据库：PostgreSQL。数据库不随桌面端安装，首次打开由用户配置外部数据库连接；应用需要在启动时执行迁移初始化。

## 必须遵守的运行约束

- **不得**用 Docker 启动后端服务；Docker 仅用于 PostgreSQL 和临时测试工具。
- 桌面端必须能访问用户本地目录，并使用用户本地安装的 Codex / Claude CLI。
- CLI 任务没有执行超时；运行中的任务应可显示实时终端式日志，并支持取消。
- 同一计划的任务必须严格顺序执行；不同计划的任务互不串行替换。
- 自动化停止时，应取消正在运行的任务；应用退出时应保证任务、进程和数据库状态一致。
- 数据库操作必须避免影响生产数据库；测试应创建并清理独立测试数据库。

## CLI 会话复用（2026-07-15）

提交 `f16a7d1` 实现了持久化 CLI 会话链路与上下文快照兜底：

```text
需求讨论会话
  -> 计划生成续接同一会话
  -> 计划执行会话继承计划会话
  -> 同一计划的任务依次续接同一执行会话
  -> 会话失效时，由持久化快照创建新会话恢复
```

关键实现：

- 新迁移：`backend/internal/migrations/sql/008_agent_sessions.sql`。
- 新表：`agent_sessions`，按 `requirement`（需求）和 `execution`（计划）用途隔离；Provider 为 `codex` / `claude`，会话状态为 `active` / `stale`。
- 新代码：
  - `backend/internal/repository/sessions.go`
  - `backend/internal/app/sessions.go`
- 需求讨论 API 和创建需求 API 传递 `sessionId`、`sessionProvider`、`requirementSessionId`、`requirementSessionProvider`。
- Codex / Claude 的计划和讨论调用不再使用临时会话参数：
  - Codex 移除 `--ephemeral`；恢复计划/讨论时通过 `-c sandbox_mode="read-only"` 保持只读。
  - Claude 移除 `--no-session-persistence`；恢复计划/讨论仍使用 plan 权限与只读工具。
- 任务执行恢复会话时保持可写执行能力；不会误用只读 sandbox。
- 会话恢复失败会将旧会话标记为 `stale`，并用受长度限制、UTF-8 安全的快照恢复需求、计划、任务状态及最终输出摘要。

验证已完成：

```bash
# 后端（宿主机无 Go 时可使用临时 golang 容器；不要启动 Docker 后端）
go test ./...

# 前端
npm --prefix frontend run typecheck
npm --prefix frontend test
```

当时结果：后端全量测试通过；前端 43 项测试通过；数据库仓储集成测试使用独立临时数据库后已清理。

## 发布与 GitHub Actions

- 发布版本由桌面端配置统一决定，必须同步修改：
  - `desktop/package.json`
  - `desktop/package-lock.json`
  - `desktop/src-tauri/Cargo.toml`
  - `desktop/src-tauri/tauri.conf.json`
- `Desktop package` 工作流：
  - `workflow_dispatch` 可构建临时 artifacts；不会创建 Release。
  - 推送 `v*` Tag 才会运行 `Publish version release`，并把 Linux / Windows / macOS 安装包发布到 GitHub Release。
- 不要为正式发布直接在 `master` 手动触发桌面打包；应先升级版本、提交、创建并推送对应的 `vX.Y.Z` Tag。
- 当前正式版本：`v1.0.15`，提交 `a087837`（`chore: release v1.0.15`）。
- Tag `v1.0.15` 已推送并触发 `Desktop package` 工作流，运行编号：`29362324743`。后续可使用：

```bash
gh run view 29362324743
gh run watch 29362324743 --exit-status
```

- 曾错误地在 `master` 手动触发过桌面构建（运行 `29361739061`），已提交取消；该构建不应作为正式发布使用。

## Git 与网络注意事项

- 远端：`origin` -> `https://github.com/lence773/specrelay.git`。
- 当前主分支与远端：`master` / `origin/master`。
- 某些本机网络环境中 GitHub 默认 DNS 地址连接会超时，而 `140.82.112.3` 可用。仅在临时推送失败时使用一次性配置，勿写入全局 Git 配置：

```bash
git -c http.curloptResolve=github.com:443:140.82.112.3 push origin master --follow-tags
```

## 常用检查

```bash
git status --short
git log -3 --oneline --decorate
gh run list --limit 10
npm --prefix frontend run typecheck
npm --prefix frontend test
```
