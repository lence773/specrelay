# SpecRelay

[English](README.md) | **简体中文**

SpecRelay 是一个**本地优先、中文界面**的智能体工作流工具：把需求和反馈整理成可审阅的计划，再按顺序调用本机的 Codex CLI 或 Claude CLI 完成任务、展示运行过程并执行验证。

> **核心边界：后端永远在宿主机运行。** 因此它可以选择真实的本地项目目录，并直接调用你已安装、已登录的 Codex / Claude CLI。Docker 仅可作为开发环境 PostgreSQL 的可选工具，绝不用于运行后端或 CLI。

## 推荐使用：桌面安装包

仓库提供 Linux、Windows 和 macOS 的原生桌面打包流程。每个安装包仅包含 Tauri 壳、Go 后端和前端资源；**不会**携带、安装、启动、停止或管理 PostgreSQL / Docker。

首次打开时，SpecRelay 会展示中文的数据库连接设置页。填写由你管理的 PostgreSQL 主机、端口、数据库名、用户名、密码和 SSL 模式后，桌面端会在宿主机启动随包附带的 Go 后端。若目标数据库为空，后端会在进入主界面前自动创建迁移元数据和全部所需数据表；初始化不会删除已有数据。

连接地址仅保存于当前操作系统用户的 SpecRelay 应用数据目录。在类 Unix 系统中，配置文件会尽可能限制为 `0600` 权限。

### 前置条件

- Linux x64、Windows x64，或 macOS（Intel / Apple Silicon）
- 一个你可管理且能访问的 PostgreSQL 16 数据库（本机、局域网或托管数据库均可）
- 至少一个已安装并完成认证的本地 CLI：`codex` 或 `claude`

桌面端不会把 CLI 放进容器，也不会上传项目目录；你可以直接在 UI 中选择已有本地目录。桌面端没有系统原生标题栏，可通过应用内顶部栏拖动窗口、最小化、最大化或关闭。

### GitHub Actions 构建

在 GitHub Actions 手动运行 **Desktop package** 工作流，可以构建所有平台并下载构建产物；推送类似 `v1.0.0` 的版本标签会执行同一组原生构建，并将全部安装包发布到对应的 GitHub Release。

| 目标平台 | 原生 Runner | 产物 |
| --- | --- | --- |
| Linux x64 | Ubuntu | `.deb`、`.AppImage`、`.rpm` |
| Windows x64 | Windows Server | NSIS 安装器（`.exe`）、MSI（`.msi`） |
| macOS Intel | macOS Intel | `.dmg` |
| macOS Apple Silicon | macOS Apple Silicon | `.dmg` |

此工作流不会对安装包进行代码签名。正式公开分发前，应配置 Apple 与 Windows 的签名 / 公证凭据；否则 macOS Gatekeeper 或 Windows SmartScreen 可能要求用户额外确认。

### 构建与安装

```bash
# 需要 Go 1.25+、Node.js 22+、Rust/cargo。
# 默认构建当前操作系统对应的原生安装包。
./scripts/package-desktop.sh

# Linux：构建所有支持的 Linux 格式，然后安装 Debian 包。
TAURI_BUNDLES=deb,appimage,rpm ./scripts/package-desktop.sh
sudo apt install ./desktop/src-tauri/target/release/bundle/deb/*.deb

# Windows（请从 Git Bash 运行）/ macOS：只能在对应的原生系统上构建。
TAURI_BUNDLES=nsis,msi ./scripts/package-desktop.sh  # Windows
TAURI_BUNDLES=dmg ./scripts/package-desktop.sh       # macOS
```

从应用菜单启动 **SpecRelay**，然后完成数据库连接表单。桌面端本身不需要 Docker。

> 构建 Linux 桌面安装包还需要 WebKitGTK、GTK 和 librsvg 开发包。Debian/Ubuntu 示例：
> `sudo apt install pkg-config libwebkit2gtk-4.1-dev libgtk-3-dev librsvg2-dev`

## 本地开发

要求：Go 1.25+、Node.js 22+、npm、Docker Compose，以及 PostgreSQL 16（可由 Docker 提供）。后端和任何 CLI 必须在宿主机运行。

```bash
# 终端 1：只启动数据库，然后在宿主机启动后端。
# 若 frontend/dist 已存在，脚本会让 Go 后端直接托管它；开发时可保持为空。
./scripts/dev/start-backend.sh

# 终端 2：启动 Vite 开发服务器（它会代理 /api、/events 和 /mcp 到后端）
cd frontend
npm ci
npm run api:generate
npm run dev
```

后端未设置 `ACCESS_TOKEN` 时会在终端打印一次性访问 URL。若希望固定开发访问地址，可显式设置：

```bash
POSTGRES_PASSWORD=specrelay-dev-only \
ACCESS_TOKEN=local-browser-token \
MCP_TOKEN=local-mcp-token \
./scripts/dev/start-backend.sh
```

然后访问 `http://127.0.0.1:43847/?token=local-browser-token`。也可以手动启动数据库：

```bash
docker compose -f deploy/docker-compose.yml up -d --wait postgres
```

`deploy/docker-compose.yml` **只有 PostgreSQL 服务**；它不构建、不启动，也不挂载后端。

## 使用本地 CLI 与目录

1. 在“创建项目”中用目录浏览器选择真实存在的本地工作目录；
2. 在项目设置中配置 Codex 或 Claude 可执行命令、模型及验证命令；
3. 在“需求”页可先与本地 CLI 多轮讨论，再创建正式需求；
4. 生成计划后可手动执行，或打开自动化让已就绪的计划依序执行。

SpecRelay 不设置 CLI 总超时时间；运行页面以终端样式显示简略日志，默认保留最新 50 条并可向上滚动加载更早内容。完整 CLI 原始日志仍受控地保存在应用数据目录中，前端不会直接读取任意本机文件。

### 安全关闭与恢复

关闭桌面窗口时，应用会先显示“正在安全停止后台任务”的中文状态，再向**本次启动的本机后端**发出带随机令牌的关闭请求。后端在最多 20 秒的收尾窗口内按以下顺序处理：

1. 停止领取新任务，并持久化当前实例拥有的运行状态；
2. 终止当前实例启动的 Codex / Claude CLI 进程组；若 CLI 不响应 `SIGTERM`，2 秒后升级为 `SIGKILL`，避免遗留子进程；
3. 只读的计划生成任务回到队列，下一次可安全继续；
4. 已开始改动工作区的代码任务回到“待处理”，所属计划标记为“已阻塞”，需要人工确认后再继续；
5. 释放工作区锁并关闭后端。

异常退出、断电或被强制结束时，运行实例心跳会在后续启动或运行中的后端中被检查。连续错过 3 次心跳（最低等待 30 秒）后，系统采用同一套收敛规则：计划生成可恢复，代码任务不会被盲目自动重跑。该机制按后端实例归属处理，不会取消仍存活的其他桌面实例的任务；也不会停止 PostgreSQL、Docker 或外部数据库服务。

## 架构与安全边界

- **后端：** Go、`net/http`、`pgx/v5`、PostgreSQL 持久化队列与迁移
- **前端：** React、TypeScript、Vite、TanStack Query
- **桌面壳：** Tauri 2；加载 Go 后端提供的 `127.0.0.1` 同源页面
- **自动化：** 宿主机 Codex CLI / Claude CLI；任务在受控项目工作目录中执行
- **实时：** SSE，支持 PostgreSQL 事件重放
- **集成：** REST API 与 MCP 使用同一服务层
- **认证：** 仅允许 loopback Host/Origin；浏览器 token 兑换为本地 `HttpOnly` Cookie，MCP 使用独立 Bearer Token

目录结构：

```text
api/        OpenAPI 接口契约
backend/    宿主机 Go API、Worker、Agent Runner、MCP 与数据库迁移
frontend/   React Web 应用与自动生成 API 客户端
desktop/    Tauri 桌面启动器与打包配置
deploy/     仅 PostgreSQL 的 Docker Compose 配置
scripts/    宿主机开发启动与桌面打包脚本
docs/       架构说明
```

## 数据、升级与备份

- 后端每次启动都会自动执行数据库迁移，包括桌面端保存新的数据库连接之后。
- 桌面端不拥有数据库生命周期。请使用你现有的数据库运维方式备份、加固、监控和升级 PostgreSQL 实例。
- 关闭桌面窗口只会结束本次启动的宿主机后端；不会执行 Docker 命令，也不会停止、删除或修改已配置的 PostgreSQL 服务。
- `deploy/docker-compose.yml` 仍可作为**仅开发用途**的 PostgreSQL 辅助工具；其中的 `specrelay-postgres` volume 不是由桌面端管理的生产备份。

可从本机访问 PostgreSQL 时的备份示例：

```bash
pg_dump "$DATABASE_URL" > specrelay-backup.sql
```

## 健康检查、API 与 MCP

宿主机后端启动后提供：

```bash
curl http://127.0.0.1:43846/healthz
curl http://127.0.0.1:43846/readyz
```

- REST 前缀：`/api/v1`
- MCP：`/mcp`（使用独立 MCP Bearer Token）
- OpenAPI 契约：[`api/openapi.yaml`](api/openapi.yaml)

详细的队列语义、工作区租约、SSE、认证与环境变量见 [架构文档](docs/go-postgres-architecture.md)。

## 验证

```bash
# 后端
cd backend
go test ./... -count=1
go vet ./...
go build -trimpath -o /tmp/specrelay ./cmd/specrelay

# 前端
cd ../frontend
npm ci
npm run api:generate
npm run typecheck
npm test
npm run build

# 原生桌面安装包（在目标操作系统上运行）
cd ..
./scripts/package-desktop.sh
```

当 `TEST_DATABASE_URL` 指向独立测试数据库时，后端测试会运行 PostgreSQL 集成用例。**绝不要**把它指向开发、桌面版或生产数据库。

## 许可证

[MIT](LICENSE)
