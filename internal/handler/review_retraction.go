package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jdmc43/kelma-sync/internal/model"
	"github.com/jdmc43/kelma-sync/internal/projection"
)

const reviewRetractionSyncCapability = "review_retractions_v1"

var errReviewRetractionIntentConflict = errors.New("review retraction intent payload conflict")

type reviewRetractionRequest struct {
	ReviewID         int64     `json:"review_id"`
	IntentID         string    `json:"intent_id"`
	ClientModifiedAt time.Time `json:"client_modified_at"`
}

type reviewRetractionResult struct {
	Accepted   bool                   `json:"accepted"`
	Applied    bool                   `json:"applied"`
	Retraction model.ReviewRetraction `json:"retraction"`
}

func validateReviewRetractionRequest(value reviewRetractionRequest, now time.Time) error {
	if value.ReviewID <= 0 || !noteMarkUUID.MatchString(value.IntentID) || value.ClientModifiedAt.IsZero() ||
		value.ClientModifiedAt.After(now.Add(maxFutureClockSkew)) {
		return errors.New("invalid review retraction intent")
	}
	return nil
}

func applyReviewRetractionTx(ctx context.Context, tx pgx.Tx, userID, clientID string,
	request reviewRetractionRequest, now time.Time,
) (reviewRetractionResult, error) {
	var intentReviewID int64
	err := tx.QueryRow(ctx, `SELECT review_id FROM review_retractions
		WHERE user_id=$1 AND intent_id=$2::uuid`, userID, request.IntentID).Scan(&intentReviewID)
	if err == nil && intentReviewID != request.ReviewID {
		return reviewRetractionResult{}, errReviewRetractionIntentConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return reviewRetractionResult{}, err
	}
	if err == nil {
		values, loadErr := loadReviewRetractions(ctx, tx, userID, []int64{request.ReviewID})
		if loadErr != nil {
			return reviewRetractionResult{}, loadErr
		}
		if len(values) != 1 {
			return reviewRetractionResult{}, errors.New("stored review retraction missing")
		}
		return reviewRetractionResult{Accepted: true, Retraction: values[0]}, nil
	}
	var noteGUID string
	var cardOrd int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(NULLIF(r.note_guid,''),c.note_guid,''),
		CASE WHEN r.note_guid='' THEN COALESCE(c.ord,r.card_ord) ELSE r.card_ord END
		FROM reviews r LEFT JOIN cards c ON c.user_id=r.user_id AND c.card_id=r.source_card_id
		WHERE r.user_id=$1 AND r.review_id=$2 FOR UPDATE OF r`, userID, request.ReviewID).Scan(
		&noteGUID, &cardOrd); err != nil {
		return reviewRetractionResult{}, err
	}
	values, err := loadReviewRetractions(ctx, tx, userID, []int64{request.ReviewID})
	if err != nil {
		return reviewRetractionResult{}, err
	}
	if len(values) == 1 {
		return reviewRetractionResult{Accepted: true, Retraction: values[0]}, nil
	}
	now, clientModifiedAt := utcWriteTimes(now, request.ClientModifiedAt)
	if _, err := tx.Exec(ctx, `INSERT INTO review_retractions
		(user_id,review_id,intent_id,client_modified_at,modified_at,last_client_id)
		VALUES($1,$2,$3::uuid,$4,$5,$6::uuid)`, userID, request.ReviewID, request.IntentID,
		clientModifiedAt, now, clientID); err != nil {
		return reviewRetractionResult{}, err
	}
	if noteGUID != "" {
		if err := projection.EnqueueReviewsTx(ctx, tx, userID, []projection.LogicalCard{{
			NoteGUID: noteGUID, CardOrd: cardOrd,
		}}); err != nil {
			return reviewRetractionResult{}, err
		}
	}
	return reviewRetractionResult{Accepted: true, Applied: true, Retraction: model.ReviewRetraction{
		ReviewID: request.ReviewID, IntentID: request.IntentID, ClientModifiedAt: clientModifiedAt,
		ModifiedAt: now, LastClientID: clientID,
	}}, nil
}

type reviewRetractionQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (h *Handler) applyBatchReviewRetractions(w http.ResponseWriter, r *http.Request,
	values []reviewRetractionRequest, now time.Time, response *batchPushResponse,
) bool {
	claims, ctx := claimsFrom(r), r.Context()
	for _, value := range values {
		tx, err := h.DB.Begin(ctx)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "review_retraction_unavailable", "could not start review retraction")
			return false
		}
		if err := projection.LockAccountTx(ctx, tx, claims.UserID); err != nil {
			_ = tx.Rollback(ctx)
			writeError(w, http.StatusServiceUnavailable, "review_retraction_unavailable", "could not lock review account")
			return false
		}
		result, err := applyReviewRetractionTx(ctx, tx, claims.UserID, claims.ClientID, value, now)
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errReviewRetractionIntentConflict) {
			_ = tx.Rollback(ctx)
			response.Conflicts["review_retractions"] = append(response.Conflicts["review_retractions"],
				conflictEntry{ReviewID: value.ReviewID, Client: value})
			continue
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			writeError(w, http.StatusServiceUnavailable, "review_retraction_unavailable", "could not apply review retraction")
			return false
		}
		if err := tx.Commit(ctx); err != nil {
			writeError(w, http.StatusServiceUnavailable, "review_retraction_unavailable", "could not commit review retraction")
			return false
		}
		response.Accepted["review_retractions"]++
		response.RetractionResults = append(response.RetractionResults, result)
	}
	return true
}

func loadReviewRetractions(ctx context.Context, db reviewRetractionQuerier, userID string, reviewIDs []int64) ([]model.ReviewRetraction, error) {
	rows, err := db.Query(ctx, `SELECT rr.review_id,rr.intent_id::text,rr.client_modified_at,
		rr.modified_at,COALESCE(rr.last_client_id::text,''),COALESCE(c.label,'')
		FROM review_retractions rr LEFT JOIN clients c ON c.id=rr.last_client_id
		WHERE rr.user_id=$1 AND rr.review_id=ANY($2::bigint[])
		ORDER BY rr.review_id`, userID, reviewIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ReviewRetraction{}
	for rows.Next() {
		var value model.ReviewRetraction
		if err := rows.Scan(&value.ReviewID, &value.IntentID, &value.ClientModifiedAt,
			&value.ModifiedAt, &value.LastClientID, &value.LastClientLabel); err != nil {
			return nil, err
		}
		value.ClientModifiedAt = value.ClientModifiedAt.UTC()
		value.ModifiedAt = value.ModifiedAt.UTC()
		out = append(out, value)
	}
	return out, rows.Err()
}
