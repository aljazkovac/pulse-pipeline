-- name: UpsertSleepSession :one
INSERT INTO sleep_sessions (
    user_id, provider, provider_session_id, sleep_date, start_at, end_at,
    is_main_sleep, efficiency_pct
) VALUES (
    @user_id, @provider, @provider_session_id, @sleep_date, @start_at, @end_at,
    @is_main_sleep, @efficiency_pct
)
ON CONFLICT (user_id, provider, provider_session_id) DO UPDATE SET
    sleep_date     = EXCLUDED.sleep_date,
    start_at       = EXCLUDED.start_at,
    end_at         = EXCLUDED.end_at,
    is_main_sleep  = EXCLUDED.is_main_sleep,
    efficiency_pct = EXCLUDED.efficiency_pct,
    updated_at     = now()
RETURNING id;

-- name: DeleteSleepStages :exec
DELETE FROM sleep_stages
WHERE session_id = @session_id AND user_id = @user_id;

-- name: InsertSleepStages :execrows
INSERT INTO sleep_stages (session_id, user_id, stage, start_at, end_at)
SELECT @session_id::uuid, @user_id::uuid,
       unnest(@stage::varchar[]), unnest(@start_at::timestamptz[]), unnest(@end_at::timestamptz[]);

-- name: ListSleepSessions :many
SELECT id, provider, provider_session_id, sleep_date, start_at, end_at,
       is_main_sleep, efficiency_pct
FROM sleep_sessions
WHERE user_id = @user_id
  AND provider = @provider
  AND sleep_date BETWEEN @start_date AND @end_date
ORDER BY sleep_date DESC, start_at DESC;

-- name: ListSleepStagesForSessions :many
SELECT session_id, stage, start_at, end_at
FROM sleep_stages
WHERE user_id = @user_id
  AND session_id = ANY(@session_ids::uuid[])
ORDER BY session_id, start_at;
