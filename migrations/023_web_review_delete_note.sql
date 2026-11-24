-- Admit presentation-derived Delete Note to the immutable browser operation
-- receipt ledger. Canonical deletion itself uses the existing note tombstone
-- protocol; this migration rewrites no notes, cards, reviews, or receipts.
ALTER TABLE web_review_operations DROP CONSTRAINT web_review_operations_kind_check;
ALTER TABLE web_review_operations ADD CONSTRAINT web_review_operations_kind_check CHECK (
    kind IN (
        'set_card_flag', 'set_note_marked',
        'bury_card', 'bury_note', 'suspend_card', 'suspend_note',
        'set_card_due_date', 'reset_card', 'create_note_copy', 'edit_note',
        'delete_note'
    )
) NOT VALID;
ALTER TABLE web_review_operations VALIDATE CONSTRAINT web_review_operations_kind_check;
