#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"

go_bin="${GO_BIN:-go}"
for command in npm cargo rustc "$go_bin"; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "Required command not found: $command" >&2
    echo "Install Node.js/npm, Rust/cargo and Go 1.25+, or set GO_BIN." >&2
    exit 1
  fi
done

target_triple="$(rustc --print host-tuple 2>/dev/null || true)"
if [[ -z "$target_triple" ]]; then
  target_triple="$(rustc -vV | awk '/^host: / { print $2 }')"
fi
if [[ -z "$target_triple" ]]; then
  echo "Unable to determine the Rust target triple." >&2
  exit 1
fi

if [[ -n "${TAURI_BUNDLES:-}" ]]; then
  tauri_bundles="$TAURI_BUNDLES"
elif [[ "$target_triple" == *-windows-* ]]; then
  tauri_bundles="nsis"
elif [[ "$target_triple" == *-apple-darwin ]]; then
  tauri_bundles="dmg"
else
  tauri_bundles="deb"
fi

sidecar="desktop/src-tauri/binaries/specrelay-${target_triple}"
if [[ "$target_triple" == *-windows-* ]]; then
  sidecar+=".exe"
fi

printf 'Building frontend…\n'
npm --prefix frontend ci
npm --prefix frontend run build

printf 'Building host backend sidecar for %s…\n' "$target_triple"
mkdir -p desktop/src-tauri/binaries desktop/src-tauri/resources/frontend
(
  cd backend
  CGO_ENABLED=0 "$go_bin" build -trimpath -ldflags='-s -w' \
    -o "../$sidecar" \
    ./cmd/specrelay
)
if [[ "$target_triple" != *-windows-* ]]; then
  chmod +x "$sidecar"
fi

# The desktop shell opens the Go server's static site so browser requests remain
# same-origin. Keep generated frontend assets out of git; this script recreates
# them for every package build.
find desktop/src-tauri/resources/frontend -mindepth 1 -maxdepth 1 ! -name .gitkeep -exec rm -rf {} +
cp -a frontend/dist/. desktop/src-tauri/resources/frontend/

printf 'Building desktop bundles (%s)…\n' "$tauri_bundles"
npm --prefix desktop ci
npm --prefix desktop exec tauri build -- --bundles "$tauri_bundles"

printf '\nDesktop bundles created under:\n  %s\n' \
  "$root_dir/desktop/src-tauri/target/release/bundle"
