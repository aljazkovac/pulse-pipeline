# ADR 0005: Phase 2 API — Authentication, Sessions, Read Model & Queued Manual Sync

- **Status:** Accepted (2026-11-23)
- **Date:** 2026-11-22

## Context

Phase 1 ends with data in PostgreSQL and no way to read it except `psql`: `cmd/api` serves only
`/healthz`. Phase 2 adds the web/PWA dashboards. Three earlier decisions were waiting for this:

- ADR-0003 §5 put off the HTTP "sync now" trigger because the API had no authentication, so an
  endpoint that accepted a `user_id` would let anyone sync for anyone.
- ADR-0004 §3 linked Google accounts from an operator CLI for the same reason. It promised a web
  callback that "only changes where the user ID comes from".
- AGENTS.md §1.1 requires every endpoint that touches health data to be scoped by `user_id`.
  Without an authenticated principal, no endpoint can honour that.

The dashboards also need things the store does not offer yet:

- **Downsampling:** intraday heart rate is about 17k rows per user per day (ADR-0002), and
  `ListIntradayMetrics` returns raw rows.
- **A connections list:** no query lists a user's connections.
- **A step goal:** nothing stores one.
- **Sleep duration:** it is not in `daily_summaries`.
- **A readiness score:** the Google Health API has none, and its `sleep_score` is always empty
  (ADR-0004).
- **A provider choice:** every list query takes a `provider`, so the API has to decide which
  provider's data a dashboard shows.

Constraints:

- **One user in practice, many by design:** sign-up stays closed, but nothing may assume one user.
- **Same origin:** the API and the web app share an origin. In development, the Vite dev server
  proxies `/api` to `:8080`. In Phase 5, the ingress routes `/api` to the API and everything else
  to the static web bundle.
- **No `any` on the frontend:** `strict` TypeScript (AGENTS.md §1.3). The API contract must
  therefore produce TypeScript types, not hand-copied interfaces.
- **Phase 3 reuse:** the MCP server needs the same reads. Go does not let `mcp/`, a separate
  module, import `backend/internal/...`.

## Decision

### 1. API shape: OpenAPI-first REST on `net/http`

- **Contract:** `backend/api/openapi.yaml` (OpenAPI 3.1) is the contract.
  - `oapi-codegen` generates the Go request/response types and a `net/http` (`std-http-server`)
    server interface into `backend/internal/api/gen`.
  - `openapi-typescript` generates `web/src/api/schema.d.ts`; the web client uses `openapi-fetch`.
  - `make generate` runs both generators, and `make generate-check` fails CI on drift, as it
    already does for `sqlc`.
- **Routing:** standard-library `http.ServeMux` method and path patterns, under `/api/v1`. No
  web framework.
- **Errors:** RFC 9457 `application/problem+json` with a stable `type` per error. Upstream and
  internal error text never reaches clients.
- **Wire formats:**
  - Local calendar days are `YYYY-MM-DD`, in the user's timezone.
  - Instants are RFC 3339 UTC.
  - Every data response carries the user's `timezone`, so the client renders in that zone, not
    the browser's.
  - Missing values are `null` and are never zero-filled.
- **Caching:** every `/api` response sends `Cache-Control: no-store`.
- **Logging:** request logs record method, route pattern, status and duration. They never record
  query strings (OAuth callbacks carry `code` and `state`), cookies or bodies.

### 2. Authentication: Google sign-in for allowlisted emails

- **Flow:** sign-in uses Google's OpenID Connect authorization-code flow with PKCE (`S256`), a
  random `state`, and only the `openid email` scopes.
  - `GET /api/v1/auth/google/login` redirects to Google.
  - `GET /api/v1/auth/google/callback` finishes the flow.
- **Separate from linking:** health linking (§4) is a separate consent with its own scopes. The
  Google account a user signs in with does not have to be the one that owns the health data, and
  signing in never asks for health scopes.
- **ID token checks:** the ID token comes straight from Google's token endpoint over TLS. Under
  OpenID Connect Core §3.1.3.7, that lets us check the claims without fetching keys or verifying
  the signature. The required claims are:
  - `iss` is `https://accounts.google.com` or `accounts.google.com`;
  - `aud` is our client ID;
  - `exp` is in the future;
  - `email_verified` is `true`.

  As in ADR-0004, this is plain `net/http` with no Google SDK.
- **Sign-in and account ownership.** Check the token email against the comma-separated
  `PULSE_ALLOWED_EMAILS` on every sign-in. Return `403` for an unlisted email. Look up a
  returning identity by its validated `(issuer, subject)`, using `https://accounts.google.com`
  for both accepted issuer spellings. The `user_identities` table uses that pair as its
  primary key and stores `user_id`, `email`, `created_at` and `last_login_at`.
  - A new identity may create a Pulse user when the allowlisted email is unused and Google is
    authoritative for it (Gmail, or verified Google Workspace email with an `hd` claim).
    Create the user and identity in one transaction. If the email already belongs to a Pulse
    user, including a case-insensitive collision during concurrent enrollment, return `403`
    without creating a session or changing either account.
  - To preserve a Phase 1 user's history, an operator independently verifies the owner and
    binds the exact Google identity to the existing Pulse user ID. Third-party email addresses
    require operator enrollment with a fresh mailbox-ownership check. Replacing an established
    identity requires operator recovery and revokes the user's existing sessions.
    [Google's guidance on email ownership](https://developers.google.com/identity/gsi/web/guides/verify-google-id-token)
- **Timezone and step goal:** both come from the user's settings (§5). The web app offers the
  browser's IANA zone when `users.timezone` is still the default `UTC`.

### 3. Sessions and request protection

- **Storage:** sessions live server-side in the new `sessions` table: `token_hash BYTEA` primary
  key, `user_id`, `created_at`, `last_seen_at`, `expires_at`.
  - The cookie holds 32 random bytes, base64url-encoded.
  - The database stores only their SHA-256, so a database leak yields no usable sessions.
- **Cookie:** in production the cookie is `__Host-pulse_session` with `HttpOnly`, `Secure`,
  `SameSite=Lax` and `Path=/`. With `ENV=development` it is `pulse_session` without `Secure`, so
  plain `http://localhost` works in every Playwright browser.
- **Lifetime:**
  - Sessions expire after 30 days idle or 90 days absolute.
  - `last_seen_at` is written at most once an hour, so reads do not turn into writes.
  - Signing in always issues a new token. `POST /api/v1/auth/logout` deletes the row and clears
    the cookie.
  - A background ticker in the API deletes expired rows hourly.
- **Lookup query:** `GetSessionByTokenHash` is the authentication boundary. It is keyed by a
  secret, returns only `user_id` and session timestamps, and is used only by the auth middleware.
  It is a system-context query in the AGENTS.md §1.1 sense and is documented as one, like
  `ListConnectionsDueForSync`.
- **Scoping:** handlers get the `user_id` from the request context set by the middleware. No
  endpoint accepts a `user_id` parameter. Every store call is scoped by that ID.
- **Cross-site requests:** Go's `http.CrossOriginProtection` (Go 1.25+) wraps the mux and rejects
  cross-origin non-safe requests, using `Sec-Fetch-Site` with an `Origin` fallback. With
  `SameSite=Lax` cookies, no CSRF tokens are needed.
- **OAuth flow cookie.** Login and health linking store `state`, the PKCE verifier and the flow
  purpose in a ten-minute cookie. In production, set `__Host-pulse_oauth` over HTTPS with
  `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/` and `Max-Age=600`. Omit `Domain`. For plain
  HTTP development, use `pulse_oauth` and omit `Secure`. [Cookie prefix requirements](https://httpwg.org/http-extensions/draft-ietf-httpbis-rfc6265bis.html#section-4.1.3.2)
  - The callback checks the purpose, compares `state` in constant time and expires the cookie
    using the same name, path and security attributes.
  - A link callback also verifies that the current session matches the session hash saved at
    the start of the flow.

### 4. Linking Google Health from the web (amends ADR-0004 §3)

- **Endpoints:**
  - `GET /api/v1/connections/google_health/link` (signed-in users only) redirects to Google. It
    uses the three read-only health scopes, PKCE, `access_type=offline` and `prompt=consent`.
  - `GET /api/v1/connections/google_health/callback` finishes the flow.
- **Shared steps:** ADR-0004's CLI steps 3–5 move into one function that both
  `pulse-worker connect-google` and the callback call:
  1. code exchange;
  2. the all-three-scopes check;
  3. the `identity` call and account-ID match;
  4. the encrypted upsert under the connection lock.

  The `user_id` comes from the session, never from the request or the flow cookie.
- **A second OAuth client:** a loopback redirect needs a *Desktop app* client, and a web redirect
  needs a *Web application* client. Google binds a refresh token to the client that issued it.
  - The worker therefore has to know which client issued each connection's tokens.
  - `provider_connections.oauth_client` stores that: `'desktop'` or `'web'`, `NULL` for providers
    without OAuth.
  - It travels with the decrypted `domain.Credentials`, and `RefreshAuth` uses the matching client.
  - If that client is not configured, the run fails with `domain.ErrUnauthorized`: the user has
    to re-link, rather than retrying hourly forever.
  - The migration sets existing `google_health` rows to `'desktop'`.
- **Configuration:**
  - The web client is `GOOGLE_WEB_OAUTH_CLIENT_ID` / `GOOGLE_WEB_OAUTH_CLIENT_SECRET`. The desktop
    client keeps its ADR-0004 variables.
  - Redirect URIs are derived from `PULSE_PUBLIC_URL` (in development
    `http://localhost:5173`, the Vite origin), which replaces `GOOGLE_OAUTH_REDIRECT_URL`. Both
    callback paths must be registered on the web client.
  - The API now needs `TOKEN_ENCRYPTION_KEY` as well (§Consequences).

### 5. Read model

- **Package:** `backend/internal/readmodel` turns store rows into dashboard shapes. The API
  handlers (and, in Phase 3, the MCP tools) depend on it, never on `store/db` directly.
  - It holds pure functions: rolling means, sleep durations, sleep consistency. These are
    unit-tested without a database.
  - It also holds `user_id`-scoped store reads, tested with Testcontainers.
- **Which provider:** the dashboards read one provider at a time. Merging providers needs
  de-duplication rules we do not have.
  - Migration `000004` sets `users.primary_provider` for each existing user with connections.
    Prefer an active connection, then the earliest `created_at`, then provider name to break
    ties. Users without connections keep `NULL`. CLI and web linking set the default in the
    connection transaction only when it is `NULL`. Later links preserve the user's choice.
  - Read endpoints accept an optional `provider`, limited to providers the user has a connection
    for.
- **Daily data and trends:** they come from `daily_summaries`. The read model adds a trailing
  7-day mean per metric, which uses only days that have a value.
- **Intraday data:** the new `ListIntradayBuckets` query uses `date_bin(@bucket, recorded_at,
  @origin)`. `@origin` is the local start of the range's first day, so hour buckets line up with
  the user's clock even in zones with a half-hour offset.
  - Heart rate, HRV and SpO2 return avg/min/max/count per bucket. Steps and calories return sums.
  - Allowed buckets are `1m`, `5m`, `15m` and `1h`. The server picks the smallest that gives at
    most 1,440 points.
  - A range is at most 7 local days.
  - The `intraday_metrics` primary key `(user_id, provider, metric_type, recorded_at)` serves
    this range scan, so no index is added.
- **Time asleep.** For staged sessions, sum the durations of deep, REM and light sleep. For
  sessions without stages, return `staged: false` and use the provider's reported minutes
  asleep when available. The Google Health adapter maps `minutesAsleep` into a nullable field
  on the normalized sleep session and the store persists it on upsert. For historical sessions
  with only start and end instants, return `null` for time asleep. A reported zero remains
  zero. Overview and Sleep use the same calculation.
- **Sleep consistency.** Compute bedtime and wake-time consistency separately for main-sleep
  sessions in the user's current timezone. Use circular standard deviation on a 24-hour clock,
  expressed in minutes. Bedtimes of 23:55 and 00:05 have a spread of about five minutes.
  Return `null` for fewer than two observations or an undefined circular mean.
- **Sleep dates.** Assign each session to the local date of its wake instant (`end_at`) in the
  user's current timezone. For an inclusive `from`–`to` request, select `end_at` from local
  midnight on `from` up to, but excluding, local midnight on the calendar day after `to`.
  Convert both boundaries to UTC and scope the query by `user_id` and provider. Overview,
  Sleep and consistency use this rule. A timezone change takes effect on the next read,
  including across 23- and 25-hour days.
- **Settings:** `users.step_goal` is `INTEGER NOT NULL DEFAULT 10000`, with a
  `CHECK (step_goal BETWEEN 1 AND 100000)`. `PATCH /api/v1/me` updates `timezone` (validated
  with `time.LoadLocation`), `step_goal` and `primary_provider`.
- **No readiness score:** Phase 2 has none. A derived score is a product decision, and it belongs
  with the Phase 4 dashboard-insights experiment.

### 6. Manual sync is queued, not run in the request (amends ADR-0003 §5)

`POST /api/v1/connections/{provider}/sync` does not call `Syncer.SyncConnection` in-process. A
first sync covers 30 days with retries, and an HTTP request should neither wait for it nor tie
an API replica to a provider's rate limit.

- **Queueing.** Lock the signed-in user's connection. Return `409` (`relink-required`) for an
  `error` or `revoked` connection. If a request is pending, preserve its `sync_requested_at`
  and return `202 queued`. If a live scheduled or manual run already covers the full window
  that an incremental sync would request now (ADR-0003 §2), return `202 running` with its run
  ID. Otherwise set `sync_requested_at = now()` and return `202 queued`. A historical backfill
  can finish while the manual request stays pending.
- **Claiming.** `ListConnectionsDueForSync` returns active connections that are due or have
  a pending request. Its `requested` flag is a hint. To claim queued work, lock the connection
  and recheck its status and `sync_requested_at`. If the marker is absent, skip the queued claim.
  If another live run holds the slot, leave the marker in place. Otherwise insert the `manual`
  run and clear the marker in the same transaction. A failed insert or rollback leaves the
  request pending. The operator CLI can still start an on-demand run without a queue marker.
- **Outcome:** as ADR-0003 §3 defines for `manual`.
  - A successful run advances the cursor and schedules the next nightly run.
  - A failed one leaves `next_sync_at`, so the nightly run is unaffected.
- **Latency and status.** An idle worker polls every `PULSE_SYNC_POLL_INTERVAL` (one minute by
  default). A running sync, retries or work ahead of the connection can delay the start beyond
  that interval. The connections response exposes `sync_requested_at` so the UI can show
  "queued" before a run exists. Once claimed, the run appears in `sync-runs`.

### 7. Endpoints (v1)

All endpoints require a session except the sign-in routes and `/healthz`.

| Method & path | Purpose |
| :--- | :--- |
| `GET /api/v1/auth/google/login`, `GET …/callback`, `POST /api/v1/auth/logout` | Sign-in and sign-out (§2, §3). |
| `GET /api/v1/me`, `PATCH /api/v1/me` | Profile and settings: email, timezone, step goal, primary provider. |
| `GET /api/v1/overview?date=` | One local day: steps vs goal, resting HR, HRV, sleep duration, last sync. |
| `GET /api/v1/daily?from=&to=&provider=` | Daily summaries with 7-day means (trends; at most 366 days). |
| `GET /api/v1/metrics/{metric}/series?from=&to=&provider=` | Bucketed intraday series (§5). |
| `GET /api/v1/sleep?from=&to=&provider=` | Sessions, stages and derived durations. |
| `GET /api/v1/connections` | The user's connections, including provider, status, scopes, last and next sync, and `sync_requested_at`. Tokens and ciphertext never appear. |
| `GET /api/v1/connections/{provider}/sync-runs` | Recent `sync_runs` for the user. |
| `POST /api/v1/connections/{provider}/sync` | Queue a manual sync (§6). |
| `GET /api/v1/connections/google_health/link`, `GET …/callback` | Web linking (§4). |

### 8. Development and E2E authentication

- **Dev login:** `POST /api/v1/dev/login` with `{"email": …}` creates a session for an existing
  user. The route is registered only when both `ENV=development` and `PULSE_ENABLE_DEV_LOGIN=true`
  are set. A test asserts it returns `404` under production configuration.
- **Fixed test data:** `seed-dev` gains `-user-id` (a fixed UUID).
  - Both binaries honour `PULSE_DEV_NOW` (an RFC 3339 instant) only when `ENV=development`.
  - Mock data depends on the user ID and the clock, so this pins every number on the dashboards.
- **E2E stack:** Playwright starts compose PostgreSQL, the API, the worker (one seed and sync) and
  Vite. The desktop (`1280x800`) and mobile (`390x844`) projects share one seeded user.

### 9. PWA caching

- **What is cached:** the service worker precaches the built app shell only. It never caches
  `/api/*` at runtime, so health data never sits in the Cache Storage of a lost phone.
- **Offline:** the app shows its offline state rather than stale data.
- **Secure origin:** installing on a real phone needs HTTPS. Until Phase 5 provides it, it is
  checked only in automated tests.

### 10. Phase 3 placement

The MCP server will move from the `mcp/` module to `backend/cmd/mcp` so it can import
`internal/readmodel` and `internal/store`. ADR-0001 already expects it to share those packages
"directly". The Phase 3 ADR records the move.

### 11. Migration `000004`

| Change | Section |
| :--- | :--- |
| `user_identities` (PK `(issuer, subject)`, `user_id` FK `ON DELETE CASCADE`) | §2 |
| `sessions` (PK `token_hash`, `user_id` FK `ON DELETE CASCADE`) | §3 |
| `users.step_goal`, `users.primary_provider` with a deterministic backfill from each user's connections | §5 |
| `sleep_sessions.minutes_asleep INTEGER NULL CHECK (minutes_asleep >= 0)`. Existing rows start at `NULL` and can acquire a value on a later sync. | §5 |
| `provider_connections.oauth_client` (with `CHECK`), backfill `'desktop'` for `google_health` | §4 |
| `provider_connections.sync_requested_at` | §6 |

## Consequences

- **Positive:**
  - Every dashboard read is `user_id`-scoped through one middleware and one package.
  - The two operator-only paths (link and sync now) become self-service, and the CLI keeps working.
  - Frontend types come from the same contract as the handlers, so drift fails CI.
  - A manual sync goes through the same claim and run guard as a scheduled one.
  - Intraday charts receive at most about 1,440 points whatever the raw volume.
- **Trade-offs:**
  - `TOKEN_ENCRYPTION_KEY` now lives in two processes (worker and API), which widens its
    exposure. The API uses it only to encrypt on link.
  - Two OAuth clients to configure and rotate. Connections linked through a client that is later
    removed must be re-linked.
  - Manual requests share the worker with scheduled syncs and backfills. The UI distinguishes
    a queued request from a running sync.
  - Server-side sessions cost one indexed lookup per request.
  - The dashboards show one provider at a time.
  - While the Google Cloud app is in *Testing*, health refresh tokens still expire after 7 days.
    Web linking makes the weekly re-link a button instead of a terminal session.
- **Follow-ups:**
  - Rate-limiting the sign-in routes if sign-up ever opens.
  - Passkeys as a second sign-in method.
  - A merged multi-provider view.
  - The Phase 4 readiness experiment.
  - Phase 5 ingress and HTTPS, which unblock real-phone PWA installation.
