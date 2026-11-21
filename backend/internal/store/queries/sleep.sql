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
SELECT ss.id, ss.provider, ss.provider_session_id, ss.sleep_date, ss.start_at, ss.end_at,
       ss.is_main_sleep, ss.efficiency_pct, st.stage, st.start_at AS stage_start_at,
       st.end_at AS stage_end_at
FROM sleep_sessions ss
LEFT JOIN sleep_stages st ON st.session_id = ss.id AND st.user_id = ss.user_id
WHERE ss.user_id = @user_id
  AND ss.provider = @provider
  AND ss.sleep_date BETWEEN @start_date AND @end_date
ORDER BY ss.sleep_date DESC, ss.start_at DESC, ss.id, st.start_at;
