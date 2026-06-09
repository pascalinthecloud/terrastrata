# terrastrata — developer task runner.
# Run `make help` for the list of targets.

BINARY      := terrastrata
PKG         := ./...
CMD         := ./cmd/terrastrata
IMAGE       ?= terrastrata:dev

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(CMD)

.PHONY: run
run: ## Run locally (uses a temp ./cache dir)
	CACHE_DIR=./cache LOG_LEVEL=debug go run $(CMD)

.PHONY: test
test: ## Run unit tests with the race detector
	go test -race -count=1 $(PKG)

.PHONY: cover
cover: ## Run tests and open an HTML coverage report
	go test -race -coverprofile=coverage.out $(PKG)
	go tool cover -html=coverage.out

.PHONY: lint
lint: ## Run golangci-lint (must be installed)
	golangci-lint run

.PHONY: vuln
vuln: ## Run govulncheck (must be installed)
	govulncheck $(PKG)

.PHONY: tidy
tidy: ## Tidy and verify go.mod/go.sum
	go mod tidy
	go mod verify

.PHONY: docker
docker: ## Build the container image
	docker build -t $(IMAGE) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin coverage.out
