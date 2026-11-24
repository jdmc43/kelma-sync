package handler

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestBatchReviewRetractionIsAccountScopedExactlyOnceAndRebuildsProjection(t *testing.T) {
	f := setupBrowserReview(t)
	intentID := "34343434-3434-4434-8434-343434343434"
	request := map[string]any{"review_retractions": []map[string]any{{
		"review_id": f.priorReviewID, "intent_id": intentID,
		"client_modified_at": f.now.Add(-time.Minute),
	}}}
	first := reqJSON(t, f.mux, "POST", "/v2/batch/push", request, f.token)
	if first.Code != http.StatusOK {
		t.Fatalf("retract=%d %s", first.Code, first.Body.String())
	}
	firstBody := decodeMap(t, first.Body.Bytes())
	if firstBody["accepted"].(map[string]any)["review_retractions"] != float64(1) ||
		len(firstBody["review_retractions"].([]any)) != 1 {
		t.Fatalf("first=%v", firstBody)
	}
	retry := reqJSON(t, f.mux, "POST", "/v2/batch/push", request, f.token)
	if retry.Code != http.StatusOK ||
		decodeMap(t, retry.Body.Bytes())["accepted"].(map[string]any)["review_retractions"] != float64(1) {
		t.Fatalf("retry=%d %s", retry.Code, retry.Body.String())
	}
	changed := map[string]any{"review_retractions": []map[string]any{{
		"review_id": f.priorReviewID + 1, "intent_id": intentID,
		"client_modified_at": f.now.Add(-time.Minute),
	}}}
	conflict := reqJSON(t, f.mux, "POST", "/v2/batch/push", changed, f.token)
	conflictBody := decodeMap(t, conflict.Body.Bytes())
	if conflict.Code != http.StatusOK ||
		conflictBody["accepted"].(map[string]any)["review_retractions"] != float64(0) ||
		len(conflictBody["conflicts"].(map[string]any)["review_retractions"].([]any)) != 1 {
		t.Fatalf("conflict=%d %s", conflict.Code, conflict.Body.String())
	}
	var reviews, retractions int
	var through int64
	if err := f.handler.DB.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM reviews WHERE user_id=$1),
		(SELECT count(*) FROM review_retractions WHERE user_id=$1),desired_through_review_id
		FROM scheduler_projections WHERE user_id=$1 AND note_guid='review-note' AND card_ord=0`, f.userID).Scan(
		&reviews, &retractions, &through,
	); err != nil || reviews != 1 || retractions != 1 || through != 0 {
		t.Fatalf("reviews=%d retractions=%d through=%d err=%v", reviews, retractions, through, err)
	}
}
