package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jdmc43/kelma-sync/internal/auth"
	"github.com/jdmc43/kelma-sync/internal/model"
	"github.com/jdmc43/kelma-sync/internal/storage"
)

func TestSyncIdentityReportsBearerAccountAndDedicatedClient(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "identity-check")
	response := reqJSON(t, mux, http.MethodGet, "/v2/sync/identity", nil, token)
	if response.Code != http.StatusOK {
		t.Fatalf("identity status=%d", response.Code)
	}
	var identity syncIdentityResponse
	if json.Unmarshal(response.Body.Bytes(), &identity) != nil || identity.UserID != auth.UIDForEmail("demo") || identity.ClientID == "" {
		t.Fatal("authenticated identity mismatch")
	}
}

func TestMediaAdoptVerifiesSharedObjectWithoutCopyingBody(t *testing.T) {
	h, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "media-mirror")
	userID := auth.UIDForEmail("demo")
	memory := h.Storage.(*storage.Memory)
	if err := memory.Put(context.Background(), storageKey(userID, "shared.bin"), bytes.NewReader([]byte("shared")), 6, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	adopted := reqJSON(t, mux, http.MethodPost, "/v2/media/adopt/shared.bin", map[string]any{"size_bytes": 6}, token)
	if adopted.Code != http.StatusOK {
		t.Fatalf("adopt status=%d body=%s", adopted.Code, adopted.Body.String())
	}
	manifestResponse := reqJSON(t, mux, http.MethodGet, "/v2/sync/manifest", nil, token)
	var manifest model.Manifest
	if json.Unmarshal(manifestResponse.Body.Bytes(), &manifest) != nil || len(manifest.Media) != 1 ||
		manifest.Media[0].Filename != "shared.bin" || manifest.Media[0].LastClientID == "" {
		t.Fatal("adopted metadata missing from manifest")
	}
	wrong := reqJSON(t, mux, http.MethodPost, "/v2/media/adopt/shared.bin", map[string]any{"size_bytes": 5}, token)
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong shared size status=%d", wrong.Code)
	}
	missing := reqJSON(t, mux, http.MethodPost, "/v2/media/adopt/missing.bin", map[string]any{"size_bytes": 1}, token)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing shared object status=%d", missing.Code)
	}
}

func TestBatchCardDeletePreservesAndPullsLogicalIdentity(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "card-tombstone")
	for _, item := range []struct {
		method, path string
		body         map[string]any
	}{
		{http.MethodPut, "/v2/notes/card-owner", map[string]any{
			"notetype_id": 1, "fields": []string{"card"}, "tags": []string{},
			"client_modified_at": "2026-07-10T00:00:00Z", "base_checksum": ""}},
		{http.MethodPut, "/v2/cards/9001", map[string]any{
			"note_guid": "card-owner", "deck_name": "Default", "ord": 2,
			"scheduling": map[string]any{}, "client_modified_at": "2026-07-10T00:00:00Z"}},
	} {
		response := reqJSON(t, mux, item.method, item.path, item.body, token)
		if response.Code != http.StatusOK {
			t.Fatalf("seed status=%d body=%s", response.Code, response.Body.String())
		}
	}
	response := reqJSON(t, mux, http.MethodPost, "/v2/batch/delete", map[string]any{
		"notes": []string{}, "cards": []int64{9001}, "notetypes": []int64{}, "decks": []string{},
	}, token)
	if response.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
	response = reqJSON(t, mux, http.MethodPost, "/v2/batch/pull", map[string]any{
		"tombstone_card_keys": []map[string]any{{"note_guid": "card-owner", "ord": 2}},
	}, token)
	var pulled struct {
		Tombstones []model.Tombstone `json:"tombstones"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &pulled) != nil ||
		len(pulled.Tombstones) != 1 || pulled.Tombstones[0].ResourceID != "9001" ||
		pulled.Tombstones[0].NoteGUID != "card-owner" || pulled.Tombstones[0].CardOrd == nil || *pulled.Tombstones[0].CardOrd != 2 {
		t.Fatalf("logical tombstone pull failed: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCardStudyStateRejectsAnOlderIndependentClock(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "state-clock")
	response := reqJSON(t, mux, http.MethodPut, "/v2/notes/state-note", map[string]any{
		"notetype_id": 1, "fields": []string{"front"}, "tags": []string{},
		"client_modified_at": "2026-01-01T00:00:00Z", "base_checksum": "",
	}, token)
	if response.Code != http.StatusOK {
		t.Fatalf("note status=%d", response.Code)
	}
	for _, value := range []struct {
		state, timestamp string
	}{{"suspended", "2026-01-02T00:00:00Z"}, {"active", "2026-01-01T00:00:00Z"}} {
		response = reqJSON(t, mux, http.MethodPut, "/v2/cards/99", map[string]any{
			"note_guid": "state-note", "deck_name": "Default", "ord": 0,
			"scheduling": map[string]any{}, "client_modified_at": value.timestamp,
			"study_state": value.state, "study_state_client_modified_at": value.timestamp,
		}, token)
		if response.Code != http.StatusOK {
			t.Fatalf("card status=%d body=%s", response.Code, response.Body.String())
		}
	}
	response = reqJSON(t, mux, http.MethodGet, "/v2/cards/99", nil, token)
	var card model.Card
	if json.Unmarshal(response.Body.Bytes(), &card) != nil || card.StudyState != "suspended" {
		t.Fatal("older card study-state clock overwrote newer state")
	}
}

func TestInternalMirrorTokenIsDedicatedAndNeverCreatesAccount(t *testing.T) {
	t.Setenv("KELMA_INTERNAL_SECRET", "test-internal-secret")
	_, mux, pool := testServer(t)
	defer pool.Close()
	_ = loginDemo(t, mux, "existing-account")
	response := reqInternalJSON(t, mux, http.MethodPost, "/v2/internal/account-mirror-token", map[string]any{"email": "demo"})
	if response.Code != http.StatusOK {
		t.Fatalf("mirror token status=%d body=%s", response.Code, response.Body.String())
	}
	var token internalTokenResponse
	if json.Unmarshal(response.Body.Bytes(), &token) != nil || token.UserID != auth.UIDForEmail("demo") || token.HKey == "" {
		t.Fatal("invalid mirror token response")
	}
	request, _ := http.NewRequest(http.MethodGet, "/v2/sync/identity", nil)
	request.Header.Set("Authorization", "Bearer "+token.HKey)
	identityResponse := httptest.NewRecorder()
	mux.ServeHTTP(identityResponse, request)
	if identityResponse.Code != http.StatusOK {
		t.Fatalf("dedicated token identity status=%d", identityResponse.Code)
	}
	var identity syncIdentityResponse
	if json.Unmarshal(identityResponse.Body.Bytes(), &identity) != nil || identity.ClientID == "" {
		t.Fatal("dedicated token did not authenticate")
	}
	var label string
	if err := pool.QueryRow(context.Background(), `SELECT label FROM clients WHERE id=$1`, identity.ClientID).Scan(&label); err != nil || label != "kelma-account-mirror" {
		t.Fatal("mirror token reused a non-mirror client")
	}
	unknown := reqInternalJSON(t, mux, http.MethodPost, "/v2/internal/account-mirror-token", map[string]any{"email": "missing@example.test"})
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("missing account was created: status=%d", unknown.Code)
	}
}
