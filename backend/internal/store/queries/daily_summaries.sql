-- name: UpsertDailySummary :exec
INSERT INTO daily_summaries (
    user_id, provider, summary_date, steps, resting_heart_rate,
    hrv_rmssd_ms, sleep_score, calories_kcal, spo2_avg_pct
) VALUES (
    @user_id, @provider, @summary_date, @steps, @resting_heart_rate,
    @hrv_rmssd_ms, @sleep_score, @calories_kcal, @spo2_avg_pct
)
ON CONFLICT (user_id, provider, summary_date) DO UPDATE SET
    steps              = EXCLUDED.steps,
    resting_heart_rate = EXCLUDED.resting_heart_rate,
    hrv_rmssd_ms       = EXCLUDED.hrv_rmssd_ms,
    sleep_score        = EXCLUDED.sleep_score,
    calories_kcal      = EXCLUDED.calories_kcal,
    spo2_avg_pct       = EXCLUDED.spo2_avg_pct,
    updated_at         = now();

-- name: ListDailySummaries :many
SELECT provider, summary_date, steps, resting_heart_rate, hrv_rmssd_ms,
       sleep_score, calories_kcal, spo2_avg_pct
FROM daily_summaries
WHERE user_id = @user_id
  AND provider = @provider
  AND summary_date BETWEEN @start_date AND @end_date
ORDER BY summary_date DESC;
