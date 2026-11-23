.PHONY: help setup-tools up down ps logs clean build test cover lint e2e db-shell generate generate-check migrate-up migrate-down

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
	docker compose up --wait

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

# The OpenAPI contract (backend/api/openapi.yaml) generates the Go server types in
# backend/internal/api/gen (oapi-codegen, pinned as a Go tool in backend/go.mod) and
# the TypeScript types in web/src/api/schema.d.ts (openapi-typescript, in web/).
OAPI_CODEGEN := go tool oapi-codegen -config api/oapi-codegen.yaml
OPENAPI_TS := $(WEB_PM) exec -- openapi-typescript ../backend/api/openapi.yaml

generate: ## Regenerate sqlc code, the OpenAPI Go server types and the web/ TypeScript API types
	cd backend && sqlc generate
	cd backend && $(OAPI_CODEGEN) -o internal/api/gen/api.gen.go api/openapi.yaml
	cd web && $(OPENAPI_TS) -o src/api/schema.d.ts

generate-check: ## Fail if committed generated code (sqlc, OpenAPI Go + TypeScript) is out of date (run in CI)
	cd backend && sqlc diff
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	(cd backend && $(OAPI_CODEGEN) -o "$$tmp/api.gen.go" api/openapi.yaml) && \
	(cd web && $(OPENAPI_TS) -o "$$tmp/schema.d.ts" >/dev/null) || exit 1; \
	stale=0; \
	diff -u backend/internal/api/gen/api.gen.go "$$tmp/api.gen.go" || stale=1; \
	diff -u web/src/api/schema.d.ts "$$tmp/schema.d.ts" || stale=1; \
	if [ $$stale -ne 0 ]; then \
		echo "OpenAPI generated code is out of date: run 'make generate' and commit the result" >&2; exit 1; \
	fi

migrate-up migrate-down: export PULSE_MIGRATION_DATABASE_URL := $(value DATABASE_URL)

migrate-up: ## Apply all database migrations to DATABASE_URL (default: local docker-compose Postgres)
	@migrate -path backend/migrations -database "$${PULSE_MIGRATION_DATABASE_URL}" up

migrate-down: ## Revert the most recent database migration on DATABASE_URL
	@migrate -path backend/migrations -database "$${PULSE_MIGRATION_DATABASE_URL}" down 1

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

# Use testdb's CI mode to fail if the integration database cannot start.
cover: ## Backend coverage across packages (needs Docker); report in bin/coverage.html
	@mkdir -p bin
	cd backend && CI=1 go test -count=1 -race -coverpkg=./... -coverprofile=../bin/coverage.out ./...
	cd backend && go tool cover -html=../bin/coverage.out -o ../bin/coverage.html
	@cd backend && go tool cover -func=../bin/coverage.out | tail -1

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
