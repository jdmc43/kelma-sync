package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jdmc43/kelma-sync/internal/auth"
	"github.com/jdmc43/kelma-sync/internal/storage"
)

func reqInternalJSON(
	t *testing.T,
	mux *http.ServeMux,
	method string,
	path string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(body); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	request := httptest.NewRequest(method, path, &encoded)
	request.Header.Set("content-type", "application/json")
	request.Header.Set("x-internal-secret", "test-internal-secret")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	return response
}

func TestInternalAccountDeletionErasesDatabaseBlobsTokensAndPreparedArchives(t *testing.T) {
	t.Setenv("KELMA_INTERNAL_SECRET", "test-internal-secret")
	h, mux, pool := testServer(t)
	defer pool.Close()

	token := loginDemo(t, mux, "deletion-test")
	userID := auth.UIDForEmail("demo")
	store := h.Storage.(*storage.Memory)
	if err := store.Put(context.Background(), storageKey(userID, "tracked.mp3"), bytes.NewReader([]byte("one")), 3, "audio/mpeg"); err != nil {
		t.Fatalf("put tracked blob: %v", err)
	}
	if err := store.Put(context.Background(), storageKey(userID, "orphan.mp3"), bytes.NewReader([]byte("two")), 3, "audio/mpeg"); err != nil {
		t.Fatalf("put orphan blob: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO media (user_id, filename, size_bytes, storage_key, modified_at)
		VALUES ($1, 'tracked.mp3', 3, $2, now())
	`, userID, storageKey(userID, "tracked.mp3")); err != nil {
		t.Fatalf("insert media metadata: %v", err)
	}

	archive, err := os.CreateTemp("", "kelma-account-deletion-test-*.tar")
	if err != nil {
		t.Fatalf("create prepared archive: %v", err)
	}
	archivePath := archive.Name()
	if err := archive.Close(); err != nil {
		t.Fatalf("close prepared archive: %v", err)
	}
	h.mediaTarJobs["delete-me"] = preparedMediaTar{
		userID: userID, path: archivePath, reservedBytes: 10, expiresAt: time.Now().Add(time.Hour),
	}
	h.mediaTarPreparedBytes = 10

	response := reqInternalJSON(t, mux, http.MethodPost, "/v2/internal/account-deletion", map[string]any{
		"email": "Demo", "confirm": "DELETE",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("delete account: got %d body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(archivePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared archive still exists: %v", err)
	}
	if h.mediaTarPreparedBytes != 0 || len(h.mediaTarJobs) != 0 {
		t.Fatalf("prepared archive accounting was not cleared")
	}
	for _, filename := range []string{"tracked.mp3", "orphan.mp3"} {
		body, _, err := store.Get(context.Background(), storageKey(userID, filename))
		if body != nil {
			body.Close()
		}
		if !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("blob %s was not deleted: %v", filename, err)
		}
	}
	var users, media, tokens int
	if err := pool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM users WHERE id = $1),
		       (SELECT count(*) FROM media WHERE user_id = $1),
		       (SELECT count(*) FROM tokens WHERE user_id = $1)
	`, userID).Scan(&users, &media, &tokens); err != nil {
		t.Fatalf("count deleted rows: %v", err)
	}
	if users != 0 || media != 0 || tokens != 0 {
		t.Fatalf("account rows remain: users=%d media=%d tokens=%d", users, media, tokens)
	}

	unauthorized := reqJSON(t, mux, http.MethodGet, "/v2/sync/manifest", nil, token)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("deleted token still works: %d", unauthorized.Code)
	}

	// Retrying deletion is safe, and a later authority-verified new account can
	// start clean under the same email-derived identity.
	retry := reqInternalJSON(t, mux, http.MethodPost, "/v2/internal/account-deletion", map[string]any{
		"email": "demo", "confirm": "DELETE",
	})
	if retry.Code != http.StatusOK {
		t.Fatalf("retry deletion: got %d body=%s", retry.Code, retry.Body.String())
	}
	recreated := reqInternalJSON(t, mux, http.MethodPost, "/v2/internal/token", map[string]any{
		"email": "demo", "isPaid": false,
	})
	if recreated.Code != http.StatusOK {
		t.Fatalf("recreate verified account: got %d body=%s", recreated.Code, recreated.Body.String())
	}
}

type failingPrefixStorage struct {
	*storage.Memory
}

func (f *failingPrefixStorage) DeletePrefix(context.Context, string) error {
	return errors.New("injected prefix deletion failure")
}

func TestInternalAccountDeletionStaysBlockedUntilStorageRetrySucceeds(t *testing.T) {
	t.Setenv("KELMA_INTERNAL_SECRET", "test-internal-secret")
	h, mux, pool := testServer(t)
	defer pool.Close()

	token := loginDemo(t, mux, "deletion-retry")
	memory := h.Storage.(*storage.Memory)
	h.Storage = &failingPrefixStorage{Memory: memory}

	requestBody := map[string]any{"email": "demo", "confirm": "DELETE"}
	failed := reqInternalJSON(t, mux, http.MethodPost, "/v2/internal/account-deletion", requestBody)
	if failed.Code != http.StatusBadGateway {
		t.Fatalf("failed storage deletion: got %d body=%s", failed.Code, failed.Body.String())
	}
	if response := reqJSON(t, mux, http.MethodGet, "/v2/sync/manifest", nil, token); response.Code != http.StatusUnauthorized {
		t.Fatalf("token was not revoked after deletion began: %d", response.Code)
	}
	blocked := reqInternalJSON(t, mux, http.MethodPost, "/v2/internal/token", map[string]any{
		"email": "demo", "isPaid": false,
	})
	if blocked.Code != http.StatusConflict {
		t.Fatalf("pending deletion allowed token minting: %d body=%s", blocked.Code, blocked.Body.String())
	}

	h.Storage = memory
	retried := reqInternalJSON(t, mux, http.MethodPost, "/v2/internal/account-deletion", requestBody)
	if retried.Code != http.StatusOK {
		t.Fatalf("retry deletion: got %d body=%s", retried.Code, retried.Body.String())
	}
}

var _ storage.Storage = (*failingPrefixStorage)(nil)
