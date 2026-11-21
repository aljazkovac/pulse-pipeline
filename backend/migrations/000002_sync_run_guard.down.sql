ALTER TABLE sync_runs DROP COLUMN IF EXISTS heartbeat_at;
DROP INDEX IF EXISTS sync_runs_one_running_idx;
