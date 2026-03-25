.PHONY: all build test lint clean dev migrate seed

# Variables
BACKEND_DIR := backend
FRONTEND_DIR := frontend
DEPLOY_DIR := deploy/docker
BIN_DIR := $(BACKEND_DIR)/bin

# Default target
all: build

# --- Backend ---

.PHONY: backend-build backend-test backend-lint

backend-build:
	cd $(BACKEND_DIR) && go build -o bin/superops ./cmd/superops
	cd $(BACKEND_DIR) && go build -o bin/worker ./cmd/worker
	cd $(BACKEND_DIR) && go build -o bin/migrate ./cmd/migrate

backend-test:
	cd $(BACKEND_DIR) && go test ./... -v -race -count=1

backend-lint:
	cd $(BACKEND_DIR) && golangci-lint run ./...

backend-tidy:
	cd $(BACKEND_DIR) && go mod tidy

# --- Frontend ---

.PHONY: frontend-install frontend-build frontend-lint frontend-dev

frontend-install:
	cd $(FRONTEND_DIR) && npm ci

frontend-build: frontend-install
	cd $(FRONTEND_DIR) && npm run build

frontend-lint:
	cd $(FRONTEND_DIR) && npm run lint

frontend-dev:
	cd $(FRONTEND_DIR) && npm run dev

# --- Database ---

.PHONY: migrate migrate-down migrate-create

migrate:
	cd $(BACKEND_DIR) && go run ./cmd/migrate -direction up

migrate-down:
	cd $(BACKEND_DIR) && go run ./cmd/migrate -direction down -steps 1

migrate-create:
	@read -p "Migration name: " name; \
	num=$$(ls -1 $(BACKEND_DIR)/migrations/*.up.sql 2>/dev/null | wc -l | tr -d ' '); \
	num=$$(printf "%03d" $$((num + 1))); \
	touch $(BACKEND_DIR)/migrations/$${num}_$${name}.up.sql; \
	touch $(BACKEND_DIR)/migrations/$${num}_$${name}.down.sql; \
	echo "Created migrations/$${num}_$${name}.{up,down}.sql"

seed:
	cd $(BACKEND_DIR) && go run ./cmd/seed

# --- Docker ---

.PHONY: docker-up docker-down docker-build docker-logs

docker-up:
	cd $(DEPLOY_DIR) && docker compose up -d

docker-down:
	cd $(DEPLOY_DIR) && docker compose down

docker-build:
	cd $(DEPLOY_DIR) && docker compose build

docker-logs:
	cd $(DEPLOY_DIR) && docker compose logs -f

docker-dev:
	cd $(DEPLOY_DIR) && docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d

# --- Combined ---

build: backend-build frontend-build

test: backend-test

lint: backend-lint frontend-lint

clean:
	rm -rf $(BIN_DIR)
	rm -rf $(FRONTEND_DIR)/dist
	rm -rf $(FRONTEND_DIR)/node_modules

dev: docker-dev
	@echo "Infrastructure services started. Run 'make frontend-dev' in another terminal."
	cd $(BACKEND_DIR) && go run ./cmd/superops
