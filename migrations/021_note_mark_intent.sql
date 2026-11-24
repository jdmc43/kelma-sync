-- Synchronize Mark/Unmark independently from whole-note content writes.
ALTER TABLE notes
    ADD COLUMN mark_intent_id UUID,
    ADD COLUMN mark_modified_at TIMESTAMPTZ,
    ADD COLUMN mark_client_modified_at TIMESTAMPTZ,
    ADD COLUMN mark_client_id UUID REFERENCES clients(id),
    ADD CONSTRAINT notes_mark_intent_complete CHECK (
        (mark_intent_id IS NULL AND mark_modified_at IS NULL
            AND mark_client_modified_at IS NULL AND mark_client_id IS NULL)
        OR
        (mark_intent_id IS NOT NULL AND mark_modified_at IS NOT NULL
            AND mark_client_modified_at IS NOT NULL AND mark_client_id IS NOT NULL)
    ) NOT VALID;

ALTER TABLE notes VALIDATE CONSTRAINT notes_mark_intent_complete;
