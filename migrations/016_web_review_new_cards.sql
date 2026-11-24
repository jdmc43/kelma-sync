-- Zero denotes a frozen presentation with no qualifying post-reset history.
-- Existing presentations and exactly-once receipts retain their identities.
ALTER TABLE web_review_presentations
    DROP CONSTRAINT web_review_presentations_through_review_id_check,
    ADD CONSTRAINT web_review_presentations_through_review_id_check CHECK (through_review_id >= 0);

-- Private immutable budget checkpoints, committed with the existing receipts.
-- Do not publish browser totals into native/imported study_days counters.
ALTER TABLE web_review_attempts
    ADD COLUMN daily_usage JSONB NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(daily_usage) = 'object');
