-- 025_review_retraction.sql
--
-- Undo is an append-only retraction intent. The original review remains an
-- immutable sync fact, while queue, scheduler, and statistics projections omit
-- reviews with an accepted retraction.

CREATE TABLE review_retractions (
    user_id            TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    review_id          BIGINT      NOT NULL,
    intent_id          UUID        NOT NULL,
    client_modified_at TIMESTAMPTZ NOT NULL,
    modified_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_client_id     UUID        REFERENCES clients(id),
    PRIMARY KEY (user_id, review_id),
    UNIQUE (user_id, intent_id),
    FOREIGN KEY (user_id, review_id)
        REFERENCES reviews(user_id, review_id) ON DELETE CASCADE
);

CREATE INDEX idx_review_retractions_user_modified
    ON review_retractions (user_id, modified_at);

-- Web Undo uses the existing exactly-once operation receipt/outbox protocol.
ALTER TABLE web_review_operations
    DROP CONSTRAINT web_review_operations_kind_check;
ALTER TABLE web_review_operations
    ADD CONSTRAINT web_review_operations_kind_check CHECK (kind IN (
        'set_card_flag','set_note_marked','suspend_card','suspend_note',
        'bury_card','bury_note','set_card_due_date','reset_card',
        'create_note_copy','edit_note','delete_note','undo_review'
    ));
