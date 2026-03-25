#!/usr/bin/env bash
set -euo pipefail

if [ $# -lt 1 ]; then
  echo "Usage: $0 <backup_file.sql.gz>"
  exit 1
fi

BACKUP_FILE="$1"

if [ ! -f "$BACKUP_FILE" ]; then
  echo "Backup file not found: $BACKUP_FILE"
  exit 1
fi

echo "=== SuperOps Restore ==="
echo "WARNING: This will DROP and recreate the superops database."
read -p "Continue? (y/N) " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then exit 1; fi

echo "Restoring from: $BACKUP_FILE"
docker exec docker-postgres-1 psql -U superops -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='superops' AND pid <> pg_backend_pid();" 2>/dev/null || true
docker exec docker-postgres-1 dropdb -U superops --if-exists superops
docker exec docker-postgres-1 createdb -U superops superops
gunzip -c "$BACKUP_FILE" | docker exec -i docker-postgres-1 psql -U superops superops

echo "=== Restore Complete ==="
