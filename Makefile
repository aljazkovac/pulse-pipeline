.PHONY: help setup-tools up down ps logs clean build test lint db-shell

# Default target
.DEFAULT_GOAL := help

help: ## Show available Make targets
	@echo "Pulse Pipeline — Developer Commands"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

setup-tools: ## Install required Go & local CLI tools via Homebrew (macOS)
	brew install go golangci-lint golang-migrate sqlc

up: ## Start local infrastructure (PostgreSQL 17, Flipt, Prometheus, Grafana)
	docker compose up -d

down: ## Stop local infrastructure containers
	docker compose down

ps: ## List running local infrastructure containers
	docker compose ps

logs: ## Tail logs from local infrastructure containers
	docker compose logs -f

clean: ## Stop containers and remove local volumes
	docker compose down -v

db-shell: ## Open a psql interactive shell in the local PostgreSQL container
	docker compose exec postgres psql -U $${POSTGRES_USER:-pulse} -d $${POSTGRES_DB:-pulse_pipeline}

build: ## Build Go backend and MCP binaries
	cd backend && go build -o ../bin/pulse-api ./cmd/api
	cd backend && go build -o ../bin/pulse-worker ./cmd/worker
	cd mcp && go build -o ../bin/pulse-mcp ./cmd/mcp

test: ## Run unit tests across backend and MCP
	cd backend && go test -v -race ./...
	cd mcp && go test -v -race ./...

lint: ## Run linters across backend and MCP
	cd backend && golangci-lint run ./...
	cd mcp && golangci-lint run ./...
