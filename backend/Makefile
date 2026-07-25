.PHONY: build test test-integration cover lint vulncheck tidy run run-worker migrate-up migrate-down

build:
	go build -o bin/superops ./cmd/superops
	go build -o bin/worker ./cmd/worker
	go build -o bin/migrate ./cmd/migrate

# Unit tests only — the integration suite is behind the `integration` build tag
# and needs Postgres/Redis/NATS.
test:
	go test ./... -v -race -count=1

# Integration suite. Needs a migrated database and running infra; prefer the
# root `make backend-test-integration`, which loads deploy/docker/.env for you.
test-integration:
	go test -tags=integration ./test/integration/... -race -count=1 -v

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

migrate-up:
	go run ./cmd/migrate -direction up

migrate-down:
	go run ./cmd/migrate -direction down -steps 1
