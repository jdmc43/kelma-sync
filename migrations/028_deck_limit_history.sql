-- A deck's daily limits (config newLimit, reviewLimit) as they were: one row per
-- change, holding the limits in effect before it. Review days score each day with
-- that day's limits, so raising a limit today doesn't rewrite yesterday. Recorded
-- by a trigger, whatever wrote the deck (Kelma Review, the website, an import).
CREATE TABLE deck_limit_history (
    id          BIGSERIAL PRIMARY KEY,
    user_id     TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    deck_name   TEXT        NOT NULL,
    limits      JSONB       NOT NULL,              -- {"newLimit": …, "reviewLimit": …} before the change
    changed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_deck_limit_history_user ON deck_limit_history (user_id, deck_name, changed_at);

-- SECURITY DEFINER: the history is written as the table owner, whichever role
-- updates the deck (a restricted app role then needs only SELECT on the history).
CREATE FUNCTION record_deck_limit_change() RETURNS trigger
    SECURITY DEFINER SET search_path = pg_catalog, public AS $$
BEGIN
    -- A renamed deck keeps its history.
    IF NEW.name IS DISTINCT FROM OLD.name THEN
        UPDATE deck_limit_history SET deck_name = NEW.name
         WHERE user_id = OLD.user_id AND deck_name = OLD.name;
    END IF;
    IF (OLD.config -> 'newLimit') IS DISTINCT FROM (NEW.config -> 'newLimit')
       OR (OLD.config -> 'reviewLimit') IS DISTINCT FROM (NEW.config -> 'reviewLimit') THEN
        INSERT INTO deck_limit_history (user_id, deck_name, limits)
        VALUES (NEW.user_id, NEW.name, jsonb_build_object(
            'newLimit', OLD.config -> 'newLimit', 'reviewLimit', OLD.config -> 'reviewLimit'));
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER decks_limit_history AFTER UPDATE ON decks
    FOR EACH ROW EXECUTE FUNCTION record_deck_limit_change();
