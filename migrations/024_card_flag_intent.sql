ALTER TABLE cards
    ADD COLUMN flag_intent_id UUID,
    ADD COLUMN flag_modified_at TIMESTAMPTZ,
    ADD COLUMN flag_client_modified_at TIMESTAMPTZ,
    ADD COLUMN flag_client_id UUID REFERENCES clients(id),
    ADD CONSTRAINT cards_flag_intent_complete CHECK (
        (flag_intent_id IS NULL AND flag_modified_at IS NULL
            AND flag_client_modified_at IS NULL AND flag_client_id IS NULL)
        OR
        (flag_intent_id IS NOT NULL AND flag_modified_at IS NOT NULL
            AND flag_client_modified_at IS NOT NULL AND flag_client_id IS NOT NULL)
    );

CREATE INDEX cards_flag_client_modified_idx
    ON cards(user_id, flag_client_modified_at)
    WHERE flag_intent_id IS NOT NULL;
