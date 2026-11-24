package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jdmc43/kelma-sync/internal/ankiimport"
	"github.com/jdmc43/kelma-sync/internal/auth"
	"github.com/jdmc43/kelma-sync/internal/projection"
)

type reviewImportResult struct {
	AddedNotes             int      `json:"added_notes"`
	ReusedNotes            int      `json:"reused_notes"`
	CopiedConflicts        int      `json:"copied_conflicts"`
	AddedCards             int      `json:"added_cards"`
	AddedReviews           int      `json:"added_reviews"`
	SkippedReviewConflicts int      `json:"skipped_review_conflicts"`
	AddedMedia             int      `json:"added_media"`
	RenamedMedia           int      `json:"renamed_media"`
	Warnings               []string `json:"warnings"`
}

type importCardRef struct {
	cardID int64
	guid   string
	ord    int
	deck   string
}

type importMediaWrite struct {
	name, key, contentType string
	data                   []byte
}

type importExistingNotetype struct {
	id             int64
	name, checksum string
}

func (h *Handler) mergeReviewImport(ctx context.Context, claims *auth.Claims, collection ankiimport.Collection) (reviewImportResult, error) {
	result := reviewImportResult{Warnings: append([]string{}, collection.Warnings...)}
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := projection.LockAccountTx(ctx, tx, claims.UserID); err != nil {
		return result, err
	}
	now := h.currentTime()

	mediaNames, mediaWrites, renamed, err := h.planReviewImportMedia(ctx, tx, claims.UserID, collection.Media)
	if err != nil {
		return result, err
	}
	result.RenamedMedia = renamed
	keepMedia := false
	defer func() {
		if keepMedia {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		for _, file := range mediaWrites {
			_ = h.Storage.Delete(cleanupContext, file.key)
		}
	}()
	for _, file := range mediaWrites {
		if err := h.Storage.Put(ctx, file.key, bytes.NewReader(file.data), int64(len(file.data)), file.contentType); err != nil {
			return result, err
		}
	}
	notetypeIDs, err := importReviewNotetypes(ctx, tx, claims, collection.Notetypes, mediaNames, now)
	if err != nil {
		return result, err
	}
	noteGUIDs, err := importReviewNotes(ctx, tx, claims, collection.Notes, notetypeIDs, mediaNames, now, &result)
	if err != nil {
		return result, err
	}
	if err := importReviewDecks(ctx, tx, claims, collection.Decks, now); err != nil {
		return result, err
	}
	cardRefs, err := importReviewCards(ctx, tx, claims, collection.Cards, noteGUIDs, now, &result)
	if err != nil {
		return result, err
	}
	if err := importReviewHistory(ctx, tx, claims, collection.Reviews, cardRefs, now, &result); err != nil {
		return result, err
	}
	if err := writeReviewImportMedia(ctx, tx, claims, mediaWrites, now); err != nil {
		return result, err
	}
	result.AddedMedia = len(mediaWrites)

	affected := make([]projection.LogicalCard, 0, len(cardRefs))
	seen := map[projection.LogicalCard]bool{}
	for _, card := range cardRefs {
		logical := projection.LogicalCard{NoteGUID: card.guid, CardOrd: card.ord}
		if !seen[logical] {
			seen[logical] = true
			affected = append(affected, logical)
		}
	}
	if err := projection.EnqueueReviewsTx(ctx, tx, claims.UserID, affected); err != nil {
		return result, err
	}
	// A commit error can be transport-ambiguous, so retain the account-scoped
	// objects once commit begins rather than risk deleting committed media.
	keepMedia = true
	if err := tx.Commit(ctx); err != nil {
		return result, err
	}
	return result, nil
}

func importReviewNotetypes(ctx context.Context, tx pgx.Tx, claims *auth.Claims, values []ankiimport.Notetype,
	mediaNames map[string]string, now time.Time) (map[int64]int64, error) {
	existing := map[int64]importExistingNotetype{}
	rows, err := tx.Query(ctx, `SELECT notetype_id,name,checksum FROM notetypes WHERE user_id=$1`, claims.UserID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var value importExistingNotetype
		if err := rows.Scan(&value.id, &value.name, &value.checksum); err != nil {
			rows.Close()
			return nil, err
		}
		existing[value.id] = value
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	mapped := map[int64]int64{}
	for _, source := range values {
		definition := rewriteImportMap(source.Definition, mediaNames)
		cs := notetypeChecksum(source.Name, definition)
		direct, directExists := existing[source.SourceID]
		id, chosen := int64(0), false
		if directExists && direct.name == source.Name && direct.checksum == cs {
			id, chosen = source.SourceID, true
		}
		if !chosen {
			for _, candidate := range existing {
				if candidate.name == source.Name && candidate.checksum == cs {
					id, chosen = candidate.id, true
					break
				}
			}
		}
		if !chosen && !directExists {
			id, chosen = source.SourceID, true
		}
		if !chosen {
			id, err = allocateImportNotetypeID(source.Name, definition, existing)
			if err != nil {
				return nil, err
			}
		}
		if _, ok := existing[id]; !ok {
			_, err := tx.Exec(ctx, `INSERT INTO notetypes(user_id,notetype_id,name,definition,checksum,modified_at,client_modified_at,last_client_id)
				VALUES($1,$2,$3,$4,$5,$6,$6,$7)`, claims.UserID, id, source.Name, definition, cs, now, claims.ClientID)
			if err != nil {
				return nil, err
			}
			existing[id] = importExistingNotetype{id: id, name: source.Name, checksum: cs}
		}
		mapped[source.SourceID] = id
		if _, err := tx.Exec(ctx, `DELETE FROM tombstones WHERE user_id=$1 AND type='notetype' AND resource_id=$2`, claims.UserID, fmt.Sprint(id)); err != nil {
			return nil, err
		}
	}
	return mapped, nil
}

func importReviewNotes(ctx context.Context, tx pgx.Tx, claims *auth.Claims, values []ankiimport.Note,
	notetypeIDs map[int64]int64, mediaNames map[string]string, now time.Time, result *reviewImportResult) (map[int64]string, error) {
	mapped := map[int64]string{}
	for _, source := range values {
		notetypeID, ok := notetypeIDs[source.NotetypeID]
		if !ok {
			return nil, errors.New("import note type is missing")
		}
		fields := make([]string, len(source.Fields))
		for index, field := range source.Fields {
			fields[index] = rewriteImportText(field, mediaNames)
		}
		tags := append([]string{}, source.Tags...)
		sort.Strings(tags)
		guid := source.GUID
		existingID, existingFields, existingTags, found, err := loadImportNote(ctx, tx, claims.UserID, guid)
		if err != nil {
			return nil, err
		}
		if found && (existingID != notetypeID || !equalStringSlices(existingFields, fields) || !equalTagSets(existingTags, tags)) {
			result.CopiedConflicts++
			guid, found, err = conflictImportGUID(ctx, tx, claims.UserID, guid, notetypeID, fields, tags)
			if err != nil {
				return nil, err
			}
		} else if found {
			result.ReusedNotes++
		}
		if !found {
			_, err := tx.Exec(ctx, `INSERT INTO notes(user_id,guid,notetype_id,fields,tags,checksum,modified_at,client_modified_at,last_client_id)
				VALUES($1,$2,$3,$4,$5,$6,$7,$7,$8)`, claims.UserID, guid, notetypeID, fields, tags, checksum(fields, tags), now, claims.ClientID)
			if err != nil {
				return nil, err
			}
			result.AddedNotes++
		} else if guid != source.GUID {
			result.ReusedNotes++
		}
		mapped[source.SourceID] = guid
		if _, err := tx.Exec(ctx, `DELETE FROM tombstones WHERE user_id=$1 AND type='note' AND resource_id=$2`, claims.UserID, guid); err != nil {
			return nil, err
		}
	}
	return mapped, nil
}

func importReviewDecks(ctx context.Context, tx pgx.Tx, claims *auth.Claims, decks []string, now time.Time) error {
	empty := map[string]any{}
	for _, name := range decks {
		if _, err := tx.Exec(ctx, `INSERT INTO decks(user_id,name,config,checksum,modified_at,client_modified_at,last_client_id)
			VALUES($1,$2,$3,$4,$5,$5,$6) ON CONFLICT(user_id,name) DO NOTHING`,
			claims.UserID, name, empty, deckChecksum(empty), now, claims.ClientID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM tombstones WHERE user_id=$1 AND type='deck' AND resource_id=$2`, claims.UserID, name); err != nil {
			return err
		}
	}
	return nil
}

func importReviewCards(ctx context.Context, tx pgx.Tx, claims *auth.Claims, values []ankiimport.Card,
	noteGUIDs map[int64]string, now time.Time, result *reviewImportResult) (map[int64]importCardRef, error) {
	mapped := map[int64]importCardRef{}
	for _, source := range values {
		guid, ok := noteGUIDs[source.NoteSourceID]
		if !ok {
			continue
		}
		var card importCardRef
		err := tx.QueryRow(ctx, `SELECT card_id,note_guid,ord,deck_name FROM cards WHERE user_id=$1 AND note_guid=$2 AND ord=$3`,
			claims.UserID, guid, source.Ordinal).Scan(&card.cardID, &card.guid, &card.ord, &card.deck)
		if errors.Is(err, pgx.ErrNoRows) {
			cardID, err := availableImportCardID(ctx, tx, claims.UserID, guid, source.Ordinal)
			if err != nil {
				return nil, err
			}
			var createdAt *time.Time
			if source.SourceID >= earliestAnkiCardCreatedAtMillis && source.SourceID <= now.Add(maxFutureClockSkew).UnixMilli() {
				value := time.UnixMilli(source.SourceID).UTC()
				createdAt = &value
			}
			_, err = tx.Exec(ctx, `INSERT INTO cards(user_id,card_id,note_guid,deck_name,ord,scheduling,created_at,modified_at,client_modified_at,last_client_id)
				VALUES($1,$2,$3,$4,$5,'{}',$6,$7,$7,$8)`, claims.UserID, cardID, guid, source.DeckName, source.Ordinal, createdAt, now, claims.ClientID)
			if err != nil {
				return nil, err
			}
			card = importCardRef{cardID: cardID, guid: guid, ord: source.Ordinal, deck: source.DeckName}
			result.AddedCards++
		} else if err != nil {
			return nil, err
		}
		mapped[source.SourceID] = card
		if _, err := tx.Exec(ctx, `DELETE FROM tombstones WHERE user_id=$1 AND type='card' AND resource_id=$2`, claims.UserID, fmt.Sprint(card.cardID)); err != nil {
			return nil, err
		}
	}
	return mapped, nil
}

func importReviewHistory(ctx context.Context, tx pgx.Tx, claims *auth.Claims, values []ankiimport.Review,
	cards map[int64]importCardRef, now time.Time, result *reviewImportResult) error {
	skippedInvalid := 0
	for _, source := range values {
		card, ok := cards[source.CardSourceID]
		if !ok || source.ReviewID > now.Add(maxFutureClockSkew).UnixMilli() || source.ReviewKind < 0 || source.ReviewKind > 4 {
			skippedInvalid++
			continue
		}
		req := putReviewRequest{ReviewID: source.ReviewID, SourceCardID: card.cardID, NoteGUID: card.guid, CardOrd: card.ord,
			DeckName: card.deck, Ease: source.Ease, Interval: source.Interval, LastInterval: source.LastInterval,
			Factor: source.Factor, TakenMillis: source.TakenMillis, ReviewKind: source.ReviewKind}
		cs := checksumForReview(req)
		var existing string
		err := tx.QueryRow(ctx, `SELECT checksum FROM reviews WHERE user_id=$1 AND review_id=$2`, claims.UserID, source.ReviewID).Scan(&existing)
		if err == nil {
			if existing != cs {
				result.SkippedReviewConflicts++
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO reviews(user_id,review_id,source_card_id,note_guid,card_ord,deck_name,ease,interval,last_interval,
			factor,taken_millis,review_kind,checksum,modified_at,last_client_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			claims.UserID, req.ReviewID, req.SourceCardID, req.NoteGUID, req.CardOrd, req.DeckName, req.Ease, req.Interval,
			req.LastInterval, req.Factor, req.TakenMillis, req.ReviewKind, cs, now, claims.ClientID)
		if err != nil {
			return err
		}
		result.AddedReviews++
	}
	if skippedInvalid > 0 {
		result.Warnings = append(result.Warnings, "Some invalid or future review rows were skipped.")
	}
	return nil
}
