package handler

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/jdmc43/kelma-sync/internal/inflight"
)

func manifestCardGUIDs(t *testing.T, mux *http.ServeMux, token, since string) (map[string]bool, string) {
	t.Helper()
	path := "/v2/sync/manifest"
	if since != "" {
		path += "?since=" + url.QueryEscape(since)
	}
	rr := reqJSON(t, mux, "GET", path, nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("manifest=%d %s", rr.Code, rr.Body.String())
	}
	body := decodeMap(t, rr.Body.Bytes())
	cards := map[string]bool{}
	for _, entry := range body["cards"].([]any) {
		cards[entry.(map[string]any)["note_guid"].(string)] = true
	}
	return cards, body["server_time"].(string)
}

// A write stamped before a manifest read but committed after it must still be
// listed by the next incremental manifest (the 116 "stuck" cards on 2026-10-21).
func TestManifestCursorNeverPassesRunningWrite(t *testing.T) {
	_, mux, pool := testServer(t)
	t.Cleanup(pool.Close)
	token := loginDemo(t, mux, "cursor-test")
	var userID string
	if err := pool.QueryRow(context.Background(), `SELECT id FROM users WHERE username='demo'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	_, cursor := manifestCardGUIDs(t, mux, token, "")

	// A writer starts (and takes its timestamp) ...
	done := inflight.Writes.Begin(userID)
	stamp := time.Now().UTC()
	time.Sleep(5 * time.Millisecond)
	// ... another device syncs while the write is still uncommitted ...
	seen, next := manifestCardGUIDs(t, mux, token, cursor)
	if seen["late-note"] {
		t.Fatal("uncommitted card listed")
	}
	nextTime, err := time.Parse(time.RFC3339Nano, next)
	if err != nil || nextTime.After(stamp) {
		t.Fatalf("cursor %s moved past the running write stamped %s", next, stamp.Format(time.RFC3339Nano))
	}
	// ... and the write commits with its earlier stamp.
	if _, err := pool.Exec(context.Background(), `INSERT INTO notes(user_id,guid,notetype_id,fields,tags,checksum,modified_at,client_modified_at)
		VALUES($1,'late-note',1,'["q","a"]','{}','c',$2,$2)`, userID, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO cards(user_id,card_id,note_guid,deck_name,ord,scheduling,modified_at,client_modified_at)
		VALUES($1,4242,'late-note','Deck',0,'{}',$2,$2)`, userID, stamp); err != nil {
		t.Fatal(err)
	}
	done()

	seen, _ = manifestCardGUIDs(t, mux, token, next)
	if !seen["late-note"] {
		t.Fatal("late-committed card was skipped by the next incremental manifest")
	}
}

func TestOnlyAccountWritesHoldBackTheCursor(t *testing.T) {
	cases := map[string]bool{
		"POST /v2/batch/push": true, "PUT /v2/cards/1": true, "DELETE /v2/decks/x": true,
		"POST /v2/sync/review-operations": true, "POST /v2/batch/pull": false,
		"GET /v2/sync/manifest": false, "POST /v2/media/download-tar": false, "HEAD /v2/media/a": false,
	}
	for route, want := range cases {
		var method, path string
		for i := range route {
			if route[i] == ' ' {
				method, path = route[:i], route[i+1:]
				break
			}
		}
		request, _ := http.NewRequest(method, path, nil)
		if got := writesAccountData(request); got != want {
			t.Fatalf("%s writes=%v want %v", route, got, want)
		}
	}
}
