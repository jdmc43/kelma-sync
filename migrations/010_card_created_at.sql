ALTER TABLE cards
    ADD COLUMN created_at TIMESTAMPTZ;

-- Native Anki card IDs are creation-time Unix milliseconds. Backfill only the
-- plausible timestamp range; Kelma-native negative IDs and arbitrary legacy
-- IDs remain NULL rather than receiving a fabricated migration date.
UPDATE cards
SET created_at = to_timestamp(card_id / 1000.0)
WHERE card_id >= 946684800000
  AND card_id <= (extract(epoch FROM now() + interval '5 minutes') * 1000)::bigint;
