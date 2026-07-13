# SpecRelay

**English** | [简体中文](README.zh-CN.md)

SpecRelay is a local-first agent workflow service that turns requirements and feedback into structured plans, ordered implementation tasks, streamed execution logs, and final validation.

The project is a clean Go + PostgreSQL + React implementation with no legacy desktop or SQLite compatibility layer.

## Stack

- **Backend:** Go, `net/http`, `pgx/v5`, PostgreSQL-backed jobs and events
- **Frontend:** React, TypeScript, Vite, TanStack Query
- **Contract:** OpenAPI 3.1 in `api/openapi.yaml`
- **Automation:** Codex CLI and Claude CLI adapters
- **Realtime:** SSE with PostgreSQL event replay
- **Integration:** REST API and MCP server sharing the same application services

## Repository layout

```text
api/        OpenAPI contract
backend/    Go API, workers, agent runner, MCP, migrations
frontend/   React web application and generated API client
deploy/     Docker Compose configuration
docs/       Architecture documentation
```

## Quick start with Docker Compose

```bash
ACCESS_TOKEN="replace-with-a-long-random-value" \
MCP_TOKEN="replace-with-another-long-random-value" \
POSTGRES_PASSWORD="replace-me" \
docker compose -f deploy/docker-compose.yml up --build -d
```

Then open `http://127.0.0.1:43846/?token=<ACCESS_TOKEN>`.

Health endpoints:

```bash
curl http://127.0.0.1:43846/healthz
curl http://127.0.0.1:43846/readyz
```

## Local development

Requirements: Go 1.25+, Node.js 22+, npm, and PostgreSQL 16+.

```bash
# Database
docker compose -f deploy/docker-compose.yml up -d postgres

# Backend
cd backend
DATABASE_URL="postgresql://specrelay:specrelay-dev-only@127.0.0.1:54329/specrelay?sslmode=disable" \
ACCESS_TOKEN="local-browser-token" \
MCP_TOKEN="local-mcp-token" \
go run ./cmd/specrelay

# Frontend (in another terminal)
cd frontend
npm ci
npm run api:generate
npm run dev
```

The Vite development server is available at `http://127.0.0.1:43847/?token=local-browser-token` and proxies API/MCP requests to the Go backend.

## Verification

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

PostgreSQL integration tests run when `TEST_DATABASE_URL` points to an isolated test database. Never point integration tests at development or production data.

See [docs/go-postgres-architecture.md](docs/go-postgres-architecture.md) for the architecture, security model, queue semantics, and configuration reference.
