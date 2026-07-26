.PHONY: all build test lint clean dev migrate seed

# Variables
BACKEND_DIR := backend
APP_DIR := app
DEPLOY_DIR := deploy/docker
BIN_DIR := $(BACKEND_DIR)/bin
ENV_FILE := $(DEPLOY_DIR)/.env

# Host-local development environment. deploy/docker/.env is the single source of
# truth for secrets (scripts/setup.sh generates it with real random values); the
# hostnames are rewritten because the compose services publish on 127.0.0.1 only.
DEV_ENV = set -a; . $(CURDIR)/$(ENV_FILE); set +a; \
	DB_HOST=127.0.0.1 \
	REDIS_ADDR=127.0.0.1:6379 \
	NATS_URL="nats://$$NATS_USER:$$NATS_PASSWORD@127.0.0.1:4222" \
	MINIO_ENDPOINT=127.0.0.1:19000 \
	MEILI_HOST=http://127.0.0.1:7700

# Default target
all: build

.PHONY: require-env
require-env:
	@test -f $(ENV_FILE) || { \
		echo "ERROR: $(ENV_FILE) is missing."; \
		echo "Run ./scripts/setup.sh (it generates real secrets) or copy $(DEPLOY_DIR)/.env.example."; \
		exit 1; \
	}

# --- Backend ---

.PHONY: backend-build backend-test backend-test-integration backend-cover backend-lint backend-tidy backend-vulncheck

backend-build:
	cd $(BACKEND_DIR) && go build -o bin/superops ./cmd/superops
	cd $(BACKEND_DIR) && go build -o bin/worker ./cmd/worker
	cd $(BACKEND_DIR) && go build -o bin/migrate ./cmd/migrate

# Unit tests only — the integration suite sits behind the `integration` build
# tag, so this target needs no infrastructure.
backend-test:
	cd $(BACKEND_DIR) && go test ./... -v -race -count=1

# Integration suite. Requires the infra stack (make docker-dev) and a migrated
# database (make migrate). Individual tests skip if the infra is unreachable.
backend-test-integration: require-env
	@set -e; cd $(BACKEND_DIR); $(DEV_ENV) \
		go test -tags=integration ./test/integration/... -race -count=1 -v -timeout 25m

backend-cover:
	cd $(BACKEND_DIR) && go test ./... -race -count=1 -coverprofile=coverage.out -covermode=atomic
	cd $(BACKEND_DIR) && go tool cover -func=coverage.out | tail -1

backend-lint:
	cd $(BACKEND_DIR) && golangci-lint run ./...

backend-vulncheck:
	cd $(BACKEND_DIR) && go run golang.org/x/vuln/cmd/govulncheck@latest ./...

backend-tidy:
	cd $(BACKEND_DIR) && go mod tidy

# --- App (React Native / Expo web client) ---

.PHONY: app-install app-build app-lint app-dev

app-install:
	cd $(APP_DIR) && npm ci

app-build: app-install
	cd $(APP_DIR) && npm run build

# No ESLint config is committed (adding one needs new devDependencies and a
# package-lock update), so "lint" for the app is the TypeScript check.
app-lint:
	cd $(APP_DIR) && npm run typecheck

app-dev:
	cd $(APP_DIR) && npm run dev

# Back-compat aliases for the old frontend-* target names.
.PHONY: frontend-install frontend-build frontend-lint frontend-dev
frontend-install: app-install
frontend-build: app-build
frontend-lint: app-lint
frontend-dev: app-dev

# --- Database ---

.PHONY: migrate migrate-down migrate-create

migrate: require-env
	@set -e; cd $(BACKEND_DIR); $(DEV_ENV) go run ./cmd/migrate -direction up

migrate-down: require-env
	@set -e; cd $(BACKEND_DIR); $(DEV_ENV) go run ./cmd/migrate -direction down -steps 1

migrate-create:
	@read -p "Migration name: " name; \
	num=$$(ls -1 $(BACKEND_DIR)/migrations/*.up.sql 2>/dev/null | wc -l | tr -d ' '); \
	num=$$(printf "%03d" $$((num + 1))); \
	touch $(BACKEND_DIR)/migrations/$${num}_$${name}.up.sql; \
	touch $(BACKEND_DIR)/migrations/$${num}_$${name}.down.sql; \
	echo "Created migrations/$${num}_$${name}.{up,down}.sql"

seed: require-env
	@set -e; cd $(BACKEND_DIR); $(DEV_ENV) go run ./cmd/seed

# --- Docker ---

.PHONY: docker-up docker-down docker-build docker-logs docker-dev

docker-up: require-env
	cd $(DEPLOY_DIR) && docker compose up -d

docker-down:
	cd $(DEPLOY_DIR) && docker compose down

docker-build:
	cd $(DEPLOY_DIR) && docker compose build

docker-logs:
	cd $(DEPLOY_DIR) && docker compose logs -f

docker-dev: require-env
	cd $(DEPLOY_DIR) && docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d

# --- Combined ---

build: backend-build app-build

test: backend-test

lint: backend-lint app-lint

clean:
	rm -rf $(BIN_DIR)
	rm -f $(BACKEND_DIR)/coverage.out
	rm -rf $(APP_DIR)/dist
	rm -rf $(APP_DIR)/node_modules

# Starts the infra stack, then runs the API server on the host with the
# generated dev credentials.
dev: docker-dev
	@echo "Infrastructure started. Run 'make app-dev' in another terminal for the web client."
	@set -e; cd $(BACKEND_DIR); $(DEV_ENV) go run ./cmd/superops
