package handler

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jdmc43/kelma-sync/internal/db"
	"github.com/jdmc43/kelma-sync/internal/model"
	"github.com/jdmc43/kelma-sync/internal/storage"
)

func testServer(t *testing.T) (*Handler, *http.ServeMux, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	// Use a DEDICATED test database. Never default to the live DB — these tests
	// TRUNCATE every table, which would destroy real synced collections.
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://kelma:kelma@localhost:5433/kelma_sync_test?sslmode=disable"
	}
	_ = os.Setenv("DATABASE_URL", url)
	_ = os.Setenv("KELMA_AUTH_MODE", "local")
	pool, err := db.Connect(ctx)
	if err != nil {
		t.Skipf("test db unavailable (%v); set TEST_DATABASE_URL to run", err)
	}
	if err := db.Migrate(ctx, pool, "../../migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	resetDB(t, pool)
	h := New(pool, storage.NewMemory())
	mux := http.NewServeMux()
	h.Routes(mux)
	return h, mux, pool
}

func resetDB(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		TRUNCATE scheduler_projections, scheduler_profiles, study_day_policies, tokens, clients,
		         tombstones, media, reviews, study_days, cards, notes, decks,
		         notetypes, users, account_deletions, deck_limit_history RESTART IDENTITY CASCADE
	`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func reqJSON(t *testing.T, mux *http.ServeMux, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func TestRegisterLoginManifest(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()

	rr := reqJSON(t, mux, "POST", "/v2/auth/register", map[string]any{
		"username": "demo",
		"password": "demo",
	}, "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: got %d body=%s", rr.Code, rr.Body.String())
	}

	rr = reqJSON(t, mux, "POST", "/v2/auth/login", map[string]any{
		"username":     "demo",
		"password":     "demo",
		"client_label": "MacBook",
	}, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("login: got %d body=%s", rr.Code, rr.Body.String())
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &login); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if login.Token == "" {
		t.Fatalf("empty token")
	}

	rr = reqJSON(t, mux, "GET", "/v2/sync/manifest", nil, login.Token)
	if rr.Code != http.StatusOK {
		t.Fatalf("manifest: got %d body=%s", rr.Code, rr.Body.String())
	}
	var manifest struct {
		Notes []any `json:"notes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest.Notes) != 0 {
		t.Fatalf("expected empty notes, got %d", len(manifest.Notes))
	}

	// Usage is authenticated and exact; a new account starts at zero bytes.
	rr = reqJSON(t, mux, "GET", "/v2/usage", nil, login.Token)
	if rr.Code != http.StatusOK {
		t.Fatalf("usage: got %d body=%s", rr.Code, rr.Body.String())
	}
	var usage usageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &usage); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	if usage.UsedBytes != 0 || usage.MediaBytes != 0 || usage.ContentBytes != 0 {
		t.Fatalf("expected empty usage, got %+v", usage)
	}
}

func loginDemo(t *testing.T, mux *http.ServeMux, label string) string {
	t.Helper()
	_ = reqJSON(t, mux, "POST", "/v2/auth/register", map[string]any{"username": "demo", "password": "demo"}, "")
	rr := reqJSON(t, mux, "POST", "/v2/auth/login", map[string]any{
		"username":     "demo",
		"password":     "demo",
		"client_label": label,
	}, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("login: got %d body=%s", rr.Code, rr.Body.String())
	}
	var login struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &login)
	return login.Token
}

func TestPutAndGetNote(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	loginToken := loginDemo(t, mux, "MacBook")

	rr := reqJSON(t, mux, "PUT", "/v2/notes/guid-1", map[string]any{
		"notetype_id":        1,
		"fields":             []string{"front", "back"},
		"tags":               []string{"x"},
		"client_modified_at": "2026-07-10T00:00:00Z",
		"base_checksum":      "",
	}, loginToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("put note: got %d body=%s", rr.Code, rr.Body.String())
	}

	rr = reqJSON(t, mux, "GET", "/v2/notes/guid-1", nil, loginToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("get note: got %d body=%s", rr.Code, rr.Body.String())
	}
	var note struct {
		GUID     string   `json:"guid"`
		Fields   []string `json:"fields"`
		Checksum string   `json:"checksum"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &note); err != nil {
		t.Fatalf("decode note: %v", err)
	}
	if note.GUID != "guid-1" || len(note.Fields) != 2 || note.Fields[0] != "front" {
		t.Fatalf("unexpected note: %+v", note)
	}

	// Card manifest rows expose deck names so v2 clients can build a visible,
	// deck-scoped comparison without calling the removed legacy inspect route.
	rr = reqJSON(t, mux, "PUT", "/v2/cards/123", map[string]any{
		"note_guid": "guid-1", "deck_name": "Compare Deck", "ord": 0,
		"scheduling": map[string]any{}, "client_modified_at": "2026-07-10T00:00:00Z",
		"created_at": "2020-02-03T04:05:06Z",
	}, loginToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("put card: got %d body=%s", rr.Code, rr.Body.String())
	}
	rr = reqJSON(t, mux, "GET", "/v2/cards/123", nil, loginToken)
	var card model.Card
	if err := json.Unmarshal(rr.Body.Bytes(), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if card.CreatedAt == nil || card.CreatedAt.UTC().Format("2006-01-02T15:04:05Z") != "2020-02-03T04:05:06Z" {
		t.Fatalf("unexpected card creation date: %+v", card.CreatedAt)
	}
	rr = reqJSON(t, mux, "PUT", "/v2/cards/123", map[string]any{
		"note_guid": "guid-1", "deck_name": "Compare Deck", "ord": 0,
		"scheduling": map[string]any{}, "client_modified_at": "2026-07-10T01:00:00Z",
		"created_at": "2021-01-01T00:00:00Z",
	}, loginToken)
	if rr.Code != http.StatusOK {
		t.Fatalf("update card: got %d body=%s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &card); err != nil {
		t.Fatalf("decode updated card: %v", err)
	}
	if card.CreatedAt == nil || card.CreatedAt.UTC().Format("2006-01-02") != "2020-02-03" {
		t.Fatalf("card creation date was overwritten: %+v", card.CreatedAt)
	}
	rr = reqJSON(t, mux, "GET", "/v2/sync/manifest", nil, loginToken)
	var manifest struct {
		Notes []struct {
			GUID             string `json:"guid"`
			ClientModifiedAt string `json:"client_modified_at"`
		} `json:"notes"`
		Cards []struct {
			DeckName string `json:"deck_name"`
		} `json:"cards"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("decode card manifest: %v", err)
	}
	if len(manifest.Cards) != 1 || manifest.Cards[0].DeckName != "Compare Deck" {
		t.Fatalf("unexpected card manifest: %+v", manifest.Cards)
	}
	if len(manifest.Notes) != 1 || manifest.Notes[0].GUID != "guid-1" ||
		manifest.Notes[0].ClientModifiedAt != "2026-07-10T00:00:00Z" {
		t.Fatalf("unexpected note manifest source timestamp: %+v", manifest.Notes)
	}
}

func TestNoteConflictAndForceOverride(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	mac := loginDemo(t, mux, "MacBook")
	phone := loginDemo(t, mux, "iPhone")

	// Initial create.
	rr := reqJSON(t, mux, "PUT", "/v2/notes/guid-1", map[string]any{
		"notetype_id":        1,
		"fields":             []string{"front", "back"},
		"tags":               []string{"x"},
		"client_modified_at": "2026-07-10T00:00:00Z",
		"base_checksum":      "",
	}, mac)
	if rr.Code != http.StatusOK {
		t.Fatalf("initial put: got %d body=%s", rr.Code, rr.Body.String())
	}
	var created struct {
		Checksum string `json:"checksum"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &created)

	// Phone updates from the correct base.
	rr = reqJSON(t, mux, "PUT", "/v2/notes/guid-1", map[string]any{
		"notetype_id":        1,
		"fields":             []string{"front edited on phone", "back"},
		"tags":               []string{"x"},
		"client_modified_at": "2026-07-10T01:00:00Z",
		"base_checksum":      created.Checksum,
	}, phone)
	if rr.Code != http.StatusOK {
		t.Fatalf("phone put: got %d body=%s", rr.Code, rr.Body.String())
	}

	// Mac now pushes from stale base -> conflict.
	rr = reqJSON(t, mux, "PUT", "/v2/notes/guid-1", map[string]any{
		"notetype_id":        1,
		"fields":             []string{"front edited on mac", "back"},
		"tags":               []string{"x"},
		"client_modified_at": "2026-07-10T02:00:00Z",
		"base_checksum":      created.Checksum,
	}, mac)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 conflict, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Force-override should succeed.
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]any{
		"notetype_id":        1,
		"fields":             []string{"front edited on mac", "back"},
		"tags":               []string{"x"},
		"client_modified_at": "2026-07-10T02:00:00Z",
		"base_checksum":      created.Checksum,
	})
	req := httptest.NewRequest("PUT", "/v2/notes/guid-1", &buf)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("Authorization", "Bearer "+mac)
	req.Header.Set("Force-Override", "true")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("force override: got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBatchDeleteWritesTombstones(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "MacBook")
	for _, guid := range []string{"batch-1", "batch-2"} {
		rr := reqJSON(t, mux, "PUT", "/v2/notes/"+guid, map[string]any{
			"notetype_id": 1, "fields": []string{guid}, "tags": []string{},
			"client_modified_at": "2026-07-10T00:00:00Z", "base_checksum": "",
		}, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("put %s: got %d body=%s", guid, rr.Code, rr.Body.String())
		}
	}
	rr := reqJSON(t, mux, "POST", "/v2/batch/delete", map[string]any{
		"notes": []string{"batch-1", "batch-2"}, "cards": []int64{},
		"notetypes": []int64{}, "decks": []string{},
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("batch delete: got %d body=%s", rr.Code, rr.Body.String())
	}
	var remaining, tombstones int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM notes WHERE guid LIKE 'batch-%'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM tombstones WHERE type='note' AND resource_id LIKE 'batch-%'`).Scan(&tombstones); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 || tombstones != 2 {
		t.Fatalf("remaining=%d tombstones=%d", remaining, tombstones)
	}
}

func TestReviewHistoryRoundTripAndStudyDayMerge(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	mobile := loginDemo(t, mux, "KelmaMobile")
	desktop := loginDemo(t, mux, "KelmaDesktop")

	review := map[string]any{
		"review_id": int64(1784420000123), "source_card_id": int64(111),
		"note_guid": "review-guid", "card_ord": 0, "deck_name": "Deck",
		"ease": 3, "interval": 30, "last_interval": 10, "factor": 2500,
		"taken_millis": 4200, "review_kind": 1,
	}
	day := map[string]any{
		"day": int64(20653), "deck_name": "Deck", "new_studied": 20,
		"review_studied": 62, "learning_studied": 0,
		"milliseconds_studied": int64(123456),
	}
	batch := map[string]any{
		"notes": []any{}, "cards": []any{}, "reviews": []any{review},
		"study_days": []any{day}, "notetypes": []any{}, "decks": []any{},
	}
	rr := reqJSON(t, mux, "POST", "/v2/batch/push", batch, mobile)
	if rr.Code != http.StatusOK {
		t.Fatalf("push review: got %d body=%s", rr.Code, rr.Body.String())
	}

	// Relaying the same immutable review from a collection with a different
	// local card id is idempotent, not a duplicate or conflict.
	relay := map[string]any{}
	for key, value := range review {
		relay[key] = value
	}
	relay["source_card_id"] = int64(999)
	relay["deck_name"] = "Deck::Moved"
	lowerDay := map[string]any{}
	for key, value := range day {
		lowerDay[key] = value
	}
	lowerDay["new_studied"] = 5
	lowerDay["review_studied"] = 10
	batch["reviews"] = []any{relay}
	batch["study_days"] = []any{lowerDay}
	rr = reqJSON(t, mux, "POST", "/v2/batch/push", batch, desktop)
	if rr.Code != http.StatusOK {
		t.Fatalf("relay review: got %d body=%s", rr.Code, rr.Body.String())
	}

	var reviewCount int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM reviews`).Scan(&reviewCount); err != nil {
		t.Fatal(err)
	}
	if reviewCount != 1 {
		t.Fatalf("idempotent relay left %d reviews", reviewCount)
	}

	rr = reqJSON(t, mux, "GET", "/v2/sync/manifest", nil, desktop)
	if rr.Code != http.StatusOK {
		t.Fatalf("review manifest: got %d body=%s", rr.Code, rr.Body.String())
	}
	var manifest struct {
		Reviews []struct {
			ReviewID int64  `json:"review_id"`
			Checksum string `json:"checksum"`
		} `json:"reviews"`
		StudyDays []model.StudyDay `json:"study_days"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Reviews) != 1 || manifest.Reviews[0].ReviewID != 1784420000123 || manifest.Reviews[0].Checksum == "" {
		t.Fatalf("unexpected review manifest: %+v", manifest.Reviews)
	}
	if len(manifest.StudyDays) != 1 || manifest.StudyDays[0].NewStudied != 20 || manifest.StudyDays[0].ReviewStudied != 62 {
		t.Fatalf("study day was not merged monotonically: %+v", manifest.StudyDays)
	}

	rr = reqJSON(t, mux, "POST", "/v2/batch/pull", map[string]any{
		"notes": []string{}, "cards": []int64{},
		"reviews": []int64{1784420000123}, "notetypes": []int64{},
		"decks": []string{},
	}, desktop)
	if rr.Code != http.StatusOK {
		t.Fatalf("pull review: got %d body=%s", rr.Code, rr.Body.String())
	}
	var pulled struct {
		Reviews []model.Review `json:"reviews"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &pulled); err != nil {
		t.Fatal(err)
	}
	if len(pulled.Reviews) != 1 || pulled.Reviews[0].NoteGUID != "review-guid" || pulled.Reviews[0].TakenMillis != 4200 {
		t.Fatalf("unexpected pulled review: %+v", pulled.Reviews)
	}

	// Reusing an Anki review id for different immutable content is explicit.
	conflicting := map[string]any{}
	for key, value := range review {
		conflicting[key] = value
	}
	conflicting["ease"] = 1
	batch["reviews"] = []any{conflicting}
	batch["study_days"] = []any{}
	rr = reqJSON(t, mux, "POST", "/v2/batch/push", batch, desktop)
	if rr.Code != http.StatusOK {
		t.Fatalf("review conflict response: got %d body=%s", rr.Code, rr.Body.String())
	}
	var result batchPushResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts["reviews"]) != 1 || result.Accepted["reviews"] != 0 {
		t.Fatalf("expected one review conflict, got %+v", result)
	}
}

func TestImmersionCardFastPathAndSummaries(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "Kelma Immersion")

	payload := map[string]any{
		"deck_name":   "Fast Deck",
		"deck_config": map[string]any{"dyn": 0},
		"create_deck": true,
		"notetype": map[string]any{
			"notetype_id": int64(7001), "name": "Anki_Upload_API_Basic",
			"definition": map[string]any{"name": "Anki_Upload_API_Basic"},
		},
		"note": map[string]any{
			"guid": "immersion-card-guid", "fields": []string{"<b>front</b>", "back"}, "tags": []string{"anki_ai"},
		},
		"card": map[string]any{
			"card_id": int64(9001), "ord": 0,
			"scheduling": map[string]any{"type": 2, "queue": 2, "ivl": 30},
			"created_at": "2026-07-01T12:34:56.789Z",
		},
		"media_files": []map[string]any{{
			"filename": "fast-card.mp3", "data_base64": base64.StdEncoding.EncodeToString([]byte("test audio")),
		}},
		"client_modified_at": "2026-07-14T00:00:00Z",
	}

	rr := reqJSON(t, mux, "POST", "/v2/immersion/cards", payload, token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("immersion card: got %d body=%s", rr.Code, rr.Body.String())
	}
	var created immersionCardResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode immersion card: %v", err)
	}
	if created.CardID != 9001 || created.MediaUploaded != 1 || created.DeckName != "Fast Deck" {
		t.Fatalf("unexpected immersion response: %+v", created)
	}

	// Retrying the same app card is idempotent even if the caller generated a
	// different candidate card id after losing the first response.
	payload["card"].(map[string]any)["card_id"] = int64(9002)
	payload["card"].(map[string]any)["created_at"] = "2026-07-02T00:00:00Z"
	rr = reqJSON(t, mux, "POST", "/v2/immersion/cards", payload, token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("immersion retry: got %d body=%s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode immersion retry: %v", err)
	}
	if created.CardID != 9001 {
		t.Fatalf("retry created a second logical card: %+v", created)
	}
	var cardCount, noteCount int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM cards`).Scan(&cardCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM notes`).Scan(&noteCount); err != nil {
		t.Fatal(err)
	}
	if cardCount != 1 || noteCount != 1 {
		t.Fatalf("idempotent retry left cards=%d notes=%d", cardCount, noteCount)
	}
	var storedCreatedAt time.Time
	if err := pool.QueryRow(
		context.Background(),
		`SELECT created_at FROM cards WHERE card_id=9001`,
	).Scan(&storedCreatedAt); err != nil {
		t.Fatal(err)
	}
	if storedCreatedAt.UTC().Format(time.RFC3339Nano) != "2026-07-01T12:34:56.789Z" {
		t.Fatalf("immersion creation date was not preserved: %s", storedCreatedAt)
	}

	rr = reqJSON(t, mux, "GET", "/v2/summary/decks", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("deck summary: got %d body=%s", rr.Code, rr.Body.String())
	}
	var decks decksSummaryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &decks); err != nil {
		t.Fatal(err)
	}
	if len(decks.Decks) != 1 || decks.Decks[0].Name != "Fast Deck" || decks.Decks[0].CardCount != 1 {
		t.Fatalf("unexpected deck summary: %+v", decks)
	}

	rr = reqJSON(t, mux, "GET", "/v2/summary/decks/Fast%20Deck/card-ids", nil, token)
	var ids deckCardIDsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &ids); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || len(ids.CardIDs) != 1 || ids.CardIDs[0] != 9001 {
		t.Fatalf("unexpected card ids: code=%d body=%s", rr.Code, rr.Body.String())
	}

	for _, path := range []string{
		"/v2/summary/decks/Fast%20Deck/card-states",
		"/v2/summary/card-states",
	} {
		rr = reqJSON(t, mux, "GET", path, nil, token)
		var states cardStatesSummaryResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &states); err != nil {
			t.Fatal(err)
		}
		if rr.Code != http.StatusOK || len(states.Cards) != 1 ||
			states.Cards[0].Ivl != 0 || states.Cards[0].ProjectionStatus != "pending" ||
			states.Cards[0].StudyState != "active" || len(states.Cards[0].Fields) != 2 {
			t.Fatalf("unexpected states for %s: code=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}

	rr = reqJSON(t, mux, "GET", "/v2/sync/manifest", nil, token)
	var manifest model.Manifest
	if err := json.Unmarshal(rr.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || len(manifest.Media) != 1 || manifest.Media[0].SizeBytes != 10 {
		t.Fatalf("manifest media size missing: code=%d body=%s", rr.Code, rr.Body.String())
	}

	rr = reqJSON(t, mux, "POST", "/v2/media/download-plan", map[string]any{
		"filenames": []string{"fast-card.mp3", "missing.mp3", "fast-card.mp3"},
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("media download plan: got %d body=%s", rr.Code, rr.Body.String())
	}
	var plan mediaDownloadPlanResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 1 || plan.Files[0].Filename != "fast-card.mp3" ||
		plan.Files[0].URL != "/v2/media/fast-card.mp3" || plan.Files[0].SizeBytes != 10 ||
		!plan.Files[0].RequiresAuth ||
		len(plan.Missing) != 1 || plan.Missing[0] != "missing.mp3" {
		t.Fatalf("unexpected media download plan: %+v", plan)
	}

	rr = reqJSON(t, mux, "POST", "/v2/media/download-plan", map[string]any{
		"filenames": make([]string, maxMediaDownloadPlanFiles+1),
	}, token)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("oversized media plan: got %d body=%s", rr.Code, rr.Body.String())
	}

	rr = reqJSON(t, mux, "POST", "/v2/media/download-tar", map[string]any{
		"filenames": []string{"fast-card.mp3"},
	}, token)
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "application/x-tar" ||
		rr.Header().Get("X-Kelma-Media-Files") != "1" || rr.Header().Get("X-Kelma-Media-Bytes") != "10" {
		t.Fatalf("media TAR: code=%d headers=%v body=%q", rr.Code, rr.Header(), rr.Body.String())
	}
	tarReader := tar.NewReader(bytes.NewReader(rr.Body.Bytes()))
	header, err := tarReader.Next()
	if err != nil {
		t.Fatalf("read media TAR header: %v", err)
	}
	archived, err := io.ReadAll(tarReader)
	if err != nil {
		t.Fatalf("read media TAR body: %v", err)
	}
	if header.Name != "0" || header.Size != 10 || string(archived) != "test audio" {
		t.Fatalf("unexpected media TAR entry: header=%+v body=%q", header, archived)
	}
	if _, err := tarReader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("media TAR has trailing entry: %v", err)
	}

	rr = reqJSON(t, mux, "POST", "/v2/media/download-tar", map[string]any{
		"filenames": []string{"missing.mp3"},
	}, token)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing media TAR: got %d body=%s", rr.Code, rr.Body.String())
	}

	rr = reqJSON(t, mux, "POST", "/v2/media/prepare-tar", map[string]any{
		"filenames": []string{"fast-card.mp3"},
	}, token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("prepare media TAR: got %d body=%s", rr.Code, rr.Body.String())
	}
	var prepared preparedMediaTarResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.JobID == "" || prepared.Files != 1 || prepared.PayloadBytes != 10 || prepared.ArchiveBytes <= 10 {
		t.Fatalf("unexpected prepared media TAR: %+v", prepared)
	}
	rr = reqJSON(t, mux, "GET", "/v2/media/prepared-tar/"+prepared.JobID, nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("get prepared media TAR: got %d body=%s", rr.Code, rr.Body.String())
	}
	tarReader = tar.NewReader(bytes.NewReader(rr.Body.Bytes()))
	header, err = tarReader.Next()
	if err != nil {
		t.Fatalf("read prepared media TAR: %v", err)
	}
	archived, err = io.ReadAll(tarReader)
	if err != nil || header.Name != "0" || string(archived) != "test audio" {
		t.Fatalf("unexpected prepared TAR entry: header=%+v body=%q err=%v", header, archived, err)
	}
	rr = reqJSON(t, mux, "DELETE", "/v2/media/prepared-tar/"+prepared.JobID, nil, token)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("downloaded media TAR was not removed automatically: got %d body=%s", rr.Code, rr.Body.String())
	}
	rr = reqJSON(t, mux, "GET", "/v2/media/prepared-tar/"+prepared.JobID, nil, token)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("downloaded prepared media TAR remained available: got %d", rr.Code)
	}

	rr = reqJSON(t, mux, "POST", "/v2/media/prepare-tar", map[string]any{
		"filenames": []string{"fast-card.mp3"},
	}, token)
	if err := json.Unmarshal(rr.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	interrupted := newInterruptedResponseWriter()
	request := httptest.NewRequest("GET", "/v2/media/prepared-tar/"+prepared.JobID, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	mux.ServeHTTP(interrupted, request)
	if interrupted.status != http.StatusOK {
		t.Fatalf("interrupted prepared media TAR did not start: got %d", interrupted.status)
	}
	rr = reqJSON(t, mux, "GET", "/v2/media/prepared-tar/"+prepared.JobID, nil, token)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("interrupted prepared media TAR remained available: got %d", rr.Code)
	}

	rr = reqJSON(t, mux, "HEAD", "/v2/media/fast-card.mp3", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("bundled media missing: got %d", rr.Code)
	}
}

type interruptedResponseWriter struct {
	header http.Header
	status int
}

func newInterruptedResponseWriter() *interruptedResponseWriter {
	return &interruptedResponseWriter{header: make(http.Header)}
}

func (w *interruptedResponseWriter) Header() http.Header { return w.header }

func (w *interruptedResponseWriter) WriteHeader(status int) { w.status = status }

func (w *interruptedResponseWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("simulated client disconnect")
}

func TestHealth(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	rr := reqJSON(t, mux, "GET", "/health", nil, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("health: got %d body=%s", rr.Code, rr.Body.String())
	}
}
