# Pulse Pipeline — Backend & Sync Worker (`backend/`)

Go service containing:
- `cmd/api`: HTTP/REST API serving the Web/PWA frontend and handling Google OAuth 2.0 callbacks.
- `cmd/worker`: Nightly health data ingestion service pulling from the Google Health API (`HealthProvider` adapter).
- `internal/`: Domain models, provider adapters, `sqlc` database repositories, and OpenTelemetry / OpenFeature integration.
- `migrations/`: PostgreSQL schema migrations (`golang-migrate`).

## Database Workflow

The schema and its rationale are in [ADR-0002](../docs/adr/0002-health-data-schema.md).

| Task | Command |
| :--- | :--- |
| Add a schema change | Create `migrations/NNNNNN_name.up.sql` **and** `.down.sql`, then `make generate` |
| Add/change a query | Edit `internal/store/queries/*.sql`, then `make generate` (never edit `internal/store/db` by hand) |
| Apply migrations locally | `make up && make migrate-up` |
| Run integration tests | `make test` (starts a throwaway PostgreSQL 17 via Testcontainers; requires Docker) |

Every health-data query must filter by `user_id`. The only cross-user query is
`ListConnectionsDueForSync`, reserved for the nightly scheduler (`AGENTS.md` §1.1).
