package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type browserReviewFixture struct {
	handler       *Handler
	mux           *http.ServeMux
	token         string
	userID        string
	clientID      string
	now           time.Time
	priorReviewID int64
}

func setupBrowserReview(t *testing.T) browserReviewFixture {
	t.Helper()
	h, mux, pool := testServer(t)
	t.Cleanup(pool.Close)
	h.BrowserReviewRoutes(mux)
	token := loginDemo(t, mux, "browser-review-test")
	fixed := time.Date(2026, 9, 8, 12, 0, 0, 250_000_000, time.UTC)
	h.now = func() time.Time { return fixed }
	var userID, clientID string
	if err := pool.QueryRow(context.Background(), `SELECT u.id,c.id::text FROM users u JOIN clients c ON c.user_id=u.id
		WHERE u.username='demo' AND c.label='browser-review-test'`).Scan(&userID, &clientID); err != nil {
		t.Fatal(err)
	}
	priorID := fixed.Add(-7 * 24 * time.Hour).UnixMilli()
	review := putReviewRequest{
		ReviewID: priorID, SourceCardID: 9001, NoteGUID: "review-note",
		CardOrd: 0, DeckName: "Review Deck::Child", Ease: 3, Interval: 1,
		LastInterval: 0, Factor: 2500, TakenMillis: 1500, ReviewKind: 0,
	}
	historyHash := strings.Repeat("a", 64)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO decks(user_id,name,config,checksum,modified_at,client_modified_at)
		VALUES($1,'Review Deck','{}','deck',now(),now())`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO notetypes(user_id,notetype_id,name,definition,checksum,modified_at,client_modified_at)
		VALUES($1,1,'Rich Basic',$2,'notetype-checksum',now(),now())`, userID, map[string]any{
		"css":  ".card { color: navy; }",
		"flds": []any{map[string]any{"name": "Front"}, map[string]any{"name": "Back"}},
		"tmpls": []any{map[string]any{
			"name": "Card 1", "ord": 0, "qfmt": "{{Front}}", "afmt": "{{FrontSide}}{{Back}}",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO notes(user_id,guid,notetype_id,fields,tags,checksum,modified_at,client_modified_at)
		VALUES($1,'review-note',1,'["question","answer"]','{}','note-checksum',now(),now())`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO cards(user_id,card_id,note_guid,deck_name,ord,scheduling,modified_at,client_modified_at,last_client_id)
		VALUES($1,9001,'review-note','Review Deck::Child',0,$2,now(),now(),$3::uuid)`, userID,
		map[string]any{"type": 2, "queue": 2, "ivl": 1}, clientID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO reviews(user_id,review_id,source_card_id,note_guid,card_ord,deck_name,ease,interval,
		last_interval,factor,taken_millis,review_kind,checksum,modified_at,last_client_id)
		VALUES($1,$2,9001,'review-note',0,'Review Deck::Child',3,1,0,2500,1500,0,$3,now(),$4::uuid)`,
		userID, priorID, checksumForReview(review), clientID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO scheduler_projections(user_id,note_guid,card_ord,desired_history_hash,desired_through_review_id,
		desired_profile_version,applied_history_hash,applied_through_review_id,applied_profile_version,status)
		VALUES($1,'review-note',0,$2,$3,0,$2,$3,0,'succeeded')`, userID, historyHash, priorID); err != nil {
		t.Fatal(err)
	}
	return browserReviewFixture{
		handler: h, mux: mux, token: token, userID: userID,
		clientID: clientID, now: fixed, priorReviewID: priorID,
	}
}

func decodeMap(t *testing.T, response []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(response, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestBrowserReviewDoesNotExposeServerOwnedSchedulingRoutes(t *testing.T) {
	f := setupBrowserReview(t)
	for _, path := range []string{
		"/v2/review/decks",
		"/v2/review/stats",
		"/v2/review/sessions/current",
		"/v2/review/attempts/22222222-2222-4222-8222-222222222222",
		"/v2/review/operations/55555555-5555-4555-8555-555555555555",
	} {
		if response := reqJSON(t, f.mux, http.MethodGet, path, nil, f.token); response.Code != http.StatusNotFound {
			t.Fatalf("removed route %s returned %d", path, response.Code)
		}
	}
}
