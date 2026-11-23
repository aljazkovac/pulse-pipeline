# Pulse Pipeline — Backend & Sync Worker (`backend/`)

Go service containing:
- `cmd/api`: HTTP/REST API serving the Web/PWA frontend under `/api/v1`, with Google sign-in and server-side sessions (see [API](#api)). Google Health is still linked with `pulse-worker connect-google` until web linking ships with the Connections view.
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
2. From the repository root, set `DATABASE_URL`, `GOOGLE_OAUTH_CLIENT_ID`,
   `GOOGLE_OAUTH_CLIENT_SECRET`, and `TOKEN_ENCRYPTION_KEY` in your local `.env`.
   Use `.env.example` as a template and keep your local `.env` out of the repository and chat.
   Generate the token key with OpenSSL.

   ```bash
   openssl rand -base64 32   # value for TOKEN_ENCRYPTION_KEY
   ```

3. The worker reads its environment. In the shell where you will run it, export the settings
   from your local `.env` and enter the backend directory.

   ```bash
   set -a
   . ./.env
   set +a
   cd backend
   ```

4. Use an existing Pulse user ID or create a user with `go run ./cmd/worker seed-dev` in
   development. Replace `PULSE_USER_ID` below with that ID. Open the consent URL printed by
   `connect-google` in a browser **on the same machine** and allow every permission. Google
   redirects to a temporary listener on `127.0.0.1`, and the worker stores the tokens encrypted.

   ```bash
   go run ./cmd/worker connect-google -user PULSE_USER_ID
   go run ./cmd/worker sync -user PULSE_USER_ID -provider google_health
   ```

While the Google Cloud app is in *Testing* status, Google expires its refresh tokens after
7 days. The next sync then fails with `invalid_grant`, the connection goes to `error`, and
`connect-google` must be run again with the same Google Health account. Re-linking keeps the
sync history. If a sync is running, wait for it to finish and retry. Connections linked before
account verification was stored can continue syncing with their existing credentials. To link
again when the original account is unknown, use a separate Pulse user to keep the histories
separate.

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
| `GOOGLE_OAUTH_CLIENT_ID` / `GOOGLE_OAUTH_CLIENT_SECRET` | — | Desktop-app OAuth client; when both are set the `google_health` provider is registered and `connect-google` and `bind-google-identity` work. |
| `TOKEN_ENCRYPTION_KEY` | — | Base64 of 32 random bytes; encrypts OAuth tokens at rest. Required with the Google provider. |
| `TOKEN_ENCRYPTION_KEY_VERSION` | `1` | Version stored with each ciphertext; raise it when rotating. |
| `TOKEN_ENCRYPTION_PREVIOUS_KEYS` | — | `version:base64key,…` of retired keys still needed to read older rows. |

## API

`cmd/api` serves the `/api/v1` contract in [`api/openapi.yaml`](api/openapi.yaml) (OpenAPI 3.1,
[ADR-0005](../docs/adr/0005-api-auth-and-read-model.md)) on the standard library's `http.ServeMux`.
The contract is the source of truth for both sides:

- `oapi-codegen` (a Go tool pinned in `go.mod`, configured by `api/oapi-codegen.yaml`) generates the
  request/response types and a strict server interface into `internal/api/gen`. Handlers in
  `internal/api` implement `gen.StrictServerInterface`, so each can only return the statuses and
  bodies the contract declares.
- `openapi-typescript` generates `web/src/api/schema.d.ts` for the web client (`openapi-fetch`).

After editing the contract, run `make generate` from the repository root and commit the result;
`make generate-check` (CI) fails when either generated file is stale.

Conventions every endpoint gets from `internal/api`:

- Errors are RFC 9457 `application/problem+json` with a stable `type` from
  [docs/api/problems.md](../docs/api/problems.md); unknown `/api` paths and methods answer with
  `not-found` / `method-not-allowed` problems. Internal and upstream error text is logged, never
  sent.
- Every `/api` response carries `Cache-Control: no-store` and `X-Content-Type-Options: nosniff`.
- One log line per request with method, route pattern, status and duration only: no URLs (OAuth
  callbacks carry `code` and `state` in the query), cookies, headers or bodies.
- A handler panic becomes a `500` `internal` problem (dropping any headers the handler had set,
  such as `Set-Cookie`) and is logged with its stack. If the reply had already started, the
  connection is aborted instead, so a truncated body cannot pass as complete.
- Non-canonical paths (`//`, `..`) under `/api` are `not-found`, not redirected.
- State-changing requests (`POST`, `PUT`, `PATCH`, `DELETE`) from another origin are rejected
  with a `cross-origin-request` problem by `http.CrossOriginProtection`, which wraps the mux.

| Endpoint | Session | Purpose |
| :--- | :--- | :--- |
| `GET /healthz` | — | Liveness probe; does not touch the database. |
| `GET /api/v1/health` | — | Readiness: pings the database, `503` `unavailable` problem when it is down. |
| `GET /api/v1/auth/google/login` | — | Starts Google sign-in: redirects to Google and sets the ten-minute OAuth flow cookie. |
| `GET /api/v1/auth/google/callback` | — | Google redirects here; on success sets the session cookie and redirects to `/`. |
| `POST /api/v1/auth/logout` | — | Ends the session, if any, and expires its cookie (`204`). |
| `GET /api/v1/me` | required | The signed-in user's email and timezone. |

```bash
export DATABASE_URL=postgres://pulse:pulse_dev_password@localhost:5432/pulse_pipeline?sslmode=disable
make migrate-up    # from the repository root; the API never migrates (see below)
go run ./cmd/api   # listens on :8080
curl -i localhost:8080/api/v1/health
```

| Variable | Default | Meaning |
| :--- | :--- | :--- |
| `DATABASE_URL` | — (required) | PostgreSQL URL. |
| `API_PORT` | `8080` | Listen port. The web dev server proxies `/api` here. |
| `ENV` | — | `development` selects the unprefixed, non-`Secure` cookies for plain HTTP. |
| `GOOGLE_WEB_OAUTH_CLIENT_ID` / `GOOGLE_WEB_OAUTH_CLIENT_SECRET` | — | Web-application OAuth client for sign-in. Without the ID the API runs, but the sign-in routes answer `503` and nobody can sign in. |
| `PULSE_PUBLIC_URL` | — | Origin browsers use (development: `http://localhost:5173`); redirect URIs are derived from it. Required with sign-in; must be HTTPS unless `ENV=development`. |
| `PULSE_ALLOWED_EMAILS` | — | Comma-separated addresses allowed to sign in, compared case-insensitively. Required with sign-in. |

The API does not apply migrations, so its database role needs no DDL rights: `pulse-worker`
migrates on startup, or run `make migrate-up`. `/api/v1/health` checks database connectivity only,
not the schema version.

On `SIGINT`/`SIGTERM` the API stops accepting connections and drains in-flight requests for up to
15 seconds before exiting (a second signal exits at once).

### Authentication

Sign-in follows [ADR-0005](../docs/adr/0005-api-auth-and-read-model.md) §2–3 (`internal/auth`):

- **Google sign-in** uses OpenID Connect with PKCE and only the `openid email` scopes; it never
  asks for health data. The ID token comes straight from Google's token endpoint over TLS, so its
  claims (`iss`, `aud`, `exp`, `email_verified`) are checked without verifying the signature.
- **Who may sign in:** the email must be in `PULSE_ALLOWED_EMAILS`, checked on every sign-in. A
  returning Google identity, found by its `(issuer, subject)` in `user_identities`, signs in to its
  user. A new identity creates a user only when Google is authoritative for its email (Gmail or a
  Google Workspace account) and neither a user nor another user's Google identity has that email
  yet. Every other case is refused with `403 sign-in-denied` and changes nothing. ID tokens must
  name exactly this client as their audience.
- **Sessions** live in the `sessions` table. The cookie holds 32 random bytes; the table stores only
  their SHA-256. Sessions end after 30 days idle or 90 days after sign-in, `last_seen_at` is
  written at most hourly, and the API deletes expired rows hourly. Each sign-in issues a new
  token and ends the browser's previous session.
- **Cookies** are `HttpOnly` and `SameSite=Lax`. In production they are `__Host-pulse_session`
  and `__Host-pulse_oauth` with `Secure`, so `PULSE_PUBLIC_URL` must be HTTPS. With
  `ENV=development` they are `pulse_session` and `pulse_oauth` without `Secure`, for plain
  `http://localhost`.
- **Scoping:** every operation needs a session unless the contract opts it out with
  `security: []`; the middleware enforces the contract's `security` (generated into
  `gen.SessionScopes`), and handlers read the user ID only from the request context. No operation
  accepts a user ID. `GetSessionByTokenHash` and `GetUserIDByIdentity` are the only queries that
  run before the user is known (the authentication boundary in `AGENTS.md` §1.1).

**Setting up Google sign-in.** In the Google Cloud project that already holds the Desktop client,
create a second OAuth client of type **Web application** and register the redirect URI
`<PULSE_PUBLIC_URL>/api/v1/auth/google/callback`, for local development
`http://localhost:5173/api/v1/auth/google/callback` (the Vite dev server proxies `/api` to the
API). Put its ID and secret, `PULSE_PUBLIC_URL` and `PULSE_ALLOWED_EMAILS` in your local `.env`.
pulse-api logs the redirect URI it uses at startup.

**Keeping a Phase 1 user's history.** If your email already belongs to a user (for example one
created before sign-in existed), signing in is refused, because a new Google identity never
takes over an existing account. Bind your Google account to that user once, with the Desktop
client configured as for `connect-google`:

```bash
go run ./cmd/worker bind-google-identity -user PULSE_USER_ID
```

The account owner signs in to Google in a browser on the same machine (loopback redirect, as with
`connect-google`). The operator must know that person owns the Pulse user. For an address that is
not Gmail or Google Workspace, first confirm that the owner controls the mailbox (for example with
a one-time code sent to it) and add `-mailbox-verified`. Binding a different Google account later
needs `-replace`, which also signs the user out of every session; a sign-in racing the replacement
cannot keep a session either.

**Enrolling a non-Google address.** A Google account whose address is not Gmail or Workspace cannot
create its own user. After the mailbox check, the operator enrolls it with
`bind-google-identity -new-user -mailbox-verified`, then adds the address to `PULSE_ALLOWED_EMAILS`.

## Database Workflow

The schema and its rationale are in [ADR-0002](../docs/adr/0002-health-data-schema.md).

| Task | Command |
| :--- | :--- |
| Add a schema change | Create `migrations/NNNNNN_name.up.sql` **and** `.down.sql`, then `make generate` |
| Add/change a query | Edit `internal/store/queries/*.sql`, then `make generate` (never edit `internal/store/db` by hand) |
| Start local infrastructure, wait for PostgreSQL, and apply migrations | `make up && make migrate-up` |
| Run integration tests | `make test` (each package with DB tests starts a throwaway PostgreSQL 17 via Testcontainers, see `internal/testdb`; requires Docker) |

Every query must filter by `user_id`, with the exceptions `AGENTS.md` §1.1 allows, none of which
returns health data or tokens: `ListConnectionsDueForSync` (the nightly scheduler) and
`DeleteExpiredSessions` (the API's hourly sweep) are system context, and `GetSessionByTokenHash`,
`GetUserIDByIdentity` and the enrollment check `CreateUserUnlessEmailBound` are the authentication
boundary that turns a credential into a user ID.
