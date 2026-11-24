package handler

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

const reviewClientPresentationID = "33333333-3333-4333-8333-333333333333"

func reviewClientRequest(operationID, kind string, payload map[string]any) map[string]any {
	return map[string]any{
		"operation_id": operationID, "presentation_id": reviewClientPresentationID,
		"kind": kind, "source_card_id": int64(9001), "note_guid": "review-note",
		"card_ord": 0, "base_note_checksum": "note-checksum", "payload": payload,
	}
}

func TestReviewClientOperationsAreCanonicalAndExactlyOnce(t *testing.T) {
	due := int64(1_795_046_400_000)
	cases := []struct {
		name, operationID, kind string
		payload                 map[string]any
		stateKey                string
		expected                any
		queueRefresh            bool
	}{
		{"flag", "10000000-0000-4000-8000-000000000001", setCardFlagKind,
			map[string]any{"flag": 3}, "flag", float64(3), false},
		{"mark", "10000000-0000-4000-8000-000000000002", setNoteMarkedKind,
			map[string]any{"marked": true}, "note_marked", true, false},
		{"suspend card", "10000000-0000-4000-8000-000000000003", suspendCardKind,
			map[string]any{}, "study_state", "suspended", true},
		{"suspend note", "10000000-0000-4000-8000-000000000004", suspendNoteKind,
			map[string]any{}, "study_state", "suspended", true},
		{"set due", "10000000-0000-4000-8000-000000000005", setCardDueDateKind,
			map[string]any{"due_at_millis": due}, "due_at_millis", float64(due), true},
		{"reset", "10000000-0000-4000-8000-000000000006", resetCardKind,
			map[string]any{}, "schedule_reset_through_review_id", nil, true},
		{"copy", "10000000-0000-4000-8000-000000000007", createNoteCopyKind,
			map[string]any{}, "note_copy_created", true, false},
		{"edit", "10000000-0000-4000-8000-000000000008", editNoteKind,
			map[string]any{"fields": []string{"changed", "answer"}, "tags": []string{"edited"}},
			"note_edited", true, false},
		{"delete", "10000000-0000-4000-8000-000000000009", deleteNoteKind,
			map[string]any{}, "note_deleted", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setupBrowserReview(t)
			request := reviewClientRequest(tc.operationID, tc.kind, tc.payload)
			first := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", request, f.token)
			if first.Code != http.StatusOK {
				t.Fatalf("operation=%d %s", first.Code, first.Body.String())
			}
			body := decodeMap(t, first.Body.Bytes())
			state := body["state"].(map[string]any)
			effects := body["effects"].(map[string]any)
			if body["accepted"] != true || body["status"] != "accepted" || body["kind"] != tc.kind ||
				body["presentation_id"] != reviewClientPresentationID ||
				effects["queue_refresh_required"] != tc.queueRefresh {
				t.Fatalf("response=%v", body)
			}
			if tc.expected == nil {
				if value, ok := state[tc.stateKey].(string); !ok || value == "" {
					t.Fatalf("state=%v", state)
				}
			} else if state[tc.stateKey] != tc.expected {
				t.Fatalf("state=%v", state)
			}
			var receipts int
			if err := f.handler.DB.QueryRow(context.Background(), `SELECT count(*) FROM review_client_operations
				WHERE user_id=$1 AND operation_id=$2::uuid`, f.userID, tc.operationID).Scan(&receipts); err != nil || receipts != 1 {
				t.Fatalf("receipts=%d err=%v", receipts, err)
			}
			retry := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", request, f.token)
			if retry.Code != http.StatusOK || !reflect.DeepEqual(decodeMap(t, retry.Body.Bytes()), body) {
				t.Fatalf("retry=%d %s", retry.Code, retry.Body.String())
			}
			receipt := reqJSON(t, f.mux, "GET", "/v2/sync/review-operations/"+tc.operationID, nil, f.token)
			if receipt.Code != http.StatusOK || !reflect.DeepEqual(decodeMap(t, receipt.Body.Bytes()), body) {
				t.Fatalf("receipt=%d %s", receipt.Code, receipt.Body.String())
			}
		})
	}
}

func TestReviewClientOperationsRejectStaleForeignAndChangedTargets(t *testing.T) {
	f := setupBrowserReview(t)
	operationID := "20000000-0000-4000-8000-000000000001"
	stale := reviewClientRequest(operationID, editNoteKind,
		map[string]any{"fields": []string{"changed", "answer"}, "tags": []string{}})
	stale["base_note_checksum"] = "stale-checksum"
	if rr := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", stale, f.token); rr.Code != http.StatusConflict {
		t.Fatalf("stale=%d %s", rr.Code, rr.Body.String())
	}
	missing := reviewClientRequest(operationID, setCardFlagKind, map[string]any{"flag": 2})
	missing["source_card_id"] = int64(9999)
	if rr := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", missing, f.token); rr.Code != http.StatusNotFound {
		t.Fatalf("missing=%d %s", rr.Code, rr.Body.String())
	}
	unsupported := reviewClientRequest(operationID, "bury_card", map[string]any{})
	if rr := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", unsupported, f.token); rr.Code != http.StatusBadRequest {
		t.Fatalf("unsupported=%d %s", rr.Code, rr.Body.String())
	}
	if rr := reqJSON(t, f.mux, "GET", "/v2/sync/review-operations/"+operationID, nil, f.token); rr.Code != http.StatusNotFound {
		t.Fatalf("missing receipt=%d", rr.Code)
	}
	if rr := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations",
		reviewClientRequest(operationID, setCardFlagKind, map[string]any{"flag": 2}), ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous=%d", rr.Code)
	}
}

func TestReviewClientOperationsSupportSignedLegacyCardIDs(t *testing.T) {
	f := setupBrowserReview(t)
	const cardID int64 = -8_599_812_345_678_901_234
	ctx := context.Background()
	if _, err := f.handler.DB.Exec(ctx, `INSERT INTO notes
		(user_id,guid,notetype_id,fields,tags,checksum,modified_at,client_modified_at)
		VALUES($1,'legacy-note',1,'["legacy"]','{}','legacy-checksum',now(),now())`, f.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.handler.DB.Exec(ctx, `INSERT INTO cards
		(user_id,card_id,note_guid,deck_name,ord,scheduling,modified_at,client_modified_at,last_client_id)
		VALUES($1,$2,'legacy-note','Review Deck',0,'{}',now(),now(),$3::uuid)`,
		f.userID, cardID, f.clientID); err != nil {
		t.Fatal(err)
	}
	request := reviewClientRequest(
		"20000000-0000-4000-8000-000000000002", setCardFlagKind, map[string]any{"flag": 5},
	)
	request["source_card_id"] = cardID
	request["note_guid"] = "legacy-note"
	response := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", request, f.token)
	if response.Code != http.StatusOK {
		t.Fatalf("operation=%d %s", response.Code, response.Body.String())
	}
	var flag string
	if err := f.handler.DB.QueryRow(ctx, `SELECT scheduling->>'flags' FROM cards
		WHERE user_id=$1 AND card_id=$2`, f.userID, cardID).Scan(&flag); err != nil || flag != "5" {
		t.Fatalf("flag=%q err=%v", flag, err)
	}
}

func TestNormalizeReviewClientNoteEditBoundsAndCanonicalizesTags(t *testing.T) {
	fields, tags, err := normalizeReviewNoteEdit(reviewNoteEditPayload{
		Fields: []string{" question ", ""},
		Tags:   []string{"topic", "topic", "MARKED"},
	})
	if err != nil || !reflect.DeepEqual(fields, []string{" question ", ""}) ||
		!reflect.DeepEqual(tags, []string{"topic", "marked"}) {
		t.Fatalf("fields=%v tags=%v err=%v", fields, tags, err)
	}
	invalid := []reviewNoteEditPayload{
		{},
		{Fields: []string{"  "}},
		{Fields: []string{"ok", string(make([]byte, maxReviewEditFieldBytes+1))}},
		{Fields: []string{"ok"}, Tags: []string{"two words"}},
	}
	for index, payload := range invalid {
		if _, _, err := normalizeReviewNoteEdit(payload); !errors.Is(err, errBadReviewNoteEdit) {
			t.Fatalf("invalid case %d: %v", index, err)
		}
	}
}

func TestReviewClientOperationIDCannotChangeIntent(t *testing.T) {
	f := setupBrowserReview(t)
	operationID := "30000000-0000-4000-8000-000000000001"
	first := reviewClientRequest(operationID, setCardFlagKind, map[string]any{"flag": 2})
	if rr := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", first, f.token); rr.Code != http.StatusOK {
		t.Fatalf("first=%d %s", rr.Code, rr.Body.String())
	}
	changed := reviewClientRequest(operationID, setCardFlagKind, map[string]any{"flag": 4})
	if rr := reqJSON(t, f.mux, "POST", "/v2/sync/review-operations", changed, f.token); rr.Code != http.StatusConflict {
		t.Fatalf("changed=%d %s", rr.Code, rr.Body.String())
	}
}
