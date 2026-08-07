IMAGE := majordomo-gateway

.PHONY: help build build-server build-cli build-mcp run test test-cover lint fmt vet tidy migrate docker-build

help:
	@echo "Available targets:"
	@echo "  build        Build all three binaries (gateway, gateway-cli, gateway-mcp)"
	@echo "  run          Build and run the gateway server"
	@echo "  test         Run all tests"
	@echo "  test-cover   Run tests with coverage report"
	@echo "  lint         Run golangci-lint"
	@echo "  fmt          Format code"
	@echo "  vet          Run go vet"
	@echo "  tidy         Run go mod tidy"
	@echo "  migrate      Run database migrations"
	@echo "  docker-build Build Docker image"

build: build-server build-cli build-mcp

build-server:
	go build -o bin/gateway ./cmd/gateway

build-cli:
	go build -o bin/gateway-cli ./cmd/gateway-cli

build-mcp:
	go build -o bin/gateway-mcp ./cmd/gateway-mcp

run: build-server
	bin/gateway

test:
	go test ./...

test-cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

lint:
	golangci-lint run ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

tidy:
	go mod tidy

migrate: build-server
	bin/gateway migrate

docker-build:
	docker build -t $(IMAGE) .
