# SpecRelay

[English](README.md) | **简体中文**

SpecRelay 是一个**本地优先、中文界面**的智能体工作流工具：把需求和反馈整理成可审阅的计划，再按顺序调用本机的 Codex CLI 或 Claude CLI 完成任务、展示运行过程并执行验证。

> **核心边界：后端永远在宿主机运行。** 因此它可以选择真实的本地项目目录，并直接调用你已安装、已登录的 Codex / Claude CLI。Docker 只用于 PostgreSQL 数据库，绝不用于运行后端或 CLI。

## 推荐使用：桌面安装包

当前仓库提供 Linux 原生 `.deb` 打包流程。桌面应用启动后会自动：

1. 使用 Docker Compose 启动或复用专用的 PostgreSQL 容器；
2. 在宿主机启动随安装包附带的 Go 后端；
3. 用同源本地页面打开中文 UI；
4. 保留数据库卷；关闭窗口只停止后端，**不会**执行 `docker compose down` 或删除数据。

### 前置条件

- Linux（当前打包产物为 `.deb`）
- Docker Engine 或 Docker Desktop 已安装且正在运行；需支持 `docker compose` 和 `docker compose up --wait`
- 已安装并登录至少一个要使用的本地 CLI：`codex` 或 `claude`

桌面应用不会把 CLI 放进容器，也不会上传项目目录。创建项目时可在界面中浏览并选择已有的本地文件夹。

### 构建并安装

```bash
# Go 1.25+、Node.js 22+、Rust/cargo、Docker Compose 均已安装
./scripts/package-desktop.sh

# 安装生成的包（文件名会随版本与架构变化）
sudo apt install ./desktop/src-tauri/target/release/bundle/deb/*.deb
```

从应用菜单启动 **SpecRelay** 即可。若本机没有可用 Docker，启动页会明确提示依赖错误；修复后重新打开应用即可。

> 打包 Linux 桌面应用还需要 WebKitGTK、GTK 和 librsvg 的开发依赖。Debian/Ubuntu 可安装：
> `sudo apt install pkg-config libwebkit2gtk-4.1-dev libgtk-3-dev librsvg2-dev`。

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

- 开发数据库卷名为 `specrelay-postgres`；桌面版数据库卷名为 `specrelay-desktop-postgres`。
- 数据库迁移在后端启动时自动执行。升级前建议先备份生产数据。
- 不要在桌面版运行期间执行 `docker compose down -v`，也不要删除上述 volume；这会删除数据库数据。
- 关闭桌面窗口只结束本次宿主机后端进程，PostgreSQL 会按 `restart: unless-stopped` 保持运行，以避免中断或误删数据。

示例备份（桌面版）：

```bash
docker exec -t specrelay-desktop-postgres-1 pg_dump -U specrelay specrelay > specrelay-backup.sql
```

容器名称可能因 Docker Compose 版本而不同；可先使用 `docker ps` 查询实际名称。

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

# 桌面安装包（Linux）
cd ..
./scripts/package-desktop.sh
```

当 `TEST_DATABASE_URL` 指向独立测试数据库时，后端测试会运行 PostgreSQL 集成用例。**绝不要**把它指向开发、桌面版或生产数据库。

## 许可证

[MIT](LICENSE)
