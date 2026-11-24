package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// Deck operations for web clients (the Kelma assistant): move cards to a deck and
// rename (move) a whole deck with its subdecks. They change only deck names; card
// scheduling, history, flags and study state are untouched.
const (
	moveCardsKind  = "move_cards"
	renameDeckKind = "rename_deck"

	maxMovedCards    = 2500
	maxDeckNameBytes = 256
)

// errReviewOperationStaleTarget means the cards or deck changed since the client
// prepared the operation (mapped to 409, like a stale note checksum).
var errReviewOperationStaleTarget = errors.New("review operation target changed")

type moveCardsPayload struct {
	CardIDs  []string `json:"card_ids"`
	DeckName *string  `json:"deck_name"`
}

type renameDeckPayload struct {
	From *string `json:"from"`
	To   *string `json:"to"`
}

// validDeckName accepts Anki-style names: no surrounding whitespace, no control
// characters, and no empty "::" components.
func validDeckName(name string) bool {
	if name == "" || len(name) > maxDeckNameBytes || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	for _, part := range strings.Split(name, "::") {
		if strings.TrimSpace(part) == "" {
			return false
		}
	}
	return true
}

func deckWithin(name, parent string) bool {
	return name == parent || strings.HasPrefix(name, parent+"::")
}

func parseDeckOperationPayload(intent *reviewOperationIntent, payload json.RawMessage) (string, error) {
	switch intent.Kind {
	case moveCardsKind:
		var value moveCardsPayload
		if decodeExactJSON(bytes.NewReader(payload), &value) != nil || value.DeckName == nil ||
			!validDeckName(*value.DeckName) || len(value.CardIDs) == 0 || len(value.CardIDs) > maxMovedCards {
			return "", errors.New("invalid card move")
		}
		seen := make(map[int64]struct{}, len(value.CardIDs))
		ids := make([]int64, 0, len(value.CardIDs))
		for _, raw := range value.CardIDs {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id == 0 || strconv.FormatInt(id, 10) != raw {
				return "", errors.New("invalid card move")
			}
			if _, duplicate := seen[id]; duplicate {
				return "", errors.New("invalid card move")
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		intent.CardIDs, intent.DeckName = ids, *value.DeckName
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = strconv.FormatInt(id, 10)
		}
		return *value.DeckName + "\n" + strings.Join(parts, ","), nil
	case renameDeckKind:
		var value renameDeckPayload
		if decodeExactJSON(bytes.NewReader(payload), &value) != nil || value.From == nil || value.To == nil ||
			!validDeckName(*value.From) || !validDeckName(*value.To) ||
			*value.From == *value.To || deckWithin(*value.To, *value.From) {
			return "", errors.New("invalid deck rename")
		}
		intent.DeckFrom, intent.DeckTo = *value.From, *value.To
		return *value.From + "\n" + *value.To, nil
	}
	return "", errors.New("unsupported deck operation")
}

func likePrefix(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value) + "::%"
}

// ensureDeckTx creates a deck record (config copied from configFrom when that deck
// exists) unless it already exists, and clears any deletion tombstone for it.
func ensureDeckTx(ctx context.Context, tx pgx.Tx, userID, clientID, name, configFrom string, now time.Time) error {
	config := map[string]any{}
	if configFrom != "" {
		var stored map[string]any
		err := tx.QueryRow(ctx, `SELECT config FROM decks WHERE user_id=$1 AND name=$2`, userID, configFrom).Scan(&stored)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if stored != nil {
			config = stored
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO decks(user_id,name,config,checksum,modified_at,client_modified_at,last_client_id)
		VALUES($1,$2,$3,$4,$5,$5,$6::uuid) ON CONFLICT (user_id,name) DO NOTHING`,
		userID, name, config, deckChecksum(config), now, clientID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM tombstones WHERE user_id=$1 AND type='deck' AND resource_id=$2`, userID, name)
	return err
}

// Card writes move both clocks strictly forward so manifests since the previous
// cursor include the change and stale client uploads cannot overwrite it.
const movedCardClocks = `modified_at=GREATEST($4,modified_at + interval '1 microsecond'),
	client_modified_at=GREATEST($4,client_modified_at + interval '1 microsecond'),
	last_client_id=$5::uuid`

func applyMoveCardsTx(ctx context.Context, tx pgx.Tx, userID, clientID string,
	intent reviewOperationIntent, now time.Time, state *reviewOperationState,
) error {
	var existing int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM cards WHERE user_id=$1 AND card_id = ANY($2)`,
		userID, intent.CardIDs).Scan(&existing); err != nil {
		return errors.New("card move lookup failed")
	}
	if existing != len(intent.CardIDs) {
		return errReviewOperationStaleTarget
	}
	// A brand-new deck inherits the options of the deck most of the cards come from.
	var source string
	_ = tx.QueryRow(ctx, `SELECT deck_name FROM cards WHERE user_id=$1 AND card_id = ANY($2)
		GROUP BY deck_name ORDER BY count(*) DESC, deck_name LIMIT 1`, userID, intent.CardIDs).Scan(&source)
	if err := ensureDeckTx(ctx, tx, userID, clientID, intent.DeckName, source, now); err != nil {
		return errors.New("deck creation failed")
	}
	tag, err := tx.Exec(ctx, `UPDATE cards SET deck_name=$3,`+movedCardClocks+`
		WHERE user_id=$1 AND card_id = ANY($2) AND deck_name <> $3`,
		userID, intent.CardIDs, intent.DeckName, now, clientID)
	if err != nil {
		return errors.New("card move failed")
	}
	state.DeckName, state.MovedCards = intent.DeckName, int(tag.RowsAffected())
	return nil
}

func applyRenameDeckTx(ctx context.Context, tx pgx.Tx, userID, clientID string,
	intent reviewOperationIntent, target reviewClientTarget, now time.Time, state *reviewOperationState,
) error {
	from, to := intent.DeckFrom, intent.DeckTo
	// The referenced card proves the deck exists for this account right now.
	if !deckWithin(target.DeckName, from) {
		return errReviewOperationStaleTarget
	}
	rows, err := tx.Query(ctx, `SELECT name FROM decks WHERE user_id=$1 AND (name=$2 OR name LIKE $3 ESCAPE '\')
		ORDER BY name`, userID, from, likePrefix(from))
	if err != nil {
		return errors.New("deck rename lookup failed")
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return errors.New("deck rename lookup failed")
		}
		names = append(names, name)
	}
	rows.Close()
	if err := ensureDeckTx(ctx, tx, userID, clientID, to, from, now); err != nil {
		return errors.New("deck rename failed")
	}
	for _, name := range names {
		renamed := to + name[len(from):]
		if err := ensureDeckTx(ctx, tx, userID, clientID, renamed, name, now); err != nil {
			return errors.New("deck rename failed")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM decks WHERE user_id=$1 AND name=$2`, userID, name); err != nil {
			return errors.New("deck rename failed")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tombstones(user_id,type,resource_id,deleted_at,last_client_id)
			VALUES($1,'deck',$2,$3,$4::uuid) ON CONFLICT (user_id,type,resource_id)
			DO UPDATE SET deleted_at=EXCLUDED.deleted_at,last_client_id=EXCLUDED.last_client_id,note_guid=NULL,card_ord=NULL`,
			userID, name, now, clientID); err != nil {
			return errors.New("deck rename failed")
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE cards SET deck_name = $6 || substr(deck_name, char_length($2) + 1),`+movedCardClocks+`
		WHERE user_id=$1 AND (deck_name=$2 OR deck_name LIKE $3 ESCAPE '\')`,
		userID, from, likePrefix(from), now, clientID, to)
	if err != nil {
		return errors.New("deck rename failed")
	}
	state.DeckName, state.MovedCards = to, int(tag.RowsAffected())
	return nil
}
