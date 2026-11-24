-- Admit the bounded, presentation-derived Edit Note operation to the immutable
-- browser operation receipt ledger. This does not rewrite existing receipts,
-- notes, cards, review history, projections, or study-day state.
ALTER TABLE web_review_operations DROP CONSTRAINT web_review_operations_kind_check;
ALTER TABLE web_review_operations ADD CONSTRAINT web_review_operations_kind_check CHECK (
    kind IN (
        'set_card_flag', 'set_note_marked',
        'bury_card', 'bury_note', 'suspend_card', 'suspend_note',
        'set_card_due_date', 'reset_card', 'create_note_copy', 'edit_note'
    )
) NOT VALID;
ALTER TABLE web_review_operations VALIDATE CONSTRAINT web_review_operations_kind_check;
