.PHONY: help setup-tools up down ps logs clean build test lint e2e db-shell generate generate-check migrate-up migrate-down

# web/ targets activate once web/package.json exists. The package must define the
# scripts `build`, `typecheck` (tsc --noEmit, strict), `lint` (ESLint incl.
# no-explicit-any), `test` (Vitest) and `test:e2e` (Playwright desktop + mobile).
WEB_PACKAGE := $(wildcard web/package.json)
WEB_PM ?= npm

# Local database for migrate-* targets; matches .env.example / docker-compose.yml.
DATABASE_URL ?= postgres://pulse:pulse_dev_password@localhost:5432/pulse_pipeline?sslmode=disable

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

generate: ## Regenerate sqlc code in backend/internal/store/db from SQL queries + migrations
	cd backend && sqlc generate

generate-check: ## Fail if committed sqlc code is out of date with the SQL (run in CI)
	cd backend && sqlc diff

migrate-up: ## Apply all database migrations to DATABASE_URL (default: local docker-compose Postgres)
	migrate -path backend/migrations -database "$(DATABASE_URL)" up

migrate-down: ## Revert the most recent database migration on DATABASE_URL
	migrate -path backend/migrations -database "$(DATABASE_URL)" down 1

build: ## Build Go backend and MCP binaries (and web/ bundle when present)
	cd backend && go build -o ../bin/pulse-api ./cmd/api
	cd backend && go build -o ../bin/pulse-worker ./cmd/worker
	cd mcp && go build -o ../bin/pulse-mcp ./cmd/mcp
ifneq ($(WEB_PACKAGE),)
	cd web && $(WEB_PM) run build
endif

test: ## Run unit + Testcontainers integration tests (needs Docker), gatekeeper tests (and web/ Vitest)
	cd backend && go test -v -race ./...
	cd mcp && go test -v -race ./...
	python3 -m unittest discover -s .agents/skills/pr-reviewer/scripts
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
