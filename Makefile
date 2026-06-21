# Sandboxed AI Agent Orchestration Platform — build helpers.
# The guest agent and orchestrator are Linux/KVM only (//go:build linux).

GO       ?= go
BIN      ?= bin
MODULE    = github.com/yourorg/sandbox-platform

.PHONY: help tidy proto-test guest-agent vet fmt clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS=":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

tidy: ## Resolve dependencies and write go.sum (needs network)
	$(GO) mod tidy

proto-test: ## Test the portable wire-protocol package (any OS, offline)
	$(GO) test ./internal/protocol/...

guest-agent: ## Build the static guest agent binary for the rootfs (Linux)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		$(GO) build -trimpath -ldflags='-s -w' -o $(BIN)/guest_agent ./cmd/guest-agent

vet: ## Vet the Linux build (run on/for a Linux host)
	GOOS=linux $(GO) vet ./...

fmt: ## gofmt the tree
	gofmt -w .

clean: ## Remove build artifacts
	rm -rf $(BIN)
