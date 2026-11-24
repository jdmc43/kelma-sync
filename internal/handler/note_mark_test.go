package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jdmc43/kelma-sync/internal/model"
)

const (
	firstMarkIntent  = "11111111-1111-4111-8111-111111111111"
	secondMarkIntent = "22222222-2222-4222-8222-222222222222"
)

func TestTypedNoteMarkIsIndependentIdempotentAndDeterministic(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	contentToken := loginDemo(t, mux, "content-client")
	markToken := loginDemo(t, mux, "mark-client")
	created := reqJSON(t, mux, http.MethodPut, "/v2/notes/mark-note", map[string]any{
		"notetype_id": 1, "fields": []string{"front", "back"},
		"tags":               []string{"topic", "MARKED", "marked"},
		"client_modified_at": "2026-07-10T00:00:00Z", "base_checksum": "",
	}, contentToken)
	if created.Code != http.StatusOK {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	var original model.Note
	if err := json.Unmarshal(created.Body.Bytes(), &original); err != nil {
		t.Fatal(err)
	}
	if len(original.Tags) != 2 || original.Tags[1] != "marked" || original.Mark != nil {
		t.Fatalf("initial note=%+v", original)
	}
	legacyUnmark := reqJSON(t, mux, http.MethodPut, "/v2/notes/mark-note", map[string]any{
		"notetype_id": 1, "fields": []string{"front", "back"}, "tags": []string{"topic"},
		"client_modified_at": "2026-07-10T00:01:00Z", "base_checksum": original.Checksum,
	}, contentToken)
	if legacyUnmark.Code != http.StatusOK || json.Unmarshal(legacyUnmark.Body.Bytes(), &original) != nil ||
		hasMarkedTag(original.Tags) || original.Mark != nil {
		t.Fatalf("legacy unmark=%d %s", legacyUnmark.Code, legacyUnmark.Body.String())
	}

	firstTime := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Millisecond)
	first := markRequest(false, firstMarkIntent, firstTime)
	response := reqJSON(t, mux, http.MethodPut, "/v2/notes/mark-note/mark", first, markToken)
	if response.Code != http.StatusOK {
		t.Fatalf("first mark=%d %s", response.Code, response.Body.String())
	}
	var firstResult noteMarkResult
	if err := json.Unmarshal(response.Body.Bytes(), &firstResult); err != nil {
		t.Fatal(err)
	}
	if !firstResult.Accepted || !firstResult.Applied || firstResult.Mark.Marked ||
		firstResult.Mark.IntentID != firstMarkIntent {
		t.Fatalf("first result=%+v", firstResult)
	}

	retry := reqJSON(t, mux, http.MethodPut, "/v2/notes/mark-note/mark", first, markToken)
	var retryResult noteMarkResult
	if retry.Code != http.StatusOK || json.Unmarshal(retry.Body.Bytes(), &retryResult) != nil ||
		retryResult.Applied || retryResult.Mark.IntentID != firstMarkIntent {
		t.Fatalf("retry=%d %s", retry.Code, retry.Body.String())
	}
	conflict := reqJSON(t, mux, http.MethodPut, "/v2/notes/mark-note/mark",
		markRequest(true, firstMarkIntent, firstTime), markToken)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("changed retry=%d %s", conflict.Code, conflict.Body.String())
	}

	secondTime := firstTime.Add(time.Minute)
	batch := reqJSON(t, mux, http.MethodPost, "/v2/batch/push", map[string]any{
		"note_marks": []any{map[string]any{"guid": "mark-note", "marked": true,
			"intent_id": secondMarkIntent, "client_modified_at": secondTime.Format(time.RFC3339Nano)}},
	}, markToken)
	if batch.Code != http.StatusOK {
		t.Fatalf("batch mark=%d %s", batch.Code, batch.Body.String())
	}
	var batchBody batchPushResponse
	if err := json.Unmarshal(batch.Body.Bytes(), &batchBody); err != nil {
		t.Fatal(err)
	}
	if batchBody.Accepted["note_marks"] != 1 || len(batchBody.MarkResults) != 1 ||
		batchBody.MarkResults[0].Mark.IntentID != secondMarkIntent || !batchBody.MarkResults[0].Mark.Marked {
		t.Fatalf("batch body=%+v", batchBody)
	}

	stale := reqJSON(t, mux, http.MethodPost, "/v2/batch/push", map[string]any{
		"note_marks": []any{map[string]any{"guid": "mark-note", "marked": false,
			"intent_id":          "00000000-0000-4000-8000-000000000001",
			"client_modified_at": firstTime.Format(time.RFC3339Nano)}},
	}, markToken)
	var staleBody batchPushResponse
	if stale.Code != http.StatusOK || json.Unmarshal(stale.Body.Bytes(), &staleBody) != nil ||
		staleBody.Accepted["note_marks"] != 1 || staleBody.MarkResults[0].Applied ||
		staleBody.MarkResults[0].Mark.IntentID != secondMarkIntent {
		t.Fatalf("stale=%d %s", stale.Code, stale.Body.String())
	}

	get := reqJSON(t, mux, http.MethodGet, "/v2/notes/mark-note", nil, contentToken)
	var note model.Note
	if get.Code != http.StatusOK || json.Unmarshal(get.Body.Bytes(), &note) != nil {
		t.Fatalf("get=%d %s", get.Code, get.Body.String())
	}
	if !note.ModifiedAt.Equal(original.ModifiedAt) || note.ClientModifiedAt != original.ClientModifiedAt ||
		note.LastClientID != original.LastClientID ||
		note.Mark == nil || note.Mark.IntentID != secondMarkIntent || note.Mark.LastClientID == note.LastClientID ||
		!hasMarkedTag(note.Tags) || note.Checksum != checksum(note.Fields, note.Tags) {
		t.Fatalf("stored note=%+v original=%+v", note, original)
	}
	manifest := reqJSON(t, mux, http.MethodGet, "/v2/sync/manifest", nil, contentToken)
	var value model.Manifest
	if manifest.Code != http.StatusOK || json.Unmarshal(manifest.Body.Bytes(), &value) != nil ||
		len(value.Notes) != 1 || value.Notes[0].Mark == nil ||
		value.Notes[0].Mark.IntentID != secondMarkIntent {
		t.Fatalf("manifest=%d %s", manifest.Code, manifest.Body.String())
	}

	if response := reqJSON(t, mux, http.MethodPost, "/v2/auth/register", map[string]any{
		"username": "mark-foreign", "password": "foreign-password",
	}, ""); response.Code != http.StatusCreated {
		t.Fatalf("foreign register=%d", response.Code)
	}
	login := reqJSON(t, mux, http.MethodPost, "/v2/auth/login", map[string]any{
		"username": "mark-foreign", "password": "foreign-password", "client_label": "foreign",
	}, "")
	var foreignLogin struct {
		Token string `json:"token"`
	}
	if login.Code != http.StatusOK || json.Unmarshal(login.Body.Bytes(), &foreignLogin) != nil || foreignLogin.Token == "" {
		t.Fatalf("foreign login=%d %s", login.Code, login.Body.String())
	}
	foreign := reqJSON(t, mux, http.MethodPut, "/v2/notes/mark-note/mark",
		markRequest(false, "33333333-3333-4333-8333-333333333333", secondTime.Add(time.Minute)), foreignLogin.Token)
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign mark=%d %s", foreign.Code, foreign.Body.String())
	}
}

func TestWholeNoteWritesPreserveTypedMarkAndAllowMarkOnlyBaseDrift(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	contentToken := loginDemo(t, mux, "content-client")
	markToken := loginDemo(t, mux, "mark-client")
	created := reqJSON(t, mux, http.MethodPut, "/v2/notes/race-note", map[string]any{
		"notetype_id": 1, "fields": []string{"before", "back"}, "tags": []string{"topic"},
		"client_modified_at": "2026-07-10T00:00:00Z", "base_checksum": "",
	}, contentToken)
	var before model.Note
	if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &before) != nil {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	markedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	marked := reqJSON(t, mux, http.MethodPut, "/v2/notes/race-note/mark",
		markRequest(true, firstMarkIntent, markedAt), markToken)
	if marked.Code != http.StatusOK {
		t.Fatalf("mark=%d %s", marked.Code, marked.Body.String())
	}

	// The base predates only the Mark operation. The content edit is accepted,
	// while the missing marked tag is restored from independently owned state.
	edit := reqJSON(t, mux, http.MethodPut, "/v2/notes/race-note", map[string]any{
		"notetype_id": 1, "fields": []string{"after", "back"}, "tags": []string{"topic"},
		"client_modified_at": "2026-07-10T01:00:00Z", "base_checksum": before.Checksum,
	}, contentToken)
	var edited model.Note
	if edit.Code != http.StatusOK || json.Unmarshal(edit.Body.Bytes(), &edited) != nil {
		t.Fatalf("edit=%d %s", edit.Code, edit.Body.String())
	}
	if edited.Fields[0] != "after" || !hasMarkedTag(edited.Tags) || edited.Mark == nil ||
		edited.Mark.IntentID != firstMarkIntent {
		t.Fatalf("edited=%+v", edited)
	}

	// Force-Override controls content conflict policy, not Mark ownership.
	force := requestJSONWithHeaders(t, mux, http.MethodPut, "/v2/notes/race-note", map[string]any{
		"notetype_id": 1, "fields": []string{"forced", "back"}, "tags": []string{"topic"},
		"client_modified_at": "2026-07-10T02:00:00Z", "base_checksum": "stale",
	}, contentToken, map[string]string{"Force-Override": "true"})
	var forced model.Note
	if force.Code != http.StatusOK || json.Unmarshal(force.Body.Bytes(), &forced) != nil ||
		!hasMarkedTag(forced.Tags) || forced.Mark == nil || forced.Mark.IntentID != firstMarkIntent {
		t.Fatalf("force=%d %s", force.Code, force.Body.String())
	}
}

func TestNoteMarkRejectsMissingMalformedAndFutureIntent(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "mark-client")
	now := time.Now().UTC()
	cases := []map[string]any{
		{"intent_id": firstMarkIntent, "client_modified_at": now.Format(time.RFC3339Nano)},
		markRequest(true, "not-a-uuid", now),
		markRequest(true, firstMarkIntent, now.Add(10*time.Minute)),
	}
	for _, body := range cases {
		response := reqJSON(t, mux, http.MethodPut, "/v2/notes/missing/mark", body, token)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("malformed=%d %s", response.Code, response.Body.String())
		}
	}
	missing := reqJSON(t, mux, http.MethodPut, "/v2/notes/missing/mark",
		markRequest(true, firstMarkIntent, now), token)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing=%d %s", missing.Code, missing.Body.String())
	}
}

func markRequest(marked bool, intentID string, modifiedAt time.Time) map[string]any {
	return map[string]any{"marked": marked, "intent_id": intentID,
		"client_modified_at": modifiedAt.UTC().Format(time.RFC3339Nano)}
}

func requestJSONWithHeaders(t *testing.T, mux *http.ServeMux, method, path string, body any,
	token string, headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(body); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, &encoded)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	return response
}
