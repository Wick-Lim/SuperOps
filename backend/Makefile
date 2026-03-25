.PHONY: build test lint tidy run run-worker

build:
	go build -o bin/superops ./cmd/superops
	go build -o bin/worker ./cmd/worker
	go build -o bin/migrate ./cmd/migrate

test:
	go test ./... -v -race -count=1

lint:
	golangci-lint run ./...

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
