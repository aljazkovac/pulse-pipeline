---
name: coder
description: >-
  Acts as the Senior Go & TypeScript Implementation Engineer for Pulse Pipeline.
  Use this skill when writing or refactoring Go code in backend/ and mcp/, SQL
  migrations and sqlc queries, or React/TypeScript frontend code in web/.
---

# Pulse Pipeline — Coder Agent Skill

Use this skill during feature implementation across `backend/`, `mcp/`, and `web/`.

## 1. Go Standards (`backend/` & `mcp/`)

- **Project Layout:**
  - `cmd/<binary>/main.go`: Thin wiring only (config loading, dependency injection, graceful shutdown via `signal.NotifyContext`).
  - `internal/domain/`: Domain structs, value types, and interfaces.
  - `internal/store/`: Database access via `pgx/v5` and `sqlc`.
  - `internal/provider/`: `HealthProvider` implementations (`mock`, `googlehealth`).
- **Context & Timeouts:**
  - Pass `ctx context.Context` as the first parameter of every I/O, database, or external API function.
  - Never use `http.DefaultClient`; always configure an `http.Client` with explicit `Timeout`.
- **Error Handling:**
  - Wrap errors with actionable context: `fmt.Errorf("googlehealth: fetch sleep sessions for user %s: %w", userID, err)`.
  - Never log and return the same error (log at the top-level handler/worker boundary, wrap and return in internal packages).
- **Database & Concurrency:**
  - Use `TIMESTAMPTZ` in PostgreSQL and `time.Time` (in UTC) in Go.
  - Ensure all ingestion writes are idempotent (`INSERT ... ON CONFLICT DO UPDATE`).

## 2. Frontend Standards (`web/`)

- **TypeScript Strictness:**
  - Zero `any` types; define explicit API response schemas and domain types.
- **Component Structure:**
  - Keep presentation components pure and props-driven so they can be tested in isolation with Vitest and toggled via OpenFeature flags.
  - Use Tailwind CSS utility classes with `tabular-nums` for all numeric health metrics.

## 3. Verification Before Hand-Off

Before marking an implementation task complete:
1. Run `make build` to verify all binaries/bundles compile cleanly.
2. Run `make test` and `make lint` to verify zero test regressions or linter warnings.
3. If `web/` changed, run `make e2e` to verify the Playwright desktop + mobile specs.
