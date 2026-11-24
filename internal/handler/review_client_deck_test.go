package handler

import (
	"context"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"
)

// seedDeckCards adds notes "deck-note-N" with one card each (card IDs 9100+N) in
// the given decks, all sharing the fixture's notetype.
func seedDeckCards(t *testing.T, f browserReviewFixture, decks ...string) []int64 {
	t.Helper()
	ctx := context.Background()
	ids := make([]int64, 0, len(decks))
	for i, deck := range decks {
		guid := "deck-note-" + strconv.Itoa(i)
		id := int64(9100 + i)
		if _, err := f.handler.DB.Exec(ctx, `INSERT INTO notes(user_id,guid,notetype_id,fields,tags,checksum,modified_at,client_modified_at)
			VALUES($1,$2,1,'["q","a"]','{}','c',now(),now())`, f.userID, guid); err != nil {
			t.Fatal(err)
		}
		if _, err := f.handler.DB.Exec(ctx, `INSERT INTO cards(user_id,card_id,note_guid,deck_name,ord,scheduling,study_state,modified_at,client_modified_at)
			VALUES($1,$2,$3,$4,0,'{"type":2,"queue":2,"ivl":9,"flags":4}','suspended',now(),now())`,
			f.userID, id, guid, deck); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func cardDecks(t *testing.T, f browserReviewFixture) map[int64]string {
	t.Helper()
	rows, err := f.handler.DB.Query(context.Background(), `SELECT card_id,deck_name FROM cards WHERE user_id=$1`, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	decks := map[int64]string{}
	for rows.Next() {
		var id int64
		var deck string
		if err := rows.Scan(&id, &deck); err != nil {
			t.Fatal(err)
		}
		decks[id] = deck
	}
	return decks
}

func deckNames(t *testing.T, f browserReviewFixture, table string) []string {
	t.Helper()
	query := `SELECT name FROM decks WHERE user_id=$1 ORDER BY name`
	if table == "tombstones" {
		query = `SELECT resource_id FROM tombstones WHERE user_id=$1 AND type='deck' ORDER BY resource_id`
	}
	rows, err := f.handler.DB.Query(context.Background(), query, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestMoveCardsCreatesDeckAndKeepsSchedulingExactlyOnce(t *testing.T) {
	f := setupBrowserReview(t)
	ctx := context.Background()
	if _, err := f.handler.DB.Exec(ctx, `UPDATE decks SET config='{"new":{"perDay":7}}' WHERE user_id=$1`, f.userID); err != nil {
		t.Fatal(err)
	}
	ids := seedDeckCards(t, f, "Review Deck", "Review Deck", "Other")
	before := time.Now().UTC()
	var schedulingBefore string
	if err := f.handler.DB.QueryRow(ctx, `SELECT scheduling::text FROM cards WHERE card_id=$1`, ids[0]).Scan(&schedulingBefore); err != nil {
		t.Fatal(err)
	}
	operationID := "40000000-0000-4000-8000-000000000001"
	request := reviewClientRequest(operationID, moveCardsKind, map[string]any{
		"deck_name": "Newer Words",
		"card_ids":  []string{strconv.FormatInt(ids[1], 10), strconv.FormatInt(ids[0], 10)},
	})
	first := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", request, f.token)
	if first.Code != http.StatusOK {
		t.Fatalf("move=%d %s", first.Code, first.Body.String())
	}
	body := decodeMap(t, first.Body.Bytes())
	state := body["state"].(map[string]any)
	if state["deck_name"] != "Newer Words" || state["moved_cards"] != float64(2) ||
		body["effects"].(map[string]any)["queue_refresh_required"] != true {
		t.Fatalf("response=%v", body)
	}
	decks := cardDecks(t, f)
	if decks[ids[0]] != "Newer Words" || decks[ids[1]] != "Newer Words" || decks[ids[2]] != "Other" ||
		decks[9001] != "Review Deck::Child" {
		t.Fatalf("decks=%v", decks)
	}
	// The new deck inherits the options of the deck the cards came from.
	var config string
	if err := f.handler.DB.QueryRow(ctx, `SELECT config::text FROM decks WHERE user_id=$1 AND name='Newer Words'`,
		f.userID).Scan(&config); err != nil || config != `{"new": {"perDay": 7}}` {
		t.Fatalf("config=%q err=%v", config, err)
	}
	// Only the deck changes; scheduling, flags and study state are untouched, and
	// both clocks advance so other devices pull the move.
	var schedulingAfter, studyState string
	var modified, clientModified time.Time
	if err := f.handler.DB.QueryRow(ctx, `SELECT scheduling::text,study_state,modified_at,client_modified_at
		FROM cards WHERE card_id=$1`, ids[0]).Scan(&schedulingAfter, &studyState, &modified, &clientModified); err != nil {
		t.Fatal(err)
	}
	if schedulingAfter != schedulingBefore || studyState != "suspended" ||
		!modified.After(before.Add(-time.Second)) || !clientModified.After(before.Add(-time.Second)) {
		t.Fatalf("scheduling=%s state=%s modified=%v client=%v", schedulingAfter, studyState, modified, clientModified)
	}
	retry := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", request, f.token)
	if retry.Code != http.StatusOK || !reflect.DeepEqual(decodeMap(t, retry.Body.Bytes()), body) {
		t.Fatalf("retry=%d %s", retry.Code, retry.Body.String())
	}
	reordered := reviewClientRequest(operationID, moveCardsKind, map[string]any{
		"deck_name": "Newer Words",
		"card_ids":  []string{strconv.FormatInt(ids[0], 10), strconv.FormatInt(ids[1], 10)},
	})
	if rr := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", reordered, f.token); rr.Code != http.StatusOK {
		t.Fatalf("reordered retry=%d %s", rr.Code, rr.Body.String())
	}
	changed := reviewClientRequest(operationID, moveCardsKind, map[string]any{
		"deck_name": "Elsewhere", "card_ids": []string{strconv.FormatInt(ids[0], 10)},
	})
	if rr := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", changed, f.token); rr.Code != http.StatusConflict {
		t.Fatalf("changed intent=%d %s", rr.Code, rr.Body.String())
	}
}

func TestMoveCardsRejectsUnknownCardsAndInvalidPayloads(t *testing.T) {
	f := setupBrowserReview(t)
	ids := seedDeckCards(t, f, "Review Deck")
	missing := reviewClientRequest("40000000-0000-4000-8000-000000000002", moveCardsKind, map[string]any{
		"deck_name": "Newer", "card_ids": []string{strconv.FormatInt(ids[0], 10), "123456"},
	})
	if rr := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", missing, f.token); rr.Code != http.StatusConflict {
		t.Fatalf("missing=%d %s", rr.Code, rr.Body.String())
	}
	if decks := cardDecks(t, f); decks[ids[0]] != "Review Deck" {
		t.Fatalf("partial move: %v", decks)
	}
	if names := deckNames(t, f, "decks"); !reflect.DeepEqual(names, []string{"Review Deck"}) {
		t.Fatalf("deck created by failed move: %v", names)
	}
	tooMany := make([]string, maxMovedCards+1)
	for i := range tooMany {
		tooMany[i] = strconv.Itoa(i + 1)
	}
	for name, payload := range map[string]map[string]any{
		"empty list":     {"deck_name": "New", "card_ids": []string{}},
		"duplicate":      {"deck_name": "New", "card_ids": []string{"9100", "9100"}},
		"padded id":      {"deck_name": "New", "card_ids": []string{"09100"}},
		"zero id":        {"deck_name": "New", "card_ids": []string{"0"}},
		"empty part":     {"deck_name": "New::", "card_ids": []string{"9100"}},
		"blank part":     {"deck_name": "A:: ::B", "card_ids": []string{"9100"}},
		"padded name":    {"deck_name": " New", "card_ids": []string{"9100"}},
		"control char":   {"deck_name": "New\x1f", "card_ids": []string{"9100"}},
		"missing deck":   {"card_ids": []string{"9100"}},
		"too many cards": {"deck_name": "New", "card_ids": tooMany},
		"unknown field":  {"deck_name": "New", "card_ids": []string{"9100"}, "note_guids": []string{}},
	} {
		request := reviewClientRequest("40000000-0000-4000-8000-000000000003", moveCardsKind, payload)
		if rr := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", request, f.token); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s=%d %s", name, rr.Code, rr.Body.String())
		}
	}
}

func TestRenameDeckMovesSubdecksCardsAndOptions(t *testing.T) {
	f := setupBrowserReview(t)
	ctx := context.Background()
	if _, err := f.handler.DB.Exec(ctx, `INSERT INTO decks(user_id,name,config,checksum,modified_at,client_modified_at)
		VALUES($1,'Review Deck::Child','{"rev":{"perDay":50}}','child',now(),now()),
		      ($1,'Review Decks','{}','x',now(),now()),($1,'Review_Deck','{}','y',now(),now())`, f.userID); err != nil {
		t.Fatal(err)
	}
	// Siblings whose names only share a prefix must not move, including LIKE wildcards.
	ids := seedDeckCards(t, f, "Review Deck", "Review Decks", "Review_Deck", "ReviewXDeck::Child")
	request := reviewClientRequest("40000000-0000-4000-8000-000000000004", renameDeckKind, map[string]any{
		"from": "Review Deck", "to": "Archive::Meine Wörter",
	})
	rr := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", request, f.token)
	if rr.Code != http.StatusOK {
		t.Fatalf("rename=%d %s", rr.Code, rr.Body.String())
	}
	state := decodeMap(t, rr.Body.Bytes())["state"].(map[string]any)
	if state["deck_name"] != "Archive::Meine Wörter" || state["moved_cards"] != float64(2) {
		t.Fatalf("state=%v", state)
	}
	decks := cardDecks(t, f)
	want := map[int64]string{
		9001: "Archive::Meine Wörter::Child", ids[0]: "Archive::Meine Wörter",
		ids[1]: "Review Decks", ids[2]: "Review_Deck", ids[3]: "ReviewXDeck::Child",
	}
	if !reflect.DeepEqual(decks, want) {
		t.Fatalf("decks=%v", decks)
	}
	if names := deckNames(t, f, "decks"); !reflect.DeepEqual(names, []string{
		"Archive::Meine Wörter", "Archive::Meine Wörter::Child", "Review Decks", "Review_Deck",
	}) {
		t.Fatalf("deck records=%v", names)
	}
	if names := deckNames(t, f, "tombstones"); !reflect.DeepEqual(names, []string{"Review Deck", "Review Deck::Child"}) {
		t.Fatalf("tombstones=%v", names)
	}
	var config string
	if err := f.handler.DB.QueryRow(ctx, `SELECT config::text FROM decks WHERE user_id=$1 AND name='Archive::Meine Wörter::Child'`,
		f.userID).Scan(&config); err != nil || config != `{"rev": {"perDay": 50}}` {
		t.Fatalf("child config=%q err=%v", config, err)
	}
	// Renaming back clears the tombstones of the names that are live again.
	back := reviewClientRequest("40000000-0000-4000-8000-000000000005", renameDeckKind, map[string]any{
		"from": "Archive::Meine Wörter", "to": "Review Deck",
	})
	if rr := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", back, f.token); rr.Code != http.StatusOK {
		t.Fatalf("rename back=%d %s", rr.Code, rr.Body.String())
	}
	if names := deckNames(t, f, "tombstones"); !reflect.DeepEqual(names, []string{
		"Archive::Meine Wörter", "Archive::Meine Wörter::Child",
	}) {
		t.Fatalf("tombstones after rename back=%v", names)
	}
}

func TestRenameDeckRejectsStaleAndInvalidRenames(t *testing.T) {
	f := setupBrowserReview(t)
	// The anchor card (9001) is in Review Deck::Child, not in "Other".
	stale := reviewClientRequest("40000000-0000-4000-8000-000000000006", renameDeckKind, map[string]any{
		"from": "Other", "to": "Renamed",
	})
	if rr := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", stale, f.token); rr.Code != http.StatusConflict {
		t.Fatalf("stale=%d %s", rr.Code, rr.Body.String())
	}
	for name, payload := range map[string]map[string]any{
		"same":       {"from": "Review Deck", "to": "Review Deck"},
		"into child": {"from": "Review Deck", "to": "Review Deck::Inner"},
		"empty":      {"from": "Review Deck", "to": ""},
		"missing":    {"from": "Review Deck"},
	} {
		request := reviewClientRequest("40000000-0000-4000-8000-000000000007", renameDeckKind, payload)
		if rr := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", request, f.token); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s=%d %s", name, rr.Code, rr.Body.String())
		}
	}
	if decks := cardDecks(t, f); decks[9001] != "Review Deck::Child" {
		t.Fatalf("decks=%v", decks)
	}
}
