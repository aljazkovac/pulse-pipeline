-- Batch upsert via unnest. Callers must de-duplicate recorded_at within one
-- batch (ON CONFLICT cannot touch the same row twice in one statement).
-- Arrays must have equal length (Store enforces it; SELECT-list unnest zips them).
-- Unchanged values are skipped so re-syncs do not rewrite identical rows.
-- name: UpsertIntradayMetrics :execrows
INSERT INTO intraday_metrics (user_id, provider, metric_type, recorded_at, value)
SELECT @user_id::uuid, @provider::varchar, @metric_type::varchar,
       unnest(@recorded_at::timestamptz[]), unnest(@value::float8[])
ON CONFLICT (user_id, provider, metric_type, recorded_at) DO UPDATE
SET value = EXCLUDED.value
WHERE intraday_metrics.value IS DISTINCT FROM EXCLUDED.value;

-- name: ListIntradayMetrics :many
SELECT recorded_at, value
FROM intraday_metrics
WHERE user_id = @user_id
  AND provider = @provider
  AND metric_type = @metric_type
  AND recorded_at >= @start_at
  AND recorded_at < @end_at
ORDER BY recorded_at;
