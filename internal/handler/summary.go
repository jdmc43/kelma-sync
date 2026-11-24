package handler

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// These endpoints are intentionally separate from the sync manifest. Kelma
// Immersion needs small read models (deck names, ids, and current card state),
// not every collection checksum followed by batch pulls of full records.

type deckSummaryRow struct {
	Name      string `json:"name"`
	CardCount int64  `json:"card_count"`
}

type decksSummaryResponse struct {
	Decks []deckSummaryRow `json:"decks"`
}

type deckCardIDsResponse struct {
	DeckName string  `json:"deck_name"`
	CardIDs  []int64 `json:"card_ids"`
}

type cardStateSummary struct {
	CardID            int64    `json:"card_id"`
	NoteGUID          string   `json:"note_guid"`
	DeckName          string   `json:"deck_name"`
	Type              int64    `json:"type"`
	Queue             int64    `json:"queue"`
	Ivl               int64    `json:"ivl"`
	ProjectionStatus  string   `json:"projection_status,omitempty"`
	StudyState        string   `json:"study_state"`
	DueDateOverrideMS int64    `json:"due_date_override_ms"`
	Fields            []string `json:"fields"`
}

type cardStatesSummaryResponse struct {
	Decks []deckSummaryRow   `json:"decks"`
	Cards []cardStateSummary `json:"cards"`
}

// GetDecksSummary returns one compact row per deck, including decks inferred
// from cards written by older clients before the explicit deck row existed.
func (h *Handler) GetDecksSummary(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	decks, err := h.loadDeckSummaries(r, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, decksSummaryResponse{Decks: decks})
}

// GetDeckCardIDsSummary returns only the ids in one deck. It replaces a full
// manifest plus pulling every card in the account just to filter their ids.
func (h *Handler) GetDeckCardIDsSummary(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	deckName := r.PathValue("name")
	if err := h.requireSummaryDeck(r, claims.UserID, deckName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "deck not found")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}

	rows, err := h.DB.Query(r.Context(),
		`SELECT card_id FROM cards
		 WHERE user_id=$1 AND deck_name=$2 ORDER BY card_id`,
		claims.UserID, deckName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, deckCardIDsResponse{DeckName: deckName, CardIDs: ids})
}

// GetDeckCardStatesSummary returns the exact text/state projection needed by
// one-deck recognition and imports, in one indexed query.
func (h *Handler) GetDeckCardStatesSummary(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	deckName := r.PathValue("name")
	if err := h.requireSummaryDeck(r, claims.UserID, deckName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "deck not found")
		} else {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	cards, err := h.loadCardStateSummaries(r, claims.UserID, deckName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cardStatesSummaryResponse{
		Decks: []deckSummaryRow{},
		Cards: cards,
	})
}

// GetAllCardStatesSummary returns the card-mirror projection account-wide. It
// is still proportional to the requested summaries, but carries no manifests,
// checksums, notetype/deck definitions, or follow-up batch-pull round trips.
func (h *Handler) GetAllCardStatesSummary(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	decks, err := h.loadDeckSummaries(r, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	cards, err := h.loadCardStateSummaries(r, claims.UserID, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cardStatesSummaryResponse{Decks: decks, Cards: cards})
}

func (h *Handler) requireSummaryDeck(r *http.Request, userID, deckName string) error {
	var exists bool
	err := h.DB.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM decks WHERE user_id=$1 AND name=$2)
		     OR EXISTS(SELECT 1 FROM cards WHERE user_id=$1 AND deck_name=$2)`,
		userID, deckName).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return pgx.ErrNoRows
	}
	return nil
}

func (h *Handler) loadDeckSummaries(r *http.Request, userID string) ([]deckSummaryRow, error) {
	rows, err := h.DB.Query(r.Context(),
		`WITH names AS (
		   SELECT name FROM decks WHERE user_id=$1
		   UNION
		   SELECT deck_name AS name FROM cards WHERE user_id=$1
		 )
		 SELECT names.name, COUNT(cards.card_id)
		 FROM names
		 LEFT JOIN cards ON cards.user_id=$1 AND cards.deck_name=names.name
		 GROUP BY names.name
		 ORDER BY lower(names.name), names.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []deckSummaryRow{}
	for rows.Next() {
		var row deckSummaryRow
		if err := rows.Scan(&row.Name, &row.CardCount); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (h *Handler) loadCardStateSummaries(r *http.Request, userID, deckName string) ([]cardStateSummary, error) {
	rows, err := h.DB.Query(r.Context(),
		`SELECT c.card_id, c.note_guid, c.deck_name,
		        CASE WHEN c.schedule_reset_through_review_id>COALESCE(p.applied_through_review_id,0)
		          THEN 0 WHEN p.user_id IS NULL OR p.applied_history_hash IS NOT NULL
		          THEN COALESCE((c.scheduling->>'type')::bigint,0) ELSE 0 END,
		        CASE WHEN c.study_state='suspended' THEN -1
		          WHEN c.schedule_reset_through_review_id>COALESCE(p.applied_through_review_id,0)
		          THEN 0 WHEN p.user_id IS NULL OR p.applied_history_hash IS NOT NULL
		          THEN COALESCE((c.scheduling->>'queue')::bigint,0) ELSE 0 END,
		        CASE WHEN c.schedule_reset_through_review_id>COALESCE(p.applied_through_review_id,0)
		          THEN 0 WHEN p.user_id IS NULL OR p.applied_history_hash IS NOT NULL
		          THEN COALESCE((c.scheduling->>'ivl')::bigint,0) ELSE 0 END,
		        COALESCE(p.status,''),c.study_state,c.due_date_override_ms,
		        COALESCE(n.fields, '[]'::jsonb)
		 FROM cards c
		 LEFT JOIN notes n ON n.user_id=c.user_id AND n.guid=c.note_guid
		 LEFT JOIN scheduler_projections p ON p.user_id=c.user_id
		   AND p.note_guid=c.note_guid AND p.card_ord=c.ord
		 WHERE c.user_id=$1 AND ($2='' OR c.deck_name=$2)
		 ORDER BY lower(c.deck_name), c.deck_name, c.card_id`,
		userID, deckName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []cardStateSummary{}
	for rows.Next() {
		var row cardStateSummary
		if err := rows.Scan(&row.CardID, &row.NoteGUID, &row.DeckName,
			&row.Type, &row.Queue, &row.Ivl, &row.ProjectionStatus,
			&row.StudyState, &row.DueDateOverrideMS, &row.Fields); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Review time per deck per day, from the review history (the revlog), for
// Kelma Immersion's progress: reviews done in any client count on the day they
// were done. Days are the learner's own calendar days (offset_minutes ahead of
// UTC, as the client says); each answer counts at most 60 s, like Anki's
// default answer-time cap. Undone (retracted) reviews don't count, as in the
// other statistics.
//
// GET /v2/summary/review-time?since_ms=&offset_minutes=
//
//	since_ms        reviews from this moment (Anki review ids are epoch ms);
//	                default and floor: 400 days ago
//	offset_minutes  -840..840
type reviewTimeRow struct {
	DeckName string `json:"deck_name"`
	Day      string `json:"day"`
	Reviews  int64  `json:"reviews"`
	Seconds  int64  `json:"seconds"`
}

type reviewTimeResponse struct {
	Days []reviewTimeRow `json:"days"`
}

const (
	reviewTimeMaxDays      = 400
	reviewTimeMaxAnswerMS  = 60_000
	reviewTimeMaxOffsetMin = 14 * 60
)

func (h *Handler) GetReviewTimeSummary(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	floor := time.Now().Add(-reviewTimeMaxDays * 24 * time.Hour).UnixMilli()
	since := floor
	if raw := r.URL.Query().Get("since_ms"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "since_ms must be epoch milliseconds")
			return
		}
		if value > since {
			since = value
		}
	}
	offset := int64(0)
	if raw := r.URL.Query().Get("offset_minutes"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < -reviewTimeMaxOffsetMin || value > reviewTimeMaxOffsetMin {
			writeError(w, http.StatusBadRequest, "bad_request", "offset_minutes must be between -840 and 840")
			return
		}
		offset = value
	}
	rows, err := h.DB.Query(r.Context(),
		`SELECT deck_name,
		        to_char(to_timestamp((review_id + $3::bigint) / 1000.0) AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day,
		        COUNT(*),
		        COALESCE(SUM(LEAST(GREATEST(taken_millis, 0), $4::int)), 0) / 1000
		 FROM reviews
		 WHERE user_id = $1 AND review_id >= $2
		   AND NOT EXISTS (SELECT 1 FROM review_retractions rr WHERE rr.user_id = reviews.user_id AND rr.review_id = reviews.review_id)
		 GROUP BY 1, 2
		 ORDER BY 2, 1`,
		claims.UserID, since, offset*60_000, reviewTimeMaxAnswerMS)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer rows.Close()
	days := []reviewTimeRow{}
	for rows.Next() {
		var row reviewTimeRow
		if err := rows.Scan(&row.DeckName, &row.Day, &row.Reviews, &row.Seconds); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		days = append(days, row)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, reviewTimeResponse{Days: days})
}
