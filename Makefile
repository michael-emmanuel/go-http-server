.DEFAULT_GOAL := help

GO        ?= go
BIN       := bin/server
IMAGE     ?= go-production-http-server
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)

.PHONY: help
help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

.PHONY: build
build: ## Compile the server to bin/server
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/server

.PHONY: run
run: ## Run the server locally on :8080
	$(GO) run ./cmd/server

.PHONY: fmt
fmt: ## Format all Go files
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofmt-formatted
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: test
test: ## Run all tests
	$(GO) test ./...

.PHONY: race
race: ## Run all tests under the race detector
	$(GO) test -race ./...

.PHONY: cover
cover: ## Coverage, attributing integration tests to the packages they exercise
	$(GO) test -race -covermode=atomic -coverpkg=./... -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -n 1

.PHONY: bench
bench: ## Run benchmarks (no tests)
	$(GO) test -run '^$$' -bench . -benchmem ./...

.PHONY: check
check: fmt-check vet build race ## Everything CI checks, except benchmarks

.PHONY: loadtest
loadtest: ## Load test a running server with `hey` (see README)
	hey -z 20s -c 50 "http://localhost:8080/api/v1/work?items=10&delay_ms=20"

.PHONY: docker-build
docker-build: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

.PHONY: docker-run
docker-run: ## Run the container on :8080
	docker run --rm -p 8080:8080 $(IMAGE):latest

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin coverage.out

.PHONY: mutation
mutation: ## Manual mutation testing (slow; see scripts/mutation-check.sh)
	./scripts/mutation-check.sh
