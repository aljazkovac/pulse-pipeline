-- ADR-0003: at most one running sync per user and provider. A manual sync and
-- the nightly scheduler (or two worker replicas) can then never ingest the
-- same connection concurrently; the second INSERT hits this index and backs off.
CREATE UNIQUE INDEX sync_runs_one_running_idx
    ON sync_runs (user_id, provider)
    WHERE status = 'running';

-- Workers bump heartbeat_at after every unit of work. A run is stale (its
-- worker crashed) when its last sign of life is older than the threshold, so
-- long backfills that keep making progress are never mistaken for crashes.
ALTER TABLE sync_runs ADD COLUMN heartbeat_at TIMESTAMPTZ;
