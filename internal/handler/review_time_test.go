package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Kelma Immersion's progress counts reviews on the day they were done, from
// any client: review time per deck per day, in the learner's own days.
func TestReviewTimeSummary(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "KelmaReview")

	// Two reviews on one evening (UTC−5), one past midnight UTC, one answer
	// over the 60 s cap, another deck the next day, and one too old to count.
	evening := time.Date(2026, 11, 15, 23, 30, 0, 0, time.UTC) // 18:30 at UTC−5
	review := func(at time.Time, deck string, millis int, card int) map[string]any {
		return map[string]any{
			"review_id": at.UnixMilli(), "source_card_id": int64(card),
			"note_guid": fmt.Sprintf("guid-%d", card), "card_ord": 0, "deck_name": deck,
			"ease": 3, "interval": 3, "last_interval": 1, "factor": 2500,
			"taken_millis": millis, "review_kind": 1,
		}
	}
	reviews := []any{
		review(evening, "Español", 8000, 1),
		review(evening.Add(45*time.Minute), "Español", 120000, 2), // 00:15 UTC on the 16th, still the 15th at UTC−5; capped at 60 s
		review(evening.Add(20*time.Hour), "Arabic", 5000, 3),
		review(time.Now().Add(-500*24*time.Hour), "Español", 9000, 4),
	}
	batch := map[string]any{"notes": []any{}, "cards": []any{}, "reviews": reviews, "study_days": []any{}, "notetypes": []any{}, "decks": []any{}}
	if rr := reqJSON(t, mux, "POST", "/v2/batch/push", batch, token); rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}

	get := func(query string) reviewTimeResponse {
		t.Helper()
		rr := reqJSON(t, mux, "GET", "/v2/summary/review-time"+query, nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("summary %s: %d %s", query, rr.Code, rr.Body.String())
		}
		var out reviewTimeResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got := get("?offset_minutes=-300")
	want := []reviewTimeRow{
		{DeckName: "Español", Day: "2026-11-15", Reviews: 2, Seconds: 68},
		{DeckName: "Arabic", Day: "2026-11-16", Reviews: 1, Seconds: 5},
	}
	if len(got.Days) != len(want) {
		t.Fatalf("days: %+v", got.Days)
	}
	for i := range want {
		if got.Days[i] != want[i] {
			t.Fatalf("day %d: got %+v want %+v", i, got.Days[i], want[i])
		}
	}
	// In UTC the second review is on the 16th.
	utc := get("")
	if len(utc.Days) != 3 || utc.Days[0] != (reviewTimeRow{DeckName: "Español", Day: "2026-11-15", Reviews: 1, Seconds: 8}) || utc.Days[2] != (reviewTimeRow{DeckName: "Español", Day: "2026-11-16", Reviews: 1, Seconds: 60}) {
		t.Fatalf("utc days: %+v", utc.Days)
	}
	// An undone review (retracted) no longer counts.
	if _, err := pool.Exec(t.Context(), `INSERT INTO review_retractions(user_id, review_id, intent_id, client_modified_at)
		SELECT user_id, review_id, gen_random_uuid(), now() FROM reviews WHERE source_card_id = 3`); err != nil {
		t.Fatal(err)
	}
	if undone := get("?offset_minutes=-300"); len(undone.Days) != 1 || undone.Days[0].DeckName != "Español" {
		t.Fatalf("after undo: %+v", undone.Days)
	}
	// since_ms narrows (never past the 400-day floor).
	if later := get(fmt.Sprintf("?since_ms=%d", evening.Add(time.Hour).UnixMilli())); len(later.Days) != 0 {
		t.Fatalf("since (the Arabic review was undone): %+v", later.Days)
	}
	if rr := reqJSON(t, mux, "GET", "/v2/summary/review-time?offset_minutes=9999", nil, token); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad offset: %d", rr.Code)
	}
	if rr := reqJSON(t, mux, "GET", "/v2/summary/review-time", nil, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rr.Code)
	}
	// Another account sees none of these.
	_ = reqJSON(t, mux, "POST", "/v2/auth/register", map[string]any{"username": "other", "password": "other"}, "")
	rr := reqJSON(t, mux, "POST", "/v2/auth/login", map[string]any{"username": "other", "password": "other", "client_label": "x"}, "")
	var login struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &login)
	rr = reqJSON(t, mux, "GET", "/v2/summary/review-time", nil, login.Token)
	if rr.Code != http.StatusOK || rr.Body.String() != "{\"days\":[]}\n" {
		t.Fatalf("other account: %d %s", rr.Code, rr.Body.String())
	}
}
