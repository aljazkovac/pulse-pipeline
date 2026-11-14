.PHONY: help setup-tools up down ps logs clean build test lint e2e db-shell

# web/ targets activate once web/package.json exists. The package must define the
# scripts `build`, `typecheck` (tsc --noEmit, strict), `lint` (ESLint incl.
# no-explicit-any), `test` (Vitest) and `test:e2e` (Playwright desktop + mobile).
WEB_PACKAGE := $(wildcard web/package.json)
WEB_PM ?= npm

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

build: ## Build Go backend and MCP binaries (and web/ bundle when present)
	cd backend && go build -o ../bin/pulse-api ./cmd/api
	cd backend && go build -o ../bin/pulse-worker ./cmd/worker
	cd mcp && go build -o ../bin/pulse-mcp ./cmd/mcp
ifneq ($(WEB_PACKAGE),)
	cd web && $(WEB_PM) run build
endif

test: ## Run unit tests across backend, MCP (and web/ Vitest when present)
	cd backend && go test -v -race ./...
	cd mcp && go test -v -race ./...
ifneq ($(WEB_PACKAGE),)
	cd web && $(WEB_PM) run test
endif

lint: ## Run linters across backend, MCP (and web/ typecheck + ESLint when present)
	cd backend && golangci-lint run ./...
	cd mcp && golangci-lint run ./...
ifneq ($(WEB_PACKAGE),)
	cd web && $(WEB_PM) run typecheck
	cd web && $(WEB_PM) run lint
endif

e2e: ## Run Playwright E2E specs for desktop + mobile viewports (requires web/)
ifneq ($(WEB_PACKAGE),)
	cd web && $(WEB_PM) run test:e2e
else
	@echo "web/package.json not found; no E2E specs to run"
endif
