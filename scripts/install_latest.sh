#!/usr/bin/env bash
set -euo pipefail

# ChatGPT2API-Go latest installer/updater
#
# Default behavior:
#   - Detect current OS/ARCH.
#   - Download latest GitHub Release archive.
#   - Install to ./chatgpt2api-go-install unless --dir is specified.
#   - Preserve existing config.json and data/ when updating.
#   - If Release asset is unavailable, use --from-source to build from source.
#
# Examples:
#   bash scripts/install_latest.sh
#   bash scripts/install_latest.sh --dir /opt/chatgpt2api-go
#   bash scripts/install_latest.sh --from-source --web
#   bash scripts/install_latest.sh --repo jwbb903/CHATGPT2API-GO --dir ./app

REPO="${CHATGPT2API_INSTALL_REPO:-jwbb903/CHATGPT2API-GO}"
INSTALL_DIR="${CHATGPT2API_INSTALL_DIR:-$PWD/chatgpt2api-go-install}"
PORT="${CHATGPT2API_INSTALL_PORT:-3000}"
FROM_SOURCE=0
BUILD_WEB=0
SKIP_TESTS=0
START_AFTER_INSTALL=0
FORCE=0

usage() {
  cat <<'EOF'
Usage: scripts/install_latest.sh [options]

Options:
  --repo OWNER/REPO      GitHub repository. Default: jwbb903/CHATGPT2API-GO
  --dir PATH            Install directory. Default: ./chatgpt2api-go-install
  --port PORT           Port shown in final run command. Default: 3000
  --from-source         Clone latest source and build locally if no release archive is used.
  --web                 Build frontend when using --from-source.
  --skip-tests          Skip go test ./... when using --from-source.
  --start               Start service after install by exec'ing start.sh.
  --force               Continue even if install dir has unknown files.
  -h, --help            Show this help.

Environment:
  CHATGPT2API_INSTALL_REPO   Same as --repo.
  CHATGPT2API_INSTALL_DIR    Same as --dir.
  CHATGPT2API_INSTALL_PORT   Same as --port.

Notes:
  - Existing config.json and data/ are preserved during update.
  - Release mode expects assets named like chatgpt2api-go-linux-amd64.tar.gz.
  - Source mode requires git, go, and optionally npm/node for --web.
EOF
}

log() { printf '==> %s\n' "$*"; }
warn() { printf '!! %s\n' "$*" >&2; }
fatal() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)
      REPO="${2:-}"; shift 2 ;;
    --dir)
      INSTALL_DIR="${2:-}"; shift 2 ;;
    --port)
      PORT="${2:-}"; shift 2 ;;
    --from-source)
      FROM_SOURCE=1; shift ;;
    --web)
      BUILD_WEB=1; shift ;;
    --skip-tests)
      SKIP_TESTS=1; shift ;;
    --start)
      START_AFTER_INSTALL=1; shift ;;
    --force)
      FORCE=1; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      fatal "Unknown option: $1" ;;
  esac
done

[[ -n "$REPO" ]] || fatal "--repo cannot be empty"
[[ "$REPO" == */* ]] || fatal "--repo must be OWNER/REPO"
INSTALL_DIR="$(mkdir -p "$(dirname "$INSTALL_DIR")" && cd "$(dirname "$INSTALL_DIR")" && pwd)/$(basename "$INSTALL_DIR")"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || fatal "Missing required command: $1"
}

os_arch() {
  local os arch
  case "$(uname -s)" in
    Linux) os="linux" ;;
    Darwin) os="darwin" ;;
    *) fatal "Unsupported OS: $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) fatal "Unsupported architecture: $(uname -m)" ;;
  esac
  printf '%s %s\n' "$os" "$arch"
}

backup_existing_state() {
  local backup_dir="$1"
  mkdir -p "$backup_dir"
  if [[ -f "$INSTALL_DIR/config.json" ]]; then
    cp "$INSTALL_DIR/config.json" "$backup_dir/config.json"
  fi
  if [[ -d "$INSTALL_DIR/data" ]]; then
    cp -a "$INSTALL_DIR/data" "$backup_dir/data"
  fi
}

restore_existing_state() {
  local backup_dir="$1"
  if [[ -f "$backup_dir/config.json" ]]; then
    cp "$backup_dir/config.json" "$INSTALL_DIR/config.json"
  elif [[ ! -f "$INSTALL_DIR/config.json" && -f "$INSTALL_DIR/config.example.json" ]]; then
    cp "$INSTALL_DIR/config.example.json" "$INSTALL_DIR/config.json"
  fi
  if [[ -d "$backup_dir/data" ]]; then
    rm -rf "$INSTALL_DIR/data"
    cp -a "$backup_dir/data" "$INSTALL_DIR/data"
  else
    mkdir -p "$INSTALL_DIR/data"
  fi
}

check_install_dir() {
  if [[ ! -e "$INSTALL_DIR" ]]; then
    return
  fi
  [[ -d "$INSTALL_DIR" ]] || fatal "$INSTALL_DIR exists but is not a directory"
  if [[ "$FORCE" -eq 0 && ! -f "$INSTALL_DIR/start.sh" && ! -f "$INSTALL_DIR/chatgpt2api-go" && ! -f "$INSTALL_DIR/chatgpt2api" ]]; then
    local count
    count="$(find "$INSTALL_DIR" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')"
    if [[ "$count" != "0" ]]; then
      fatal "$INSTALL_DIR is not empty and does not look like a ChatGPT2API-Go install dir. Use --force to continue."
    fi
  fi
}

install_from_release() {
  need_cmd curl
  need_cmd tar

  read -r os arch < <(os_arch)
  local asset="chatgpt2api-go-${os}-${arch}.tar.gz"
  local url="https://github.com/${REPO}/releases/latest/download/${asset}"
  local tmp archive extract_dir payload_dir backup_dir
  tmp="$(mktemp -d)"
  archive="$tmp/$asset"
  extract_dir="$tmp/extract"
  backup_dir="$tmp/backup"
  mkdir -p "$extract_dir"
  trap 'rm -rf "$tmp"' RETURN

  log "Downloading latest release asset: $url"
  if ! curl -fL --retry 3 --retry-delay 2 -o "$archive" "$url"; then
    warn "Release asset download failed: $asset"
    return 1
  fi

  log "Extracting archive"
  tar -xzf "$archive" -C "$extract_dir"
  payload_dir="$(find "$extract_dir" -mindepth 1 -maxdepth 1 -type d | head -n 1)"
  [[ -n "$payload_dir" ]] || fatal "Archive did not contain an install directory"

  check_install_dir
  backup_existing_state "$backup_dir"
  rm -rf "$INSTALL_DIR"
  mkdir -p "$INSTALL_DIR"
  cp -a "$payload_dir"/. "$INSTALL_DIR"/
  restore_existing_state "$backup_dir"
  chmod +x "$INSTALL_DIR/start.sh" 2>/dev/null || true
  chmod +x "$INSTALL_DIR/chatgpt2api-go" 2>/dev/null || true
  chmod +x "$INSTALL_DIR/chatgpt2api" 2>/dev/null || true
  find "$INSTALL_DIR/data/bin/curl-impersonate" -type f -exec chmod +x {} + 2>/dev/null || true
  return 0
}

install_from_source() {
  need_cmd git
  need_cmd go

  local tmp src backup_dir bin_name
  tmp="$(mktemp -d)"
  src="$tmp/src"
  backup_dir="$tmp/backup"
  trap 'rm -rf "$tmp"' RETURN

  log "Cloning latest source: https://github.com/${REPO}.git"
  git clone --depth 1 "https://github.com/${REPO}.git" "$src"
  cd "$src"

  if [[ "$SKIP_TESTS" -eq 0 ]]; then
    log "Running Go tests"
    go test ./...
  fi

  if [[ "$BUILD_WEB" -eq 1 ]]; then
    need_cmd npm
    log "Building frontend"
    make web
  elif [[ ! -d web_dist ]]; then
    warn "web_dist not found in source checkout; install will be API-only unless you use --web"
  fi

  log "Building backend"
  mkdir -p bin
  CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/chatgpt2api-go ./cmd/server

  check_install_dir
  backup_existing_state "$backup_dir"
  rm -rf "$INSTALL_DIR"
  mkdir -p "$INSTALL_DIR/bin"
  cp bin/chatgpt2api-go "$INSTALL_DIR/bin/chatgpt2api-go"
  cp bin/chatgpt2api-go "$INSTALL_DIR/chatgpt2api-go"
  cp start.sh README.md LICENSE VERSION "$INSTALL_DIR"/ 2>/dev/null || true
  [[ -d web_dist ]] && cp -a web_dist "$INSTALL_DIR/web_dist"
  if [[ -f config.json ]]; then
    cp config.json "$INSTALL_DIR/config.example.json"
  fi
  restore_existing_state "$backup_dir"
  chmod +x "$INSTALL_DIR/start.sh" "$INSTALL_DIR/chatgpt2api-go" "$INSTALL_DIR/bin/chatgpt2api-go" 2>/dev/null || true
}

main() {
  log "Repository: $REPO"
  log "Install dir: $INSTALL_DIR"

  if [[ "$FROM_SOURCE" -eq 1 ]]; then
    install_from_source
  else
    if ! install_from_release; then
      fatal "Could not install from latest release. Re-run with --from-source to build locally."
    fi
  fi

  log "Install/update complete"
  if [[ -f "$INSTALL_DIR/config.json" ]]; then
    log "Config: $INSTALL_DIR/config.json"
  else
    warn "config.json was not created; please create it before starting"
  fi
  log "Data dir: $INSTALL_DIR/data"
  log "Run: cd '$INSTALL_DIR' && ./start.sh --port '$PORT'"

  if [[ "$START_AFTER_INSTALL" -eq 1 ]]; then
    cd "$INSTALL_DIR"
    exec ./start.sh --port "$PORT"
  fi
}

main
