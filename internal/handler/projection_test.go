package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jdmc43/kelma-sync/internal/model"
	"github.com/jdmc43/kelma-sync/internal/projection"
)

func TestAcceptedReviewsProduceManagedServerProjection(t *testing.T) {
	h, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "Kotlin")

	createProjectionCard(t, mux, token, "fsrs-card", 7001)
	reviews := []map[string]any{
		projectionReview(1735689600000, "fsrs-card", 3),
		projectionReview(1735690200000, "fsrs-card", 3),
	}
	pushReviews(t, mux, token, reviews)

	var status string
	var desiredHash string
	if err := pool.QueryRow(context.Background(),
		`SELECT status,desired_history_hash FROM scheduler_projections
		 WHERE note_guid='fsrs-card' AND card_ord=0`).Scan(&status, &desiredHash); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || desiredHash == "" {
		t.Fatalf("projection status/hash = %q/%q", status, desiredHash)
	}
	pendingCard := getProjectionCard(t, mux, token, 7001)
	if pendingCard.Projection == nil || pendingCard.Projection.Status != "pending" ||
		pendingCard.Projection.AppliedHistoryHash != "" {
		t.Fatalf("pending desired/applied state not observable: %+v", pendingCard.Projection)
	}
	rr := reqJSON(t, mux, "GET", "/v2/sync/manifest", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("projection manifest: got %d body=%s", rr.Code, rr.Body.String())
	}
	var manifest model.Manifest
	if err := json.Unmarshal(rr.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Cards) != 1 || manifest.Cards[0].ProjectionStatus != "pending" ||
		manifest.Cards[0].DesiredHistoryHash != desiredHash ||
		manifest.Cards[0].AppliedHistoryHash != "" {
		t.Fatalf("manifest projection identity missing: %+v", manifest.Cards)
	}
	rr = reqJSON(t, mux, "GET", "/v2/summary/card-states", nil, token)
	var stateSummary cardStatesSummaryResponse
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &stateSummary) != nil ||
		len(stateSummary.Cards) != 1 || stateSummary.Cards[0].Queue != 0 ||
		stateSummary.Cards[0].ProjectionStatus != "pending" {
		t.Fatalf("unapplied projection leaked into summary: code=%d body=%s", rr.Code, rr.Body.String())
	}

	processed, err := projection.NewWorker(h.DB).ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("process projection: processed=%v error=%v", processed, err)
	}

	card := getProjectionCard(t, mux, token, 7001)
	if number(card.Scheduling["queue"]) != 2 || number(card.Scheduling["ivl"]) != 2 || number(card.Scheduling["reps"]) != 2 {
		t.Fatalf("unexpected projected scheduling: %+v", card.Scheduling)
	}
	metadata, ok := card.Scheduling["_kelma_fsrs"].(map[string]any)
	if !ok || metadata["history_hash"] != desiredHash || number(metadata["through_review_id"]) != 1735690200000 {
		t.Fatalf("unexpected projection metadata: %+v", card.Scheduling["_kelma_fsrs"])
	}
	if data, _ := card.Scheduling["data"].(string); data == "" {
		t.Fatalf("projection omitted FSRS memory data: %+v", card.Scheduling)
	}
	if card.Projection == nil || card.Projection.Status != "succeeded" ||
		card.Projection.AppliedThroughReviewID == nil ||
		*card.Projection.AppliedThroughReviewID != 1735690200000 {
		t.Fatalf("projection observability missing from card: %+v", card.Projection)
	}
	rr = reqJSON(t, mux, "GET", "/v2/summary/card-states", nil, token)
	stateSummary = cardStatesSummaryResponse{}
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &stateSummary) != nil ||
		len(stateSummary.Cards) != 1 || stateSummary.Cards[0].Ivl != 2 ||
		stateSummary.Cards[0].ProjectionStatus != "succeeded" {
		t.Fatalf("applied projection missing from summary: code=%d body=%s", rr.Code, rr.Body.String())
	}

	var appliedHash string
	if err := pool.QueryRow(context.Background(),
		`SELECT status,applied_history_hash FROM scheduler_projections
		 WHERE note_guid='fsrs-card' AND card_ord=0`).Scan(&status, &appliedHash); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" || appliedHash != desiredHash {
		t.Fatalf("applied status/hash = %q/%q, desired %q", status, appliedHash, desiredHash)
	}
	pushReviews(t, mux, token, reviews)
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM scheduler_projections
		 WHERE note_guid='fsrs-card' AND card_ord=0`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" {
		t.Fatalf("identical review retry requeued projection: %q", status)
	}

	// Once managed, a legacy scheduling upload may still move card content but
	// cannot overwrite the server-owned compatibility projection.
	uploadedTime := time.Now().UTC().Add(time.Minute)
	rr = reqJSON(t, mux, "PUT", "/v2/cards/7001", map[string]any{
		"note_guid": "fsrs-card", "deck_name": "Deck::Moved", "ord": 0,
		"scheduling":                     map[string]any{"queue": -1, "ivl": 9999},
		"study_state":                    "suspended",
		"study_state_client_modified_at": uploadedTime.Format(time.RFC3339Nano),
		"client_modified_at":             uploadedTime.Format(time.RFC3339Nano),
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("legacy card push: got %d body=%s", rr.Code, rr.Body.String())
	}
	card = getProjectionCard(t, mux, token, 7001)
	if number(card.Scheduling["ivl"]) != 2 || number(card.Scheduling["queue"]) != 2 ||
		card.StudyState != "suspended" || card.DeckName != "Deck::Moved" ||
		!card.ClientModifiedAt.After(uploadedTime) || card.LastClientID != "" ||
		card.Projection == nil || card.Projection.Status != "succeeded" {
		t.Fatalf("managed scheduling/content ownership failed: %+v", card)
	}
	rr = reqJSON(t, mux, "GET", "/v2/summary/card-states", nil, token)
	stateSummary = cardStatesSummaryResponse{}
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &stateSummary) != nil ||
		len(stateSummary.Cards) != 1 || stateSummary.Cards[0].Queue != -1 ||
		stateSummary.Cards[0].StudyState != "suspended" {
		t.Fatalf("suspension missing from summary: code=%d body=%s", rr.Code, rr.Body.String())
	}

	// A late-arriving review inside the already-applied time range changes the
	// history hash even though the through-review ID is unchanged.
	pushReviews(t, mux, token, []map[string]any{
		projectionReview(1735689900000, "fsrs-card", 1),
	})
	var outOfOrderHash string
	var through int64
	if err := pool.QueryRow(context.Background(),
		`SELECT status,desired_history_hash,desired_through_review_id
		 FROM scheduler_projections WHERE note_guid='fsrs-card' AND card_ord=0`,
	).Scan(&status, &outOfOrderHash, &through); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || outOfOrderHash == desiredHash || through != 1735690200000 {
		t.Fatalf("out-of-order identity = %q/%q/%d", status, outOfOrderHash, through)
	}
	if processed, err := projection.NewWorker(h.DB).ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("out-of-order reprojection: processed=%v error=%v", processed, err)
	}
	card = getProjectionCard(t, mux, token, 7001)
	if number(card.Scheduling["reps"]) != 3 || card.StudyState != "suspended" ||
		card.Projection == nil || card.Projection.AppliedHistoryHash != outOfOrderHash {
		t.Fatalf("out-of-order review not applied: %+v", card)
	}
}

func TestSynchronizedScheduleResetReprojectsCardAsNewWithoutDeletingHistory(t *testing.T) {
	h, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "Kotlin")
	createProjectionCard(t, mux, token, "reset-card", 7101)
	pushReviews(t, mux, token, []map[string]any{
		projectionReview(1735689600000, "reset-card", 3),
		projectionReview(1735690200000, "reset-card", 3),
	})
	worker := projection.NewWorker(h.DB)
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("initial projection: processed=%v error=%v", processed, err)
	}

	resetAt := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	rr := reqJSON(t, mux, http.MethodPut, "/v2/cards/7101", map[string]any{
		"note_guid": "reset-card", "deck_name": "Deck", "ord": 0,
		"scheduling":                        map[string]any{},
		"schedule_reset_through_review_id":  1735690200000,
		"schedule_reset_client_modified_at": resetAt.Format(time.RFC3339Nano),
		"client_modified_at":                resetAt.Format(time.RFC3339Nano),
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("reset card: code=%d body=%s", rr.Code, rr.Body.String())
	}
	var reset model.Card
	if err := json.Unmarshal(rr.Body.Bytes(), &reset); err != nil {
		t.Fatal(err)
	}
	if reset.ScheduleResetThroughReviewID != 1735690200000 || reset.Projection == nil ||
		reset.Projection.Status != "pending" {
		t.Fatalf("reset metadata/projection missing: %+v", reset)
	}
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("reset projection: processed=%v error=%v", processed, err)
	}
	reset = getProjectionCard(t, mux, token, 7101)
	if number(reset.Scheduling["type"]) != 0 || number(reset.Scheduling["queue"]) != 0 ||
		number(reset.Scheduling["reps"]) != 0 {
		t.Fatalf("reset did not project as new: %+v", reset.Scheduling)
	}
	var reviewCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM reviews WHERE note_guid='reset-card'`).Scan(&reviewCount); err != nil {
		t.Fatal(err)
	}
	if reviewCount != 2 {
		t.Fatalf("reset deleted review history: count=%d", reviewCount)
	}

	pushReviews(t, mux, token, []map[string]any{
		projectionReview(1735776000000, "reset-card", 3),
	})
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("post-reset projection: processed=%v error=%v", processed, err)
	}
	reset = getProjectionCard(t, mux, token, 7101)
	if number(reset.Scheduling["reps"]) != 1 {
		t.Fatalf("post-reset review did not start fresh history: %+v", reset.Scheduling)
	}

	rr = reqJSON(t, mux, http.MethodPost, "/v2/batch/push", map[string]any{
		"cards": []map[string]any{{
			"card_id": 7101, "note_guid": "reset-card", "deck_name": "Deck", "ord": 0,
			"scheduling":                        map[string]any{},
			"schedule_reset_through_review_id":  1735776000000,
			"schedule_reset_client_modified_at": resetAt.Add(time.Hour).Format(time.RFC3339Nano),
			"client_modified_at":                resetAt.Add(time.Hour).Format(time.RFC3339Nano),
		}},
	}, token)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"cards":1`) {
		t.Fatalf("batch reset: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("second reset projection: processed=%v error=%v", processed, err)
	}
	reset = getProjectionCard(t, mux, token, 7101)
	if number(reset.Scheduling["reps"]) != 0 || reset.ScheduleResetThroughReviewID != 1735776000000 {
		t.Fatalf("batch reset did not restore New: %+v", reset)
	}
}

func TestDueDateOverrideUsesIndependentNewestWinsClockAndClearsOnReview(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "Kotlin")
	createProjectionCard(t, mux, token, "due-card", 7201)
	newer := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	older := newer.Add(-time.Hour)

	rr := reqJSON(t, mux, http.MethodPut, "/v2/cards/7201", map[string]any{
		"note_guid": "due-card", "deck_name": "Deck", "ord": 0,
		"scheduling":                           map[string]any{},
		"due_date_override_ms":                 1736467200000,
		"due_date_override_client_modified_at": newer.Format(time.RFC3339Nano),
		"client_modified_at":                   newer.Format(time.RFC3339Nano),
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("set due date: code=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = reqJSON(t, mux, http.MethodPost, "/v2/batch/push", map[string]any{
		"cards": []map[string]any{{
			"card_id": 7201, "note_guid": "due-card", "deck_name": "Deck", "ord": 0,
			"scheduling":                           map[string]any{},
			"due_date_override_ms":                 1736380800000,
			"due_date_override_client_modified_at": older.Format(time.RFC3339Nano),
			"client_modified_at":                   older.Format(time.RFC3339Nano),
		}},
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("stale due date: code=%d body=%s", rr.Code, rr.Body.String())
	}
	card := getProjectionCard(t, mux, token, 7201)
	if card.DueDateOverrideMS != 1736467200000 || !card.DueDateOverrideClientModifiedAt.Equal(newer) {
		t.Fatalf("stale due date won: %+v", card)
	}
	rr = reqJSON(t, mux, http.MethodGet, "/v2/sync/manifest", nil, token)
	var manifest model.Manifest
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &manifest) != nil ||
		len(manifest.Cards) != 1 || manifest.Cards[0].DueDateOverrideMS != 1736467200000 {
		t.Fatalf("due date missing from manifest: code=%d body=%s", rr.Code, rr.Body.String())
	}

	resetAt := time.Date(2025, 1, 4, 0, 0, 0, 0, time.UTC)
	rr = reqJSON(t, mux, http.MethodPut, "/v2/cards/7201", map[string]any{
		"note_guid": "due-card", "deck_name": "Deck", "ord": 0,
		"scheduling":                        map[string]any{},
		"schedule_reset_through_review_id":  1,
		"schedule_reset_client_modified_at": resetAt.Format(time.RFC3339Nano),
		"client_modified_at":                resetAt.Format(time.RFC3339Nano),
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("reset card: code=%d body=%s", rr.Code, rr.Body.String())
	}
	card = getProjectionCard(t, mux, token, 7201)
	if card.DueDateOverrideMS != 0 {
		t.Fatalf("reset did not clear due date override: %+v", card)
	}

	latest := time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC)
	rr = reqJSON(t, mux, http.MethodPut, "/v2/cards/7201", map[string]any{
		"note_guid": "due-card", "deck_name": "Deck", "ord": 0,
		"scheduling":                           map[string]any{},
		"due_date_override_ms":                 1736726400000,
		"due_date_override_client_modified_at": latest.Format(time.RFC3339Nano),
		"client_modified_at":                   latest.Format(time.RFC3339Nano),
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("replace due date: code=%d body=%s", rr.Code, rr.Body.String())
	}
	pushReviews(t, mux, token, []map[string]any{
		projectionReview(1736812800000, "due-card", 3),
	})
	card = getProjectionCard(t, mux, token, 7201)
	if card.DueDateOverrideMS != 0 {
		t.Fatalf("review did not clear due date override: %+v", card)
	}
}

func TestProjectionFailureIsVisibleAndCardCreationRetriesIt(t *testing.T) {
	h, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "Kotlin")

	pushReviews(t, mux, token, []map[string]any{
		projectionReview(1735689600000, "late-card", 3),
	})
	processed, err := projection.NewWorker(h.DB).ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("process missing card: processed=%v error=%v", processed, err)
	}
	var status, code string
	if err := pool.QueryRow(context.Background(),
		`SELECT status,COALESCE(last_error_code,'') FROM scheduler_projections
		 WHERE note_guid='late-card' AND card_ord=0`).Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || code != "missing_card" {
		t.Fatalf("failure state = %q/%q", status, code)
	}

	t.Setenv("KELMA_INTERNAL_SECRET", "projection-health-test")
	if unauthorized := reqJSON(t, mux, http.MethodGet, "/v2/internal/projections", nil, ""); unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unguarded projection health returned %d", unauthorized.Code)
	}
	internalRequest := httptest.NewRequest(http.MethodGet, "/v2/internal/projections", nil)
	internalRequest.Header.Set("x-internal-secret", "projection-health-test")
	internalResponse := httptest.NewRecorder()
	mux.ServeHTTP(internalResponse, internalRequest)
	var health projectionHealthResponse
	if internalResponse.Code != http.StatusOK ||
		json.Unmarshal(internalResponse.Body.Bytes(), &health) != nil ||
		health.Total != 1 || health.Failed != 1 || health.Stale != 1 {
		t.Fatalf("global projection health: code=%d body=%s", internalResponse.Code, internalResponse.Body.String())
	}

	rr := reqJSON(t, mux, "GET", "/v2/scheduler-profile", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("get profile: got %d body=%s", rr.Code, rr.Body.String())
	}
	var profileResponse schedulerProfileResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &profileResponse); err != nil {
		t.Fatal(err)
	}
	if profileResponse.Projections.Failed != 1 || profileResponse.Projections.Stale != 1 {
		t.Fatalf("failure not observable: %+v", profileResponse.Projections)
	}

	createProjectionCard(t, mux, token, "late-card", 7002)
	if err := pool.QueryRow(context.Background(),
		`SELECT status,COALESCE(last_error_code,'') FROM scheduler_projections
		 WHERE note_guid='late-card' AND card_ord=0`).Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || code != "" {
		t.Fatalf("card creation did not retry projection: %q/%q", status, code)
	}

	if _, err := pool.Exec(context.Background(),
		`UPDATE scheduler_projections SET status='running',last_attempt_at=now()-interval '10 minutes'
		 WHERE note_guid='late-card' AND card_ord=0`); err != nil {
		t.Fatal(err)
	}
	recovered, err := projection.NewWorker(h.DB).RecoverInterrupted(context.Background())
	if err != nil || recovered != 1 {
		t.Fatalf("recover interrupted: recovered=%d error=%v", recovered, err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT status,COALESCE(last_error_code,'') FROM scheduler_projections
		 WHERE note_guid='late-card' AND card_ord=0`).Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || code != "worker_interrupted" {
		t.Fatalf("recovered state = %q/%q", status, code)
	}
}

func TestProfileChangeRequeuesSuccessfulProjection(t *testing.T) {
	h, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "Kotlin")
	createProjectionCard(t, mux, token, "profile-card", 7003)
	pushReviews(t, mux, token, []map[string]any{
		projectionReview(1735689600000, "profile-card", 3),
		projectionReview(1735690200000, "profile-card", 3),
	})
	worker := projection.NewWorker(h.DB)
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("initial projection: processed=%v error=%v", processed, err)
	}

	candidate := manualProfileCandidate("profile-requeue", 0, 0.95)
	rr := reqJSON(t, mux, "PUT", "/v2/scheduler-profile", candidate, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("put profile: got %d body=%s", rr.Code, rr.Body.String())
	}
	var status string
	var desired, applied int64
	if err := pool.QueryRow(context.Background(),
		`SELECT status,desired_profile_version,applied_profile_version
		 FROM scheduler_projections WHERE note_guid='profile-card' AND card_ord=0`,
	).Scan(&status, &desired, &applied); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || desired != 1 || applied != 0 {
		t.Fatalf("profile requeue state = %q desired=%d applied=%d", status, desired, applied)
	}
	if processed, err := worker.ProcessNext(context.Background()); err != nil || !processed {
		t.Fatalf("profile reprojection: processed=%v error=%v", processed, err)
	}
	card := getProjectionCard(t, mux, token, 7003)
	metadata := card.Scheduling["_kelma_fsrs"].(map[string]any)
	if number(metadata["profile_version"]) != 1 || card.Projection == nil ||
		card.Projection.AppliedProfileVersion == nil || *card.Projection.AppliedProfileVersion != 1 {
		t.Fatalf("new profile not applied: metadata=%+v projection=%+v", metadata, card.Projection)
	}
}

func TestSchedulerProfileIsVersionedIdempotentAndConflictSafe(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "Kotlin")

	candidate := manualProfileCandidate("profile-request-1", 0, 0.92)
	rr := reqJSON(t, mux, "PUT", "/v2/scheduler-profile", candidate, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("put profile: got %d body=%s", rr.Code, rr.Body.String())
	}
	var first schedulerProfileResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Profile.Version != 1 || first.Profile.DesiredRetention != 0.92 || first.Profile.RetentionSource != "manual" {
		t.Fatalf("unexpected profile: %+v", first.Profile)
	}

	// Lost-response retry returns the original version even though its base is
	// no longer current.
	rr = reqJSON(t, mux, "PUT", "/v2/scheduler-profile", candidate, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("idempotent profile retry: got %d body=%s", rr.Code, rr.Body.String())
	}
	var retried schedulerProfileResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	if retried.Profile.Version != 1 {
		t.Fatalf("idempotent retry created version %d", retried.Profile.Version)
	}

	conflicting := manualProfileCandidate("profile-request-2", 0, 0.95)
	rr = reqJSON(t, mux, "PUT", "/v2/scheduler-profile", conflicting, token)
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale profile base: got %d body=%s", rr.Code, rr.Body.String())
	}

	optimized := manualProfileCandidate("profile-request-3", 1, 0.92)
	optimized["parameter_source"] = "client_optimized"
	optimized["optimizer"] = "fsrs-optimizer"
	optimized["optimizer_version"] = "6.5.0"
	optimized["optimization_history_hash"] = "not-the-account-history"
	optimized["optimization_through_review_id"] = int64(1735689600000)
	rr = reqJSON(t, mux, "PUT", "/v2/scheduler-profile", optimized, token)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "optimization_history_mismatch") {
		t.Fatalf("unverified optimizer provenance: got %d body=%s", rr.Code, rr.Body.String())
	}
}

func createProjectionCard(t *testing.T, mux *http.ServeMux, token, guid string, cardID int64) {
	t.Helper()
	rr := reqJSON(t, mux, "PUT", "/v2/notes/"+guid, map[string]any{
		"notetype_id": 1, "fields": []string{"front", "back"}, "tags": []string{},
		"client_modified_at": "2025-01-01T00:00:00Z", "base_checksum": "",
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("put note: got %d body=%s", rr.Code, rr.Body.String())
	}
	rr = reqJSON(t, mux, "PUT", "/v2/cards/"+int64ToStr(cardID), map[string]any{
		"note_guid": guid, "deck_name": "Deck", "ord": 0,
		"scheduling":         map[string]any{"type": 2, "queue": 2, "ivl": 999},
		"client_modified_at": "2025-01-01T00:00:00Z",
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("put card: got %d body=%s", rr.Code, rr.Body.String())
	}
}

func projectionReview(reviewID int64, guid string, ease int) map[string]any {
	return map[string]any{
		"review_id": reviewID, "source_card_id": int64(7001),
		"note_guid": guid, "card_ord": 0, "deck_name": "Deck", "ease": ease,
		"interval": 0, "last_interval": 0, "factor": 0,
		"taken_millis": 1000, "review_kind": 0,
	}
}

func pushReviews(t *testing.T, mux *http.ServeMux, token string, reviews []map[string]any) {
	t.Helper()
	rr := reqJSON(t, mux, "POST", "/v2/batch/push", map[string]any{
		"notes": []any{}, "cards": []any{}, "reviews": reviews,
		"study_days": []any{}, "notetypes": []any{}, "decks": []any{},
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("push reviews: got %d body=%s", rr.Code, rr.Body.String())
	}
}

func getProjectionCard(t *testing.T, mux *http.ServeMux, token string, cardID int64) model.Card {
	t.Helper()
	rr := reqJSON(t, mux, "GET", "/v2/cards/"+int64ToStr(cardID), nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("get card: got %d body=%s", rr.Code, rr.Body.String())
	}
	var card model.Card
	if err := json.Unmarshal(rr.Body.Bytes(), &card); err != nil {
		t.Fatal(err)
	}
	return card
}

func manualProfileCandidate(idempotencyKey string, baseVersion int64, retention float64) map[string]any {
	profile := projection.DefaultProfile()
	return map[string]any{
		"base_profile_version": baseVersion, "idempotency_key": idempotencyKey,
		"parameters": profile.Parameters, "parameter_source": "default",
		"desired_retention": retention, "retention_source": "manual",
		"maximum_interval":         profile.MaximumInterval,
		"learning_steps_seconds":   profile.LearningStepsSeconds,
		"relearning_steps_seconds": profile.RelearningStepsSeconds,
		"enable_short_term":        true, "enable_fuzzing": false,
		"quality_metrics": map[string]any{},
	}
}

func number(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case json.Number:
		result, _ := typed.Int64()
		return result
	default:
		return 0
	}
}
