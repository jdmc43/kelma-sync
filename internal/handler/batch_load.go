package handler

import (
	"context"

	"github.com/jdmc43/kelma-sync/internal/model"
)

// batchPullLimit bounds one pull request. It is a safety ceiling, not a working
// limit: clients pull whole collections in one request and loaders below use
// set queries, so large collections stay fast.
const batchPullLimit = 1_000_000

// batchChunk keeps individual array parameters a sane size for PostgreSQL.
const batchChunk = 5000

func chunked[T any](items []T, fn func([]T) error) error {
	for start := 0; start < len(items); start += batchChunk {
		end := min(start+batchChunk, len(items))
		if err := fn(items[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// loadNotes returns every requested note that exists for userID.
func (h *Handler) loadNotes(ctx context.Context, userID string, guids []string) ([]model.Note, error) {
	out := []model.Note{}
	err := chunked(guids, func(chunk []string) error {
		rows, err := h.DB.Query(ctx, noteSelectSQLByGUIDs, userID, chunk)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			note, err := scanNote(rows)
			if err != nil {
				return err
			}
			out = append(out, note)
		}
		return rows.Err()
	})
	return out, err
}

// loadCardsByIDs returns every requested card (by Anki card id) for userID.
func (h *Handler) loadCardsByIDs(ctx context.Context, userID string, ids []int64) ([]model.Card, error) {
	out := []model.Card{}
	err := chunked(ids, func(chunk []int64) error {
		cards, err := h.loadCardsWhere(ctx, userID, "c.card_id = ANY($2::bigint[])", chunk)
		out = append(out, cards...)
		return err
	})
	return out, err
}

// loadCardsByKeys returns every requested card by logical (note_guid, ord) key.
func (h *Handler) loadCardsByKeys(ctx context.Context, userID string, keys []batchCardKey) ([]model.Card, error) {
	out := []model.Card{}
	err := chunked(keys, func(chunk []batchCardKey) error {
		guids := make([]string, len(chunk))
		ords := make([]int32, len(chunk))
		for i, key := range chunk {
			guids[i] = key.NoteGUID
			ords[i] = int32(key.Ord)
		}
		cards, err := h.loadCardsWhere(ctx, userID,
			"(c.note_guid, c.ord) IN (SELECT * FROM unnest($2::text[], $3::int[]))", guids, ords)
		out = append(out, cards...)
		return err
	})
	return out, err
}

// loadNotetypes returns every requested notetype for userID.
func (h *Handler) loadNotetypes(ctx context.Context, userID string, ids []int64) ([]model.Notetype, error) {
	out := []model.Notetype{}
	err := chunked(ids, func(chunk []int64) error {
		rows, err := h.DB.Query(ctx,
			`SELECT n.id, n.user_id, n.notetype_id, n.name, n.definition, n.checksum,
			        n.modified_at, n.client_modified_at,
			        COALESCE(n.last_client_id::text, ''), COALESCE(c.label, '')
			 FROM notetypes n LEFT JOIN clients c ON c.id = n.last_client_id
			 WHERE n.user_id = $1 AND n.notetype_id = ANY($2::bigint[])`, userID, chunk)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var nt model.Notetype
			if err := rows.Scan(&nt.ID, &nt.UserID, &nt.NotetypeID, &nt.Name, &nt.Definition, &nt.Checksum,
				&nt.ModifiedAt, &nt.ClientModifiedAt, &nt.LastClientID, &nt.LastClientLabel); err != nil {
				return err
			}
			out = append(out, nt)
		}
		return rows.Err()
	})
	return out, err
}

// loadDecks returns every requested deck for userID.
func (h *Handler) loadDecks(ctx context.Context, userID string, names []string) ([]model.Deck, error) {
	out := []model.Deck{}
	err := chunked(names, func(chunk []string) error {
		rows, err := h.DB.Query(ctx,
			`SELECT d.id, d.user_id, d.name, d.config, d.checksum,
			        d.modified_at, d.client_modified_at,
			        COALESCE(d.last_client_id::text, ''), COALESCE(c.label, '')
			 FROM decks d LEFT JOIN clients c ON c.id = d.last_client_id
			 WHERE d.user_id = $1 AND d.name = ANY($2::text[])`, userID, chunk)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d model.Deck
			if err := rows.Scan(&d.ID, &d.UserID, &d.Name, &d.Config, &d.Checksum,
				&d.ModifiedAt, &d.ClientModifiedAt, &d.LastClientID, &d.LastClientLabel); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}
