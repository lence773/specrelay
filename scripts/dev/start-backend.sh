#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

go_bin="${GO_BIN:-go}"
if ! command -v "$go_bin" >/dev/null 2>&1; then
  echo "Go was not found. Install Go 1.25+ or set GO_BIN to its executable path." >&2
  exit 1
fi

postgres_password="${POSTGRES_PASSWORD:-specrelay-dev-only}"
postgres_port="${POSTGRES_PORT:-54329}"
export DATABASE_URL="${DATABASE_URL:-postgresql://specrelay:${postgres_password}@127.0.0.1:${postgres_port}/specrelay?sslmode=disable}"
export HTTP_ADDR="${HTTP_ADDR:-127.0.0.1:43846}"
export DATA_DIR="${DATA_DIR:-$root_dir/.specrelay-runtime/data}"
export WORKER_CONCURRENCY="${WORKER_CONCURRENCY:-2}"

# Set PUBLIC_DIR only when a production frontend build exists. In Vite dev mode,
# leave it unset and use the Vite proxy at http://127.0.0.1:43847 instead.
if [[ -z "${PUBLIC_DIR:-}" && -f "$root_dir/frontend/dist/index.html" ]]; then
  export PUBLIC_DIR="$root_dir/frontend/dist"
fi

docker compose -f deploy/docker-compose.yml up -d --wait postgres
cd "$root_dir/backend"
exec "$go_bin" run ./cmd/specrelay
