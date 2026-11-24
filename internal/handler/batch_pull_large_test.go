package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Clients pull whole collections in one request; the old 12,000-identifier
// cap rejected ordinary large collections ("batch pull is limited to 12000
// identifiers"). Loads are set-based now, so large pulls are allowed and fast.
func TestBatchPullLargeCollection(t *testing.T) {
	_, mux, _ := testServer(t)
	token := loginDemo(t, mux, "large-test")
	now := time.Now().UTC()
	const noteCount = 7000 // 7000 notes + 7000 cards = 14,000 identifiers (> old cap)

	notetypes := []map[string]any{{"notetype_id": 1, "name": "Basic", "definition": map[string]any{"fields": []string{"Front", "Back"}}, "client_modified_at": now}}
	decks := []map[string]any{{"name": "Large", "config": map[string]any{}, "client_modified_at": now}}
	for start := 0; start < noteCount; start += 2000 {
		notes := []map[string]any{}
		cards := []map[string]any{}
		for i := start; i < min(start+2000, noteCount); i++ {
			guid := fmt.Sprintf("guid-%05d", i)
			notes = append(notes, map[string]any{"guid": guid, "notetype_id": 1, "fields": []string{fmt.Sprintf("front %d", i), "back"}, "tags": []string{}, "client_modified_at": now})
			cards = append(cards, map[string]any{"card_id": 1000000 + i, "note_guid": guid, "deck_name": "Large", "ord": 0, "scheduling": map[string]any{}, "client_modified_at": now})
		}
		body := map[string]any{"notes": notes, "cards": cards}
		if start == 0 {
			body["notetypes"] = notetypes
			body["decks"] = decks
		}
		rr := reqJSON(t, mux, http.MethodPost, "/v2/batch/push", body, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("push %d: %d %s", start, rr.Code, rr.Body.String())
		}
	}

	guids := make([]string, noteCount)
	ids := make([]int64, noteCount)
	keys := make([]map[string]any, 0, 3)
	for i := range guids {
		guids[i] = fmt.Sprintf("guid-%05d", i)
		ids[i] = int64(1000000 + i)
	}
	for i := 0; i < 3; i++ {
		keys = append(keys, map[string]any{"note_guid": guids[i], "ord": 0})
	}
	rr := reqJSON(t, mux, http.MethodPost, "/v2/batch/pull", map[string]any{
		"notes": guids, "cards": ids, "card_keys": keys, "notetypes": []int64{1, 999}, "decks": []string{"Large", "missing"},
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("large pull: %d %s", rr.Code, rr.Body.String())
	}
	var resp batchPullResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Notes) != noteCount || len(resp.Cards) != noteCount+3 || len(resp.Notetypes) != 1 || len(resp.Decks) != 1 {
		t.Fatalf("pulled notes=%d cards=%d notetypes=%d decks=%d", len(resp.Notes), len(resp.Cards), len(resp.Notetypes), len(resp.Decks))
	}
	seen := map[string]bool{}
	for _, n := range resp.Notes {
		seen[n.GUID] = true
	}
	if len(seen) != noteCount || resp.Notes[0].Fields[0] == "" {
		t.Fatalf("notes were not returned exactly once with content: %d distinct", len(seen))
	}

	// The safety ceiling still rejects absurd requests.
	huge := make([]string, batchPullLimit+1)
	rr = reqJSON(t, mux, http.MethodPost, "/v2/batch/pull", map[string]any{"notes": huge}, token)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected ceiling rejection, got %d", rr.Code)
	}
}
