# Pulse Pipeline — Backend & Sync Worker (`backend/`)

Go service containing:
- `cmd/api`: HTTP/REST API serving the Web/PWA frontend and handling Google OAuth 2.0 callbacks.
- `cmd/worker`: Nightly health data ingestion service pulling from the Google Health API (`HealthProvider` adapter).
- `internal/`: Domain models, provider adapters, `sqlc` database repositories, and OpenTelemetry / OpenFeature integration.
- `migrations/`: PostgreSQL schema migrations (`golang-migrate`).
