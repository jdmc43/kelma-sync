package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jdmc43/kelma-sync/internal/model"
)

// GetCard returns the full card record for a card_id.
func (h *Handler) GetCard(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	cardID, err := strconv.ParseInt(r.PathValue("card_id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "card_id must be an integer")
		return
	}
	c, err := h.loadCard(r, claims.UserID, cardID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "card not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

type putCardRequest struct {
	NoteGUID                        string         `json:"note_guid"`
	DeckName                        string         `json:"deck_name"`
	Ord                             int            `json:"ord"`
	Scheduling                      map[string]any `json:"scheduling"`
	CreatedAt                       *time.Time     `json:"created_at,omitempty"`
	StudyState                      *string        `json:"study_state,omitempty"`
	StudyStateClientModifiedAt      *time.Time     `json:"study_state_client_modified_at,omitempty"`
	ScheduleResetThroughReviewID    *int64         `json:"schedule_reset_through_review_id,omitempty"`
	ScheduleResetClientModifiedAt   *time.Time     `json:"schedule_reset_client_modified_at,omitempty"`
	DueDateOverrideMS               *int64         `json:"due_date_override_ms,omitempty"`
	DueDateOverrideClientModifiedAt *time.Time     `json:"due_date_override_client_modified_at,omitempty"`
	ClientModifiedAt                time.Time      `json:"client_modified_at"`
}

// PutCard upserts a card using per-card timestamp comparison. If the stored
// card was modified more recently by the client clock, the incoming push is
// ignored and the existing record is returned (silent newest-wins).
func (h *Handler) PutCard(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	cardID, err := strconv.ParseInt(r.PathValue("card_id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "card_id must be an integer")
		return
	}
	var req putCardRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Scheduling == nil {
		req.Scheduling = map[string]any{}
	}
	if err := validateStudyState(req.StudyState); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.DueDateOverrideMS != nil &&
		(*req.DueDateOverrideMS < 0 || *req.DueDateOverrideMS%86_400_000 != 0) {
		writeError(w, http.StatusBadRequest, "bad_request", "due_date_override_ms must be 0 or a UTC day boundary")
		return
	}
	now, normalizedClientTime := utcWriteTimes(time.Now(), req.ClientModifiedAt)
	req.ClientModifiedAt = normalizedClientTime

	// Resolve by logical identity so a device pushing its own card_id updates the
	// existing logical card instead of creating a duplicate.
	existing, err := h.loadCardLogical(r, claims.UserID, req.NoteGUID, req.Ord)
	notFound := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !notFound {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !notFound {
		if _, err := h.fillCardCreatedAt(
			r.Context(), claims.UserID, req.NoteGUID, req.Ord, existing.CardID, req.CreatedAt, now,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if _, err := h.updateCardStudyState(
			r.Context(), claims.UserID, claims.ClientID, req.NoteGUID, req.Ord, req, now,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if _, err := h.updateCardScheduleReset(
			r.Context(), claims.UserID, claims.ClientID, req.NoteGUID, req.Ord, req, now,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if _, err := h.updateCardDueDateOverride(
			r.Context(), claims.UserID, claims.ClientID, req.NoteGUID, req.Ord, req, now,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		// Card content, study state, schedule reset, and due-date override have independent clocks. A stale content
		// upload may still contain a valid newer suspension transition.
		if existing.ClientModifiedAt.After(req.ClientModifiedAt) {
			saved, err := h.loadCardLogical(r, claims.UserID, req.NoteGUID, req.Ord)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, saved)
			return
		}
		cardID = existing.CardID // keep canonical id
	}

	saved, err := h.upsertCard(r.Context(), claims.UserID, claims.ClientID, cardID, req, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// DeleteCard soft-deletes a card by writing a tombstone.
func (h *Handler) DeleteCard(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	cardIDStr := r.PathValue("card_id")
	cardID, err := strconv.ParseInt(cardIDStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "card_id must be an integer")
		return
	}
	var noteGUID string
	var ord int
	var ordValue *int
	err = h.DB.QueryRow(r.Context(),
		`DELETE FROM cards WHERE user_id=$1 AND card_id=$2 RETURNING note_guid,ord`, claims.UserID, cardID).Scan(&noteGUID, &ord)
	if err == nil {
		ordValue = &ord
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := h.writeCardTombstone(r, claims.UserID, cardIDStr, claims.ClientID, noteGUID, ordValue); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) loadCard(r *http.Request, userID string, cardID int64) (model.Card, error) {
	return h.loadCardWhere(r, userID, "c.card_id = $2", cardID)
}

// loadCardLogical loads a card by its cross-device identity (note_guid, ord).
func (h *Handler) loadCardLogical(r *http.Request, userID, noteGUID string, ord int) (model.Card, error) {
	return h.loadCardWhere(r, userID, "c.note_guid = $2 AND c.ord = $3", noteGUID, ord)
}

func (h *Handler) loadCardWhere(
	r *http.Request,
	userID string,
	where string,
	args ...any,
) (model.Card, error) {
	cards, err := h.loadCardsWhere(r.Context(), userID, where, args...)
	if err != nil {
		return model.Card{}, err
	}
	if len(cards) == 0 {
		return model.Card{}, pgx.ErrNoRows
	}
	return cards[0], nil
}

// cardSelectSQL is the shared card projection; callers append a WHERE clause
// that starts with the user filter (see loadCardsWhere).
const cardSelectSQL = `SELECT c.id,c.user_id,c.card_id,c.note_guid,c.deck_name,c.ord,c.scheduling,c.created_at,
	               c.study_state,c.study_state_modified_at,c.study_state_client_modified_at,
	               COALESCE(c.study_state_client_id::text,''),
	               c.schedule_reset_through_review_id,c.schedule_reset_modified_at,
	               c.schedule_reset_client_modified_at,COALESCE(c.schedule_reset_client_id::text,''),
	               c.due_date_override_ms,c.due_date_override_modified_at,
	               c.due_date_override_client_modified_at,COALESCE(c.due_date_override_client_id::text,''),
	               COALESCE(c.flag_intent_id::text,''),c.flag_modified_at,c.flag_client_modified_at,
	               COALESCE(c.flag_client_id::text,''),COALESCE(flag_client.label,''),
	               CASE WHEN jsonb_typeof(c.scheduling->'flags')='number'
	                    THEN (c.scheduling->>'flags')::int ELSE 0 END,
	               c.modified_at,c.client_modified_at,
	               COALESCE(c.last_client_id::text,''),COALESCE(cl.label,''),
	               COALESCE(p.status,''),COALESCE(p.desired_history_hash,''),
	               COALESCE(p.desired_through_review_id,0),COALESCE(p.desired_profile_version,0),
	               COALESCE(p.applied_history_hash,''),p.applied_through_review_id,
	               p.applied_profile_version,COALESCE(p.last_error_code,'')
	        FROM cards c LEFT JOIN clients cl ON cl.id=c.last_client_id
	        LEFT JOIN clients flag_client ON flag_client.id=c.flag_client_id
	        LEFT JOIN scheduler_projections p ON p.user_id=c.user_id
	          AND p.note_guid=c.note_guid AND p.card_ord=c.ord
	        WHERE c.user_id=$1 AND `

// loadCardsWhere returns every card of userID matching where ($2... are args).
func (h *Handler) loadCardsWhere(
	ctx context.Context,
	userID string,
	where string,
	args ...any,
) ([]model.Card, error) {
	params := append([]any{userID}, args...)
	rows, err := h.DB.Query(ctx, cardSelectSQL+where, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Card{}
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanCard(row pgx.Row) (model.Card, error) {
	var c model.Card
	var status, desiredHash, appliedHash, errorCode string
	var flagIntentID, flagClientID, flagClientLabel string
	var flagValue int
	var flagModifiedAt, flagClientModifiedAt *time.Time
	var desiredThrough, desiredProfile int64
	var appliedThrough, appliedProfile sql.NullInt64
	err := row.Scan(
		&c.ID, &c.UserID, &c.CardID, &c.NoteGUID, &c.DeckName, &c.Ord, &c.Scheduling, &c.CreatedAt,
		&c.StudyState, &c.StudyStateModifiedAt, &c.StudyStateClientModifiedAt, &c.StudyStateClientID,
		&c.ScheduleResetThroughReviewID, &c.ScheduleResetModifiedAt,
		&c.ScheduleResetClientModifiedAt, &c.ScheduleResetClientID,
		&c.DueDateOverrideMS, &c.DueDateOverrideModifiedAt,
		&c.DueDateOverrideClientModifiedAt, &c.DueDateOverrideClientID,
		&flagIntentID, &flagModifiedAt, &flagClientModifiedAt, &flagClientID, &flagClientLabel, &flagValue,
		&c.ModifiedAt, &c.ClientModifiedAt, &c.LastClientID, &c.LastClientLabel,
		&status, &desiredHash, &desiredThrough, &desiredProfile, &appliedHash,
		&appliedThrough, &appliedProfile, &errorCode,
	)
	if err != nil {
		return c, err
	}
	if flagIntentID != "" {
		c.Flag = &model.CardFlag{Flag: flagValue, IntentID: flagIntentID,
			ModifiedAt: flagModifiedAt.UTC(), ClientModifiedAt: flagClientModifiedAt.UTC(),
			LastClientID: flagClientID, LastClientLabel: flagClientLabel}
	}
	setCardProjection(&c, status, desiredHash, desiredThrough, desiredProfile,
		appliedHash, appliedThrough, appliedProfile, errorCode)
	return c, nil
}

func validateStudyState(state *string) error {
	if state == nil || *state == "active" || *state == "suspended" {
		return nil
	}
	return errors.New("study_state must be active or suspended")
}

func setCardProjection(
	card *model.Card,
	status, desiredHash string,
	desiredThrough, desiredProfile int64,
	appliedHash string,
	appliedThrough, appliedProfile sql.NullInt64,
	errorCode string,
) {
	if status == "" {
		return
	}
	projection := &model.SchedulingProjection{
		Status: status, DesiredHistoryHash: desiredHash,
		DesiredThroughReviewID: desiredThrough, DesiredProfileVersion: desiredProfile,
		AppliedHistoryHash: appliedHash, LastErrorCode: errorCode,
	}
	if appliedThrough.Valid {
		projection.AppliedThroughReviewID = &appliedThrough.Int64
	}
	if appliedProfile.Valid {
		projection.AppliedProfileVersion = &appliedProfile.Int64
	}
	card.Projection = projection
}
