# Pulse Pipeline — Backend & Sync Worker (`backend/`)

Go service containing:
- `cmd/api`: HTTP/REST API serving the Web/PWA frontend (the Google OAuth web callback arrives with Phase 2 auth; until then accounts are linked with `pulse-worker connect-google`).
- `cmd/worker`: Nightly health data ingestion service pulling from the Google Health API (`HealthProvider` adapter).
- `internal/`: Domain models, provider adapters, `sqlc` database repositories, and OpenTelemetry / OpenFeature integration.
- `migrations/`: PostgreSQL schema migrations (`golang-migrate`).

## Health Providers

`internal/provider/mock` is a deterministic synthetic `HealthProvider` for local development
and tests: the same seed, user and local day always yield the same data, whatever range is
requested. Daily summaries equal the aggregates of the intraday series for that local day, data
stops at the injected `Now`, and some days have realistic gaps (watch off all day, a charging
gap, no SpO2/HRV). Use it wherever code needs provider data without Google credentials:

```go
p := mock.New(mock.Options{Seed: 1, Now: func() time.Time { return fixedNow }})
```

`internal/provider/googlehealth` reads Fitbit Air data from the
[Google Health API](https://developers.google.com/health) v4 ([ADR-0004](../docs/adr/0004-google-health-provider.md)):
daily steps and calories from daily rollups; resting heart rate, HRV and SpO2 from Google's daily
types; intraday heart rate, HRV, SpO2 and steps from `:reconcile`; intraday calories from 60-second
rollups; and sleep sessions with deep/REM/light/awake stages. The API has no sleep score, so
`sleep_score` stays empty for Google data. Its OAuth tokens are stored AES-256-GCM encrypted
(`internal/tokencrypt`, `internal/credentials`).

### Linking a Google Health account

1. In Google Cloud Console, enable the Google Health API, configure the OAuth consent screen
   (External, Testing) with your Google account as a test user, and create an OAuth client of
   type **Desktop app**. Open the Google Health app on your phone once so the account is linked.
2. Put the client ID and secret, and a token key, in your local `.env` (never in the repository
   or in chat):

   ```bash
   openssl rand -base64 32   # value for TOKEN_ENCRYPTION_KEY
   ```

3. Create a user (for example with `seed-dev`) and link it. The command prints a Google consent
   URL; open it in a browser **on the same machine** and allow every permission. Google redirects
   to a temporary listener on `127.0.0.1`, and the command stores the tokens encrypted:

   ```bash
   go run ./cmd/worker connect-google -user <ID>
   go run ./cmd/worker sync -user <ID> -provider google_health
   ```

While the Google Cloud app is in *Testing* status, Google expires its refresh tokens after
7 days. The next sync then fails with `invalid_grant`, the connection goes to `error`, and
`connect-google` must be run again (it keeps the sync history).

**Rotating `TOKEN_ENCRYPTION_KEY`:** set the new key with a higher
`TOKEN_ENCRYPTION_KEY_VERSION` and move the old one to `TOKEN_ENCRYPTION_PREVIOUS_KEYS`
(`1:<old base64 key>`). Each token refresh re-encrypts with the new key; drop the old key once
`SELECT token_key_version, count(*) FROM provider_connections GROUP BY 1` shows no rows on it.

## Sync Worker

`cmd/worker` runs the ingestion orchestrator in `internal/sync` ([ADR-0003](../docs/adr/0003-sync-worker.md)).
Each run ingests a date range idempotently, retries transient provider errors with backoff and
records itself in `sync_runs`. A successful run schedules the next one at 04:00 in the user's
timezone; a partial or failed scheduled run is retried after an hour.

To get mock data into a local database (`make up` first):

```bash
export DATABASE_URL=postgres://pulse:pulse_dev_password@localhost:5432/pulse_pipeline?sslmode=disable
export ENV=development PULSE_ENABLE_MOCK_PROVIDER=true
go run ./cmd/worker seed-dev                       # prints the new user's ID
go run ./cmd/worker                                # polls and syncs due connections (Ctrl-C to stop)
go run ./cmd/worker sync -user <ID> -provider mock # one manual sync
go run ./cmd/worker sync -user <ID> -provider mock -from 2026-01-01 -to 2026-01-31  # backfill
```

| Variable | Default | Meaning |
| :--- | :--- | :--- |
| `DATABASE_URL` | — (required) | PostgreSQL URL; migrations are applied on startup. |
| `PULSE_SYNC_POLL_INTERVAL` | `1m` | How often `run` checks for due connections. |
| `PULSE_ENABLE_MOCK_PROVIDER` | `false` | Register the synthetic `mock` provider. |
| `ENV` | — | `seed-dev` refuses to run unless it is `development`. |
| `GOOGLE_OAUTH_CLIENT_ID` / `GOOGLE_OAUTH_CLIENT_SECRET` | — | Desktop-app OAuth client; when both are set the `google_health` provider is registered and `connect-google` works. |
| `TOKEN_ENCRYPTION_KEY` | — | Base64 of 32 random bytes; encrypts OAuth tokens at rest. Required with the Google provider. |
| `TOKEN_ENCRYPTION_KEY_VERSION` | `1` | Version stored with each ciphertext; raise it when rotating. |
| `TOKEN_ENCRYPTION_PREVIOUS_KEYS` | — | `version:base64key,…` of retired keys still needed to read older rows. |

## Database Workflow

The schema and its rationale are in [ADR-0002](../docs/adr/0002-health-data-schema.md).

| Task | Command |
| :--- | :--- |
| Add a schema change | Create `migrations/NNNNNN_name.up.sql` **and** `.down.sql`, then `make generate` |
| Add/change a query | Edit `internal/store/queries/*.sql`, then `make generate` (never edit `internal/store/db` by hand) |
| Start local infrastructure, wait for PostgreSQL, and apply migrations | `make up && make migrate-up` |
| Run integration tests | `make test` (each package with DB tests starts a throwaway PostgreSQL 17 via Testcontainers, see `internal/testdb`; requires Docker) |

Every health-data query must filter by `user_id`. The only cross-user query is
`ListConnectionsDueForSync`, reserved for the nightly scheduler (`AGENTS.md` §1.1).
