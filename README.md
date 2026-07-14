# SpecRelay

**English** | [简体中文](README.zh-CN.md)

SpecRelay is a **local-first workflow tool with a Chinese UI**. It turns requirements and feedback into reviewable plans, then uses the Codex CLI or Claude CLI installed on your computer to execute ordered tasks, stream progress, and run validation.

> **Core boundary: the backend always runs on the host.** It can therefore browse real local project directories and call your already-installed, authenticated Codex / Claude CLI. Docker is used for PostgreSQL only—never for the backend or CLI.

## Recommended: desktop installer

This repository includes a native Linux `.deb` packaging flow. On launch, the desktop app automatically:

1. starts or reuses its dedicated PostgreSQL Docker Compose service;
2. starts the bundled Go backend on the host machine;
3. opens the Chinese UI from the same-origin local server;
4. preserves the database volume. Closing the window stops only the backend; it **does not** run `docker compose down` or remove data.

### Prerequisites

- Linux (the current package target is `.deb`)
- Docker Engine or Docker Desktop running, with `docker compose` and `docker compose up --wait`
- At least one installed and authenticated local CLI: `codex` or `claude`

The desktop app does not put a CLI in a container and does not upload project folders. You select existing local folders directly in the UI.

### GitHub Actions build

Push a version tag such as `v1.0.0`, or run the **Desktop package** workflow manually in GitHub Actions, to build a Linux `.deb` on an Ubuntu runner. The workflow uploads the installer as a downloadable build artifact.

### Build and install

```bash
# Requires Go 1.25+, Node.js 22+, Rust/cargo, and Docker Compose.
./scripts/package-desktop.sh

# Package names vary by version and architecture.
sudo apt install ./desktop/src-tauri/target/release/bundle/deb/*.deb
```

Launch **SpecRelay** from the application menu. If Docker is unavailable, the startup screen explains the missing dependency; fix it and open the app again.

> Building the Linux desktop package also needs WebKitGTK, GTK, and librsvg development packages. On Debian/Ubuntu:
> `sudo apt install pkg-config libwebkit2gtk-4.1-dev libgtk-3-dev librsvg2-dev`

## Local development

Requirements: Go 1.25+, Node.js 22+, npm, Docker Compose, and PostgreSQL 16 (Docker can provide PostgreSQL). The backend and every CLI run on the host.

```bash
# Terminal 1: start only the database, then run the host backend.
# If frontend/dist exists, the Go server will serve it directly.
./scripts/dev/start-backend.sh

# Terminal 2: Vite proxies /api, /events, and /mcp to the host backend.
cd frontend
npm ci
npm run api:generate
npm run dev
```

When `ACCESS_TOKEN` is unset, the backend prints a one-time access URL. To use a stable local URL instead:

```bash
POSTGRES_PASSWORD=specrelay-dev-only \
ACCESS_TOKEN=local-browser-token \
MCP_TOKEN=local-mcp-token \
./scripts/dev/start-backend.sh
```

Open `http://127.0.0.1:43847/?token=local-browser-token`. You can also start the database manually:

```bash
docker compose -f deploy/docker-compose.yml up -d --wait postgres
```

`deploy/docker-compose.yml` contains **PostgreSQL only**. It neither builds nor starts nor mounts the backend.

## Local CLI and folders

1. Choose an existing local workspace with the directory browser in **Create project**.
2. Configure the Codex or Claude executable, model, and validation command in project settings.
3. Use the **Requirements** page to discuss a proposal with the local CLI before creating the formal requirement.
4. Generate a plan, run it manually, or enable automation to process ready plans in order.

SpecRelay does not impose a CLI-wide timeout. The run view uses a terminal-style concise log: it starts with the newest 50 entries and lazily loads older entries when scrolling upward. Full raw CLI output remains in the controlled application data directory; the browser never gets arbitrary filesystem access.

## Architecture and security boundaries

- **Backend:** Go, `net/http`, `pgx/v5`, PostgreSQL-backed jobs and migrations
- **Frontend:** React, TypeScript, Vite, TanStack Query
- **Desktop shell:** Tauri 2, loading the Go server's same-origin `127.0.0.1` page
- **Automation:** host Codex CLI / Claude CLI in controlled project workspaces
- **Realtime:** SSE with PostgreSQL event replay
- **Integration:** REST API and MCP share the same service layer
- **Authentication:** loopback Host/Origin only; the browser token is exchanged for a local `HttpOnly` cookie and MCP has its own bearer token

```text
api/        OpenAPI contract
backend/    Host Go API, workers, agent runner, MCP, migrations
frontend/   React web application and generated API client
desktop/    Tauri desktop launcher and package configuration
deploy/     PostgreSQL-only Docker Compose configuration
scripts/    Host development and desktop packaging scripts
docs/       Architecture documentation
```

## Data, upgrades, and backups

- The development volume is `specrelay-postgres`; the desktop volume is `specrelay-desktop-postgres`.
- Database migrations run automatically when the backend starts. Back up production data before upgrades.
- Do not run `docker compose down -v` or remove either volume while using the app; that deletes database data.
- Closing the desktop window ends the host backend for that launch only. PostgreSQL stays up with `restart: unless-stopped`, preventing accidental data loss or task interruption from a database shutdown.

Desktop backup example:

```bash
docker exec -t specrelay-desktop-postgres-1 pg_dump -U specrelay specrelay > specrelay-backup.sql
```

Container names vary across Docker Compose versions; check `docker ps` first.

## Health, API, and MCP

Once the host backend is running:

```bash
curl http://127.0.0.1:43846/healthz
curl http://127.0.0.1:43846/readyz
```

- REST prefix: `/api/v1`
- MCP: `/mcp` (separate MCP bearer token)
- OpenAPI contract: [`api/openapi.yaml`](api/openapi.yaml)

For queue semantics, workspace leases, SSE, authentication, and environment variables, see [the architecture document](docs/go-postgres-architecture.md).

## Verification

```bash
# Backend
cd backend
go test ./... -count=1
go vet ./...
go build -trimpath -o /tmp/specrelay ./cmd/specrelay

# Frontend
cd ../frontend
npm ci
npm run api:generate
npm run typecheck
npm test
npm run build

# Linux desktop installer
cd ..
./scripts/package-desktop.sh
```

When `TEST_DATABASE_URL` points to an isolated database, backend tests run PostgreSQL integration cases. **Never** point it at development, desktop, or production data.

## License

[MIT](LICENSE)
