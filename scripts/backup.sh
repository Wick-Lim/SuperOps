#!/usr/bin/env bash
#
# Full SuperOps backup: Postgres (custom format) + the MinIO bucket that holds
# the actual file bytes. A SQL-only backup restores rows in `files` pointing at
# objects that no longer exist, so both halves are taken as one set.
#
# Usage: scripts/backup.sh [backup_dir]      (default: <repo>/backups)
#
# Environment:
#   BACKUP_KEEP=7               backup sets to retain (0 = keep everything)
#   BACKUP_GPG_RECIPIENT=<key>  encrypt the dump to this gpg key
#   BACKUP_SKIP_OBJECTS=1       skip the MinIO mirror

set -euo pipefail

# shellcheck source=scripts/_common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_common.sh"

require_cmd docker
load_env

mkdir -p "${1:-$REPO_ROOT/backups}"
BACKUP_DIR="$(cd "${1:-$REPO_ROOT/backups}" && pwd)"
BACKUP_KEEP="${BACKUP_KEEP:-7}"
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
SET_DIR="$BACKUP_DIR/$TIMESTAMP"
mkdir -p "$SET_DIR"

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

# A failed run must not leave anything that looks like a usable backup behind.
cleanup_failed() {
  rc=$?
  if [ $rc -ne 0 ]; then
    echo "Backup FAILED — removing the partial set $SET_DIR" >&2
    rm -rf "$SET_DIR"
  fi
  exit $rc
}
trap cleanup_failed EXIT

echo "=== SuperOps Backup ==="
echo "Target: $SET_DIR"

PG_CID="$(service_cid postgres)"

# --- 1. Globals (roles are not part of a per-database dump) ---
echo "Dumping globals..."
docker exec "$PG_CID" pg_dumpall -U "$(db_user)" --globals-only > "$SET_DIR/globals.sql.part"
mv "$SET_DIR/globals.sql.part" "$SET_DIR/globals.sql"

# --- 2. Database in custom format: compressed, selective/parallel restore ---
# Written to .part first. The old script redirected with `>` before pg_dump ran,
# so a failed dump left a zero-byte .sql.gz that looked like a valid backup.
echo "Dumping database..."
docker exec "$PG_CID" pg_dump -U "$(db_user)" -d "$(db_name)" -Fc --compress=9 \
  > "$SET_DIR/database.dump.part"

# --- 3. Verify before calling it a backup ---
echo "Verifying dump..."
docker exec -i "$PG_CID" pg_restore -l < "$SET_DIR/database.dump.part" > "$SET_DIR/database.toc" \
  || die "pg_restore could not read the dump — backup discarded"
[ -s "$SET_DIR/database.toc" ] || die "dump table of contents is empty — backup discarded"
mv "$SET_DIR/database.dump.part" "$SET_DIR/database.dump"

# --- 4. Object storage ---
if [ "${BACKUP_SKIP_OBJECTS:-0}" = "1" ]; then
  echo "Skipping MinIO mirror (BACKUP_SKIP_OBJECTS=1)."
else
  echo "Mirroring MinIO bucket '${MINIO_BUCKET:-superops}'..."
  BACKUP_DIR="$BACKUP_DIR" compose --profile tools run --rm -T mc \
    "mc mirror --overwrite superops/${MINIO_BUCKET:-superops} /backup/$TIMESTAMP/objects" \
    || die "MinIO mirror failed — backup discarded"
fi

# --- 5. Optional encryption ---
if [ -n "${BACKUP_GPG_RECIPIENT:-}" ]; then
  require_cmd gpg
  echo "Encrypting dump for $BACKUP_GPG_RECIPIENT..."
  gpg --batch --yes --encrypt --recipient "$BACKUP_GPG_RECIPIENT" \
    --output "$SET_DIR/database.dump.gpg" "$SET_DIR/database.dump"
  rm -f "$SET_DIR/database.dump"
fi

# --- 6. Manifest + checksums ---
migration_version="$(docker exec "$PG_CID" psql -U "$(db_user)" -d "$(db_name)" -tAc \
  "SELECT version || CASE WHEN dirty THEN ' (DIRTY)' ELSE '' END FROM schema_migrations" 2>/dev/null || echo unknown)"
{
  echo "timestamp=$TIMESTAMP"
  echo "database=$(db_name)"
  echo "format=custom(-Fc)"
  echo "encrypted=$([ -n "${BACKUP_GPG_RECIPIENT:-}" ] && echo yes || echo no)"
  echo "objects=$([ "${BACKUP_SKIP_OBJECTS:-0}" = "1" ] && echo skipped || echo included)"
  echo "schema_migration=${migration_version:-unknown}"
} > "$SET_DIR/manifest.txt"

( cd "$SET_DIR" && find . -type f ! -name SHA256SUMS | while read -r f; do sha256 "$f"; done > SHA256SUMS )

trap - EXIT

# --- 7. Rotation ---
if [ "$BACKUP_KEEP" -gt 0 ]; then
  find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -type d -name '20*' \
    | sort -r | tail -n "+$((BACKUP_KEEP + 1))" \
    | while read -r old; do
        echo "Rotating out $old"
        rm -rf "$old"
      done
fi

echo ""
echo "=== Backup Complete ==="
du -sh "$SET_DIR"
cat "$SET_DIR/manifest.txt"
echo "Restore with: scripts/restore.sh $SET_DIR"
