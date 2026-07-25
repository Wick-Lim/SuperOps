#!/usr/bin/env bash
#
# Restore a backup set produced by scripts/backup.sh.
#
# Usage: scripts/restore.sh <backup_set_dir>
#        scripts/restore.sh backups/20260725_101500
#
# Environment:
#   RESTORE_SKIP_OBJECTS=1   restore the database only, leave MinIO untouched
#   RESTORE_YES=1            skip the interactive confirmation (for cron)

set -euo pipefail

# shellcheck source=scripts/_common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_common.sh"

require_cmd docker
load_env

[ $# -ge 1 ] || die "usage: $0 <backup_set_dir>"
SET_DIR="$(cd "$1" 2>/dev/null && pwd)" || die "backup set not found: $1"

DUMP="$SET_DIR/database.dump"
DECRYPTED=0
APP_STOPPED=0

# One EXIT handler for the whole script: bring the app back up whatever happens,
# and never leave a decrypted dump lying around.
cleanup() {
  rc=$?
  # `[ ... ] && cmd` would abort this handler under `set -e` when the test is
  # false, skipping the restart below.
  if [ "$DECRYPTED" = "1" ]; then
    rm -f "$DUMP"
  fi
  if [ "$APP_STOPPED" = "1" ]; then
    echo "Starting application services..."
    compose start backend worker >/dev/null || true
  fi
  exit $rc
}
trap cleanup EXIT

if [ ! -f "$DUMP" ]; then
  if [ -f "$DUMP.gpg" ]; then
    require_cmd gpg
    echo "Decrypting $DUMP.gpg..."
    gpg --batch --yes --decrypt --output "$DUMP" "$DUMP.gpg"
    DECRYPTED=1
  else
    die "no database.dump (or .gpg) in $SET_DIR"
  fi
fi

if [ -f "$SET_DIR/SHA256SUMS" ]; then
  echo "Verifying checksums..."
  ( cd "$SET_DIR" && if command -v sha256sum >/dev/null 2>&1; then
      sha256sum --quiet --check SHA256SUMS
    else
      shasum -a 256 --check SHA256SUMS >/dev/null
    fi ) || die "checksum mismatch in $SET_DIR — refusing to restore"
fi

echo "=== SuperOps Restore ==="
if [ -f "$SET_DIR/manifest.txt" ]; then
  cat "$SET_DIR/manifest.txt"
fi
echo "WARNING: this DROPS and recreates the '$(db_name)' database and stops the app."
if [ "${RESTORE_YES:-0}" != "1" ]; then
  read -r -p "Continue? (y/N) " -n 1 REPLY
  echo
  case "$REPLY" in [Yy]) ;; *) exit 1 ;; esac
fi

PG_CID="$(service_cid postgres)"

# The app MUST be stopped first. pgxpool keeps MinConns open and reconnects
# within milliseconds, so terminating backends without stopping backend/worker
# leaves dropdb failing with "database is being accessed by other users" —
# after the live sessions have already been killed.
echo "Stopping application services..."
compose stop backend worker >/dev/null
APP_STOPPED=1

echo "Terminating remaining sessions..."
docker exec "$PG_CID" psql -U "$(db_user)" -d postgres -v ON_ERROR_STOP=1 -c \
  "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='$(db_name)' AND pid <> pg_backend_pid();" >/dev/null

echo "Recreating database..."
docker exec "$PG_CID" dropdb -U "$(db_user)" --if-exists "$(db_name)"
docker exec "$PG_CID" createdb -U "$(db_user)" "$(db_name)"

if [ -f "$SET_DIR/globals.sql" ]; then
  echo "Restoring globals..."
  docker exec -i "$PG_CID" psql -U "$(db_user)" -d postgres -v ON_ERROR_STOP=1 -q < "$SET_DIR/globals.sql" \
    || echo "  (globals partially skipped — roles usually already exist)"
fi

# --exit-on-error is the pg_restore equivalent of psql's ON_ERROR_STOP. Without
# it (and the old script piped plain SQL into psql without ON_ERROR_STOP)
# every statement could fail and the script would still print "Restore Complete".
echo "Restoring database..."
docker exec -i "$PG_CID" pg_restore -U "$(db_user)" -d "$(db_name)" \
  --no-owner --no-privileges --exit-on-error < "$DUMP" \
  || die "pg_restore failed — the database is NOT restored"

# --- Object storage ---
if [ "${RESTORE_SKIP_OBJECTS:-0}" = "1" ]; then
  echo "Skipping MinIO restore (RESTORE_SKIP_OBJECTS=1)."
elif [ -d "$SET_DIR/objects" ]; then
  echo "Restoring MinIO bucket '${MINIO_BUCKET:-superops}'..."
  BACKUP_DIR="$(dirname "$SET_DIR")" compose --profile tools run --rm -T mc \
    "mc mb --ignore-existing superops/${MINIO_BUCKET:-superops} && mc mirror --overwrite /backup/$(basename "$SET_DIR")/objects superops/${MINIO_BUCKET:-superops}" \
    || die "MinIO restore failed — database is restored but file bytes are missing"
else
  echo "No objects/ directory in the set — skipping MinIO (files will 404)."
fi

# --- Schema version sanity check ---
restored="$(docker exec "$PG_CID" psql -U "$(db_user)" -d "$(db_name)" -tAc \
  'SELECT version FROM schema_migrations' 2>/dev/null || echo '')"
latest="$(find "$REPO_ROOT/backend/migrations" -name '*.up.sql' -exec basename {} \; \
  | sed 's/_.*//' | sort -n | tail -1 | sed 's/^0*//')"
echo "Restored schema version: ${restored:-unknown} (latest migration in tree: ${latest:-unknown})"
if [ -n "$restored" ] && [ -n "$latest" ] && [ "$restored" != "$latest" ]; then
  echo "NOTE: run 'make migrate' — the dump predates the migrations in this checkout."
fi

echo "=== Restore Complete ==="
