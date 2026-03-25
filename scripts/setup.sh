#!/usr/bin/env bash
set -euo pipefail

echo "=== SuperOps Development Setup ==="

# Check prerequisites
command -v go >/dev/null 2>&1 || { echo "Go is required. Install from https://go.dev"; exit 1; }
command -v node >/dev/null 2>&1 || { echo "Node.js is required. Install from https://nodejs.org"; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "Docker is required. Install from https://docker.com"; exit 1; }

# Backend dependencies
echo "Installing backend dependencies..."
cd backend && go mod download && cd ..

# Frontend dependencies
echo "Installing frontend dependencies..."
cd frontend && npm ci && cd ..

# Docker env
if [ ! -f deploy/docker/.env ]; then
  cp deploy/docker/.env.example deploy/docker/.env
  echo "Created deploy/docker/.env from template. Edit secrets before running."
fi

# Start infrastructure
echo "Starting infrastructure services..."
cd deploy/docker && docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d && cd ../..

echo "Waiting for PostgreSQL..."
until docker exec docker-postgres-1 pg_isready -U superops 2>/dev/null; do sleep 1; done

# Run migrations
echo "Running database migrations..."
cd backend && JWT_SECRET=dev_secret_change_me_32chars_long DB_HOST=localhost DB_PASSWORD=changeme_db_password go run ./cmd/migrate -direction up && cd ..

echo ""
echo "=== Setup Complete ==="
echo "Start backend:  cd backend && JWT_SECRET=dev_secret_change_me_32chars_long DB_HOST=localhost DB_PASSWORD=changeme_db_password REDIS_PASSWORD=changeme_redis_password go run ./cmd/superops"
echo "Start frontend: cd frontend && npm run dev"
