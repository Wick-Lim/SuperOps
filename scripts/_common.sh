#!/usr/bin/env bash
# Shared helpers for the operational scripts.
#
# Nothing here hardcodes a container name. The old scripts used
# `docker-postgres-1`, which is only correct when Compose derives the project
# name from a directory literally called "docker" — any -p flag,
# COMPOSE_PROJECT_NAME, or renamed checkout broke backup, restore and setup at
# the same time.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
COMPOSE_DIR="$REPO_ROOT/deploy/docker"
COMPOSE_FILE="$COMPOSE_DIR/docker-compose.yml"

die() {
  echo "ERROR: $*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed."
}

# compose <args...> — docker compose bound to this repo's project.
# COMPOSE_PROJECT_NAME (if the caller exports one) is honoured by docker itself.
compose() {
  docker compose --project-directory "$COMPOSE_DIR" -f "$COMPOSE_FILE" "$@"
}

# service_cid <service> — container id of a *running* compose service.
service_cid() {
  local svc="$1" cid
  cid="$(compose ps -q "$svc" 2>/dev/null || true)"
  [ -n "$cid" ] || die "compose service '$svc' is not running (try: cd deploy/docker && docker compose up -d $svc)"
  printf '%s' "$cid"
}

# load_env — export deploy/docker/.env into the environment.
load_env() {
  [ -f "$COMPOSE_DIR/.env" ] || die "$COMPOSE_DIR/.env is missing. Run scripts/setup.sh first."
  set -a
  # shellcheck disable=SC1091
  . "$COMPOSE_DIR/.env"
  set +a
}

# db_user / db_name default to the compose values but follow .env when loaded.
db_user() { printf '%s' "${DB_USER:-superops}"; }
db_name() { printf '%s' "${DB_NAME:-superops}"; }
