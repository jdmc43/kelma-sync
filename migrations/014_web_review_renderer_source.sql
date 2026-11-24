-- Freeze the source needed by the versioned Vue card renderer with each
-- durable browser presentation. Existing presentations retain field-pair mode.

ALTER TABLE web_review_presentations
    DROP CONSTRAINT web_review_presentations_fields_check,
    ADD CONSTRAINT web_review_presentations_fields_check
        CHECK (jsonb_typeof(fields) = 'array' AND jsonb_array_length(fields) >= 1),
    ADD COLUMN renderer TEXT NOT NULL DEFAULT 'field_pair_html_v1',
    ADD COLUMN tags TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN notetype_definition JSONB NOT NULL DEFAULT '{}'
        CHECK (jsonb_typeof(notetype_definition) = 'object'),
    ADD COLUMN notetype_checksum TEXT NOT NULL DEFAULT '';
