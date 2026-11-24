-- Web clients (the Kelma assistant) can move cards between decks and rename decks.
ALTER TABLE review_client_operations DROP CONSTRAINT review_client_operations_kind_check;
ALTER TABLE review_client_operations ADD CONSTRAINT review_client_operations_kind_check CHECK (kind IN (
    'set_card_flag', 'set_note_marked', 'suspend_card', 'suspend_note',
    'set_card_due_date', 'reset_card', 'create_note_copy', 'edit_note',
    'delete_note', 'move_cards', 'rename_deck'
));
