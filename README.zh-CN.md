# SpecRelay

[English](README.md) | **简体中文**

SpecRelay 是一个本地优先的智能体工作流服务，可将需求与反馈转化为结构化计划、按顺序执行的实现任务、实时传输的执行日志以及最终验证结果。

本项目采用简洁的 Go + PostgreSQL + React 技术栈实现，不包含旧版桌面应用或 SQLite 兼容层。

## 技术栈

- **后端：** Go、`net/http`、`pgx/v5`，使用 PostgreSQL 持久化任务与事件
- **前端：** React、TypeScript、Vite、TanStack Query
- **接口契约：** 位于 `api/openapi.yaml` 的 OpenAPI 3.1 定义
- **自动化：** Codex CLI 与 Claude CLI 适配器
- **需求讨论：** 在新建需求页面中调用项目配置的本地 CLI，只读分析项目并通过多轮中文讨论整理需求草稿
- **实时通信：** SSE，并支持从 PostgreSQL 重放事件
- **集成方式：** REST API 与 MCP 服务器共享同一套应用服务

## 本地 CLI 需求讨论

新建需求时，可以展开“与本地 CLI 讨论需求”面板。SpecRelay 会使用项目设置中的 Codex 或 Claude CLI，在项目工作目录中进行只读分析，并根据多轮讨论回填中文标题和 Markdown 需求说明。用户确认表单内容后再创建需求；如果项目已开启自动化，则沿用现有流程继续生成计划。

该能力需要后端直接运行在宿主机上，才能访问宿主机目录和本地 CLI。可以只用 Docker 启动 PostgreSQL，但不建议为此功能把整个应用运行在容器中。当前讨论记录保存在浏览器页面状态中，刷新、取消或离开新建页面后不会保留。

## 仓库结构

```text
api/        OpenAPI 接口契约
backend/    Go API、工作进程、智能体运行器、MCP 与数据库迁移
frontend/   React Web 应用与自动生成的 API 客户端
deploy/     Docker Compose 配置
docs/       架构文档
```

## 使用 Docker Compose 快速启动

```bash
ACCESS_TOKEN="replace-with-a-long-random-value" \
MCP_TOKEN="replace-with-another-long-random-value" \
POSTGRES_PASSWORD="replace-me" \
docker compose -f deploy/docker-compose.yml up --build -d
```

启动后访问 `http://127.0.0.1:43846/?token=<ACCESS_TOKEN>`。

健康检查端点：

```bash
curl http://127.0.0.1:43846/healthz
curl http://127.0.0.1:43846/readyz
```

## 本地开发

环境要求：Go 1.25+、Node.js 22+、npm 和 PostgreSQL 16+。

```bash
# 数据库
docker compose -f deploy/docker-compose.yml up -d postgres

# 后端
cd backend
DATABASE_URL="postgresql://specrelay:specrelay-dev-only@127.0.0.1:54329/specrelay?sslmode=disable" \
ACCESS_TOKEN="local-browser-token" \
MCP_TOKEN="local-mcp-token" \
go run ./cmd/specrelay

# 前端（在另一个终端中运行）
cd frontend
npm ci
npm run api:generate
npm run dev
```

Vite 开发服务器地址为 `http://127.0.0.1:43847/?token=local-browser-token`，并会将 API/MCP 请求代理到 Go 后端。

## 验证

```bash
cd backend
go test ./... -count=1
go vet ./...
go build -trimpath -o /tmp/specrelay ./cmd/specrelay

cd ../frontend
npm ci
npm run api:generate
npm run typecheck
npm test
npm run build
```

当 `TEST_DATABASE_URL` 指向一个独立的测试数据库时，将运行 PostgreSQL 集成测试。切勿将集成测试连接到开发或生产数据。

有关架构、安全模型、队列语义和配置参考，请参阅 [docs/go-postgres-architecture.md](docs/go-postgres-architecture.md)。
