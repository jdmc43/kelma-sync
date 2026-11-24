-- Durable, account-scoped web review sessions and idempotency receipts.
-- Canonical review facts remain in reviews; these rows never replace history.

CREATE TABLE web_review_sessions (
    id          UUID        PRIMARY KEY,
    user_id     TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    client_id   UUID        NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
    deck_names  TEXT[]      NOT NULL CHECK (cardinality(deck_names) BETWEEN 1 AND 50),
    allow_new   BOOLEAN     NOT NULL DEFAULT FALSE,
    status      TEXT        NOT NULL CHECK (status IN ('active','closed','expired')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    CHECK (expires_at > created_at),
    UNIQUE (user_id, id)
);

CREATE UNIQUE INDEX idx_web_review_one_active_session
    ON web_review_sessions (user_id) WHERE status='active';
CREATE INDEX idx_web_review_sessions_account
    ON web_review_sessions (user_id, created_at DESC);

CREATE TABLE web_review_presentations (
    id                       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                  TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id               UUID        NOT NULL,
    source_card_id           BIGINT      NOT NULL,
    note_guid                TEXT        NOT NULL,
    card_ord                 INT         NOT NULL CHECK (card_ord IN (0,1)),
    deck_name                TEXT        NOT NULL,
    fields                   JSONB       NOT NULL CHECK (jsonb_typeof(fields)='array' AND jsonb_array_length(fields)>=2),
    candidate_hash           TEXT        NOT NULL CHECK (length(candidate_hash)=64),
    history_hash             TEXT        NOT NULL CHECK (length(history_hash)=64),
    through_review_id        BIGINT      NOT NULL CHECK (through_review_id>0),
    profile_version          BIGINT      NOT NULL CHECK (profile_version>=0),
    due_at_millis            BIGINT      NOT NULL CHECK (due_at_millis>0),
    status                   TEXT        NOT NULL CHECK (status IN ('active','answered','expired')),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at               TIMESTAMPTZ NOT NULL,
    answered_at              TIMESTAMPTZ,
    CHECK (expires_at > created_at),
    CHECK ((status='answered') = (answered_at IS NOT NULL)),
    UNIQUE (user_id, id),
    UNIQUE (user_id, session_id, id),
    FOREIGN KEY (user_id, session_id)
        REFERENCES web_review_sessions(user_id, id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX idx_web_review_one_active_presentation
    ON web_review_presentations (user_id, session_id) WHERE status='active';
CREATE INDEX idx_web_review_presentations_account_card
    ON web_review_presentations (user_id, note_guid, card_ord, created_at DESC);

CREATE TABLE web_review_attempts (
    user_id        TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    attempt_id     UUID        NOT NULL,
    session_id     UUID        NOT NULL,
    presentation_id UUID       NOT NULL,
    request_hash   TEXT        NOT NULL CHECK (length(request_hash)=64),
    review_id      BIGINT      NOT NULL,
    response       JSONB       NOT NULL CHECK (jsonb_typeof(response)='object'),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, attempt_id),
    UNIQUE (user_id, review_id),
    FOREIGN KEY (user_id, session_id, presentation_id)
        REFERENCES web_review_presentations(user_id, session_id, id)
);

CREATE INDEX idx_web_review_attempts_session
    ON web_review_attempts (user_id, session_id, created_at DESC);
