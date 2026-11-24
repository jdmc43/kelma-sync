CREATE TABLE study_day_policies (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    version BIGINT NOT NULL CHECK (version > 0),
    timezone_id TEXT NOT NULL,
    day_start_hour SMALLINT NOT NULL CHECK (day_start_hour BETWEEN 0 AND 23),
    idempotency_key TEXT NOT NULL,
    modified_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_client_id UUID REFERENCES clients(id),
    PRIMARY KEY (user_id, version),
    UNIQUE (user_id, idempotency_key)
);
