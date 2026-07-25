#!/usr/bin/env bash
#
# Local development setup: dependencies, a .env with REAL generated secrets,
# the infrastructure stack, and the database schema.
#
# Re-running is safe: an existing deploy/docker/.env is never overwritten.

set -euo pipefail

# shellcheck source=scripts/_common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_common.sh"

echo "=== SuperOps Development Setup ==="

require_cmd go
require_cmd node
require_cmd docker

# --- Dependencies ---
echo "Installing backend dependencies..."
( cd "$REPO_ROOT/backend" && go mod download )

echo "Installing app dependencies..."
( cd "$REPO_ROOT/app" && npm ci )

# --- Environment ---
# Never let changeme_ placeholders reach a running stack: generate real values.
#
# `tr -dc ... </dev/urandom | head -c N` is the obvious one-liner and is wrong
# here: head exits early, tr dies of SIGPIPE, and `set -o pipefail` turns the
# whole script into an exit-141. Read a bounded chunk instead.
gen_secret() {
  local len="${1:-40}" out=""
  while [ "${#out}" -lt "$len" ]; do
    out="$out$(LC_ALL=C head -c 256 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9')"
  done
  printf '%s' "${out:0:$len}"
}

ENV_FILE="$COMPOSE_DIR/.env"
if [ -f "$ENV_FILE" ]; then
  echo "Keeping existing $ENV_FILE."
else
  # Postgres applies POSTGRES_PASSWORD only when it initializes an EMPTY data
  # directory. Generating fresh secrets against a volume that already holds a
  # cluster leaves .env and the database disagreeing, and every service then
  # fails with "password authentication failed for user superops" — which reads
  # like a broken image rather than stale state. Warn before that happens.
  if command -v docker >/dev/null 2>&1 &&
     docker volume ls --format '{{.Name}}' 2>/dev/null | grep -q '_postgres_data$'; then
    echo "WARNING: an existing Postgres volume was found, but $ENV_FILE is missing."
    echo "         New secrets will NOT match the password already baked into that"
    echo "         volume, and the stack will fail to authenticate."
    echo "         Either restore the original .env, or wipe the data:"
    echo "             cd $COMPOSE_DIR && docker compose down -v"
    if [ "${SUPEROPS_FORCE_NEW_SECRETS:-}" = "1" ]; then
      echo "         SUPEROPS_FORCE_NEW_SECRETS=1 — continuing anyway."
    else
      # Read from stdin, not /dev/tty: a piped answer must work, and a
      # non-interactive run with nothing on stdin must fall through to the safe
      # default (abort) rather than erroring on a missing terminal.
      printf "         Continue and generate new secrets anyway? [y/N] "
      reply=""
      read -r reply || true
      case "$reply" in
        [yY]*) ;;
        *) die "Aborted. No secrets were generated. Set SUPEROPS_FORCE_NEW_SECRETS=1 to skip this prompt." ;;
      esac
    fi
  fi

  echo "Generating $ENV_FILE with random secrets..."
  ( umask 077; cp "$COMPOSE_DIR/.env.example" "$ENV_FILE" )

  set_env() { # set_env KEY VALUE
    local key="$1" val="$2" tmp="$ENV_FILE.tmp"
    awk -v k="$key" -v v="$val" -F= '
      $1 == k { print k "=" v; found = 1; next }
      { print }
      END { if (!found) print k "=" v }
    ' "$ENV_FILE" > "$tmp"
    mv "$tmp" "$ENV_FILE"
  }

  DB_PW="$(gen_secret 32)"
  set_env DB_PASSWORD "$DB_PW"
  set_env POSTGRES_PASSWORD "$DB_PW"
  set_env REDIS_PASSWORD "$(gen_secret 32)"
  set_env NATS_PASSWORD "$(gen_secret 32)"
  set_env JWT_SECRET "$(gen_secret 64)"
  set_env ADMIN_PASSWORD "$(gen_secret 24)"
  # STORAGE_*, not MINIO_*. Both names are read (STORAGE_ wins), but only one
  # may be written: the MinIO container takes its root credentials from the same
  # pair, and generating both would leave the server on one password and the
  # application on the other.
  set_env STORAGE_ACCESS_KEY "superops_$(gen_secret 12)"
  set_env STORAGE_SECRET_KEY "$(gen_secret 40)"
  set_env MEILI_MASTER_KEY "$(gen_secret 40)"
  set_env METRICS_TOKEN "$(gen_secret 32)"
  set_env GRAFANA_ADMIN_PASSWORD "$(gen_secret 24)"
  chmod 600 "$ENV_FILE"

  # Assignments only — the file header mentions the word in a comment.
  if grep -Eq '^[A-Za-z_][A-Za-z0-9_]*=changeme_' "$ENV_FILE"; then
    echo "Remaining placeholders:" >&2
    grep -En '^[A-Za-z_][A-Za-z0-9_]*=changeme_' "$ENV_FILE" >&2
    die "$ENV_FILE still contains changeme_ placeholders — secret generation failed"
  fi
  echo "  -> secrets generated (stored only in $ENV_FILE; nothing is printed here)"
fi

load_env

# Prometheus reads the metrics bearer token from this file (compose secret).
mkdir -p "$COMPOSE_DIR/secrets"
( umask 077; printf '%s' "${METRICS_TOKEN:-}" > "$COMPOSE_DIR/secrets/metrics_token" )

# --- Infrastructure ---
echo "Starting infrastructure services..."
compose -f "$COMPOSE_DIR/docker-compose.dev.yml" up -d

echo "Waiting for PostgreSQL (up to 120s)..."
PG_CID="$(service_cid postgres)"
deadline=$(( $(date +%s) + 120 ))
until docker exec "$PG_CID" pg_isready -U "$(db_user)" -d "$(db_name)" >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || die "PostgreSQL not ready within 120s (cd deploy/docker && docker compose logs postgres)"
  sleep 2
done
echo "  -> ready"

# --- Migrations ---
# Credentials are passed through the environment from .env, so no secret is
# echoed to the terminal or left in shell history.
echo "Running database migrations..."
( cd "$REPO_ROOT/backend" \
  && DB_HOST=127.0.0.1 REDIS_ADDR=127.0.0.1:6379 \
     NATS_URL="nats://${NATS_USER:-superops}:${NATS_PASSWORD}@127.0.0.1:4222" \
     go run ./cmd/migrate -direction up )

cat <<'EOF'

=== Setup Complete ===
Secrets live in deploy/docker/.env (mode 600) and are never printed.

  make dev                      # infra + API server on the host
  make app-dev                  # Expo web client
  make seed                     # demo data (demo users password: demo_password_123)
  make backend-test             # unit tests
  make backend-test-integration # integration suite (needs the infra above)

Admin login — read it from the env file when you need it:

  grep -E '^ADMIN_(EMAIL|PASSWORD)=' deploy/docker/.env
EOF
