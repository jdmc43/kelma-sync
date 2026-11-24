package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestReviewDaysSummary(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "KelmaReview")
	ctx := t.Context()

	// The account's day: New York, from 4 am.
	if rr := reqJSON(t, mux, "PUT", "/v2/study-day-policy", map[string]any{
		"base_version": 0, "timezone_id": "America/New_York", "day_start_hour": 4, "idempotency_key": "review-days",
	}, token); rr.Code != http.StatusOK {
		t.Fatalf("policy: %d %s", rr.Code, rr.Body.String())
	}
	var userID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE username='demo'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	newYork, _ := time.LoadLocation("America/New_York")
	studyDate := time.Now().In(newYork).Add(-4 * time.Hour)
	// k days before today's study day, at a local hour.
	at := func(k, hour int) time.Time {
		year, month, day := studyDate.AddDate(0, 0, -k).Date()
		return time.Date(year, month, day, hour, 0, 0, 0, newYork)
	}
	dayText := func(k int) string { return studyDate.AddDate(0, 0, -k).Format("2006-01-02") }

	// Cards that exist now (7 is suspended); card 6 was deleted.
	for card := 1; card <= 7; card++ {
		if card == 6 {
			continue
		}
		state := "active"
		if card == 4 {
			state = "suspended"
		}
		if _, err := pool.Exec(ctx, `INSERT INTO cards(user_id, card_id, note_guid, deck_name, ord, scheduling, modified_at, client_modified_at, study_state)
			VALUES($1, $2, $3, 'Deck', 0, '{}', now(), now(), $4)`, userID, 100+card, fmt.Sprintf("guid-%d", card), state); err != nil {
			t.Fatal(err)
		}
	}
	review := func(card, k, hour, interval, ease, kind int) map[string]any {
		return map[string]any{
			"review_id": at(k, hour).UnixMilli() + int64(card), "source_card_id": int64(100 + card),
			"note_guid": fmt.Sprintf("guid-%d", card), "card_ord": 0, "deck_name": "Deck",
			"ease": ease, "interval": interval, "last_interval": 0, "factor": 2500,
			"taken_millis": 5000, "review_kind": kind,
		}
	}
	reviews := []any{
		review(1, 10, 10, 3, 3, 1), // due 7 days ago; answered then, due 2 days ago; not since
		review(1, 7, 10, 5, 3, 1),
		review(2, 10, 10, 2, 3, 1), // due 8 days ago, answered 2 days late
		review(2, 6, 10, 10, 3, 1),
		review(3, 5, 10, -600, 1, 0), // learning (10 minutes): due the next day
		review(3, 4, 10, 1, 3, 0),
		review(3, 3, 10, 4, 3, 1),
		review(4, 10, 10, 1, 3, 1), // suspended today: not due before; handled (due, done) today
		review(5, 9, 10, 3, 3, 1),  // forgotten (reset to new) before it came due
		review(5, 8, 10, 0, 0, 4),
		review(6, 1, 10, 1, 3, 1), // deleted: its answer counts, it isn't due
		review(7, 2, 2, 30, 3, 1), // 2 am: still the study day before
	}
	batch := map[string]any{"notes": []any{}, "cards": []any{}, "reviews": reviews, "study_days": []any{}, "notetypes": []any{}, "decks": []any{}}
	if rr := reqJSON(t, mux, "POST", "/v2/batch/push", batch, token); rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}

	rr := reqJSON(t, mux, "GET", "/v2/summary/review-days?offset_minutes=-300", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("summary: %d %s", rr.Code, rr.Body.String())
	}
	var got reviewDaysResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.TimezoneID != "America/New_York" || got.DayStartHour != 4 || got.Today != dayText(0) {
		t.Fatalf("day: %+v", got)
	}
	want := []reviewDayRow{
		{Day: dayText(10), Due: 0, Arrived: 0, Done: 0, Reviews: 3},
		{Day: dayText(9), Due: 0, Arrived: 0, Done: 0, Reviews: 1},
		{Day: dayText(8), Due: 1, Arrived: 1, Done: 0, Reviews: 0},
		{Day: dayText(7), Due: 2, Arrived: 1, Done: 1, Reviews: 1},
		{Day: dayText(6), Due: 1, Arrived: 0, Done: 1, Reviews: 1},
		{Day: dayText(5), Due: 0, Arrived: 0, Done: 0, Reviews: 1},
		{Day: dayText(4), Due: 1, Arrived: 1, Done: 1, Reviews: 1},
		{Day: dayText(3), Due: 1, Arrived: 1, Done: 1, Reviews: 2},
		{Day: dayText(2), Due: 1, Arrived: 1, Done: 0, Reviews: 0},
		{Day: dayText(1), Due: 1, Arrived: 0, Done: 0, Reviews: 1},
		{Day: dayText(0), Due: 2, Arrived: 0, Done: 1, Reviews: 0},
	}
	if len(got.Days) != len(want) {
		t.Fatalf("days: %+v", got.Days)
	}
	for i := range want {
		if got.Days[i] != want[i] {
			t.Fatalf("day %d: got %+v want %+v", i, got.Days[i], want[i])
		}
	}
	firstKnown := map[int64]string{}
	for _, row := range got.FirstKnown {
		firstKnown[row.CardID] = row.Day
	}
	// Card 5 was reset to new after its first answer: not known now.
	wantKnown := map[int64]string{101: dayText(10), 102: dayText(10), 103: dayText(4), 104: dayText(10), 107: dayText(3)}
	if len(firstKnown) != len(wantKnown) {
		t.Fatalf("first known: %+v", got.FirstKnown)
	}
	for card, day := range wantKnown {
		if firstKnown[card] != day {
			t.Fatalf("first known %d: got %q want %q (%+v)", card, firstKnown[card], day, got.FirstKnown)
		}
	}

	// Without a policy: offset_minutes, from 4 am.
	if _, err := pool.Exec(ctx, `DELETE FROM study_day_policies WHERE user_id=$1`, userID); err != nil {
		t.Fatal(err)
	}
	rr = reqJSON(t, mux, "GET", "/v2/summary/review-days?offset_minutes=-300", nil, token)
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || got.TimezoneID != "" || got.DayStartHour != 4 {
		t.Fatalf("offset day: %+v err=%v", got, err)
	}
	if rr := reqJSON(t, mux, "GET", "/v2/summary/review-days?offset_minutes=900", nil, token); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad offset: %d", rr.Code)
	}
}

// Per language (a group of decks), the day as Kelma Review serves it: new
// cards first within the new-card limits (counting against the review limit),
// then due cards in what is left of the review limits, only in decks reviewed
// in the last 14 days.
func TestReviewDaysGroups(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "KelmaReview")
	ctx := t.Context()
	if rr := reqJSON(t, mux, "PUT", "/v2/study-day-policy", map[string]any{
		"base_version": 0, "timezone_id": "America/New_York", "day_start_hour": 4, "idempotency_key": "review-groups",
	}, token); rr.Code != http.StatusOK {
		t.Fatalf("policy: %d %s", rr.Code, rr.Body.String())
	}
	var userID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE username='demo'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	newYork, _ := time.LoadLocation("America/New_York")
	studyDate := time.Now().In(newYork).Add(-4 * time.Hour)
	at := func(k, hour int) time.Time {
		year, month, day := studyDate.AddDate(0, 0, -k).Date()
		return time.Date(year, month, day, hour, 0, 0, 0, newYork)
	}
	dayText := func(k int) string { return studyDate.AddDate(0, 0, -k).Format("2006-01-02") }
	// German: at most 2 reviews and 1 new card a day; its subdeck uses the defaults.
	for _, deck := range []struct{ name, config string }{{"German", `{"newLimit": 1, "reviewLimit": 2}`}, {"German::A", `{}`}, {"French", `{"conf": 1, "newLimit": null}`}} {
		if _, err := pool.Exec(ctx, `INSERT INTO decks(user_id, name, config, checksum, modified_at, client_modified_at) VALUES($1, $2, $3, 'x', now(), now())`, userID, deck.name, deck.config); err != nil {
			t.Fatal(err)
		}
	}
	cards := []struct {
		id      int
		deck    string
		created int
	}{{1, "German::A", 10}, {2, "German::A", 10}, {3, "German", 10}, {4, "German::A", 5}, {5, "German::A", 5}, {6, "French", -1}}
	for _, card := range cards {
		var created any
		if card.created >= 0 {
			created = at(card.created, 10)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO cards(user_id, card_id, note_guid, deck_name, ord, scheduling, modified_at, client_modified_at, created_at)
			VALUES($1, $2, $3, $4, 0, '{}', now(), now(), $5)`, userID, 100+card.id, fmt.Sprintf("guid-%d", card.id), card.deck, created); err != nil {
			t.Fatal(err)
		}
	}
	review := func(card, k, interval int) map[string]any {
		return map[string]any{
			"review_id": at(k, 10).UnixMilli() + int64(card), "source_card_id": int64(100 + card),
			"note_guid": fmt.Sprintf("guid-%d", card), "card_ord": 0, "deck_name": "x",
			"ease": 3, "interval": interval, "last_interval": 0, "factor": 2500, "taken_millis": 5000, "review_kind": 0,
		}
	}
	batch := map[string]any{"notes": []any{}, "cards": []any{}, "study_days": []any{}, "notetypes": []any{}, "decks": []any{}, "reviews": []any{
		review(1, 3, 1), review(2, 3, 1), review(3, 3, 2), // learned 3 days ago
		review(5, 0, 1),  // a new card started today
		review(6, 20, 1), // French: not reviewed for 20 days
	}}
	if rr := reqJSON(t, mux, "POST", "/v2/batch/push", batch, token); rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	rr := reqJSON(t, mux, "POST", "/v2/summary/review-days", map[string]any{"offset_minutes": -300, "groups": map[string]any{"German": []string{"German", "German::A"}, "French": []string{"French"}}}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("groups: %d %s", rr.Code, rr.Body.String())
	}
	var got reviewDaysResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 2 || got.Groups[0].Name != "French" || got.Groups[1].Name != "German" {
		t.Fatalf("groups: %+v", got.Groups)
	}
	french, german := got.Groups[0], got.Groups[1]
	if strings.Join(german.Decks, ",") != "German,German::A" || german.ReviewLimit != 2 || german.NewLimit != 1 {
		t.Fatalf("german: %+v", german)
	}
	want := []reviewGroupDay{
		// 3 days ago: three new cards started (one served: the new limit).
		{Day: dayText(3), NewServed: 1, NewDone: 3, Reviews: 3},
		// New cards first (1, the limit), then reviews in what is left of the limit of 2.
		{Day: dayText(2), Due: 2, Served: 1, NewServed: 1},
		{Day: dayText(1), Due: 3, Served: 1, NewServed: 1},
		{Day: dayText(0), Due: 3, Served: 1, NewServed: 1, NewDone: 1, Reviews: 1},
	}
	if len(german.Days) != len(want) {
		t.Fatalf("german days: %+v", german.Days)
	}
	for i := range want {
		if german.Days[i] != want[i] {
			t.Fatalf("german day %d: got %+v want %+v", i, german.Days[i], want[i])
		}
	}
	// French: Kelma Review's defaults; its deck counts only while reviewed (14 days after the answer).
	if french.ReviewLimit != 200 || french.NewLimit != 20 {
		t.Fatalf("french limits: %+v", french)
	}
	if first := french.Days[0]; first != (reviewGroupDay{Day: dayText(20), NewServed: 1, NewDone: 1, Reviews: 1}) {
		t.Fatalf("french first day: %+v", first)
	}
	last := french.Days[len(french.Days)-1]
	if last.Day != dayText(7) || last.Due != 1 || last.Served != 1 || len(french.Days) != 14 {
		t.Fatalf("french last day: %+v (%d days)", last, len(french.Days))
	}
	if rr := reqJSON(t, mux, "POST", "/v2/summary/review-days", map[string]any{"groups": map[string]any{"x": []string{"a"}}, "other": 1}, token); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", rr.Code)
	}
}

// Kelma Review and the website work a card's due date out again from its whole
// history, so they can serve a card a day before its interval says: answered
// then, it was due and done that day. A card suspended counts as handled the day
// it was suspended (when due or new): suspending it mid-review uses up its place
// in Kelma Review's day. The other days leave a suspended card out, as before.
func TestReviewDaysEarlyAndSuspended(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "KelmaReview")
	ctx := t.Context()
	if rr := reqJSON(t, mux, "PUT", "/v2/study-day-policy", map[string]any{
		"base_version": 0, "timezone_id": "America/New_York", "day_start_hour": 4, "idempotency_key": "review-early",
	}, token); rr.Code != http.StatusOK {
		t.Fatalf("policy: %d %s", rr.Code, rr.Body.String())
	}
	var userID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE username='demo'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	newYork, _ := time.LoadLocation("America/New_York")
	studyDate := time.Now().In(newYork).Add(-4 * time.Hour)
	at := func(k, hour int) time.Time {
		year, month, day := studyDate.AddDate(0, 0, -k).Date()
		return time.Date(year, month, day, hour, 0, 0, 0, newYork)
	}
	dayText := func(k int) string { return studyDate.AddDate(0, 0, -k).Format("2006-01-02") }
	// At most 1 review a day: the day is full (capped), as with the learner's deck.
	if _, err := pool.Exec(ctx, `INSERT INTO decks(user_id, name, config, checksum, modified_at, client_modified_at) VALUES($1, 'D', '{"newLimit": 5, "reviewLimit": 6}', 'x', now(), now())`, userID); err != nil {
		t.Fatal(err)
	}
	// card: (suspended at k days ago, or -1), created k days ago
	cards := []struct{ id, suspendedK, createdK int }{
		{1, -1, 20}, // answered 6 days ago (interval 6), again 1 day early (yesterday)
		{2, -1, 20}, // the same, interval 6 from 6 days ago, not answered since: due today
		{3, 1, 20},  // a review due 3 days ago, suspended yesterday: handled yesterday
		{4, 2, 10},  // a new card suspended 2 days ago: a new card handled then
		{5, 1, 20},  // answered and suspended yesterday
	}
	for _, card := range cards {
		state, suspendedAt := "active", any(at(card.createdK, 9))
		if card.suspendedK >= 0 {
			state, suspendedAt = "suspended", at(card.suspendedK, 18)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO cards(user_id, card_id, note_guid, deck_name, ord, scheduling, modified_at, client_modified_at, study_state, study_state_client_modified_at, created_at)
			VALUES($1, $2, $3, 'D', 0, '{}', now(), now(), $4, $5, $6)`, userID, 300+card.id, fmt.Sprintf("early-%d", card.id), state, suspendedAt, at(card.createdK, 9)); err != nil {
			t.Fatal(err)
		}
	}
	review := func(card, k, interval, kind int) map[string]any {
		return map[string]any{
			"review_id": at(k, 10).UnixMilli() + int64(card), "source_card_id": int64(300 + card),
			"note_guid": fmt.Sprintf("early-%d", card), "card_ord": 0, "deck_name": "D",
			"ease": 3, "interval": interval, "last_interval": 0, "factor": 2500, "taken_millis": 5000, "review_kind": kind,
		}
	}
	reviews := []any{
		review(1, 12, 6, 1), review(1, 6, 6, 1), review(1, 1, 10, 1),
		review(2, 12, 6, 1), review(2, 6, 6, 1),
		review(3, 8, 5, 1),
		review(5, 9, 8, 1), review(5, 1, 10, 1),
	}
	batch := map[string]any{"notes": []any{}, "cards": []any{}, "reviews": reviews, "study_days": []any{}, "notetypes": []any{}, "decks": []any{}}
	if rr := reqJSON(t, mux, "POST", "/v2/batch/push", batch, token); rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	rr := reqJSON(t, mux, "POST", "/v2/summary/review-days", map[string]any{"offset_minutes": -300, "groups": map[string]any{"D": []string{"D"}}}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("groups: %d %s", rr.Code, rr.Body.String())
	}
	var got reviewDaysResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || len(got.Groups) != 1 {
		t.Fatalf("groups: %s err=%v", rr.Body.String(), err)
	}
	byDay := map[string]reviewGroupDay{}
	for _, day := range got.Groups[0].Days {
		byDay[day.Day] = day
	}
	// Yesterday: card 1 answered a day early (due, done), card 3 suspended while due
	// (due, done), card 5 answered and suspended (due, done): 3 due, all handled.
	if y := byDay[dayText(1)]; y.Due != 3 || y.Done != 3 || y.Served != 3 || y.NewServed != 0 {
		t.Fatalf("yesterday: %+v", y)
	}
	// Card 3 was due 3 and 2 days ago and is suspended now: not counted then (as before).
	if d := byDay[dayText(2)]; d.Due != 0 || d.NewServed != 1 || d.NewDone != 1 {
		t.Fatalf("2 days ago (card 4, a new card suspended): %+v", d)
	}
	if d := byDay[dayText(3)]; d.Due != 0 {
		t.Fatalf("3 days ago: %+v", d)
	}
	// Today: card 2 is due (its interval: 6 days); card 1 isn't (answered yesterday).
	if d := byDay[dayText(0)]; d.Due != 1 || d.Done != 0 {
		t.Fatalf("today: %+v", d)
	}
	// Card 4 is new only until the day it was suspended.
	for k := 1; k >= 0; k-- {
		if d := byDay[dayText(k)]; d.NewServed != 0 {
			t.Fatalf("day -%d new: %+v", k, d)
		}
	}
}

// A deck's limits as they were each day: raising a limit today doesn't make
// yesterday short (deck_limit_history, kept by a trigger on decks).
func TestReviewDaysLimitHistory(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "KelmaReview")
	ctx := t.Context()
	if rr := reqJSON(t, mux, "PUT", "/v2/study-day-policy", map[string]any{
		"base_version": 0, "timezone_id": "America/New_York", "day_start_hour": 4, "idempotency_key": "review-limits",
	}, token); rr.Code != http.StatusOK {
		t.Fatalf("policy: %d %s", rr.Code, rr.Body.String())
	}
	var userID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE username='demo'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	newYork, _ := time.LoadLocation("America/New_York")
	studyDate := time.Now().In(newYork).Add(-4 * time.Hour)
	at := func(k, hour int) time.Time {
		year, month, day := studyDate.AddDate(0, 0, -k).Date()
		return time.Date(year, month, day, hour, 0, 0, 0, newYork)
	}
	dayText := func(k int) string { return studyDate.AddDate(0, 0, -k).Format("2006-01-02") }
	if _, err := pool.Exec(ctx, `INSERT INTO decks(user_id, name, config, checksum, modified_at, client_modified_at)
		VALUES($1, 'L', '{"conf": 1, "newLimit": 0, "reviewLimit": 2}', 'x', now(), now())`, userID); err != nil {
		t.Fatal(err)
	}
	// Four cards due yesterday (answered 12 days ago, interval 11); two answered yesterday.
	reviews := []any{}
	for card := 1; card <= 4; card++ {
		if _, err := pool.Exec(ctx, `INSERT INTO cards(user_id, card_id, note_guid, deck_name, ord, scheduling, modified_at, client_modified_at, study_state)
			VALUES($1, $2, $3, 'L', 0, '{}', now(), now(), 'active')`, userID, 400+card, fmt.Sprintf("limit-%d", card)); err != nil {
			t.Fatal(err)
		}
		days := []int{12}
		if card <= 2 {
			days = append(days, 1)
		}
		for _, k := range days {
			reviews = append(reviews, map[string]any{
				"review_id": at(k, 10).UnixMilli() + int64(card), "source_card_id": int64(400 + card),
				"note_guid": fmt.Sprintf("limit-%d", card), "card_ord": 0, "deck_name": "L",
				"ease": 3, "interval": 11, "last_interval": 0, "factor": 2500, "taken_millis": 5000, "review_kind": 1,
			})
		}
	}
	batch := map[string]any{"notes": []any{}, "cards": []any{}, "reviews": reviews, "study_days": []any{}, "notetypes": []any{}, "decks": []any{}}
	if rr := reqJSON(t, mux, "POST", "/v2/batch/push", batch, token); rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	// Today the learner raises the review limit to 4 (any write: here the deck PUT).
	if rr := reqJSON(t, mux, "PUT", "/v2/decks/L", map[string]any{
		"config": map[string]any{"conf": 1, "newLimit": 0, "reviewLimit": 4}, "client_modified_at": time.Now().UTC().Format(time.RFC3339), "base_checksum": "x",
	}, token); rr.Code != http.StatusOK {
		t.Fatalf("deck: %d %s", rr.Code, rr.Body.String())
	}
	var before string
	if err := pool.QueryRow(ctx, `SELECT limits::text FROM deck_limit_history WHERE user_id=$1 AND deck_name='L'`, userID).Scan(&before); err != nil || before != `{"newLimit": 0, "reviewLimit": 2}` {
		t.Fatalf("history: %q err=%v", before, err)
	}
	rr := reqJSON(t, mux, "POST", "/v2/summary/review-days", map[string]any{"offset_minutes": -300, "groups": map[string]any{"L": []string{"L"}}}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("groups: %d %s", rr.Code, rr.Body.String())
	}
	var got reviewDaysResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || len(got.Groups) != 1 {
		t.Fatalf("groups: %s err=%v", rr.Body.String(), err)
	}
	byDay := map[string]reviewGroupDay{}
	for _, day := range got.Groups[0].Days {
		byDay[day.Day] = day
	}
	// Yesterday: 4 due, 2 served (the limit then), both done — not 4 served (2 short).
	if y := byDay[dayText(1)]; y.Due != 4 || y.Served != 2 || y.Done != 2 {
		t.Fatalf("yesterday: %+v", y)
	}
	// Today: the new limit (4): the 2 cards still due.
	if d := byDay[dayText(0)]; d.Due != 2 || d.Served != 2 || d.Done != 0 {
		t.Fatalf("today: %+v", d)
	}
	if got.Groups[0].ReviewLimit != 4 {
		t.Fatalf("current limit: %+v", got.Groups[0])
	}

	// Other config changes leave no history; a renamed deck keeps its own.
	if _, err := pool.Exec(ctx, `UPDATE decks SET config = config || '{"desc": "x"}' WHERE user_id=$1 AND name='L'`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE decks SET name = 'L2' WHERE user_id=$1 AND name='L'`, userID); err != nil {
		t.Fatal(err)
	}
	var rows int
	var name string
	if err := pool.QueryRow(ctx, `SELECT count(*), max(deck_name) FROM deck_limit_history WHERE user_id=$1`, userID).Scan(&rows, &name); err != nil || rows != 1 || name != "L2" {
		t.Fatalf("history after edits: %d %q err=%v", rows, name, err)
	}
}

// A card's current due date as Kelma Review and the website have it (they work it
// out again from the whole history): the FSRS projection's, when up to date; a
// due date set by hand since the last answer; none after a schedule reset. Seen:
// ten cards due by their last interval that the projection had due in 1–21 days
// kept "8 left" on an empty queue.
func TestReviewDaysProjection(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "KelmaReview")
	ctx := t.Context()
	if rr := reqJSON(t, mux, "PUT", "/v2/study-day-policy", map[string]any{
		"base_version": 0, "timezone_id": "America/New_York", "day_start_hour": 4, "idempotency_key": "review-projection",
	}, token); rr.Code != http.StatusOK {
		t.Fatalf("policy: %d %s", rr.Code, rr.Body.String())
	}
	var userID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE username='demo'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	newYork, _ := time.LoadLocation("America/New_York")
	studyDate := time.Now().In(newYork).Add(-4 * time.Hour)
	at := func(k, hour int) time.Time {
		year, month, day := studyDate.AddDate(0, 0, -k).Date()
		return time.Date(year, month, day, hour, 0, 0, 0, newYork)
	}
	dayText := func(k int) string { return studyDate.AddDate(0, 0, -k).Format("2006-01-02") }
	if _, err := pool.Exec(ctx, `INSERT INTO decks(user_id, name, config, checksum, modified_at, client_modified_at) VALUES($1, 'P', '{"newLimit": 0, "reviewLimit": 50}', 'x', now(), now())`, userID); err != nil {
		t.Fatal(err)
	}
	reviewAt := func(card, k int) int64 { return at(k, 10).UnixMilli() + int64(card) }
	type cardCase struct {
		id, answeredK, interval, kind int
		scheduling                    string
		overrideK                     int // -1: none
		reset                         bool
	}
	cases := []cardCase{
		{1, 6, 6, 1, fmt.Sprintf(`{"_kelma_fsrs": {"due_at_millis": %d, "through_review_id": %d}}`, at(-3, 10).UnixMilli(), reviewAt(1, 6)), -1, false},      // due today by interval; projection: in 3 days
		{2, 6, 6, 1, fmt.Sprintf(`{"_kelma_fsrs": {"due_at_millis": %d, "through_review_id": %d}}`, at(-3, 10).UnixMilli(), reviewAt(2, 6)-1000), -1, false}, // projection behind its history: the interval
		{3, 10, 0, 0, fmt.Sprintf(`{"_kelma_fsrs": {"due_at_millis": %d, "through_review_id": %d}}`, at(-20, 10).UnixMilli(), reviewAt(3, 10)), -1, false},   // a learning answer with interval 0 (overdue by it): projection in 20 days
		{4, 3, 30, 1, `{}`, 0, false}, // due in a month, a due date set to today since
		{5, 8, 2, 1, `{}`, -1, true},  // overdue, then reset to new
	}
	reviews := []any{}
	for _, c := range cases {
		override, overrideAt := int64(0), time.Unix(0, 0)
		if c.overrideK >= 0 {
			year, month, day := studyDate.AddDate(0, 0, -c.overrideK).Date()
			override, overrideAt = time.Date(year, month, day, 0, 0, 0, 0, time.UTC).UnixMilli(), at(1, 12)
		}
		reset := int64(0)
		if c.reset {
			reset = reviewAt(c.id, c.answeredK)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO cards(user_id, card_id, note_guid, deck_name, ord, scheduling, modified_at, client_modified_at, study_state, due_date_override_ms, due_date_override_client_modified_at, schedule_reset_through_review_id)
			VALUES($1, $2, $3, 'P', 0, $4, now(), now(), 'active', $5, $6, $7)`, userID, 500+c.id, fmt.Sprintf("proj-%d", c.id), c.scheduling, override, overrideAt, reset); err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, map[string]any{
			"review_id": reviewAt(c.id, c.answeredK), "source_card_id": int64(500 + c.id),
			"note_guid": fmt.Sprintf("proj-%d", c.id), "card_ord": 0, "deck_name": "P",
			"ease": 3, "interval": c.interval, "last_interval": 0, "factor": 2500, "taken_millis": 5000, "review_kind": c.kind,
		})
	}
	// An answer yesterday keeps the deck among those reviewed.
	if _, err := pool.Exec(ctx, `INSERT INTO cards(user_id, card_id, note_guid, deck_name, ord, scheduling, modified_at, client_modified_at, study_state) VALUES($1, 599, 'proj-active', 'P', 0, '{}', now(), now(), 'active')`, userID); err != nil {
		t.Fatal(err)
	}
	reviews = append(reviews, map[string]any{"review_id": at(1, 11).UnixMilli(), "source_card_id": int64(599), "note_guid": "proj-active", "card_ord": 0, "deck_name": "P", "ease": 3, "interval": 30, "last_interval": 0, "factor": 2500, "taken_millis": 5000, "review_kind": 1})
	batch := map[string]any{"notes": []any{}, "cards": []any{}, "reviews": reviews, "study_days": []any{}, "notetypes": []any{}, "decks": []any{}}
	if rr := reqJSON(t, mux, "POST", "/v2/batch/push", batch, token); rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	rr := reqJSON(t, mux, "POST", "/v2/summary/review-days", map[string]any{"offset_minutes": -300, "groups": map[string]any{"P": []string{"P"}}}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("groups: %d %s", rr.Code, rr.Body.String())
	}
	var got reviewDaysResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || len(got.Groups) != 1 {
		t.Fatalf("groups: %s err=%v", rr.Body.String(), err)
	}
	byDay := map[string]reviewGroupDay{}
	for _, day := range got.Groups[0].Days {
		byDay[day.Day] = day
	}
	// Today: card 2 (stale projection: its interval) and card 4 (set to today) only.
	if d := byDay[dayText(0)]; d.Due != 2 || d.Served != 2 {
		t.Fatalf("today: %+v (want cards 2 and 4 due)", d)
	}
	// Card 3 was never due in the window (its projection: in 20 days); card 5 not since its reset.
	if d := byDay[dayText(5)]; d.Due != 0 {
		t.Fatalf("5 days ago: %+v", d)
	}
}

// Suspending or deleting a card later never undoes a day: the days it was answered
// still count (the learner's German day: 26 new cards started, one suspended the
// next day, and the day showed 25 of 26).
func TestReviewDaysAnswersStayAfterSuspendOrDelete(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	token := loginDemo(t, mux, "KelmaReview")
	ctx := t.Context()
	if rr := reqJSON(t, mux, "PUT", "/v2/study-day-policy", map[string]any{
		"base_version": 0, "timezone_id": "America/New_York", "day_start_hour": 4, "idempotency_key": "review-gone",
	}, token); rr.Code != http.StatusOK {
		t.Fatalf("policy: %d %s", rr.Code, rr.Body.String())
	}
	var userID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE username='demo'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	newYork, _ := time.LoadLocation("America/New_York")
	studyDate := time.Now().In(newYork).Add(-4 * time.Hour)
	at := func(k, hour int) time.Time {
		year, month, day := studyDate.AddDate(0, 0, -k).Date()
		return time.Date(year, month, day, hour, 0, 0, 0, newYork)
	}
	dayText := func(k int) string { return studyDate.AddDate(0, 0, -k).Format("2006-01-02") }
	if _, err := pool.Exec(ctx, `INSERT INTO decks(user_id, name, config, checksum, modified_at, client_modified_at) VALUES($1, 'G', '{"newLimit": 2, "reviewLimit": 10}', 'x', now(), now())`, userID); err != nil {
		t.Fatal(err)
	}
	// 1, 2: new cards started 2 days ago; 1 suspended yesterday. 3: a review answered
	// 3 days ago when due, deleted yesterday (a tombstone names it). 4: a review
	// answered 4 days ago when due, deleted (no tombstone that names it).
	// 5: plenty of other new cards (the limit, not supply, decides).
	for _, card := range []struct {
		id        int
		suspended bool
	}{{1, true}, {2, false}, {3, false}, {4, false}, {5, false}, {6, false}, {7, false}} {
		state, suspendedAt := "active", any(at(20, 9))
		if card.suspended {
			state, suspendedAt = "suspended", at(1, 18)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO cards(user_id, card_id, note_guid, deck_name, ord, scheduling, modified_at, client_modified_at, study_state, study_state_client_modified_at, created_at)
			VALUES($1, $2, $3, 'G', 0, '{}', now(), now(), $4, $5, $6)`, userID, 700+card.id, fmt.Sprintf("gone-%d", card.id), state, suspendedAt, at(20, 9)); err != nil {
			t.Fatal(err)
		}
	}
	review := func(card, k, interval, kind int) map[string]any {
		return map[string]any{
			"review_id": at(k, 10).UnixMilli() + int64(card), "source_card_id": int64(700 + card),
			"note_guid": fmt.Sprintf("gone-%d", card), "card_ord": 0, "deck_name": "G",
			"ease": 3, "interval": interval, "last_interval": 0, "factor": 2500, "taken_millis": 5000, "review_kind": kind,
		}
	}
	reviews := []any{
		review(1, 2, 5, 0), review(2, 2, 5, 0), // started 2 days ago (due in 3 days)
		review(3, 10, 7, 1), review(3, 3, 9, 1), // due 3 days ago, answered then
		review(4, 9, 5, 1), review(4, 4, 9, 1), // due 4 days ago, answered then
	}
	batch := map[string]any{"notes": []any{}, "cards": []any{}, "reviews": reviews, "study_days": []any{}, "notetypes": []any{}, "decks": []any{}}
	if rr := reqJSON(t, mux, "POST", "/v2/batch/push", batch, token); rr.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rr.Code, rr.Body.String())
	}
	// Delete cards 3 and 4 (3 with a tombstone that names it, deleted yesterday).
	if _, err := pool.Exec(ctx, `DELETE FROM cards WHERE user_id=$1 AND note_guid IN ('gone-3', 'gone-4')`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tombstones(user_id, type, resource_id, deleted_at, note_guid, card_ord) VALUES($1, 'card', '703', $2, 'gone-3', 0)`, userID, at(1, 18)); err != nil {
		t.Fatal(err)
	}
	rr := reqJSON(t, mux, "POST", "/v2/summary/review-days", map[string]any{"offset_minutes": -300, "groups": map[string]any{"G": []string{"G"}}}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("groups: %d %s", rr.Code, rr.Body.String())
	}
	var got reviewDaysResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || len(got.Groups) != 1 {
		t.Fatalf("groups: %s err=%v", rr.Body.String(), err)
	}
	byDay := map[string]reviewGroupDay{}
	for _, day := range got.Groups[0].Days {
		byDay[day.Day] = day
	}
	// 2 days ago: both new cards started (the suspended one too): 2 of 2.
	if d := byDay[dayText(2)]; d.NewServed != 2 || d.NewDone != 2 {
		t.Fatalf("2 days ago (a started card suspended since): %+v", d)
	}
	// 3 days ago: the review answered when due, deleted since: due and done.
	if d := byDay[dayText(3)]; d.Due != 1 || d.Done != 1 || d.Served != 1 {
		t.Fatalf("3 days ago (an answered card deleted since): %+v", d)
	}
	// 4 days ago: the same without a tombstone that names it.
	if d := byDay[dayText(4)]; d.Due != 1 || d.Done != 1 {
		t.Fatalf("4 days ago (an answered card deleted, no tombstone): %+v", d)
	}
	// Yesterday: card 1 (started, not due) suspended, card 3 (not due) deleted: nothing
	// due; and nothing after for either.
	if d := byDay[dayText(1)]; d.Due != 0 || d.Done != 0 {
		t.Fatalf("yesterday: %+v", d)
	}
	if d := byDay[dayText(0)]; d.Due != 0 {
		t.Fatalf("today: %+v", d)
	}
}
