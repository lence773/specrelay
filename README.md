# SpecRelay

**English** | [简体中文](README.zh-CN.md)

SpecRelay is a **local-first workflow tool with a Chinese UI**. It turns requirements and feedback into reviewable plans, then uses the Codex CLI or Claude CLI installed on your computer to execute ordered tasks, stream progress, and run validation.

> **Core boundary: the backend always runs on the host.** It can therefore browse real local project directories and call your already-installed, authenticated Codex / Claude CLI. Docker is an optional development PostgreSQL helper only—never a runtime for the backend or CLI.

## Recommended: desktop installer

This repository includes native desktop packaging for Linux, Windows, and macOS. Each installer bundles only the Tauri shell, the Go backend, and the frontend. It **does not** bundle, install, start, stop, or manage PostgreSQL/Docker.

On first launch, SpecRelay shows a Chinese database-connection setup page. Enter the host, port, database name, user, password, and SSL mode for a PostgreSQL instance you manage. After saving, it starts the bundled Go backend on the host machine. If the database is empty, the backend automatically creates the migration metadata and all required tables before opening the main UI. Existing data is never deleted by this initialization step.

The connection URL is stored only in the current operating-system user's SpecRelay application-data directory. On Unix-like systems the configuration file is restricted to mode `0600` when possible.

### Prerequisites

- Linux x64, Windows x64, or macOS (Intel or Apple Silicon)
- A reachable PostgreSQL 16 database that you operate (local, LAN, or managed service)
- At least one installed and authenticated local CLI: `codex` or `claude`

The desktop app does not put a CLI in a container and does not upload project folders. You select existing local folders directly in the UI. It has no native system title bar; use the in-app top bar to drag, minimize, maximize, or close the window.

### GitHub Actions build

Run the **Desktop package** workflow manually in GitHub Actions to create downloadable build artifacts for every platform. Pushing a version tag such as `v1.0.0` runs the same native builds and publishes all generated installers to a GitHub Release.

| Target | Native runner | Outputs |
| --- | --- | --- |
| Linux x64 | Ubuntu | `.deb`, `.AppImage`, `.rpm` |
| Windows x64 | Windows Server | NSIS installer (`.exe`), MSI (`.msi`) |
| macOS Intel | macOS Intel | `.dmg` |
| macOS Apple Silicon | macOS Apple Silicon | `.dmg` |

The installers are not code-signed by this workflow. Before public distribution, configure the appropriate Apple and Windows code-signing/notarization credentials; otherwise macOS Gatekeeper or Windows SmartScreen may require an explicit user confirmation.

### Build and install

```bash
# Requires Go 1.25+, Node.js 22+, and Rust/cargo.
# Builds the native package type for the current operating system.
./scripts/package-desktop.sh

# Linux: build all supported Linux formats, then install the Debian package.
TAURI_BUNDLES=deb,appimage,rpm ./scripts/package-desktop.sh
sudo apt install ./desktop/src-tauri/target/release/bundle/deb/*.deb

# Windows (run from Git Bash) / macOS: native installers are built only on
# their matching operating system.
TAURI_BUNDLES=nsis,msi ./scripts/package-desktop.sh  # Windows
TAURI_BUNDLES=dmg ./scripts/package-desktop.sh       # macOS
```

Launch **SpecRelay** from the application menu and complete the database connection form. Docker is not needed for the desktop application itself.

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

### Safe shutdown and recovery

When the desktop window closes, it first shows a clear shutdown status and sends an authenticated request only to the host backend launched by that desktop instance. The backend uses a **20-second cleanup window** (not a normal CLI execution timeout) to stop claiming new work, persist ownership state, and terminate the Codex / Claude process groups it started. An unresponsive CLI is sent `SIGTERM` and escalated to `SIGKILL` after two seconds, preventing orphaned child processes.

Read-only plan-generation jobs are returned to the queue and can resume safely. Code-execution work that may already have changed a workspace is reset to pending and its plan is marked blocked, so it is never blindly rerun. Workspace leases are released. After a crash or forced termination, runtime heartbeats are checked by a later or still-running backend: after three missed beats (with a 30-second minimum), the same recovery rules are applied. Ownership is instance-scoped, so a live second desktop instance is not interrupted, and neither PostgreSQL nor Docker is stopped or modified.

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

- Database migrations run automatically whenever the backend starts, including after a desktop user saves a new database connection.
- The desktop app does not own the database lifecycle. Back up, secure, monitor, and upgrade the PostgreSQL instance using your normal database operations.
- Closing the desktop window stops only that launch's host backend. It does not issue Docker commands and does not stop, delete, or modify the configured PostgreSQL service.
- `deploy/docker-compose.yml` remains an optional **development-only** PostgreSQL helper. Its `specrelay-postgres` volume must not be treated as a desktop-managed production backup.

Example backup for a PostgreSQL instance reachable from your machine:

```bash
pg_dump "$DATABASE_URL" > specrelay-backup.sql
```

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

# Native desktop package (run on the target operating system)
cd ..
./scripts/package-desktop.sh
```

When `TEST_DATABASE_URL` points to an isolated database, backend tests run PostgreSQL integration cases. **Never** point it at development, desktop, or production data.

## License

[MIT](LICENSE)
