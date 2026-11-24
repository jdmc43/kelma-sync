ALTER TABLE cards
    ADD COLUMN schedule_reset_through_review_id BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN schedule_reset_modified_at TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01T00:00:00Z',
    ADD COLUMN schedule_reset_client_modified_at TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01T00:00:00Z',
    ADD COLUMN schedule_reset_client_id UUID REFERENCES clients(id);
