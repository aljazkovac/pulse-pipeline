-- ADR-0002: canonical multi-user, multi-provider health data schema.
-- All timestamps are TIMESTAMPTZ (UTC); local calendar days are DATE in users.timezone.

CREATE TABLE users (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email       TEXT        NOT NULL,
    timezone    TEXT        NOT NULL DEFAULT 'UTC',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT users_email_not_blank CHECK (btrim(email) <> '')
);

CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email));

CREATE TABLE provider_connections (
    id                        UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                   UUID         NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider                  VARCHAR(32)  NOT NULL,
    status                    VARCHAR(16)  NOT NULL DEFAULT 'active',
    scopes                    TEXT[]       NOT NULL DEFAULT '{}',
    -- AES-256-GCM ciphertext (nonce || sealed); plaintext tokens never stored.
    access_token_ciphertext   BYTEA,
    refresh_token_ciphertext  BYTEA,
    token_key_version         SMALLINT,
    token_expires_at          TIMESTAMPTZ,
    sync_cursor               JSONB        NOT NULL DEFAULT '{}',
    next_sync_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    last_synced_at            TIMESTAMPTZ,
    created_at                TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT provider_connections_user_provider_key UNIQUE (user_id, provider),
    CONSTRAINT provider_connections_status_check
        CHECK (status IN ('active', 'revoked', 'error')),
    CONSTRAINT provider_connections_token_key_version_check
        CHECK ((access_token_ciphertext IS NULL AND refresh_token_ciphertext IS NULL)
               OR token_key_version IS NOT NULL)
);

CREATE TABLE daily_summaries (
    user_id            UUID             NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider           VARCHAR(32)      NOT NULL,
    summary_date       DATE             NOT NULL,
    steps              INTEGER          CHECK (steps >= 0),
    resting_heart_rate SMALLINT         CHECK (resting_heart_rate BETWEEN 20 AND 250),
    hrv_rmssd_ms       DOUBLE PRECISION CHECK (hrv_rmssd_ms >= 0),
    sleep_score        SMALLINT         CHECK (sleep_score BETWEEN 0 AND 100),
    calories_kcal      INTEGER          CHECK (calories_kcal >= 0),
    spo2_avg_pct       DOUBLE PRECISION CHECK (spo2_avg_pct BETWEEN 0 AND 100),
    ingested_at        TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ      NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, provider, summary_date)
);

CREATE TABLE intraday_metrics (
    user_id      UUID             NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider     VARCHAR(32)      NOT NULL,
    metric_type  VARCHAR(32)      NOT NULL,
    recorded_at  TIMESTAMPTZ      NOT NULL,
    value        DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (user_id, provider, metric_type, recorded_at),
    CONSTRAINT intraday_metrics_metric_type_check
        CHECK (metric_type IN ('heart_rate', 'steps', 'hrv_rmssd', 'spo2', 'calories'))
);

CREATE TABLE sleep_sessions (
    id                   UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id              UUID         NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider             VARCHAR(32)  NOT NULL,
    provider_session_id  TEXT         NOT NULL,
    sleep_date           DATE         NOT NULL,
    start_at             TIMESTAMPTZ  NOT NULL,
    end_at               TIMESTAMPTZ  NOT NULL,
    is_main_sleep        BOOLEAN      NOT NULL DEFAULT true,
    efficiency_pct       SMALLINT     CHECK (efficiency_pct BETWEEN 0 AND 100),
    ingested_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT sleep_sessions_natural_key UNIQUE (user_id, provider, provider_session_id),
    -- Target of the sleep_stages composite FK (keeps stages in the same tenant).
    CONSTRAINT sleep_sessions_id_user_key UNIQUE (id, user_id),
    CONSTRAINT sleep_sessions_time_order CHECK (end_at > start_at)
);

CREATE INDEX sleep_sessions_user_provider_date_idx
    ON sleep_sessions (user_id, provider, sleep_date);

CREATE TABLE sleep_stages (
    session_id  UUID         NOT NULL,
    user_id     UUID         NOT NULL,
    stage       VARCHAR(16)  NOT NULL,
    start_at    TIMESTAMPTZ  NOT NULL,
    end_at      TIMESTAMPTZ  NOT NULL,
    PRIMARY KEY (session_id, start_at),
    FOREIGN KEY (session_id, user_id)
        REFERENCES sleep_sessions (id, user_id) ON DELETE CASCADE,
    CONSTRAINT sleep_stages_stage_check CHECK (stage IN ('deep', 'rem', 'light', 'awake')),
    CONSTRAINT sleep_stages_time_order CHECK (end_at > start_at)
);

CREATE TABLE sync_runs (
    id                UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           UUID         NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider          VARCHAR(32)  NOT NULL,
    trigger           VARCHAR(16)  NOT NULL,
    status            VARCHAR(16)  NOT NULL DEFAULT 'running',
    range_start       DATE         NOT NULL,
    range_end         DATE         NOT NULL,
    records_ingested  INTEGER      NOT NULL DEFAULT 0 CHECK (records_ingested >= 0),
    error_message     TEXT,
    started_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    finished_at       TIMESTAMPTZ,
    CONSTRAINT sync_runs_trigger_check CHECK (trigger IN ('scheduled', 'manual', 'backfill')),
    CONSTRAINT sync_runs_status_check
        CHECK (status IN ('running', 'succeeded', 'partial', 'failed')),
    CONSTRAINT sync_runs_range_order CHECK (range_end >= range_start),
    CONSTRAINT sync_runs_finished_consistency
        CHECK ((status = 'running') = (finished_at IS NULL))
);

CREATE INDEX sync_runs_user_provider_started_idx
    ON sync_runs (user_id, provider, started_at DESC);
