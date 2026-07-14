# SpecRelay Go + PostgreSQL Web 架构

> SpecRelay 是独立的 Go + PostgreSQL + React 项目，不包含旧桌面版或 SQLite 数据兼容逻辑。

## 目录与职责

- `api/openapi.yaml`：REST API 的唯一契约，前端 SDK 由该文件生成。
- `backend/`：Go HTTP API、SSE、MCP、持久化队列、Agent Runner 和数据库迁移。
- `frontend/`：Vite + React + TypeScript，服务端状态由 TanStack Query 管理。
- `deploy/`：PostgreSQL 16 和自包含 Web 镜像的 Docker Compose 配置。

浏览器不访问本机文件系统，也不直接执行 Codex、Claude 或验证命令。Go 服务必须运行在能够访问项目 workspace 的机器上，并且只有后端可以启动 Agent CLI。

## 启动顺序

后端启动时按固定顺序执行：

1. 读取环境变量并创建 `DATA_DIR`。
2. 建立 `pgxpool` 并主动 `Ping` PostgreSQL。
3. 获取 PostgreSQL advisory lock，执行嵌入二进制的编号 migration。
4. 恢复崩溃前遗留的作业和过期租约。
5. 启动作业 Worker、事件 broker、REST/SSE/MCP 和静态前端。

`/healthz` 只表示进程存活；`/readyz` 会检查数据库。数据库不可用时普通 API 返回统一的 `503 database_unavailable`，Worker 不会可靠地领取新作业。

## 数据库和队列

PostgreSQL 16 是最低支持版本。PostgreSQL 18 验证当前按计划暂停，CI 暂时只运行 PostgreSQL 16。所有数据库访问使用 `pgx/v5 + pgxpool` 和显式 SQL，不使用 ORM。

领域变更、作业入队和事件写入在同一事务完成。Worker 使用 `FOR UPDATE SKIP LOCKED` 原子领取作业，领取事务会立即提交；耗时的 Agent 进程不持有数据库行锁。`LISTEN/NOTIFY` 只负责低延迟唤醒，`jobs` 和 `events` 表始终是事实来源，5 秒轮询负责通知丢失、延迟重试和恢复兜底。

每个代码修改作业还必须取得 `workspace_leases` 排他租约。默认租约 30 秒、每 10 秒续租；同一 workspace 只能有一个修改作业，不同 workspace 可在 `WORKER_CONCURRENCY` 限制内并发。

状态更新使用 `version BIGINT` 乐观锁。版本不匹配时 REST 返回 `409 resource_version_conflict`，客户端应刷新资源后再决定是否重试。

## PlanSpec 与 Agent 执行

Agent 生成 JSON `PlanSpec`，后端负责 schema 校验、scope 规范化、连续任务编号、最终验收补全和确定性 Markdown 渲染。Agent 不能直接决定最终计划 Markdown。

首期 Adapter：

- Codex CLI
- Claude CLI
- validation（只用于最终验证 run 记录）

Agent binary 和参数直接传给进程，不默认经过 shell。Runner 负责进程组取消、超时、stdout/stderr 流式日志、最大捕获大小、PID/session 记录和敏感环境变量限制。**项目验证命令是唯一允许通过 `/bin/sh` 执行的项目命令**，工作目录固定为项目 workspace。

Agent 完整输出写入 `DATA_DIR/logs`。数据库事件只保存 `agent-run:<run-id>` 日志引用和累计字节数，不复制完整输出或密钥。事件历史与 SSE 会过滤 `agent.output`，并且新的 `agent.output` 不再写入事件表；这不会删除独立的智能体运行记录或日志文件，`/api/v1/projects/{projectId}/agent-runs` 与 `/api/v1/agent-runs/{agentRunId}/log` 仍用于查看和分页读取完整运行日志。

## API、SSE 与 MCP

REST 前缀为 `/api/v1`。异步动作返回 `202` 及 `jobId`、状态和资源版本。错误结构统一为：

```json
{
  "code": "resource_version_conflict",
  "message": "Resource version conflict",
  "details": null,
  "requestId": "..."
}
```

事件历史地址为 `/api/v1/events?projectId=<uuid>`。`projectId` 必须是合法 UUID；省略 `before` 时返回最新一页，`limit` 默认 10、允许范围为 1–1000。每页 `items` 按 `events.id` 从大到小排列；加载更早历史时，把上一页非空的 `nextBefore` 原样作为排他的 `before`，服务端只返回 `id < before` 的事件。`hasMore=false` 时 `nextBefore` 为 `null`。非法 UUID、非正数/非整数 `before` 以及越界 `limit` 都返回明确的 `400`，不会回退到默认参数。

SSE 地址为 `/api/v1/events/stream?projectId=<uuid>`。PostgreSQL `events.id` 同时作为 SSE `id`；客户端可通过 `Last-Event-ID` 请求头或非负的 `after` 查询参数恢复增量流，同时提供二者时取较大的游标，并且只回放 `id` 严格大于该值的事件。历史回放和后续推送始终按 `events.id` 从小到大（旧到新）发送，以保持状态更新顺序；非法项目 UUID、`Last-Event-ID` 或 `after` 返回 `400`。

`agent.output` 不属于领域状态事件：写入层拒绝新增此类记录，历史 REST 查询和 SSE 查询也显式排除数据库中可能遗留的旧记录。过滤只降低事件总线的高频噪声；独立的 `agent_runs` 元数据和 `DATA_DIR/logs` 运行日志仍完整保留，并通过专用运行日志 API 查询。

MCP 暴露在 `/mcp`，使用独立 Bearer Token。MCP 和 REST 复用同一个 application service 与 repository，因此产生相同的资源、作业和领域事件。MCP 不提供任意路径读取或任意命令执行工具。

## 认证和本机边界

默认 HTTP 地址为 `127.0.0.1:43846`。中间件拒绝非 loopback Host，以及来源不是 loopback 的 Origin。

- `ACCESS_TOKEN`：浏览器 bootstrap token。未配置时启动生成一次性 token，并将登录 URL 输出到后端日志；前端兑换为 `HttpOnly`、`SameSite=Strict` Cookie。
- `MCP_TOKEN`：独立 MCP Bearer Token。未配置时启动生成并只在日志中输出一次。
- Token 摘要写入 PostgreSQL，API、事件和 Agent 日志不得暴露明文 token、数据库 URL 或 Agent 密钥。

文件路径只允许位于已登记的项目 workspace、`DATA_DIR` 和受控 attachment 目录。路径策略会检查绝对路径、真实路径、符号链接和目录边界。

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DATABASE_URL` | 无，必填 | PostgreSQL 连接字符串；生产环境应启用 TLS |
| `HTTP_ADDR` | `127.0.0.1:43846` | HTTP 监听地址 |
| `DATA_DIR` | 用户配置目录下的 `specrelay` | attachment、Agent 日志等受控数据 |
| `PUBLIC_DIR` | 空 | 前端静态目录；容器内为 `/app/frontend` |
| `WORKER_CONCURRENCY` | `2` | 全局 Worker 并发，范围 1–64 |
| `LOG_LEVEL` | `info` | `debug`、`info`、`warn` 或 `error` |
| `WORKSPACE_LEASE_DURATION` | `30s` | workspace 租约时长 |
| `WORKSPACE_LEASE_HEARTBEAT` | `10s` | 租约续租间隔，必须短于租约时长 |
| `JOB_POLL_INTERVAL` | `5s` | 队列轮询兜底间隔 |
| `ACCESS_TOKEN` | 随机生成 | 浏览器 bootstrap token |
| `MCP_TOKEN` | 随机生成 | MCP Bearer Token |

项目级 Codex、Claude、超时、重试、验证命令和允许传递的环境变量存放在 `project_settings`。

## 本地开发

要求 Go 1.25+、Node.js 22+、npm 和 PostgreSQL 16+。

```bash
# 只启动开发数据库
docker compose -f deploy/docker-compose.yml up -d postgres

# 后端
cd backend
DATABASE_URL='postgresql://specrelay:specrelay-dev-only@127.0.0.1:54329/specrelay?sslmode=disable' \
ACCESS_TOKEN='local-browser-token' \
MCP_TOKEN='local-mcp-token' \
go run ./cmd/specrelay

# 另一个终端：前端，/api 和 /mcp 会代理到 127.0.0.1:43846
cd frontend
npm ci
npm run api:generate
npm run dev
```

打开 `http://127.0.0.1:43847/?token=local-browser-token`。

## Docker Compose

```bash
ACCESS_TOKEN='replace-with-a-long-random-value' \
MCP_TOKEN='replace-with-another-long-random-value' \
POSTGRES_PASSWORD='replace-me' \
docker compose -f deploy/docker-compose.yml up --build -d

curl http://127.0.0.1:43846/healthz
curl http://127.0.0.1:43846/readyz
```

镜像内包含编译后的 React 前端和 `/bin/sh`，不依赖宿主机 `frontend/dist`。Compose 默认只把 HTTP 和 PostgreSQL 绑定到 `127.0.0.1`。

容器内创建项目时，workspace 路径必须使用容器可见路径。默认把 `${WORKSPACE_ROOT:-..}` 挂载到 `/workspaces`；例如宿主机仓库位于挂载根下的 `demo`，项目路径应填写 `/workspaces/demo`。Codex/Claude CLI 也必须以镜像扩展、受控 sidecar 或明确挂载的方式在后端容器内可用。

## 测试

```bash
# 前端
cd frontend
npm run api:generate
npm run typecheck
npm test
npm run build

# 后端单元测试、静态检查和构建
cd ../backend
go test ./... -count=1
go vet ./...
go build -trimpath -o /tmp/specrelay ./cmd/specrelay

# PostgreSQL 集成测试
TEST_DATABASE_URL='postgresql://specrelay:specrelay_test@127.0.0.1:55432/specrelay_test?sslmode=disable' \
go test ./... -count=1
```

数据库集成测试会清空测试数据库中的 SpecRelay 表，禁止指向开发或生产数据库。
