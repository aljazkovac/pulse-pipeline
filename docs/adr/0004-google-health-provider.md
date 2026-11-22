# ADR 0004: Google Health Provider, Token Encryption & Account Linking

- **Status:** Accepted
- **Date:** 2026-11-22

## Context

Phase 1 ends with the real data source: Fitbit Air data through the **Google Health API**
(`developers.google.com/health`), the successor to the Fitbit Web API, which Google turned off on
2026-11-02 (it now answers `410 Gone`). ADR-0002 fixed the `HealthProvider` contract and the
token columns; ADR-0003 fixed how the worker calls providers, retries `domain.ErrTransient` and
disables connections on `domain.ErrUnauthorized`. This ADR fixes how the adapter maps the API,
how tokens are encrypted, and how an account gets linked while the API has no user
authentication.

Facts from the official docs that drive the design (research notes, with a source URL per fact,
are kept outside the repo; links below are to the canonical pages):

- **Base URL** `https://health.googleapis.com/v4` (stable; `v4beta` is unstable and not for
  production). Resources are `users/me/dataTypes/{kebab-type}/dataPoints` with `list`,
  `:reconcile` (GET), `:rollUp` and `:dailyRollUp` (POST). Filters use AIP-160 with only `>=`,
  `<` and `AND`, and snake_case type names. 64-bit integers are JSON strings
  ([reference](https://developers.google.com/health/reference/rest),
  [filters](https://developers.google.com/health/filters)).
- **Scopes** are three read-only scopes, all *Restricted*:
  `googlehealth.activity_and_fitness.readonly` (steps, calories),
  `googlehealth.health_metrics_and_measurements.readonly` (heart rate, resting HR, HRV, SpO2),
  `googlehealth.sleep.readonly`. Users may grant a subset. An app in *Testing* status is
  capped at 100 test users and its refresh tokens **expire after 7 days**
  ([scopes](https://developers.google.com/health/scopes),
  [setup](https://developers.google.com/health/setup)).
- **Which endpoint gives what** ([data types](https://developers.google.com/health/data-types),
  [aggregation](https://developers.google.com/health/aggregate-data)):

  | Pulse field | Google data type | Endpoint | Per-call range |
  | :--- | :--- | :--- | :--- |
  | daily steps | `steps` | `dailyRollUp` → `steps.countSum` | 90 days |
  | daily calories | `total-calories` | `dailyRollUp` → `totalCalories.kcalSum` | **14 days** |
  | resting HR | `daily-resting-heart-rate` | `reconcile`, filter `….date` → `beatsPerMinute` | none documented |
  | HRV (RMSSD) | `daily-heart-rate-variability` | `reconcile` → `averageHeartRateVariabilityMilliseconds` | none documented |
  | SpO2 average | `daily-oxygen-saturation` | `reconcile` → `averagePercentage` | none documented |
  | sleep score | — | **not provided by the API** | — |
  | intraday heart rate / HRV / SpO2 | `heart-rate`, `heart-rate-variability`, `oxygen-saturation` | `reconcile`, filter `….sample_time.physical_time` | none documented |
  | intraday steps | `steps` | `reconcile`, filter `steps.interval.start_time` | none documented |
  | intraday calories | `total-calories` | `rollUp`, `windowSize: "60s"` (cannot be listed) | **14 days** |
  | sleep sessions | `sleep` | `reconcile`, filter `sleep.interval.civil_end_time` only | none documented |

  Fitbit Air is listed as a compatible device for every type above.
- **Paging:** `list`/`reconcile`/`rollUp` page up to 10,000 items (sleep: 25) with
  `pageToken`/`nextPageToken`; `dailyRollUp` pages up to 90 days (14 for the 14-day types).
- **Limits and errors:** 300 requests/minute per user. Errors are `google.rpc.Status` JSON.
  `401` means the access token is invalid or expired; `403` means a valid token lacks a scope
  or permission (refreshing does not help); `429`, `503`, `504` should be retried with backoff.
  A user who never opened the Google Health app gets `400 FAILED_PRECONDITION` with reason
  `ACCOUNT_NOT_LINKED`. The token endpoint answers `invalid_grant` for revoked or expired
  refresh tokens ([rate limits](https://developers.google.com/health/rate-limits),
  [troubleshooting](https://developers.google.com/health/troubleshooting),
  [OAuth expiration](https://developers.google.com/identity/protocols/oauth2#expiration)).
- **OAuth for installed apps:** Google recommends a *Desktop app* client with a loopback redirect
  (`http://127.0.0.1:<any port>`) and PKCE `S256`; desktop clients always receive a refresh
  token ([native apps](https://developers.google.com/identity/protocols/oauth2/native-app)).

## Decision

### 1. Package `provider/googlehealth`

`googlehealth.Provider` implements `domain.HealthProvider` with plain `net/http` and
`encoding/json` (no Google SDK: the surface is small and the generated client would leak vendor
types further). It owns its `http.Client` (30 s timeout by default), passes `ctx` to every
request, and keeps every JSON type unexported. Endpoints are configurable only so tests can
point the adapter at an `httptest` server.

- **Reconcile over list.** Every list-style read uses `:reconcile`, which Google recommends for
  sleep and which de-duplicates overlapping intervals from several sources and re-bucketed
  syncs, so the natural-key upserts see one value per instant.
- **Range limits are the adapter's job.** Rollups of `total-calories` are split into ≤14-day
  calls and `steps` into ≤90-day calls inside the adapter, so `sync.Config` keeps its defaults
  (`ChunkDays` 31, `IntradayChunkDays` 7). Pagination follows `nextPageToken`, stops if a token
  repeats, and caps the number of pages per call.
- **Daily summaries** merge five reads per chunk (steps and calories rollups, and the three
  `daily-*` types) by civil date. A day appears only if at least one metric has a value.
  `SleepScore` stays nil: the API has no sleep score. Values outside the schema's `CHECK`
  ranges (e.g. resting HR outside 20–250) are dropped, not written, so one bad upstream value
  cannot fail a whole unit.
- **Days and time zones.** Rollups and `daily-*` types are keyed by Google's *civil* date (the
  user's local date using the UTC offset recorded on the device). The adapter requests civil
  ranges `[r.Start, r.End+1)` and keeps those dates. They match `conn.Location` except while the
  user travels away from `users.timezone`; Google's day is what the device showed, so it wins.
  Sleep sessions carry instants, so their `SleepDate` is the wake-up date in `conn.Location`:
  the adapter widens the `civil_end_time` filter by one day on each side and keeps only sessions
  whose local wake-up date is in `r`. Intraday reads use the physical `TimeRange` the worker
  computed from local midnights (ADR-0003 §2), so 23 h and 25 h DST days are exact.
- **Intraday values:** heart rate `beatsPerMinute`, HRV
  `rootMeanSquareOfSuccessiveDifferencesMilliseconds`, SpO2 `percentage` (sparse by design),
  each at `sampleTime.physicalTime`; steps `count` at `interval.startTime`, where a missing
  `count` on a returned interval means a worn-but-idle minute and is stored as 0; calories
  `kcalSum` per 60 s rollup window, where a window without a value (unworn) is skipped.
  Points outside the requested range are dropped.
- **Sleep:** `ProviderSessionID` is the last segment of `dataPointName` (stable across
  `users/me` and `users/{id}` forms). `IsMainSleep` is `metadata.mainSleep`. Efficiency is not
  a field; it is computed with Google's documented formula,
  `round(minutesAsleep / minutesInSleepPeriod × 100)`, capped at 100. Stages `DEEP`, `REM`,
  `LIGHT`, `AWAKE` of `STAGES` sessions map to the four Pulse stages; unknown segment types,
  empty segments and a second segment with the same start are dropped. *Classic* sessions
  (`ASLEEP`, `RESTLESS`, `AWAKE`) get **no** stages: their asleep time has no faithful Pulse
  stage, and keeping only the `AWAKE` segments would read as a night spent awake. The session
  interval and efficiency are kept. Unprocessed sessions (`processed: false`) are stored as-is;
  the next sync's overlap replaces their stages.
- **Error classification** (`errors.go`): `429`, `500`, `502`, `503`, `504`, timeouts and
  network errors wrap `domain.ErrTransient`; `401` wraps `domain.ErrUnauthorized` (the worker
  then refreshes once before giving up, §2); anything else (including `403` missing scope and
  `ACCOUNT_NOT_LINKED`) is a plain error that fails only that unit. Error text carries the HTTP
  status, the RPC status, the `ErrorInfo` reason and the message capped at 200 bytes, never a
  raw body, a request header, a token or a decoded value (JSON errors report only the field
  path). A cancelled `ctx` is returned as-is so the worker does not retry it. The default HTTP
  client does not follow redirects, so a redirected token request cannot re-send the client
  secret and refresh token elsewhere.
- **`RefreshAuth`** posts `grant_type=refresh_token` to `oauth2.googleapis.com/token`. If Google
  rotates the refresh token, the new one is kept; otherwise the old one is carried over.
  `invalid_grant` wraps `domain.ErrUnauthorized` (the user must re-link); `429`/`5xx` are
  transient; anything else (e.g. `invalid_client`, a configuration mistake) is a plain error so
  a misconfigured deployment does not disable every connection. Only the token endpoint's
  `error` and `error_description` fields reach error text.

### 2. Refreshing during long runs

ADR-0003 refreshed credentials once, at the start of a run. Google access tokens live one
hour and a large backfill can take longer, which would end in a `401` and a disabled
connection. Two changes in `sync`:

- The worker checks expiry before **every unit** and refreshes when the token expires within
  `RefreshBefore`; units read the connection's current credentials when they run.
- If a unit still fails with `ErrUnauthorized` while the credentials have an expiry (a long,
  retried unit that crossed it; clock skew; early invalidation), the worker forces one refresh
  and re-runs that unit. A second `401` fails the unit and leaves the connection eligible for
  scheduled sync. Only a rejected refresh (`invalid_grant`) sets the connection to `error`.
  Providers without expiring credentials (mock) keep ADR-0003's behaviour.

### 3. Linking an account: operator CLI with a loopback redirect

The API has no user authentication yet, so a web callback would have to trust a `user_id`
from the request, letting any caller attach any Google account to any user (the reason
ADR-0003 §5 deferred the HTTP sync trigger). Linking is therefore an operator command:

```
pulse-worker connect-google -user <USER_ID>
```

1. It checks the user exists, then listens on `127.0.0.1:<random port>` for a single request,
   for at most 5 minutes.
2. It prints Google's consent URL with the three read-only scopes, PKCE (`S256`), a random
   `state`, `access_type=offline` and `prompt=consent` (so a re-link always returns a refresh
   token).
3. The browser is redirected to the loopback address. Requests with the wrong `state`
   (compared in constant time) are refused and ignored; the first valid one ends the wait, and
   the page only says to return to the terminal. The command exchanges the code with the PKCE
   verifier (on the caller's context, not the 5-minute wait), and rejects the grant unless **all three
   scopes** were granted: with partial consent every run would end `partial`, and ADR-0003
   never advances the cursor of a partial run.
4. It calls `GET /v4/users/me/identity` to verify the account and obtain Google's `healthUserId`.
5. It stores the verified ID in `provider_connections.provider_account_id` with the encrypted
   tokens (§4) and marks the connection active and due now. Re-linking requires a matching
   account ID and no running sync. It keeps the sync cursor. The transaction locks the connection
   before checking for a running sync, using the same lock as the worker's claim and finish.

This needs a **Desktop app** OAuth client. Its ID and secret come from
`GOOGLE_OAUTH_CLIENT_ID` / `GOOGLE_OAUTH_CLIENT_SECRET` in a local `.env`, never from
the repository. Phase 2 adds the web callback for authenticated users; it reuses the code
exchange, scope check and vault, and only changes where the user ID comes from.

> **Amended by [ADR-0005](0005-api-auth-and-read-model.md) §4 (proposed):** web linking needs a separate
> *Web application* OAuth client; because refresh tokens are bound to their issuing client, each
> connection records `oauth_client` (`desktop` | `web`) and `RefreshAuth` uses the matching one.

### 4. Token encryption (completes ADR-0002 §3)

- `internal/tokencrypt.Keyring`: AES-256-GCM with a random 96-bit nonce per value, stored as
  `nonce‖ciphertext‖tag` in `BYTEA`. The current key is `TOKEN_ENCRYPTION_KEY` (base64 of
  32 bytes), its version is `TOKEN_ENCRYPTION_KEY_VERSION` (default 1) and is stored in
  `token_key_version`. `TOKEN_ENCRYPTION_PREVIOUS_KEYS` (`version:base64,…`) keeps retired keys
  readable during a rotation. Errors never contain key material.
- **Associated data** binds each value to
  `pulse-pipeline/provider_connections/{connection id}/{user id}/{column}`,
  so ciphertext copied to another user's row, or swapped between the access and refresh
  columns, fails authentication instead of decrypting.
- **Rotation:** deploy a new key with a higher version and the old one in
  `TOKEN_ENCRYPTION_PREVIOUS_KEYS`. Every refresh (at least nightly, since access tokens last an
  hour) re-seals both tokens with the current key. Retire the old key once
  `SELECT token_key_version, count(*) FROM provider_connections GROUP BY 1` shows no rows on it.
- `store` reads and writes token ciphertext by `user_id`. `LinkConnection` stores the connection
  and tokens in one transaction. A refresh writes tokens only while its run still owns the
  connection. `internal/credentials.Vault` seals and opens tokens and implements
  `sync.CredentialStore`. Connections without tokens (mock) load as empty credentials, so one
  vault serves every provider.
- The worker registers the Google provider only when `GOOGLE_OAUTH_CLIENT_ID` and
  `GOOGLE_OAUTH_CLIENT_SECRET` are set, and then requires `TOKEN_ENCRYPTION_KEY`.

## Consequences

- **Positive:** Real Fitbit Air data flows through the existing worker.
  Vendor JSON stays inside one package. Tokens at rest are useless without the key and cannot be
  moved between rows. Re-linking a broken connection is one command and keeps history.
- **Trade-offs:**
  - While the Google Cloud app stays in *Testing*, refresh tokens die after 7 days; the next run
    gets `invalid_grant`, the connection goes to `error`, and the operator must re-run
    `connect-google` weekly until the app is published and verified (Restricted scopes need
    Google's review and, above 100 users, an annual security assessment).
  - `sleep_score` stays empty for Google data. Classic-sleep nights have no light/deep/REM
    stages.
  - Daily values follow the device's civil day, which can differ from `users.timezone` while
    travelling.
  - Linking needs a terminal with a browser on the same machine; in a cluster the operator runs
    it locally against a port-forwarded database.
  - Raw intraday heart rate is high-volume (up to one sample per second); a 7-day chunk can be
    a dozen 10,000-point pages. Per-minute rollups are a local change if storage grows too fast.
  - Existing connections without a stored account ID can sync with their current grant.
    A new grant requires a separate Pulse user when the original account is unknown.
  - A transient error late in a paged fetch retries the whole unit from page 1, spending part of
    the 300 requests/minute per-user quota again.
- **Follow-ups** include the Phase 2 web OAuth callback, per-page retries inside the adapter,
  a scope-aware sync that fetches only the granted data types, and Phase 4 metrics for upstream
  `429`s and page counts.
