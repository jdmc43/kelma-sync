-- 006_card_study_state.sql
--
-- Durable user intent is independent from derived scheduling projections.

ALTER TABLE cards
    ADD COLUMN study_state TEXT NOT NULL DEFAULT 'active'
        CHECK (study_state IN ('active', 'suspended')),
    ADD COLUMN study_state_modified_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN study_state_client_modified_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN study_state_client_id UUID REFERENCES clients(id);

UPDATE cards
SET study_state_modified_at = '1970-01-01T00:00:00Z',
    study_state_client_modified_at = '1970-01-01T00:00:00Z',
    study_state_client_id = NULL;

UPDATE cards
SET study_state = 'suspended',
    study_state_modified_at = modified_at,
    study_state_client_modified_at = client_modified_at,
    study_state_client_id = last_client_id
WHERE scheduling->>'queue' = '-1';

CREATE INDEX idx_cards_study_state
    ON cards (user_id, study_state)
    WHERE study_state = 'suspended';
