# Go toolchain resolution: uses `go` from PATH when available, otherwise
# falls back to mise (`mise exec go -- ...`), matching how this repo is
# built locally.
ifeq ($(shell command -v go 2>/dev/null),)
GO    := mise exec go -- go
GOFMT := mise exec go -- gofmt
else
GO    := go
GOFMT := gofmt
endif

ifeq ($(shell command -v golangci-lint 2>/dev/null),)
GOLANGCI_LINT := mise exec golangci-lint -- golangci-lint
else
GOLANGCI_LINT := golangci-lint
endif

BIN_DIR := bin

.DEFAULT_GOAL := help

.PHONY: help
help: ## list available targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  \033[1m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## build server and CLI into ./bin (later-server, later)
	$(GO) build -o $(BIN_DIR)/later-server ./cmd/later-server
	$(GO) build -o $(BIN_DIR)/later ./cmd/later

.PHONY: test
test: ## run all tests
	$(GO) test ./...

.PHONY: race
race: ## run all tests with the race detector
	$(GO) test -race ./...

.PHONY: cover
cover: ## per-package test coverage summary
	$(GO) test -cover ./...

.PHONY: cover-html
cover-html: ## open the line-by-line coverage report in the browser
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out

.PHONY: fmt
fmt: ## gofmt all files in place
	$(GOFMT) -w .

.PHONY: fmt-check
fmt-check: ## fail if any file is not gofmt'd
	@out="$$($(GOFMT) -l .)"; if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## golangci-lint (part of `check`; from PATH or mise, same resolution as $(GO))
	$(GOLANGCI_LINT) run ./...

.PHONY: check
check: fmt-check vet lint race ## everything CI would run: fmt-check + vet + lint + race tests

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: run
run: ## run the server against ./config.yaml
	$(GO) run ./cmd/later-server -config config.yaml

.PHONY: clean
clean: ## remove build artifacts and coverage output
	rm -rf $(BIN_DIR) coverage.out
