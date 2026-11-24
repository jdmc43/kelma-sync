package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jdmc43/kelma-sync/internal/model"
	"github.com/jdmc43/kelma-sync/internal/projection"
)

const cardFlagSyncCapability = "card_flag_intents_v1"

type cardFlagRequest struct {
	Flag             *int      `json:"flag"`
	IntentID         string    `json:"intent_id"`
	ClientModifiedAt time.Time `json:"client_modified_at"`
}

type batchCardFlag struct {
	NoteGUID string `json:"note_guid"`
	CardOrd  int    `json:"card_ord"`
	cardFlagRequest
}

type cardFlagResult struct {
	NoteGUID string         `json:"note_guid,omitempty"`
	CardOrd  int            `json:"card_ord"`
	Accepted bool           `json:"accepted"`
	Applied  bool           `json:"applied"`
	Flag     model.CardFlag `json:"flag"`
}

var errCardFlagIntentConflict = errors.New("card flag intent payload conflict")

func schedulingFlag(scheduling map[string]any) int {
	value, ok := scheduling["flags"]
	if !ok {
		return 0
	}
	var flag int
	switch typed := value.(type) {
	case int:
		flag = typed
	case int64:
		flag = int(typed)
	case float64:
		flag = int(typed)
	default:
		return 0
	}
	if flag < 0 || flag > 7 {
		return 0
	}
	return flag
}

func (h *Handler) PutCardFlag(w http.ResponseWriter, r *http.Request) {
	cardID, err := strconv.ParseInt(r.PathValue("card_id"), 10, 64)
	if err != nil || cardID <= 0 {
		writeError(w, http.StatusBadRequest, "bad_card_id", "invalid card id")
		return
	}
	var req cardFlagRequest
	if err := decode(r, &req); err != nil || validateCardFlagRequest(req, time.Now().UTC()) != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid card flag intent")
		return
	}
	claims := claimsFrom(r)
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", "database error")
		return
	}
	defer tx.Rollback(r.Context())
	if err := projection.LockAccountTx(r.Context(), tx, claims.UserID); err != nil {
		writeError(w, http.StatusServiceUnavailable, "card_flag_unavailable", "could not lock card flag account")
		return
	}
	var noteGUID string
	var ord int
	if err := tx.QueryRow(r.Context(), `SELECT note_guid,ord FROM cards
		WHERE user_id=$1 AND card_id=$2 FOR UPDATE`, claims.UserID, cardID).Scan(&noteGUID, &ord); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "card not found")
		} else {
			writeError(w, http.StatusInternalServerError, "db_error", "database error")
		}
		return
	}
	result, err := applyCardFlagTx(r.Context(), tx, claims.UserID, claims.ClientID,
		noteGUID, ord, req, time.Now().UTC(), false)
	if err != nil {
		if errors.Is(err, errCardFlagIntentConflict) {
			writeJSON(w, http.StatusConflict, model.ConflictResponse{
				Error: "flag_intent_conflict", Server: result.Flag, Client: req,
			})
		} else {
			writeError(w, http.StatusInternalServerError, "db_error", "database error")
		}
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", "database error")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func validateCardFlagRequest(req cardFlagRequest, now time.Time) error {
	if req.Flag == nil || *req.Flag < 0 || *req.Flag > 7 || !noteMarkUUID.MatchString(req.IntentID) ||
		req.ClientModifiedAt.IsZero() || req.ClientModifiedAt.After(now.Add(maxFutureClockSkew)) {
		return errors.New("invalid card flag intent")
	}
	return nil
}

func applyCardFlagTx(
	ctx context.Context,
	tx pgx.Tx,
	userID, clientID, noteGUID string,
	ord int,
	req cardFlagRequest,
	now time.Time,
	advanceAfterCurrent bool,
) (cardFlagResult, error) {
	var currentFlag int
	var currentIntentID, currentClientID, currentClientLabel string
	var currentModifiedAt, currentClientModifiedAt *time.Time
	err := tx.QueryRow(ctx, `SELECT
		CASE WHEN jsonb_typeof(c.scheduling->'flags')='number'
			THEN (c.scheduling->>'flags')::int ELSE 0 END,
		COALESCE(c.flag_intent_id::text,''),c.flag_modified_at,c.flag_client_modified_at,
		COALESCE(c.flag_client_id::text,''),COALESCE(cl.label,'')
		FROM cards c LEFT JOIN clients cl ON cl.id=c.flag_client_id
		WHERE c.user_id=$1 AND c.note_guid=$2 AND c.ord=$3 FOR UPDATE OF c`,
		userID, noteGUID, ord).Scan(&currentFlag, &currentIntentID, &currentModifiedAt,
		&currentClientModifiedAt, &currentClientID, &currentClientLabel)
	if err != nil {
		return cardFlagResult{}, err
	}
	if currentFlag < 0 || currentFlag > 7 {
		currentFlag = 0
	}
	current := model.CardFlag{Flag: currentFlag, IntentID: currentIntentID,
		LastClientID: currentClientID, LastClientLabel: currentClientLabel}
	if currentModifiedAt != nil {
		current.ModifiedAt = currentModifiedAt.UTC()
	}
	if currentClientModifiedAt != nil {
		current.ClientModifiedAt = currentClientModifiedAt.UTC()
	}
	result := cardFlagResult{NoteGUID: noteGUID, CardOrd: ord, Accepted: true, Flag: current}
	if currentIntentID == req.IntentID {
		if currentFlag != *req.Flag || currentClientModifiedAt == nil ||
			!normalizedMarkTime(*currentClientModifiedAt).Equal(normalizedMarkTime(req.ClientModifiedAt)) {
			return result, errCardFlagIntentConflict
		}
		return result, nil
	}
	if advanceAfterCurrent && currentClientModifiedAt != nil && !req.ClientModifiedAt.After(*currentClientModifiedAt) {
		req.ClientModifiedAt = currentClientModifiedAt.Add(time.Microsecond)
	}
	incomingTime := normalizedMarkTime(req.ClientModifiedAt)
	if currentIntentID != "" && !(incomingTime.After(normalizedMarkTime(*currentClientModifiedAt)) ||
		(incomingTime.Equal(normalizedMarkTime(*currentClientModifiedAt)) &&
			strings.Compare(req.IntentID, currentIntentID) > 0)) {
		return result, nil
	}
	now, clientModifiedAt := utcWriteTimes(now, req.ClientModifiedAt)
	tag, err := tx.Exec(ctx, `UPDATE cards SET
		scheduling=jsonb_set(scheduling,'{flags}',to_jsonb($1::int),true),
		flag_intent_id=$2::uuid,flag_modified_at=$3,flag_client_modified_at=$4,flag_client_id=$5::uuid
		WHERE user_id=$6 AND note_guid=$7 AND ord=$8`, *req.Flag, req.IntentID, now,
		clientModifiedAt, clientID, userID, noteGUID, ord)
	if err != nil || tag.RowsAffected() != 1 {
		return cardFlagResult{}, errors.New("card flag update failed")
	}
	result.Applied = true
	result.Flag = model.CardFlag{Flag: *req.Flag, IntentID: req.IntentID, ModifiedAt: now,
		ClientModifiedAt: clientModifiedAt, LastClientID: clientID}
	return result, nil
}

func (h *Handler) applyBatchCardFlags(w http.ResponseWriter, r *http.Request, flags []batchCardFlag,
	now time.Time, response *batchPushResponse,
) bool {
	claims, ctx := claimsFrom(r), r.Context()
	for _, flag := range flags {
		tx, err := h.DB.Begin(ctx)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "card_flag_unavailable", "could not start card flag")
			return false
		}
		if err := projection.LockAccountTx(ctx, tx, claims.UserID); err != nil {
			_ = tx.Rollback(ctx)
			writeError(w, http.StatusServiceUnavailable, "card_flag_unavailable", "could not lock card flag account")
			return false
		}
		result, err := applyCardFlagTx(ctx, tx, claims.UserID, claims.ClientID,
			flag.NoteGUID, flag.CardOrd, flag.cardFlagRequest, now, false)
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errCardFlagIntentConflict) {
			_ = tx.Rollback(ctx)
			var server any
			if errors.Is(err, errCardFlagIntentConflict) {
				server = result.Flag
			}
			response.Conflicts["card_flags"] = append(response.Conflicts["card_flags"],
				conflictEntry{GUID: flag.NoteGUID, Server: server, Client: flag})
			continue
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			writeError(w, http.StatusServiceUnavailable, "card_flag_unavailable", "could not apply card flag")
			return false
		}
		if err := tx.Commit(ctx); err != nil {
			writeError(w, http.StatusServiceUnavailable, "card_flag_unavailable", "could not commit card flag")
			return false
		}
		response.Accepted["card_flags"]++
		response.FlagResults = append(response.FlagResults, result)
	}
	return true
}
