package ankiimport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const (
	maxNotes         = 100_000
	maxCards         = 200_000
	maxReviews       = 1_000_000
	maxNotetypes     = 2_000
	maxDecks         = 20_000
	maxNoteBytes     = 256 << 10
	maxTemplateBytes = 512 << 10
)

func readSQLite(parent context.Context, data []byte) (Collection, error) {
	dir, err := os.MkdirTemp("", "kelma-apkg-")
	if err != nil {
		return Collection{}, err
	}
	defer os.RemoveAll(dir)
	filename := filepath.Join(dir, "collection.db")
	if err := os.WriteFile(filename, data, 0o600); err != nil {
		return Collection{}, err
	}
	db, err := sql.Open("sqlite", "file:"+filename+"?mode=ro&immutable=1")
	if err != nil {
		return Collection{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	for _, pragma := range []string{"PRAGMA trusted_schema=OFF", "PRAGMA query_only=ON"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return Collection{}, err
		}
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&integrity); err != nil || integrity != "ok" {
		return Collection{}, errors.New("Anki SQLite integrity check failed")
	}
	for _, table := range []string{"col", "notes", "cards"} {
		present, err := hasTable(ctx, db, table)
		if err != nil || !present {
			return Collection{}, errors.New("required Anki table is missing")
		}
	}
	if err := validateRowCount(ctx, db, "notes", maxNotes); err != nil {
		return Collection{}, err
	}
	if err := validateRowCount(ctx, db, "cards", maxCards); err != nil {
		return Collection{}, err
	}
	modelsJSON, decksJSON, err := collectionMetadata(ctx, db)
	if err != nil {
		return Collection{}, err
	}
	collection := Collection{}
	if normalized, _ := hasTable(ctx, db, "notetypes"); normalized {
		collection.Notetypes, err = readNormalizedNotetypes(ctx, db)
	} else {
		collection.Notetypes, err = readLegacyNotetypes(modelsJSON)
	}
	if err != nil {
		return Collection{}, err
	}
	var deckNames map[int64]string
	if normalized, _ := hasTable(ctx, db, "decks"); normalized {
		deckNames, err = readNormalizedDecks(ctx, db)
	} else {
		deckNames, err = readLegacyDecks(decksJSON)
	}
	if err != nil {
		return Collection{}, err
	}
	collection.Notes, err = readNotes(ctx, db)
	if err != nil {
		return Collection{}, err
	}
	collection.Cards, err = readCards(ctx, db, deckNames)
	if err != nil {
		return Collection{}, err
	}
	if present, _ := hasTable(ctx, db, "revlog"); present {
		if err := validateRowCount(ctx, db, "revlog", maxReviews); err != nil {
			return Collection{}, err
		}
		collection.Reviews, err = readReviews(ctx, db)
		if err != nil {
			return Collection{}, err
		}
	}
	collection.Decks = declaredDecks(deckNames, collection.Cards)
	return collection, validateReferences(collection)
}

func hasTable(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count)
	return count == 1, err
}

func validateRowCount(ctx context.Context, db *sql.DB, table string, maximum int) error {
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
		return err
	}
	if count > maximum {
		return fmt.Errorf("Anki %s exceeds the web import limit", table)
	}
	return nil
}

func collectionMetadata(ctx context.Context, db *sql.DB) (string, string, error) {
	var models, decks string
	if err := db.QueryRowContext(ctx, `SELECT models,decks FROM col LIMIT 1`).Scan(&models, &decks); err != nil {
		return "", "", err
	}
	if len(models) > 8<<20 || len(decks) > 8<<20 {
		return "", "", errors.New("Anki collection metadata exceeds the web import limit")
	}
	return models, decks, nil
}

func readLegacyNotetypes(data string) ([]Notetype, error) {
	values, err := decodeJSONObject(data)
	if err != nil {
		return nil, errors.New("Anki note type metadata is invalid")
	}
	if len(values) > maxNotetypes {
		return nil, errors.New("Anki note types exceed the web import limit")
	}
	keys := sortedKeys(values)
	out := make([]Notetype, 0, len(keys))
	seen := map[int64]bool{}
	for _, key := range keys {
		definition, ok := values[key].(map[string]any)
		if !ok {
			continue
		}
		id, ok := jsonInt64(definition["id"])
		if !ok {
			var parseErr error
			id, parseErr = strconv.ParseInt(key, 10, 64)
			if parseErr != nil {
				continue
			}
		}
		if seen[id] {
			return nil, errors.New("Anki note type IDs are not unique")
		}
		seen[id] = true
		name, _ := definition["name"].(string)
		if strings.TrimSpace(name) == "" {
			name = fmt.Sprintf("Imported %d", id)
		}
		if err := validateNotetype(name, definition); err != nil {
			return nil, err
		}
		out = append(out, Notetype{SourceID: id, Name: name, Definition: definition})
	}
	return out, nil
}

func readNormalizedNotetypes(ctx context.Context, db *sql.DB) ([]Notetype, error) {
	if err := validateRowCount(ctx, db, "notetypes", maxNotetypes); err != nil {
		return nil, err
	}
	type field struct {
		ord  int
		name string
	}
	type template struct {
		ord                    int
		name, question, answer string
	}
	fields := map[int64][]field{}
	if present, _ := hasTable(ctx, db, "fields"); present {
		if err := validateRowCount(ctx, db, "fields", maxNotetypes*128); err != nil {
			return nil, err
		}
		rows, err := db.QueryContext(ctx, `SELECT ntid,ord,name FROM fields ORDER BY ntid,ord`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var value field
			if err := rows.Scan(&id, &value.ord, &value.name); err != nil {
				rows.Close()
				return nil, err
			}
			fields[id] = append(fields[id], value)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	templates := map[int64][]template{}
	if present, _ := hasTable(ctx, db, "templates"); present {
		if err := validateRowCount(ctx, db, "templates", maxNotetypes*128); err != nil {
			return nil, err
		}
		rows, err := db.QueryContext(ctx, `SELECT ntid,ord,name,config FROM templates ORDER BY ntid,ord`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var value template
			var config []byte
			if err := rows.Scan(&id, &value.ord, &value.name, &config); err != nil {
				rows.Close()
				return nil, err
			}
			proto, err := decodeProto(config)
			if err != nil {
				rows.Close()
				return nil, err
			}
			value.question, err = fieldString(proto, 1)
			if err != nil {
				rows.Close()
				return nil, err
			}
			value.answer, err = fieldString(proto, 2)
			if err != nil {
				rows.Close()
				return nil, err
			}
			templates[id] = append(templates[id], value)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT id,name,config FROM notetypes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Notetype, 0)
	for rows.Next() {
		var id int64
		var name string
		var config []byte
		if err := rows.Scan(&id, &name, &config); err != nil {
			return nil, err
		}
		proto, err := decodeProto(config)
		if err != nil {
			return nil, err
		}
		css, err := fieldString(proto, 3)
		if err != nil {
			return nil, err
		}
		fieldValues := make([]any, 0, len(fields[id]))
		for _, value := range fields[id] {
			fieldValues = append(fieldValues, map[string]any{"name": value.name, "ord": value.ord})
		}
		templateValues := make([]any, 0, len(templates[id]))
		for _, value := range templates[id] {
			templateValues = append(templateValues, map[string]any{"name": value.name, "ord": value.ord, "qfmt": value.question, "afmt": value.answer})
		}
		definition := map[string]any{"css": css, "flds": fieldValues, "tmpls": templateValues}
		if err := validateNotetype(name, definition); err != nil {
			return nil, err
		}
		out = append(out, Notetype{SourceID: id, Name: name, Definition: definition})
	}
	return out, rows.Err()
}

func readLegacyDecks(data string) (map[int64]string, error) {
	values, err := decodeJSONObject(data)
	if err != nil {
		return nil, errors.New("Anki deck metadata is invalid")
	}
	if len(values) > maxDecks {
		return nil, errors.New("Anki decks exceed the web import limit")
	}
	out := map[int64]string{}
	for key, raw := range values {
		value, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, exists := jsonInt64(value["id"])
		if !exists {
			id, err = strconv.ParseInt(key, 10, 64)
			if err != nil {
				continue
			}
		}
		name, _ := value["name"].(string)
		name, err = normalizeDeckName(name)
		if err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, nil
}

func readNormalizedDecks(ctx context.Context, db *sql.DB) (map[int64]string, error) {
	if err := validateRowCount(ctx, db, "decks", maxDecks); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id,name FROM decks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		name, err = normalizeDeckName(name)
		if err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

func readNotes(ctx context.Context, db *sql.DB) ([]Note, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,guid,mid,tags,flds FROM notes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Note, 0)
	for rows.Next() {
		var note Note
		var tags, fields string
		if err := rows.Scan(&note.SourceID, &note.GUID, &note.NotetypeID, &tags, &fields); err != nil {
			return nil, err
		}
		if !utf8.ValidString(note.GUID) || len([]byte(note.GUID)) > 255 {
			return nil, errors.New("Anki note GUID is invalid")
		}
		if strings.TrimSpace(note.GUID) == "" {
			note.GUID = fmt.Sprintf("anki-%d", note.SourceID)
		}
		if strings.IndexFunc(note.GUID, func(r rune) bool { return r < 32 }) >= 0 {
			return nil, errors.New("Anki note GUID is invalid")
		}
		if !utf8.ValidString(tags) || len([]byte(tags)) > maxNoteBytes {
			return nil, errors.New("Anki note tags are invalid")
		}
		note.Fields = strings.Split(fields, "\x1f")
		if err := validateFields(note.Fields); err != nil {
			return nil, err
		}
		note.Tags = normalizedTags(strings.Fields(tags))
		out = append(out, note)
	}
	return out, rows.Err()
}

func readCards(ctx context.Context, db *sql.DB, decks map[int64]string) ([]Card, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,nid,did,ord FROM cards ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Card, 0)
	for rows.Next() {
		var card Card
		var deckID int64
		if err := rows.Scan(&card.SourceID, &card.NoteSourceID, &deckID, &card.Ordinal); err != nil {
			return nil, err
		}
		if card.Ordinal < 0 || card.Ordinal > 127 {
			return nil, errors.New("Anki card ordinal is invalid")
		}
		card.DeckName = decks[deckID]
		if card.DeckName == "" {
			card.DeckName = "Imported"
		}
		out = append(out, card)
	}
	return out, rows.Err()
}

func readReviews(ctx context.Context, db *sql.DB) ([]Review, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,cid,ease,ivl,lastIvl,factor,time,type FROM revlog ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Review, 0)
	for rows.Next() {
		var review Review
		if err := rows.Scan(&review.ReviewID, &review.CardSourceID, &review.Ease, &review.Interval,
			&review.LastInterval, &review.Factor, &review.TakenMillis, &review.ReviewKind); err != nil {
			return nil, err
		}
		if review.Interval < -2_147_483_648 || review.Interval > 2_147_483_647 ||
			review.LastInterval < -2_147_483_648 || review.LastInterval > 2_147_483_647 ||
			review.Factor < -2_147_483_648 || review.Factor > 2_147_483_647 {
			return nil, errors.New("Anki review value is out of range")
		}
		if review.ReviewID > 0 && review.Ease >= 1 && review.Ease <= 4 {
			if review.TakenMillis < 0 {
				review.TakenMillis = 0
			}
			if review.TakenMillis > 600_000 {
				review.TakenMillis = 600_000
			}
			out = append(out, review)
		}
	}
	return out, rows.Err()
}
