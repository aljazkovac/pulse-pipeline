# ADR 0003: Sync Worker — Scheduling, Idempotent Ingestion, Retries & Run Auditing

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

ADR-0002 fixed the schema, the natural-key upserts and the `HealthProvider` contract. Phase 1
now needs the process that moves data from providers into PostgreSQL every night, and on demand.
Its constraints:

- **Convergence:** retries, overlapping runs and backfills must leave the same rows as one clean
  run (ADR-0002 §1 upserts make this possible; the worker must not break it).
- **Late data:** wearables upload hours late and providers re-score recent nights, so a day is not
  final when it ends.
- **Partial failure:** one failing endpoint (rate limit, missing scope) should not throw away what
  the others returned.
- **No double work:** the scheduler, a manual sync and (later) several worker replicas must never
  ingest the same connection at the same time.
- **Crash safety:** a worker killed mid-run must not block that connection forever.
- **Isolation:** the scheduler is the one system-context reader (`ListConnectionsDueForSync`,
  ADR-0002 §4); everything after it is scoped by the connection's `user_id`.

## Decision

### 1. Package and entry points

`backend/internal/sync` holds the orchestrator (`Syncer`). It depends on a narrow `Store`
interface (satisfied by `*store.Store`), a set of `domain.HealthProvider`s keyed by `Name()`, and a
`CredentialStore` for decrypted OAuth tokens (`NoCredentials` until the encrypted store lands with
`GoogleHealthProvider`, ADR-0002 §3). `cmd/worker` is thin wiring:

| Command | Trigger | Behaviour |
| :--- | :--- | :--- |
| `pulse-worker [run]` | `scheduled` | Polls `ListConnectionsDueForSync` every `PULSE_SYNC_POLL_INTERVAL` (default 1m) and syncs each due connection sequentially. |
| `pulse-worker sync -user -provider` | `manual` | One incremental sync now. |
| `pulse-worker sync … -from -to` | `backfill` | Ingests an explicit date range. |
| `pulse-worker seed-dev` | — | `ENV=development` only: creates a local user with a `mock` connection. |

Every command applies pending migrations at startup (golang-migrate takes an advisory lock, so
concurrent replicas are safe). The mock provider is registered only with
`PULSE_ENABLE_MOCK_PROVIDER=true`.

### 2. What a run covers

Each connection's `sync_cursor` JSON carries `synced_through` (the last local day a scheduled or
manual run fully ingested); other keys are reserved for provider adapters and preserved.

- **First sync:** the last `InitialBackfillDays` (30) local days, ending today.
- **Incremental:** from `synced_through − OverlapDays` (3) through today. The overlap re-fetches
  days providers revise; today is partial and is re-fetched by the next run's overlap.
- **Backfill:** the requested range, capped at today. Backfills touch no connection state
  (cursor, schedule, `last_synced_at`): values read before the run may be stale once it ends,
  and filling old history must never rewind or skip the nightly window.
- "Today" is the user's local date (`users.timezone`); intraday requests span local midnights, so
  23h/25h DST days are covered exactly.

### 3. Units, retries and outcome

A run is split into **units**, each one provider request plus its upsert: daily summaries and
sleep in chunks of up to `ChunkDays` (31), each intraday metric in chunks of `IntradayChunkDays` (7).
Providers classify failures by wrapping two sentinels in `domain`:

- `domain.ErrTransient` (429, 5xx, timeouts) is retried per unit up to `Retry.Attempts` (4) with
  exponential backoff and equal jitter (1s, 2s, 4s…, capped at 30s).
- `domain.ErrUnauthorized` stops the run immediately and sets the connection to `error`, which
  takes it out of scheduling until the user reconnects.
- Any other error fails that unit only.

Run status: **succeeded** (no unit failed), **partial** (some failed), **failed** (none succeeded).
Only a succeeded run advances the cursor and schedules the next run at the user's local
`SyncHour` (04:00). A partial or failed scheduled run keeps the cursor and is retried after
`RetryAfter` (1h); because ingestion is idempotent, re-covering the range costs only time.
Rows whose `Provider` differs from the adapter's name are rejected, so a buggy adapter cannot write
under another provider's key. The stored error summary is capped at 2,000 bytes (cut on a UTF-8
boundary) and has the run's token values scrubbed, a backstop for adapters that echo upstream
error bodies; adapters remain responsible for keeping tokens out of their errors.

### 4. Concurrency guard and crash recovery

Migration `000002` adds a partial unique index `sync_runs (user_id, provider) WHERE status =
'running'`. `StartSyncRun` inserts with `ON CONFLICT … DO NOTHING` and returns
`store.ErrSyncInProgress` when another run holds the slot; the scheduler skips that connection,
without rescheduling it, until its next poll.

A running run records a **heartbeat** (`sync_runs.heartbeat_at`) after every unit. Before starting,
the worker fails runs of the same connection whose last heartbeat (or start) is older than
`StaleRunAfter` (2h) — measured on the database clock, which set both — so a crashed worker
releases its slot while a long backfill that keeps making progress is never mistaken for one. If a
run's heartbeat finds it was abandoned anyway, it stops at once and writes no connection state,
since another run may own the connection. Run bookkeeping uses a context detached from
cancellation, so a worker stopping on SIGTERM still closes its audit row as `failed`.

### 5. Manual trigger over HTTP is deferred

The roadmap pairs the worker with a "manual trigger endpoint". The API has no authentication yet,
and an endpoint that accepts a `user_id` without it would let any caller trigger syncs for any user.
The manual and backfill paths ship now as the `pulse-worker sync` command (operator access only).
The HTTP endpoint will call `Syncer.SyncConnection` with the authenticated user's ID and lands with
the Phase 2 Sync & Connections view, once the API authenticates requests.

## Consequences

- **Positive:** re-running anything converges; one bad endpoint degrades a run instead of failing
  it; late provider corrections are picked up within `OverlapDays`; two syncs of one connection
  cannot interleave, even across replicas; a crash is self-healing after `StaleRunAfter`; every run
  is auditable in `sync_runs` with a bounded, token-scrubbed error summary.
- **Trade-offs:** one extra `UPDATE` per unit for the heartbeat. Connections are synced sequentially (ample at single-user scale; a bounded worker
  pool is a local change later). A connection with a permanently failing unit retries hourly
  without advancing its cursor until the cause is fixed — visible as repeated `partial` runs. The
  overlap re-writes recent daily and sleep rows every night (intraday upserts skip unchanged
  values). No HTTP manual trigger until Phase 2 auth.
- **Follow-ups:** `GoogleHealthProvider` supplies the encrypted `CredentialStore` and maps HTTP
  errors onto `ErrTransient` / `ErrUnauthorized`; Phase 4 adds metrics for run duration, status and
  rows ingested.
