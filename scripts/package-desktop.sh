#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"

is_semver() {
  local version="$1"
  local semver_pattern='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+)(\.[0-9A-Za-z-]+)*)?(\+([0-9A-Za-z-]+)(\.[0-9A-Za-z-]+)*)?$'
  if [[ ! "$version" =~ $semver_pattern ]]; then
    return 1
  fi

  local prerelease=''
  if [[ "$version" == *-* ]]; then
    prerelease="${version#*-}"
    prerelease="${prerelease%%+*}"
  fi
  if [[ -n "$prerelease" ]]; then
    local identifier
    IFS='.' read -r -a prerelease_identifiers <<< "$prerelease"
    for identifier in "${prerelease_identifiers[@]}"; do
      if [[ "$identifier" =~ ^[0-9]+$ && "$identifier" != '0' && "$identifier" == 0* ]]; then
        return 1
      fi
    done
  fi
}

read_json_version() {
  local path="$1"
  node -e \
    'const fs = require("fs"); const value = JSON.parse(fs.readFileSync(process.argv[1], "utf8")).version; if (typeof value !== "string" || value.length === 0) process.exit(1); process.stdout.write(value);' \
    "$path"
}

read_cargo_version() {
  awk '
    /^\[package\][[:space:]]*$/ { in_package = 1; next }
    in_package && /^\[/ { exit }
    in_package && /^[[:space:]]*version[[:space:]]*=/ {
      value = $0
      sub(/^[^=]*=[[:space:]]*"/, "", value)
      sub(/"[[:space:]]*$/, "", value)
      print value
      exit
    }
  ' desktop/src-tauri/Cargo.toml
}

validate_versions() {
  if ! command -v node >/dev/null 2>&1; then
    echo 'Required command not found for version validation: node' >&2
    exit 1
  fi

  local package_version cargo_version tauri_version release_tag release_version
  package_version="$(read_json_version desktop/package.json)"
  cargo_version="$(read_cargo_version)"
  tauri_version="$(read_json_version desktop/src-tauri/tauri.conf.json)"

  if [[ -z "$cargo_version" ]]; then
    echo 'Unable to read [package].version from desktop/src-tauri/Cargo.toml.' >&2
    exit 1
  fi
  if ! is_semver "$package_version"; then
    echo "desktop/package.json has an invalid SemVer version: $package_version" >&2
    exit 1
  fi
  if [[ "$cargo_version" != "$package_version" || "$tauri_version" != "$package_version" ]]; then
    printf 'Desktop versions must match exactly before packaging:\n' >&2
    printf '  desktop/package.json:              %s\n' "$package_version" >&2
    printf '  desktop/src-tauri/Cargo.toml:      %s\n' "$cargo_version" >&2
    printf '  desktop/src-tauri/tauri.conf.json: %s\n' "$tauri_version" >&2
    exit 1
  fi

  release_tag="$1"
  if [[ -n "$release_tag" ]]; then
    if [[ "$release_tag" != v* ]]; then
      echo "Release tag must start with v: $release_tag" >&2
      exit 1
    fi
    release_version="${release_tag#v}"
    if ! is_semver "$release_version"; then
      echo "Release tag is not a valid SemVer tag: $release_tag" >&2
      exit 1
    fi
    if [[ "$release_version" != "$package_version" ]]; then
      printf 'Release tag and desktop versions must match exactly:\n' >&2
      printf '  tag:                                %s\n' "$release_version" >&2
      printf '  desktop/package.json:               %s\n' "$package_version" >&2
      printf '  desktop/src-tauri/Cargo.toml:       %s\n' "$cargo_version" >&2
      printf '  desktop/src-tauri/tauri.conf.json:  %s\n' "$tauri_version" >&2
      exit 1
    fi
  fi

  printf '%s\n' "$package_version"
}

if [[ "${1:-}" == '--check-version-only' ]]; then
  validate_versions "${2:-${SPECRELAY_RELEASE_TAG:-}}"
  exit 0
fi
if [[ $# -ne 0 ]]; then
  echo "Usage: $0 [--check-version-only [vSEMVER]]" >&2
  exit 2
fi

version="$(validate_versions "${SPECRELAY_RELEASE_TAG:-}")"
if [[ "${SPECRELAY_OFFICIAL_RELEASE:-false}" == 'true' ]]; then
  official_release=true
else
  official_release=false
fi
printf 'Validated desktop version %s%s (%s build).\n' \
  "$version" "${SPECRELAY_RELEASE_TAG:+ against tag $SPECRELAY_RELEASE_TAG}" \
  "$([[ "$official_release" == true ]] && printf official || printf unofficial)"

# Manual CI builds must not accidentally consume partially configured signing
# credentials. They are intentionally marked as unsigned/unofficial artifacts;
# only a version-tagged official release may import credentials and sign bundles.
if [[ "$official_release" != true ]]; then
  unset TAURI_SIGNING_PRIVATE_KEY TAURI_SIGNING_PRIVATE_KEY_PASSWORD TAURI_UPDATER_PUBLIC_KEY
  unset WINDOWS_CERTIFICATE_THUMBPRINT WINDOWS_TIMESTAMP_URL
  unset APPLE_SIGNING_IDENTITY APPLE_ID APPLE_PASSWORD APPLE_TEAM_ID
fi

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
  tauri_bundles='nsis'
elif [[ "$target_triple" == *-apple-darwin ]]; then
  tauri_bundles='dmg'
else
  tauri_bundles='deb'
fi

sidecar="desktop/src-tauri/binaries/specrelay-${target_triple}"
if [[ "$target_triple" == *-windows-* ]]; then
  sidecar+='.exe'
fi

require_nonempty() {
  local variable_name="$1"
  local purpose="$2"
  if [[ -z "${!variable_name:-}" ]]; then
    printf 'Required %s variable is empty: %s\n' "$purpose" "$variable_name" >&2
    exit 1
  fi
}

updater_signing=false
if [[ -n "${TAURI_SIGNING_PRIVATE_KEY:-}${TAURI_SIGNING_PRIVATE_KEY_PASSWORD:-}${TAURI_UPDATER_PUBLIC_KEY:-}" ]]; then
  require_nonempty TAURI_SIGNING_PRIVATE_KEY 'updater signing'
  require_nonempty TAURI_UPDATER_PUBLIC_KEY 'updater verification'
  updater_signing=true
fi
if [[ "$official_release" == true ]]; then
  require_nonempty TAURI_SIGNING_PRIVATE_KEY 'official updater signing'
  require_nonempty TAURI_SIGNING_PRIVATE_KEY_PASSWORD 'official updater signing'
  require_nonempty TAURI_UPDATER_PUBLIC_KEY 'official updater verification'
  updater_signing=true
fi

code_signing_status='unsigned'
code_signing_detail='No platform code-signing credential was supplied.'
if [[ "$target_triple" == *-windows-* ]]; then
  if [[ "$official_release" == true ]]; then
    require_nonempty WINDOWS_CERTIFICATE_THUMBPRINT 'official Windows Authenticode signing'
  fi
  if [[ -n "${WINDOWS_CERTIFICATE_THUMBPRINT:-}" ]]; then
    code_signing_status='pending-authenticode-verification'
    code_signing_detail='A trusted Windows certificate was selected; installer verification is pending.'
  fi
elif [[ "$target_triple" == *-apple-darwin ]]; then
  apple_variables=(
    APPLE_SIGNING_IDENTITY
    APPLE_ID
    APPLE_PASSWORD
    APPLE_TEAM_ID
  )
  apple_credentials_present=false
  for variable_name in "${apple_variables[@]}"; do
    if [[ -n "${!variable_name:-}" ]]; then
      apple_credentials_present=true
    fi
  done
  if [[ "$official_release" == true || "$apple_credentials_present" == true ]]; then
    for variable_name in "${apple_variables[@]}"; do
      require_nonempty "$variable_name" 'macOS Developer ID signing and notarization'
    done
    code_signing_status='pending-developer-id-verification'
    code_signing_detail='Developer ID signing and notarization credentials were supplied; verification is pending.'
  fi
else
  code_signing_status='not-available'
  code_signing_detail='Linux has no single OS-vendor platform code-signing and notarization trust chain; updater signatures and SHA-256 hashes provide artifact integrity.'
fi

updater_config="$({
  UPDATER_ENABLED="$updater_signing" \
  WINDOWS_THUMBPRINT="${WINDOWS_CERTIFICATE_THUMBPRINT:-}" \
  WINDOWS_TIMESTAMP_URL="${WINDOWS_TIMESTAMP_URL:-http://timestamp.digicert.com}" \
  MACOS_SIGNING_IDENTITY="${APPLE_SIGNING_IDENTITY:-}" \
  node <<'NODE'
const updaterEnabled = process.env.UPDATER_ENABLED === 'true';
const config = {
  bundle: { createUpdaterArtifacts: updaterEnabled },
  plugins: {
    updater: {
      pubkey: updaterEnabled ? (process.env.TAURI_UPDATER_PUBLIC_KEY || '') : ''
    }
  }
};
if (process.env.WINDOWS_THUMBPRINT) {
  config.bundle.windows = {
    certificateThumbprint: process.env.WINDOWS_THUMBPRINT,
    digestAlgorithm: 'sha256',
    timestampUrl: process.env.WINDOWS_TIMESTAMP_URL,
    tsp: false
  };
}
if (process.env.MACOS_SIGNING_IDENTITY) {
  config.bundle.macOS = {
    signingIdentity: process.env.MACOS_SIGNING_IDENTITY,
    hardenedRuntime: true
  };
}
process.stdout.write(JSON.stringify(config));
NODE
})"

printf 'Building frontend…\n'
npm --prefix frontend ci
npm --prefix frontend run build

printf 'Building host backend sidecar for %s…\n' "$target_triple"
mkdir -p desktop/src-tauri/binaries desktop/src-tauri/resources/frontend
backend_ldflags='-s -w'
if [[ "$target_triple" == *-windows-* ]]; then
  # The backend is owned by the desktop app; compiling it as a GUI subsystem
  # executable prevents Windows from creating a visible CMD window at startup.
  backend_ldflags+=' -H=windowsgui'
fi
(
  cd backend
  CGO_ENABLED=0 "$go_bin" build -trimpath -ldflags="$backend_ldflags" \
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

printf 'Building desktop bundles (%s); updater signing: %s…\n' "$tauri_bundles" "$updater_signing"
npm --prefix desktop ci
npm --prefix desktop exec tauri build -- \
  --bundles "$tauri_bundles" \
  --config "$updater_config"

bundle_dir="$root_dir/desktop/src-tauri/target/release/bundle"
find_one_bundle() {
  local relative_dir="$1"
  local pattern="$2"
  local path_type="$3"
  local description="$4"
  local -a matches=()
  if [[ -d "$bundle_dir/$relative_dir" ]]; then
    while IFS= read -r -d '' match; do
      matches+=("$match")
    done < <(
      find "$bundle_dir/$relative_dir" -maxdepth 1 -type "$path_type" -name "$pattern" -print0
    )
  fi
  if [[ "${#matches[@]}" -ne 1 ]]; then
    printf 'Expected exactly one %s (%s/%s), found %d.\n' \
      "$description" "$relative_dir" "$pattern" "${#matches[@]}" >&2
    exit 1
  fi
  printf 'Verified %s: %s\n' "$description" "${matches[0]}" >&2
  printf '%s\n' "${matches[0]}"
}

require_update_signature() {
  local relative_dir="$1"
  local pattern="$2"
  local description="$3"
  if [[ "$updater_signing" == true ]]; then
    find_one_bundle "$relative_dir" "$pattern" f "$description" >/dev/null
  fi
}

IFS=',' read -r -a requested_bundles <<< "$tauri_bundles"
for requested_bundle in "${requested_bundles[@]}"; do
  requested_bundle="${requested_bundle//[[:space:]]/}"
  requested_bundle="$(printf '%s' "$requested_bundle" | tr '[:upper:]' '[:lower:]')"
  case "$requested_bundle" in
    deb)
      find_one_bundle deb '*.deb' f 'Linux DEB installer/update payload' >/dev/null
      require_update_signature deb '*.deb.sig' 'Linux DEB update signature'
      ;;
    appimage)
      find_one_bundle appimage '*.AppImage' f 'Linux AppImage installer/update payload' >/dev/null
      require_update_signature appimage '*.AppImage.sig' 'Linux AppImage update signature'
      ;;
    rpm)
      find_one_bundle rpm '*.rpm' f 'Linux RPM installer/update payload' >/dev/null
      require_update_signature rpm '*.rpm.sig' 'Linux RPM update signature'
      ;;
    nsis)
      find_one_bundle nsis '*.exe' f 'Windows NSIS installer/update payload' >/dev/null
      require_update_signature nsis '*.exe.sig' 'Windows NSIS update signature'
      ;;
    msi)
      find_one_bundle msi '*.msi' f 'Windows MSI installer/update payload' >/dev/null
      require_update_signature msi '*.msi.sig' 'Windows MSI update signature'
      ;;
    dmg)
      find_one_bundle dmg '*.dmg' f 'macOS DMG installer' >/dev/null
      if [[ "$updater_signing" == true ]]; then
        find_one_bundle macos '*.app.tar.gz' f 'macOS updater archive' >/dev/null
        find_one_bundle macos '*.app.tar.gz.sig' f 'macOS updater signature' >/dev/null
      fi
      ;;
    *)
      echo "Unsupported TAURI_BUNDLES entry for artifact verification: $requested_bundle" >&2
      exit 1
      ;;
  esac
done

if [[ "$target_triple" == *-windows-* && -n "${WINDOWS_CERTIFICATE_THUMBPRINT:-}" ]]; then
  printf 'Verifying Authenticode signatures and trust status…\n'
  windows_bundle_dir="$bundle_dir"
  if command -v cygpath >/dev/null 2>&1; then
    windows_bundle_dir="$(cygpath -w "$bundle_dir")"
  fi
  # The single-quoted program is expanded by PowerShell, not Bash.
  # shellcheck disable=SC2016
  BUNDLE_DIR="$windows_bundle_dir" powershell.exe -NoProfile -NonInteractive -Command '
    $ErrorActionPreference = "Stop"
    $files = Get-ChildItem -Path $env:BUNDLE_DIR -Recurse -File |
      Where-Object { $_.Extension -in ".exe", ".msi" }
    if ($files.Count -eq 0) { throw "No Windows installers were found for Authenticode verification." }
    foreach ($file in $files) {
      $signature = Get-AuthenticodeSignature -LiteralPath $file.FullName
      if ($signature.Status -ne "Valid") {
        throw "Invalid Authenticode signature for $($file.FullName): $($signature.Status) $($signature.StatusMessage)"
      }
      Write-Host "Verified Authenticode signature: $($file.FullName) [$($signature.SignerCertificate.Subject)]"
    }
  '
  code_signing_status='verified-authenticode'
  code_signing_detail='All generated Windows installers have a valid Authenticode trust result and SHA-256 timestamped signature.'
fi

if [[ "$target_triple" == *-apple-darwin && -n "${APPLE_SIGNING_IDENTITY:-}" ]]; then
  app_path="$(find_one_bundle macos '*.app' d 'macOS application bundle')"
  dmg_path="$(find_one_bundle dmg '*.dmg' f 'macOS DMG for notarization')"

  printf 'Verifying Developer ID application signature…\n'
  codesign --verify --deep --strict --verbose=2 "$app_path"
  codesign -dv --verbose=4 "$app_path" 2>&1 | grep -F 'Authority=Developer ID Application:' >/dev/null

  # Tauri notarizes the application when the Apple credentials are present. If
  # the ticket was not stapled, submit the exact app once, staple it, and refresh
  # the updater archive/signature so the update payload contains that ticket.
  if ! xcrun stapler validate "$app_path" >/dev/null 2>&1; then
    app_zip="$bundle_dir/macos/SpecRelay-notarization.zip"
    rm -f "$app_zip"
    ditto -c -k --keepParent "$app_path" "$app_zip"
    xcrun notarytool submit "$app_zip" \
      --apple-id "$APPLE_ID" \
      --password "$APPLE_PASSWORD" \
      --team-id "$APPLE_TEAM_ID" \
      --wait
    xcrun stapler staple "$app_path"
    rm -f "$app_zip"

    if [[ "$updater_signing" == true ]]; then
      updater_archive="$(find_one_bundle macos '*.app.tar.gz' f 'macOS updater archive to refresh')"
      rm -f "$updater_archive" "$updater_archive.sig"
      tar -czf "$updater_archive" -C "$(dirname "$app_path")" "$(basename "$app_path")"
      npm --prefix desktop exec tauri signer sign -- "$updater_archive"
      find_one_bundle macos '*.app.tar.gz.sig' f 'refreshed macOS updater signature' >/dev/null
    fi
  fi
  xcrun stapler validate "$app_path"
  spctl --assess --type execute --verbose=2 "$app_path"

  # Sign the final disk image itself, submit that exact immutable byte stream,
  # staple its ticket, and verify both the signature and Gatekeeper assessment.
  codesign --force --timestamp --sign "$APPLE_SIGNING_IDENTITY" "$dmg_path"
  xcrun notarytool submit "$dmg_path" \
    --apple-id "$APPLE_ID" \
    --password "$APPLE_PASSWORD" \
    --team-id "$APPLE_TEAM_ID" \
    --wait
  xcrun stapler staple "$dmg_path"
  xcrun stapler validate "$dmg_path"
  codesign --verify --strict --verbose=2 "$dmg_path"
  codesign -dv --verbose=4 "$dmg_path" 2>&1 | grep -F 'Authority=Developer ID Application:' >/dev/null
  spctl --assess --type open --context context:primary-signature --verbose=2 "$dmg_path"

  code_signing_status='verified-developer-id-notarized-stapled'
  code_signing_detail='The application and final DMG passed Developer ID signature, notarization ticket, stapling, and Gatekeeper verification.'
fi

updater_signing_status='unsigned'
updater_signing_detail='Updater artifacts were disabled because no independent Tauri updater signing key was supplied.'
if [[ "$updater_signing" == true ]]; then
  updater_signing_status='signed'
  updater_signing_detail='Every generated updater payload has a detached Tauri updater signature; the desktop configuration contains only the matching public key.'
fi

mkdir -p "$bundle_dir"
metadata_path="$bundle_dir/build-metadata-${target_triple}.json"
commit_sha="$(git rev-parse HEAD 2>/dev/null || true)"
run_url=''
if [[ -n "${GITHUB_SERVER_URL:-}" && -n "${GITHUB_REPOSITORY:-}" && -n "${GITHUB_RUN_ID:-}" ]]; then
  run_url="$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID"
fi
META_VERSION="$version" \
META_TAG="${SPECRELAY_RELEASE_TAG:-}" \
META_COMMIT_SHA="$commit_sha" \
META_REPOSITORY="${GITHUB_REPOSITORY:-}" \
META_RUN_ID="${GITHUB_RUN_ID:-}" \
META_RUN_ATTEMPT="${GITHUB_RUN_ATTEMPT:-}" \
META_RUN_URL="$run_url" \
META_BUILD_TIME="$(date -u +'%Y-%m-%dT%H:%M:%SZ')" \
META_TARGET_TRIPLE="$target_triple" \
META_BUNDLES="$tauri_bundles" \
META_OFFICIAL="$official_release" \
META_CODE_SIGNING_STATUS="$code_signing_status" \
META_CODE_SIGNING_DETAIL="$code_signing_detail" \
META_UPDATER_SIGNING_STATUS="$updater_signing_status" \
META_UPDATER_SIGNING_DETAIL="$updater_signing_detail" \
META_GO_VERSION="$("$go_bin" version)" \
META_NODE_VERSION="$(node --version)" \
META_NPM_VERSION="$(npm --version)" \
META_CARGO_VERSION="$(cargo --version)" \
META_RUSTC_VERSION="$(rustc --version)" \
META_TAURI_VERSION="$(npm --prefix desktop exec tauri -- --version)" \
node > "$metadata_path" <<'NODE'
const env = process.env;
const bundles = env.META_BUNDLES.split(',').map((value) => value.trim()).filter(Boolean);
const platform = env.META_TARGET_TRIPLE.includes('windows')
  ? 'windows'
  : env.META_TARGET_TRIPLE.includes('apple-darwin')
    ? 'macos'
    : 'linux';
const architecture = env.META_TARGET_TRIPLE.startsWith('aarch64') ? 'arm64' :
  env.META_TARGET_TRIPLE.startsWith('x86_64') ? 'x64' : env.META_TARGET_TRIPLE.split('-')[0];
const nullable = (value) => value || null;
process.stdout.write(`${JSON.stringify({
  schema_version: 1,
  version: env.META_VERSION,
  tag: nullable(env.META_TAG),
  commit_sha: env.META_COMMIT_SHA,
  repository: nullable(env.META_REPOSITORY),
  github_actions: {
    run_id: nullable(env.META_RUN_ID),
    run_attempt: nullable(env.META_RUN_ATTEMPT),
    run_url: nullable(env.META_RUN_URL)
  },
  build_time: env.META_BUILD_TIME,
  official_release: env.META_OFFICIAL === 'true',
  target: {
    triple: env.META_TARGET_TRIPLE,
    platform,
    architecture,
    install_formats: bundles
  },
  code_signing: {
    status: env.META_CODE_SIGNING_STATUS,
    detail: env.META_CODE_SIGNING_DETAIL
  },
  updater_signing: {
    status: env.META_UPDATER_SIGNING_STATUS,
    detail: env.META_UPDATER_SIGNING_DETAIL
  },
  toolchain: {
    go: env.META_GO_VERSION,
    node: env.META_NODE_VERSION,
    npm: env.META_NPM_VERSION,
    cargo: env.META_CARGO_VERSION,
    rustc: env.META_RUSTC_VERSION,
    tauri_cli: env.META_TAURI_VERSION
  }
}, null, 2)}\n`);
NODE

if [[ "$official_release" != true ]]; then
  cat > "$bundle_dir/UNOFFICIAL-BUILD.txt" <<EOF_MARKER
SpecRelay $version unofficial local/CI build

This directory is not an official GitHub Release. Do not redistribute these files as official artifacts.
Platform code signing status: $code_signing_status
Updater signing status: $updater_signing_status
Target: $target_triple
Commit: ${commit_sha:-unknown}

When credentials are absent, installers are intentionally unsigned and updater artifacts are disabled.
Use an official Release plus release-manifest.json.sig and SHA256SUMS for distribution.
EOF_MARKER
  printf '\nWARNING: produced an UNOFFICIAL build (%s; updater: %s).\n' \
    "$code_signing_status" "$updater_signing_status" >&2
fi

printf '\nDesktop bundles created and verified under:\n  %s\n' "$bundle_dir"
printf 'Build metadata: %s\n' "$metadata_path"
