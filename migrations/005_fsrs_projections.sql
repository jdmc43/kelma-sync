-- 005_fsrs_projections.sql
--
-- Immutable review history is authoritative. Account scheduler profiles and
-- per-logical-card server schedules are versioned derived state.

CREATE TABLE scheduler_profiles (
    id                         UUID             PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                    TEXT             NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    version                    BIGINT           NOT NULL CHECK (version > 0),
    algorithm                  TEXT             NOT NULL DEFAULT 'fsrs-6',
    scheduler_implementation   TEXT             NOT NULL DEFAULT 'kelma-go-fsrs',
    scheduler_version          TEXT             NOT NULL,
    idempotency_key            TEXT             NOT NULL,
    parameters                 JSONB            NOT NULL CHECK (jsonb_typeof(parameters) = 'array' AND jsonb_array_length(parameters) = 21),
    parameter_source           TEXT             NOT NULL CHECK (parameter_source IN ('default','client_optimized','manual')),
    desired_retention          DOUBLE PRECISION NOT NULL CHECK (desired_retention >= 0.70 AND desired_retention <= 0.99),
    retention_source           TEXT             NOT NULL CHECK (retention_source IN ('default','client_optimized','manual')),
    maximum_interval           INT              NOT NULL CHECK (maximum_interval BETWEEN 1 AND 36500),
    learning_steps_seconds     INT[]            NOT NULL,
    relearning_steps_seconds   INT[]            NOT NULL,
    enable_short_term          BOOLEAN          NOT NULL DEFAULT TRUE,
    enable_fuzzing             BOOLEAN          NOT NULL DEFAULT FALSE,
    config_hash                TEXT             NOT NULL,
    optimizer                  TEXT,
    optimizer_version          TEXT,
    optimization_history_hash  TEXT,
    optimization_through_review_id BIGINT,
    quality_metrics            JSONB            NOT NULL DEFAULT '{}'
        CHECK (jsonb_typeof(quality_metrics) = 'object'),
    created_at                 TIMESTAMPTZ      NOT NULL DEFAULT now(),
    last_client_id             UUID             REFERENCES clients(id),
    UNIQUE (user_id, version),
    UNIQUE (user_id, idempotency_key),
    CHECK (enable_short_term AND NOT enable_fuzzing),
    CHECK (
        (parameter_source <> 'client_optimized' AND retention_source <> 'client_optimized')
        OR (optimizer IS NOT NULL AND optimizer_version IS NOT NULL
            AND optimization_history_hash IS NOT NULL
            AND optimization_through_review_id > 0)
    )
);

CREATE INDEX idx_scheduler_profiles_user_version
    ON scheduler_profiles (user_id, version DESC);

CREATE TABLE scheduler_projections (
    user_id                    TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    note_guid                  TEXT        NOT NULL,
    card_ord                   INT         NOT NULL,
    desired_generation        BIGINT      NOT NULL DEFAULT 1,
    desired_history_hash      TEXT        NOT NULL,
    desired_through_review_id BIGINT      NOT NULL,
    desired_profile_version   BIGINT      NOT NULL DEFAULT 0,
    applied_history_hash      TEXT,
    applied_through_review_id BIGINT,
    applied_profile_version   BIGINT,
    status                     TEXT        NOT NULL CHECK (status IN ('pending','running','succeeded','failed','superseded')),
    job_id                     UUID        NOT NULL DEFAULT gen_random_uuid(),
    attempt_count              INT         NOT NULL DEFAULT 0,
    last_error_code            TEXT,
    last_attempt_at            TIMESTAMPTZ,
    next_retry_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, note_guid, card_ord)
);

CREATE INDEX idx_scheduler_projections_work
    ON scheduler_projections (status, next_retry_at, updated_at)
    WHERE status IN ('pending','failed');
CREATE INDEX idx_scheduler_projections_user_status
    ON scheduler_projections (user_id, status);

-- Existing histories become pending work. The history hash is deliberately
-- based only on immutable review identity/checksum pairs.
WITH histories AS (
    SELECT r.user_id, r.note_guid, r.card_ord,
           MAX(r.review_id) AS through_review_id,
           encode(digest(string_agg(r.review_id::text || ':' || r.checksum, E'\n' ORDER BY r.review_id), 'sha256'), 'hex') AS history_hash
    FROM reviews r
    WHERE r.note_guid <> '' AND r.ease BETWEEN 1 AND 4
    GROUP BY r.user_id, r.note_guid, r.card_ord
)
INSERT INTO scheduler_projections (
    user_id, note_guid, card_ord, desired_history_hash,
    desired_through_review_id, desired_profile_version, status
)
SELECT h.user_id, h.note_guid, h.card_ord, h.history_hash,
       h.through_review_id,
       COALESCE((SELECT MAX(p.version) FROM scheduler_profiles p WHERE p.user_id=h.user_id), 0),
       'pending'
FROM histories h
ON CONFLICT (user_id, note_guid, card_ord) DO NOTHING;
