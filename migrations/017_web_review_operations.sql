-- Exactly-once receipts for bounded non-review actions. These rows record only
-- accepted operations and never replace review history or native sync records.
CREATE TABLE web_review_operations (
    user_id         TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    operation_id    UUID        NOT NULL,
    session_id      UUID        NOT NULL,
    presentation_id UUID        NOT NULL,
    kind             TEXT        NOT NULL CHECK (kind IN (
        'set_card_flag', 'set_note_marked',
        'bury_card', 'bury_note', 'suspend_card', 'suspend_note'
    )),
    request_hash    TEXT        NOT NULL CHECK (length(request_hash) = 64),
    response        JSONB       NOT NULL CHECK (jsonb_typeof(response) = 'object'),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, operation_id),
    FOREIGN KEY (user_id, session_id, presentation_id)
        REFERENCES web_review_presentations(user_id, session_id, id)
);

CREATE INDEX idx_web_review_operations_session
    ON web_review_operations (user_id, session_id, created_at DESC);
