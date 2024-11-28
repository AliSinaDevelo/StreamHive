.PHONY: build run test test-race vet cover lint ci help

build:
	@mkdir -p bin
	@go build -o bin/fs .

run: build
	@./bin/fs

test:
	@go test -count=1 ./...

test-race:
	@go test -race -count=1 ./...

vet:
	@go vet ./...

cover:
	@go test -count=1 -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out

lint:
	@golangci-lint run ./...

ci: vet test-race lint

help:
	@echo "Targets: build run test test-race vet cover lint ci"
