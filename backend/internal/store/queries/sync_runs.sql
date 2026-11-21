-- Returns no row when the user already has a running sync for the provider
-- (partial unique index sync_runs_one_running_idx, ADR-0003).
-- name: CreateSyncRun :one
INSERT INTO sync_runs (user_id, provider, trigger, range_start, range_end)
VALUES (@user_id, @provider, @trigger, @range_start, @range_end)
ON CONFLICT (user_id, provider) WHERE status = 'running' DO NOTHING
RETURNING id, user_id, provider, trigger, status, range_start, range_end,
          records_ingested, error_message, started_at, finished_at, heartbeat_at;

-- name: FinishSyncRun :one
UPDATE sync_runs
SET status           = @status,
    records_ingested = @records_ingested,
    error_message    = @error_message,
    finished_at      = now()
WHERE id = @id AND user_id = @user_id AND status = 'running'
RETURNING id, user_id, provider, trigger, status, range_start, range_end,
          records_ingested, error_message, started_at, finished_at, heartbeat_at;

-- name: ListSyncRuns :many
SELECT id, user_id, provider, trigger, status, range_start, range_end,
       records_ingested, error_message, started_at, finished_at, heartbeat_at
FROM sync_runs
WHERE user_id = @user_id AND provider = @provider
ORDER BY started_at DESC
LIMIT @max_rows;

-- Fails runs a crashed worker left 'running', so they stop blocking new syncs.
-- Age is the time since the run's last heartbeat (or its start), measured on
-- the database clock, the same clock that set both.
-- name: AbandonStaleSyncRuns :execrows
UPDATE sync_runs
SET status        = 'failed',
    error_message = 'abandoned: worker stopped before the run finished',
    finished_at   = now()
WHERE user_id = @user_id AND provider = @provider
  AND status = 'running'
  AND coalesce(heartbeat_at, started_at) < now() - make_interval(secs => @older_than_seconds::double precision);

-- Returns 0 rows when the run is no longer running (finished or abandoned).
-- name: TouchSyncRun :execrows
UPDATE sync_runs
SET heartbeat_at = now()
WHERE id = @id AND user_id = @user_id AND status = 'running';
