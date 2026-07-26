.PHONY: build test test-integration cover lint vulncheck tidy run run-worker reindex migrate-up migrate-down

build:
	go build -o bin/superops ./cmd/superops
	go build -o bin/worker ./cmd/worker
	go build -o bin/migrate ./cmd/migrate
	go build -o bin/reindex ./cmd/reindex

# Unit tests only — the integration suite is behind the `integration` build tag
# and needs Postgres/Redis/NATS.
test:
	go test ./... -v -race -count=1

# Integration suite. Needs a migrated database and running infra; prefer the
# root `make backend-test-integration`, which loads deploy/docker/.env for you.
test-integration:
	go test -tags=integration ./test/integration/... -race -count=1 -v -timeout 25m

cover:
	go test ./... -race -count=1 -coverprofile=coverage.out -covermode=atomic
	go tool cover -func=coverage.out | tail -1

lint:
	golangci-lint run ./...

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

tidy:
	go mod tidy

run:
	go run ./cmd/superops

run-worker:
	go run ./cmd/worker

# Rebuild the Meilisearch index from Postgres. The index is otherwise only ever
# written incrementally by the worker's JetStream consumer, so this is the only
# way back after a Meilisearch data loss or a schema change.
#
# Takes the same env as the server (DB_*, MEILI_HOST, MEILI_MASTER_KEY,
# SEARCH_ENABLED). Pass flags through ARGS, e.g.
#   make reindex ARGS="-dry-run"
#   make reindex ARGS="-workspace 8f1c... -batch 1000"
reindex:
	go run ./cmd/reindex $(ARGS)

migrate-up:
	go run ./cmd/migrate -direction up

migrate-down:
	go run ./cmd/migrate -direction down -steps 1
