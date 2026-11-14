---
name: architect
description: >-
  Acts as the Staff Software Architect for Pulse Pipeline. Use this skill when
  designing PostgreSQL schemas, defining Go domain interfaces (such as
  HealthProvider), designing MCP server tools/resources, evaluating infrastructure
  trade-offs, or writing Architecture Decision Records (ADRs) in docs/adr/.
---

# Pulse Pipeline — Architect Agent Skill

Use this skill before implementing new domain subsystems, database migrations, external provider integrations, or infrastructure modules.

## Architectural Principles to Enforce

1. **Multi-User & Multi-Provider Readiness:**
   - Every tenant-owned table requires `user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE`.
   - Every ingested health table requires `provider VARCHAR(32) NOT NULL` (e.g., `'google_health'`, `'mock'`) and a unique constraint on its natural key so nightly syncs and backfills can safely `ON CONFLICT DO UPDATE`. The natural key depends on the table's shape:

     | Table | Unique key | Why |
     | :--- | :--- | :--- |
     | `intraday_metrics` | `(user_id, provider, metric_type, recorded_at)` | One sample per metric per instant. |
     | `daily_summaries` | `(user_id, provider, summary_date)` | One row per local calendar day; `summary_date` is a `DATE` in the user's timezone. |
     | `sleep_sessions` | `(user_id, provider, provider_session_id)` | Upstream session ID stays stable when the provider corrects start/end times; keying on timestamps would insert duplicates. |
2. **Clean Package Boundaries (`backend/` & `mcp/`):**
   - `internal/domain`: Pure Go structs and interfaces—zero imports of HTTP, SQL (`pgx`), or third-party vendor SDKs.
   - `internal/provider`: Implementations of `domain.HealthProvider` (`googlehealth`, `mock`).
   - `internal/store`: `sqlc`-generated queries and `pgx` repository implementations.
   - `internal/sync`: Nightly ingestion orchestrator, retry/backoff, and `sync_runs` audit logging.
3. **Architecture Decision Records (ADRs):**
   - Record significant architectural choices in `docs/adr/NNNN-title-with-dashes.md` using the structure:
     - **Status:** `Proposed` | `Accepted` | `Superseded`
     - **Context:** What problem or force is driving this decision?
     - **Decision:** What concrete design/technology did we choose?
     - **Consequences:** What becomes easier, and what trade-offs or constraints must we manage?

## Architect Review Checklist

Before signing off on a design or schema migration, verify:
- [ ] Are all time-series timestamps stored as `TIMESTAMPTZ` (UTC) alongside optional user timezone metadata?
- [ ] Does each table's unique-key index also serve its main dashboard/MCP read pattern (e.g. `WHERE user_id = $1 AND provider = $2 AND metric_type = $3 ORDER BY recorded_at DESC` uses the `intraday_metrics` key via a backward scan)? Only add a second index when `EXPLAIN` shows a query it cannot serve — avoid overlapping indexes with reordered columns.
- [ ] Can the nightly sync job be re-run for the same date range multiple times without duplicating rows (idempotency)?
- [ ] Are OAuth 2.0 access/refresh tokens isolated in `provider_connections` and never leaked in API responses or logs?
