package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jdmc43/kelma-sync/internal/model"
	"github.com/jdmc43/kelma-sync/internal/projection"
)

type batchNote struct {
	GUID string `json:"guid"`
	putNoteRequest
}
type batchCard struct {
	CardID int64 `json:"card_id"`
	putCardRequest
}
type batchNotetype struct {
	NotetypeID int64 `json:"notetype_id"`
	putNotetypeRequest
}
type batchDeck struct {
	Name string `json:"name"`
	putDeckRequest
}

type batchPushRequest struct {
	Notes             []batchNote               `json:"notes"`
	NoteMarks         []batchNoteMark           `json:"note_marks"`
	CardFlags         []batchCardFlag           `json:"card_flags"`
	ReviewRetractions []reviewRetractionRequest `json:"review_retractions"`
	Cards             []batchCard               `json:"cards"`
	Reviews           []putReviewRequest        `json:"reviews"`
	StudyDays         []putStudyDayRequest      `json:"study_days"`
	Notetypes         []batchNotetype           `json:"notetypes"`
	Decks             []batchDeck               `json:"decks"`
}

type conflictEntry struct {
	GUID       string `json:"guid,omitempty"`
	ReviewID   int64  `json:"review_id,omitempty"`
	NotetypeID int64  `json:"notetype_id,omitempty"`
	Name       string `json:"name,omitempty"`
	Server     any    `json:"server"`
	Client     any    `json:"client"`
}

type batchPushResponse struct {
	Accepted          map[string]int             `json:"accepted"`
	Conflicts         map[string][]conflictEntry `json:"conflicts"`
	MarkResults       []noteMarkResult           `json:"note_marks"`
	FlagResults       []cardFlagResult           `json:"card_flags"`
	RetractionResults []reviewRetractionResult   `json:"review_retractions"`
}

// BatchPush writes bounded resource batches and reports mutable conflicts.
func (h *Handler) BatchPush(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	ctx := r.Context()
	force := forceOverride(r)

	var req batchPushRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	resp := batchPushResponse{
		Accepted: map[string]int{
			"notes": 0, "note_marks": 0, "card_flags": 0, "review_retractions": 0,
			"cards": 0, "reviews": 0, "study_days": 0,
			"notetypes": 0, "decks": 0,
		},
		Conflicts: map[string][]conflictEntry{
			"notes": {}, "note_marks": {}, "card_flags": {}, "review_retractions": {},
			"reviews": {}, "notetypes": {}, "decks": {},
		},
		MarkResults:       []noteMarkResult{},
		FlagResults:       []cardFlagResult{},
		RetractionResults: []reviewRetractionResult{},
	}
	now := time.Now().UTC()
	for _, mark := range req.NoteMarks {
		if mark.GUID == "" || validateNoteMark(mark.noteMarkRequest, now) != nil {
			writeError(w, http.StatusBadRequest, "bad_note_mark", "invalid note mark intent")
			return
		}
	}
	for _, flag := range req.CardFlags {
		if flag.NoteGUID == "" || flag.CardOrd < 0 || validateCardFlagRequest(flag.cardFlagRequest, now) != nil {
			writeError(w, http.StatusBadRequest, "bad_card_flag", "invalid card flag intent")
			return
		}
	}
	for _, retraction := range req.ReviewRetractions {
		if validateReviewRetractionRequest(retraction, now) != nil {
			writeError(w, http.StatusBadRequest, "bad_review_retraction", "invalid review retraction intent")
			return
		}
	}

	if len(req.Notes) > 0 {
		tx, err := h.DB.Begin(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if err := projection.LockAccountTx(ctx, tx, claims.UserID); err != nil {
			_ = tx.Rollback(ctx)
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		existingNotes := make([]model.Note, len(req.Notes))
		noteExists := make([]bool, len(req.Notes))
		noteItems := make([][]any, len(req.Notes))
		for i := range req.Notes {
			existing, err := loadNoteTx(ctx, tx, claims.UserID, req.Notes[i].GUID)
			notFound := errors.Is(err, pgx.ErrNoRows)
			if err != nil && !notFound {
				_ = tx.Rollback(ctx)
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			existingNotes[i], noteExists[i] = existing, !notFound
			req.Notes[i].putNoteRequest = preserveNoteMark(req.Notes[i].putNoteRequest, existing, !notFound)
			noteItems[i] = []any{req.Notes[i].Fields, req.Notes[i].Tags}
		}
		noteChecksums := checksumBatch(noteItems)
		for i, note := range req.Notes {
			existing, exists, cs := existingNotes[i], noteExists[i], noteChecksums[i]
			if !force && exists && !noteBaseMatches(existing, note.BaseChecksum) && existing.Checksum != cs {
				resp.Conflicts["notes"] = append(resp.Conflicts["notes"],
					conflictEntry{GUID: note.GUID, Server: existing, Client: note})
				continue
			}
			if _, err := upsertNoteTx(ctx, tx, claims.UserID, claims.ClientID,
				note.GUID, note.putNoteRequest, cs, now); err != nil {
				_ = tx.Rollback(ctx)
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			resp.Accepted["notes"]++
		}
		if err := tx.Commit(ctx); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}

	if !h.applyBatchNoteMarks(w, r, req.NoteMarks, now, &resp) ||
		!h.applyBatchCardFlags(w, r, req.CardFlags, now, &resp) ||
		!h.applyBatchReviewRetractions(w, r, req.ReviewRetractions, now, &resp) {
		return
	}

	for _, c := range req.Cards {
		_, c.ClientModifiedAt = utcWriteTimes(now, c.ClientModifiedAt)
		if err := validateStudyState(c.StudyState); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if c.DueDateOverrideMS != nil &&
			(*c.DueDateOverrideMS < 0 || *c.DueDateOverrideMS%86_400_000 != 0) {
			writeError(w, http.StatusBadRequest, "bad_request", "due_date_override_ms must be 0 or a UTC day boundary")
			return
		}
		if c.Scheduling == nil {
			c.Scheduling = map[string]any{}
		}
		if c.NoteGUID == "" {
			continue // blank GUID is not a valid logical card identity
		}
		// Resolve by logical identity (note_guid, ord), not card_id.
		existing, err := h.loadCardLogical(r, claims.UserID, c.NoteGUID, c.Ord)
		notFound := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !notFound {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if !notFound {
			createdAtUpdated, err := h.fillCardCreatedAt(
				ctx, claims.UserID, c.NoteGUID, c.Ord, existing.CardID, c.CreatedAt, now,
			)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			stateUpdated, err := h.updateCardStudyState(
				ctx, claims.UserID, claims.ClientID, c.NoteGUID, c.Ord, c.putCardRequest, now,
			)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			resetUpdated, err := h.updateCardScheduleReset(
				ctx, claims.UserID, claims.ClientID, c.NoteGUID, c.Ord, c.putCardRequest, now,
			)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			dueDateUpdated, err := h.updateCardDueDateOverride(
				ctx, claims.UserID, claims.ClientID, c.NoteGUID, c.Ord, c.putCardRequest, now,
			)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			if existing.ClientModifiedAt.After(c.ClientModifiedAt) {
				if createdAtUpdated || stateUpdated || resetUpdated || dueDateUpdated || c.StudyState != nil ||
					c.ScheduleResetThroughReviewID != nil || c.DueDateOverrideMS != nil {
					resp.Accepted["cards"]++
				}
				continue // content and independently versioned card state use separate clocks
			}
		}
		cardID := c.CardID
		if !notFound {
			cardID = existing.CardID // keep canonical id
		}
		if _, err := h.upsertCard(ctx, claims.UserID, claims.ClientID, cardID, c.putCardRequest, now); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		resp.Accepted["cards"]++
	}

	for _, review := range req.Reviews {
		if review.ReviewID <= 0 {
			writeError(w, http.StatusBadRequest, "bad_review_history", "review_id must be positive")
			return
		}
	}
	reviewCollisions, err := h.insertReviews(
		ctx, claims.UserID, claims.ClientID, req.Reviews, now,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for _, collision := range reviewCollisions {
		resp.Conflicts["reviews"] = append(resp.Conflicts["reviews"],
			conflictEntry{
				ReviewID: collision.Client.ReviewID,
				Server:   collision.Server,
				Client:   collision.Client,
			})
	}
	// Idempotent duplicates count as accepted: the caller can safely clear the
	// local pending USN once immutable content is confirmed equal.
	resp.Accepted["reviews"] = len(req.Reviews) - len(reviewCollisions)

	for _, day := range req.StudyDays {
		if _, err := h.upsertStudyDay(ctx, claims.UserID, claims.ClientID, day, now); err != nil {
			writeError(w, http.StatusBadRequest, "bad_study_day", err.Error())
			return
		}
		resp.Accepted["study_days"]++
	}

	for _, nt := range req.Notetypes {
		cs := notetypeChecksum(nt.Name, nt.Definition)
		existing, err := h.loadNotetype(r, claims.UserID, nt.NotetypeID)
		notFound := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !notFound {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if !force && !notFound && existing.Checksum != nt.BaseChecksum && existing.Checksum != cs {
			resp.Conflicts["notetypes"] = append(resp.Conflicts["notetypes"],
				conflictEntry{NotetypeID: nt.NotetypeID, Server: existing, Client: nt})
			continue
		}
		if _, err := h.upsertNotetype(ctx, claims.UserID, claims.ClientID, nt.NotetypeID, nt.putNotetypeRequest, cs, now); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		resp.Accepted["notetypes"]++
	}

	for _, d := range req.Decks {
		if d.Config == nil {
			d.Config = map[string]any{}
		}
		cs := deckChecksum(d.Config)
		existing, err := h.loadDeck(r, claims.UserID, d.Name)
		notFound := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !notFound {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if !force && !notFound && existing.Checksum != d.BaseChecksum && existing.Checksum != cs {
			resp.Conflicts["decks"] = append(resp.Conflicts["decks"],
				conflictEntry{Name: d.Name, Server: existing, Client: d})
			continue
		}
		if _, err := h.upsertDeck(ctx, claims.UserID, claims.ClientID, d.Name, d.putDeckRequest, cs, now); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		resp.Accepted["decks"]++
	}

	writeJSON(w, http.StatusOK, resp)
}

type batchCardKey struct {
	NoteGUID string `json:"note_guid"`
	Ord      int    `json:"ord"`
}

type batchStudyDayKey struct {
	Day      int64  `json:"day"`
	DeckName string `json:"deck_name"`
}

type batchTombstoneKey struct {
	Type       string `json:"type"`
	ResourceID string `json:"resource_id"`
}

type batchPullRequest struct {
	Notes             []string            `json:"notes"`
	Cards             []int64             `json:"cards"`
	CardKeys          []batchCardKey      `json:"card_keys"`
	Reviews           []int64             `json:"reviews"`
	ReviewRetractions []int64             `json:"review_retractions"`
	StudyDays         []batchStudyDayKey  `json:"study_days"`
	Notetypes         []int64             `json:"notetypes"`
	Decks             []string            `json:"decks"`
	Media             []string            `json:"media"`
	Tombstones        []batchTombstoneKey `json:"tombstones"`
	TombstoneCardKeys []batchCardKey      `json:"tombstone_card_keys"`
}

type batchPullResponse struct {
	Notes             []model.Note             `json:"notes"`
	Cards             []model.Card             `json:"cards"`
	Reviews           []model.Review           `json:"reviews"`
	ReviewRetractions []model.ReviewRetraction `json:"review_retractions"`
	StudyDays         []model.StudyDay         `json:"study_days"`
	Notetypes         []model.Notetype         `json:"notetypes"`
	Decks             []model.Deck             `json:"decks"`
	Media             []model.Media            `json:"media"`
	Tombstones        []model.Tombstone        `json:"tombstones"`
}

// BatchPull returns full records for the requested identifiers.
type batchDeleteRequest struct {
	Notes     []string `json:"notes"`
	Cards     []int64  `json:"cards"`
	Notetypes []int64  `json:"notetypes"`
	Decks     []string `json:"decks"`
}

type batchDeleteResponse struct {
	Requested map[string]int `json:"requested"`
	Deleted   map[string]int `json:"deleted"`
}

// BatchDelete removes many resources transactionally and writes matching
// tombstones. The client plans deletion from a scope-bound local snapshot; the
// server limits request size as a final defense against accidental mass wipes.
func (h *Handler) BatchDelete(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	var req batchDeleteRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	total := len(req.Notes) + len(req.Cards) + len(req.Notetypes) + len(req.Decks)
	if total == 0 {
		writeJSON(w, http.StatusOK, batchDeleteResponse{
			Requested: map[string]int{"notes": 0, "cards": 0, "notetypes": 0, "decks": 0},
			Deleted:   map[string]int{"notes": 0, "cards": 0, "notetypes": 0, "decks": 0},
		})
		return
	}
	if total > 12000 {
		writeError(w, http.StatusBadRequest, "too_many_deletes", "batch delete is limited to 12000 resources")
		return
	}

	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck

	deleted := map[string]int{"notes": 0, "cards": 0, "notetypes": 0, "decks": 0}
	var cardsDeleted int
	if err := tx.QueryRow(r.Context(), `
		WITH removed AS (
		 DELETE FROM cards WHERE user_id=$1 AND card_id=ANY($2::bigint[])
		 RETURNING card_id,note_guid,ord
		), requested AS (SELECT value AS card_id FROM unnest($2::bigint[]) value),
		stones AS (
		 INSERT INTO tombstones(user_id,type,resource_id,last_client_id,note_guid,card_ord)
		 SELECT $1,'card',requested.card_id::text,$3,removed.note_guid,removed.ord
		 FROM requested LEFT JOIN removed USING(card_id)
		 ON CONFLICT(user_id,type,resource_id) DO UPDATE SET
		  deleted_at=now(),last_client_id=EXCLUDED.last_client_id,
		  note_guid=COALESCE(EXCLUDED.note_guid,tombstones.note_guid),
		  card_ord=COALESCE(EXCLUDED.card_ord,tombstones.card_ord)
		 RETURNING 1)
		SELECT count(*) FROM removed`, claims.UserID, req.Cards, claims.ClientID).Scan(&cardsDeleted); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not delete cards")
		return
	}
	deleted["cards"] = cardsDeleted
	types := []struct {
		name      string
		values    any
		deleteSQL string
		stoneSQL  string
	}{
		{"notes", req.Notes,
			`DELETE FROM notes WHERE user_id=$1 AND guid=ANY($2::text[])`,
			`INSERT INTO tombstones(user_id,type,resource_id,last_client_id)
			 SELECT $1,'note',value,$3 FROM unnest($2::text[]) value
			 ON CONFLICT(user_id,type,resource_id) DO UPDATE SET deleted_at=now(),last_client_id=EXCLUDED.last_client_id`},
		{"notetypes", req.Notetypes,
			`DELETE FROM notetypes WHERE user_id=$1 AND notetype_id=ANY($2::bigint[])`,
			`INSERT INTO tombstones(user_id,type,resource_id,last_client_id)
			 SELECT $1,'notetype',value::text,$3 FROM unnest($2::bigint[]) value
			 ON CONFLICT(user_id,type,resource_id) DO UPDATE SET deleted_at=now(),last_client_id=EXCLUDED.last_client_id`},
		{"decks", req.Decks,
			`DELETE FROM decks WHERE user_id=$1 AND name=ANY($2::text[])`,
			`INSERT INTO tombstones(user_id,type,resource_id,last_client_id)
			 SELECT $1,'deck',value,$3 FROM unnest($2::text[]) value
			 ON CONFLICT(user_id,type,resource_id) DO UPDATE SET deleted_at=now(),last_client_id=EXCLUDED.last_client_id`},
	}
	for _, item := range types {
		tag, err := tx.Exec(r.Context(), item.deleteSQL, claims.UserID, item.values)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		deleted[item.name] = int(tag.RowsAffected())
		if _, err := tx.Exec(r.Context(), item.stoneSQL, claims.UserID, item.values, claims.ClientID); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, batchDeleteResponse{
		Requested: map[string]int{"notes": len(req.Notes), "cards": len(req.Cards), "notetypes": len(req.Notetypes), "decks": len(req.Decks)},
		Deleted:   deleted,
	})
}

func (h *Handler) BatchPull(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	var req batchPullRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	total := len(req.Notes) + len(req.Cards) + len(req.CardKeys) + len(req.Reviews) + len(req.ReviewRetractions) +
		len(req.StudyDays) + len(req.Notetypes) + len(req.Decks) + len(req.Media) + len(req.Tombstones) + len(req.TombstoneCardKeys)
	if total > batchPullLimit {
		writeError(w, http.StatusBadRequest, "too_many_records", "batch pull is limited to 1000000 identifiers")
		return
	}
	resp := batchPullResponse{
		Notes: []model.Note{}, Cards: []model.Card{}, Reviews: []model.Review{},
		ReviewRetractions: []model.ReviewRetraction{}, StudyDays: []model.StudyDay{}, Notetypes: []model.Notetype{},
		Decks: []model.Deck{}, Media: []model.Media{}, Tombstones: []model.Tombstone{},
	}
	for _, key := range req.CardKeys {
		if key.NoteGUID == "" || key.Ord < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid logical card key")
			return
		}
	}
	// Set-based loads: whole collections are pulled in one request, so one
	// query per identifier does not scale.
	if notes, err := h.loadNotes(r.Context(), claims.UserID, req.Notes); err == nil {
		resp.Notes = notes
	} else {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if cards, err := h.loadCardsByIDs(r.Context(), claims.UserID, req.Cards); err == nil {
		resp.Cards = append(resp.Cards, cards...)
	} else {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if cards, err := h.loadCardsByKeys(r.Context(), claims.UserID, req.CardKeys); err == nil {
		resp.Cards = append(resp.Cards, cards...)
	} else {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if reviews, err := h.loadReviews(r.Context(), claims.UserID, req.Reviews); err == nil {
		resp.Reviews = reviews
	} else {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if retractions, err := loadReviewRetractions(r.Context(), h.DB, claims.UserID, req.ReviewRetractions); err == nil {
		resp.ReviewRetractions = retractions
	} else {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for _, key := range req.StudyDays {
		var day model.StudyDay
		err := h.DB.QueryRow(r.Context(), `SELECT id,user_id,day,deck_name,new_studied,
			review_studied,learning_studied,milliseconds_studied,modified_at,
			COALESCE(last_client_id::text,'') FROM study_days
			WHERE user_id=$1 AND day=$2 AND deck_name=$3`, claims.UserID, key.Day, key.DeckName).Scan(
			&day.ID, &day.UserID, &day.Day, &day.DeckName, &day.NewStudied,
			&day.ReviewStudied, &day.LearningStudied, &day.MillisecondsStudied,
			&day.ModifiedAt, &day.LastClientID)
		if err == nil {
			resp.StudyDays = append(resp.StudyDays, day)
		}
	}
	if notetypes, err := h.loadNotetypes(r.Context(), claims.UserID, req.Notetypes); err == nil {
		resp.Notetypes = notetypes
	} else {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if decks, err := h.loadDecks(r.Context(), claims.UserID, req.Decks); err == nil {
		resp.Decks = decks
	} else {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for _, filename := range req.Media {
		var media model.Media
		err := h.DB.QueryRow(r.Context(), `SELECT id,user_id,filename,size_bytes,storage_key,
			ref_count,modified_at,COALESCE(last_client_id::text,'') FROM media
			WHERE user_id=$1 AND filename=$2`, claims.UserID, filename).Scan(
			&media.ID, &media.UserID, &media.Filename, &media.SizeBytes, &media.StorageKey,
			&media.RefCount, &media.ModifiedAt, &media.LastClientID)
		if err == nil {
			resp.Media = append(resp.Media, media)
		}
	}
	loadTombstone := func(query string, args ...any) error {
		var tombstone model.Tombstone
		err := h.DB.QueryRow(r.Context(), query, args...).Scan(&tombstone.ID, &tombstone.UserID,
			&tombstone.Type, &tombstone.ResourceID, &tombstone.DeletedAt, &tombstone.LastClientID,
			&tombstone.NoteGUID, &tombstone.CardOrd)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		resp.Tombstones = append(resp.Tombstones, tombstone)
		return nil
	}
	for _, key := range req.Tombstones {
		if err := loadTombstone(`SELECT id,user_id,type,resource_id,deleted_at,last_client_id,
			COALESCE(note_guid,''),card_ord FROM tombstones
			WHERE user_id=$1 AND type=$2 AND resource_id=$3`, claims.UserID, key.Type, key.ResourceID); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "could not load tombstone")
			return
		}
	}
	for _, key := range req.TombstoneCardKeys {
		if key.NoteGUID == "" || key.Ord < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid logical tombstone card key")
			return
		}
		if err := loadTombstone(`SELECT id,user_id,type,resource_id,deleted_at,last_client_id,
			COALESCE(note_guid,''),card_ord FROM tombstones
			WHERE user_id=$1 AND type='card' AND note_guid=$2 AND card_ord=$3`,
			claims.UserID, key.NoteGUID, key.Ord); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "could not load tombstone")
			return
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
