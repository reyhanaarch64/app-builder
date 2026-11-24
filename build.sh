#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"
source "$ROOT_DIR/scripts/platform.sh"

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'command not found: %s\n' "$1" >&2
    exit 1
  fi
}

ARCH="$(detect_linux_arch)"
PLATFORM="linux/$ARCH"
OUTPUT_DIR="$ROOT_DIR/.build/$ARCH"

require_command go
require_command gcc
require_command node
require_command npm
require_command docker

if ! docker info >/dev/null 2>&1; then
  printf 'Docker daemon is not available; start Docker and retry\n' >&2
  exit 1
fi

if ! docker compose version >/dev/null 2>&1; then
  printf 'Docker Compose plugin is required\n' >&2
  exit 1
fi

if [[ ! -d "$ROOT_DIR/frontend/node_modules" ]]; then
  npm --prefix frontend ci
fi
npm --prefix frontend run build

mkdir -p "$OUTPUT_DIR"
(
  cd "$ROOT_DIR/backend"
  CGO_ENABLED=1 GOOS=linux GOARCH="$ARCH" go build -trimpath -o "$OUTPUT_DIR/android-ide-server" .
)

docker build --platform "$PLATFORM" -t "android-web-ide-builder:$ARCH" "$ROOT_DIR/build-image"
docker build --platform "$PLATFORM" -t "app-builder-ide:$ARCH" "$ROOT_DIR"

printf '\nbuild complete for %s\n' "$PLATFORM"
printf 'native executable: %s\n' "$OUTPUT_DIR/android-ide-server"
printf 'service image: app-builder-ide:%s\n' "$ARCH"
printf 'builder image: android-web-ide-builder:%s\n' "$ARCH"
printf 'start the service with: bash start.sh\n'
