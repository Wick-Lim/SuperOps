#!/usr/bin/env bash
set -euo pipefail

BACKUP_DIR="${1:-./backups}"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
mkdir -p "$BACKUP_DIR"

echo "=== SuperOps Backup ==="

# PostgreSQL
echo "Backing up PostgreSQL..."
docker exec docker-postgres-1 pg_dump -U superops superops | gzip > "$BACKUP_DIR/superops_db_${TIMESTAMP}.sql.gz"
echo "  -> $BACKUP_DIR/superops_db_${TIMESTAMP}.sql.gz"

echo ""
echo "=== Backup Complete ==="
echo "Files saved to: $BACKUP_DIR"
ls -lh "$BACKUP_DIR"/*${TIMESTAMP}*
