package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jdmc43/kelma-sync/internal/model"
)

const (
	firstFlagIntent  = "44444444-4444-4444-8444-444444444444"
	secondFlagIntent = "55555555-5555-4555-8555-555555555555"
)

func TestCardFlagIntentIsIndependentIdempotentAndDeterministic(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	contentToken := loginDemo(t, mux, "card-content-client")
	flagToken := loginDemo(t, mux, "card-flag-client")
	now := time.Now().UTC().Truncate(time.Millisecond)

	note := reqJSON(t, mux, http.MethodPut, "/v2/notes/flag-note", map[string]any{
		"notetype_id": 1, "fields": []string{"front", "back"}, "tags": []string{"topic"},
		"client_modified_at": now.Add(-10 * time.Minute), "base_checksum": "",
	}, contentToken)
	if note.Code != http.StatusOK {
		t.Fatalf("create note=%d %s", note.Code, note.Body.String())
	}
	created := reqJSON(t, mux, http.MethodPut, "/v2/cards/7001", map[string]any{
		"note_guid": "flag-note", "deck_name": "Flags", "ord": 0,
		"scheduling":         map[string]any{"flags": 2, "due": 1},
		"client_modified_at": now.Add(-9 * time.Minute),
	}, contentToken)
	var original model.Card
	if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &original) != nil {
		t.Fatalf("create card=%d %s", created.Code, created.Body.String())
	}
	if original.Flag != nil {
		t.Fatalf("legacy card unexpectedly has typed flag: %+v", original.Flag)
	}

	firstTime := now.Add(-2 * time.Minute)
	first := cardFlagBody(4, firstFlagIntent, firstTime)
	response := reqJSON(t, mux, http.MethodPut, "/v2/cards/7001/flag", first, flagToken)
	var firstResult cardFlagResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &firstResult) != nil ||
		!firstResult.Accepted || !firstResult.Applied || firstResult.Flag.Flag != 4 ||
		firstResult.Flag.IntentID != firstFlagIntent {
		t.Fatalf("first=%d %s", response.Code, response.Body.String())
	}
	retry := reqJSON(t, mux, http.MethodPut, "/v2/cards/7001/flag", first, flagToken)
	var retryResult cardFlagResult
	if retry.Code != http.StatusOK || json.Unmarshal(retry.Body.Bytes(), &retryResult) != nil ||
		retryResult.Applied || retryResult.Flag.IntentID != firstFlagIntent {
		t.Fatalf("retry=%d %s", retry.Code, retry.Body.String())
	}
	conflict := reqJSON(t, mux, http.MethodPut, "/v2/cards/7001/flag",
		cardFlagBody(5, firstFlagIntent, firstTime), flagToken)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("changed retry=%d %s", conflict.Code, conflict.Body.String())
	}

	// Equal-time intents converge by UUID lexical order on every client.
	batch := reqJSON(t, mux, http.MethodPost, "/v2/batch/push", map[string]any{
		"card_flags": []any{map[string]any{
			"note_guid": "flag-note", "card_ord": 0, "flag": 6,
			"intent_id": secondFlagIntent, "client_modified_at": firstTime,
		}},
	}, flagToken)
	var batchBody batchPushResponse
	if batch.Code != http.StatusOK || json.Unmarshal(batch.Body.Bytes(), &batchBody) != nil ||
		batchBody.Accepted["card_flags"] != 1 || len(batchBody.FlagResults) != 1 ||
		batchBody.FlagResults[0].Flag.Flag != 6 || batchBody.FlagResults[0].Flag.IntentID != secondFlagIntent {
		t.Fatalf("batch=%d %s", batch.Code, batch.Body.String())
	}
	stale := reqJSON(t, mux, http.MethodPost, "/v2/batch/push", map[string]any{
		"card_flags": []any{map[string]any{
			"note_guid": "flag-note", "card_ord": 0, "flag": 1,
			"intent_id":          "00000000-0000-4000-8000-000000000001",
			"client_modified_at": firstTime.Add(-time.Minute),
		}},
	}, flagToken)
	var staleBody batchPushResponse
	if stale.Code != http.StatusOK || json.Unmarshal(stale.Body.Bytes(), &staleBody) != nil ||
		staleBody.Accepted["card_flags"] != 1 || staleBody.FlagResults[0].Applied ||
		staleBody.FlagResults[0].Flag.IntentID != secondFlagIntent {
		t.Fatalf("stale=%d %s", stale.Code, stale.Body.String())
	}

	// A whole-card upload can update content but cannot seize typed flag ownership.
	updated := reqJSON(t, mux, http.MethodPut, "/v2/cards/7001", map[string]any{
		"note_guid": "flag-note", "deck_name": "Flags::Moved", "ord": 0,
		"scheduling":         map[string]any{"flags": 1, "due": 9},
		"client_modified_at": now.Add(-time.Minute),
	}, contentToken)
	var card model.Card
	if updated.Code != http.StatusOK || json.Unmarshal(updated.Body.Bytes(), &card) != nil ||
		card.DeckName != "Flags::Moved" || schedulingFlag(card.Scheduling) != 6 ||
		card.Flag == nil || card.Flag.IntentID != secondFlagIntent ||
		!card.ModifiedAt.After(original.ModifiedAt) || card.LastClientID == card.Flag.LastClientID {
		t.Fatalf("updated=%d %+v body=%s", updated.Code, card, updated.Body.String())
	}
	manifest := reqJSON(t, mux, http.MethodGet, "/v2/sync/manifest", nil, contentToken)
	var value model.Manifest
	if manifest.Code != http.StatusOK || json.Unmarshal(manifest.Body.Bytes(), &value) != nil ||
		len(value.Capabilities) != 3 || value.Capabilities[0] != cardFlagSyncCapability ||
		value.Capabilities[1] != reviewRetractionSyncCapability ||
		value.Capabilities[2] != reviewClientOperationsCapability ||
		len(value.Cards) != 1 || value.Cards[0].Flag == nil ||
		value.Cards[0].Flag.IntentID != secondFlagIntent {
		t.Fatalf("manifest=%d %s", manifest.Code, manifest.Body.String())
	}

	if response := reqJSON(t, mux, http.MethodPost, "/v2/auth/register", map[string]any{
		"username": "flag-foreign", "password": "foreign-password",
	}, ""); response.Code != http.StatusCreated {
		t.Fatalf("foreign register=%d", response.Code)
	}
	login := reqJSON(t, mux, http.MethodPost, "/v2/auth/login", map[string]any{
		"username": "flag-foreign", "password": "foreign-password", "client_label": "foreign",
	}, "")
	var foreignLogin struct {
		Token string `json:"token"`
	}
	if login.Code != http.StatusOK || json.Unmarshal(login.Body.Bytes(), &foreignLogin) != nil {
		t.Fatalf("foreign login=%d %s", login.Code, login.Body.String())
	}
	foreign := reqJSON(t, mux, http.MethodPut, "/v2/cards/7001/flag",
		cardFlagBody(1, "66666666-6666-4666-8666-666666666666", now), foreignLogin.Token)
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign flag=%d %s", foreign.Code, foreign.Body.String())
	}
}

func TestCardFlagRejectsMalformedFutureAndForeignIntents(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "flag-validation-client")
	now := time.Now().UTC()
	for _, body := range []map[string]any{
		{"intent_id": firstFlagIntent, "client_modified_at": now},
		cardFlagBody(8, firstFlagIntent, now),
		cardFlagBody(1, "bad-id", now),
		cardFlagBody(1, firstFlagIntent, now.Add(maxFutureClockSkew+time.Minute)),
	} {
		response := reqJSON(t, mux, http.MethodPut, "/v2/cards/1/flag", body, token)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("malformed=%d %s", response.Code, response.Body.String())
		}
	}
	missing := reqJSON(t, mux, http.MethodPut, "/v2/cards/1/flag",
		cardFlagBody(1, firstFlagIntent, now), token)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing=%d %s", missing.Code, missing.Body.String())
	}
}

func cardFlagBody(flag int, intentID string, modifiedAt time.Time) map[string]any {
	return map[string]any{"flag": flag, "intent_id": intentID,
		"client_modified_at": modifiedAt.UTC().Format(time.RFC3339Nano)}
}
