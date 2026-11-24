-- Account mirror protocol metadata. Blob bytes remain in the shared object store.
ALTER TABLE media
    ADD COLUMN IF NOT EXISTS last_client_id UUID REFERENCES clients(id) ON DELETE SET NULL;

ALTER TABLE tombstones
    ADD COLUMN IF NOT EXISTS note_guid TEXT,
    ADD COLUMN IF NOT EXISTS card_ord INTEGER;

CREATE INDEX IF NOT EXISTS idx_media_user_modified
    ON media(user_id, modified_at);
