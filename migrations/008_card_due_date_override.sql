ALTER TABLE cards
    ADD COLUMN due_date_override_ms BIGINT NOT NULL DEFAULT 0
        CHECK (due_date_override_ms >= 0 AND due_date_override_ms % 86400000 = 0),
    ADD COLUMN due_date_override_modified_at TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01T00:00:00Z',
    ADD COLUMN due_date_override_client_modified_at TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01T00:00:00Z',
    ADD COLUMN due_date_override_client_id UUID REFERENCES clients(id);
