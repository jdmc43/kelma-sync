-- Exactly-once receipts for scheduler-independent review client intents. These
-- rows contain no card content and do not replace canonical synchronized state.
CREATE TABLE review_client_operations (
    user_id         TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    operation_id    UUID        NOT NULL,
    presentation_id UUID        NOT NULL,
    source_card_id  BIGINT      NOT NULL,
    note_guid       TEXT        NOT NULL,
    card_ord        INTEGER     NOT NULL CHECK (card_ord >= 0),
    kind            TEXT        NOT NULL CHECK (kind IN (
        'set_card_flag', 'set_note_marked', 'suspend_card', 'suspend_note',
        'set_card_due_date', 'reset_card', 'create_note_copy', 'edit_note',
        'delete_note'
    )),
    request_hash    TEXT        NOT NULL CHECK (length(request_hash) = 64),
    response        JSONB       NOT NULL CHECK (jsonb_typeof(response) = 'object'),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, operation_id)
);

CREATE INDEX idx_review_client_operations_created
    ON review_client_operations (user_id, created_at DESC);
