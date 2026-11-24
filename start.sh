#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"
source "$ROOT_DIR/scripts/platform.sh"
IDE_ARCH="$(detect_linux_arch)"
export IDE_ARCH
ACTION="${1:-start}"
export HOST_DATA_DIR="$ROOT_DIR/data"

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'command not found: %s\n' "$1" >&2
    exit 1
  fi
}

require_command docker

case "$ACTION" in
  stop)
    docker compose stop ide
    exit 0
    ;;
  status)
    docker compose ps
    exit 0
    ;;
  logs)
    docker compose logs -f --tail=100 ide
    exit 0
    ;;
  start|restart)
    ;;
  *)
    printf 'usage: bash start.sh [start|restart|stop|status|logs]\n' >&2
    exit 2
    ;;
esac

require_command node

mkdir -p "$ROOT_DIR/data"
CONFIG_FILE="$ROOT_DIR/data/IDE.config"
if [[ ! -f "$CONFIG_FILE" ]]; then
  cp "$ROOT_DIR/IDE.config" "$CONFIG_FILE"
  chmod 600 "$CONFIG_FILE"
  printf 'created config: %s\n' "$CONFIG_FILE"
fi

IDE_PORT="$(node -e '
  const config = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
  if (!Number.isInteger(config.port) || config.port < 1 || config.port > 65535) process.exit(1);
  process.stdout.write(String(config.port));
' "$CONFIG_FILE")"
export IDE_PORT

if ! docker image inspect "android-web-ide-builder:$IDE_ARCH" >/dev/null 2>&1 ||
   ! docker image inspect "app-builder-ide:$IDE_ARCH" >/dev/null 2>&1; then
  printf 'required Docker images are missing; building for linux/%s...\n' "$IDE_ARCH"
  bash "$ROOT_DIR/build.sh"
fi

docker compose up -d --force-recreate ide

require_command curl
for attempt in $(seq 1 60); do
  if curl --silent --fail "http://127.0.0.1:${IDE_PORT}/api/health" >/dev/null; then
    printf 'App Builder is ready at http://localhost:%s\n' "$IDE_PORT"
    printf 'For remote access, open the published HTTPS URL for port %s.\n' "$IDE_PORT"
    printf 'Run bash build.sh after changing source code or dependencies.\n'
    printf 'To change access mode, edit %s and run: bash start.sh restart\n' "$CONFIG_FILE"
    exit 0
  fi
  sleep 2
done

printf 'App Builder did not become ready; recent service logs:\n' >&2
docker compose logs --tail=100 ide >&2
exit 1
