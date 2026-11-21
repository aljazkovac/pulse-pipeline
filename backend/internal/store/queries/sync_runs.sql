-- name: CreateSyncRun :one
INSERT INTO sync_runs (user_id, provider, trigger, range_start, range_end)
VALUES (@user_id, @provider, @trigger, @range_start, @range_end)
RETURNING id, user_id, provider, trigger, status, range_start, range_end,
          records_ingested, error_message, started_at, finished_at;

-- name: FinishSyncRun :one
UPDATE sync_runs
SET status           = @status,
    records_ingested = @records_ingested,
    error_message    = @error_message,
    finished_at      = now()
WHERE id = @id AND user_id = @user_id AND status = 'running'
RETURNING id, user_id, provider, trigger, status, range_start, range_end,
          records_ingested, error_message, started_at, finished_at;

-- name: ListSyncRuns :many
SELECT id, user_id, provider, trigger, status, range_start, range_end,
       records_ingested, error_message, started_at, finished_at
FROM sync_runs
WHERE user_id = @user_id AND provider = @provider
ORDER BY started_at DESC
LIMIT @max_rows;
