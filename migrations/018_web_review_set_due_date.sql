-- Extend the closed immutable-receipt union for the synchronized Set Due Date
-- action. Existing receipts, ownership constraints, and grants remain unchanged.
ALTER TABLE web_review_operations
    DROP CONSTRAINT web_review_operations_kind_check,
    ADD CONSTRAINT web_review_operations_kind_check CHECK (kind IN (
        'set_card_flag', 'set_note_marked',
        'bury_card', 'bury_note', 'suspend_card', 'suspend_note',
        'set_card_due_date'
    ));
