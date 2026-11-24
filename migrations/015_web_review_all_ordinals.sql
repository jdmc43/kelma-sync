-- Browser review uses the canonical nonnegative card ordinal, not a
-- basic/reverse-only subset. Existing presentations and receipts are retained.
ALTER TABLE web_review_presentations
    DROP CONSTRAINT web_review_presentations_card_ord_check,
    ADD CONSTRAINT web_review_presentations_card_ord_check CHECK (card_ord >= 0);
