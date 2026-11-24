package handler

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "modernc.org/sqlite"
)

func TestBrowserReviewAPKGImportIsAccountScopedAdditiveAndIdempotent(t *testing.T) {
	h, mux, pool := testServer(t)
	defer pool.Close()
	h.BrowserReviewRoutes(mux)
	token := loginDemo(t, mux, "web-import-test")
	data := syntheticReviewAPKG(t)
	userID := demoUserID(t, pool)
	originalMediaKey := storageKey(userID, "picture.png")
	if err := h.Storage.Put(t.Context(), originalMediaKey, bytes.NewReader([]byte("account-owned image")), 19, "image/png"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO media(user_id,filename,size_bytes,storage_key,modified_at)
		VALUES($1,'picture.png',19,$2,$3)`, userID, originalMediaKey, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	renamedMedia := importCollisionFilename("picture.png", []byte("synthetic image bytes"), 1)

	response := requestReviewImport(t, mux, token, "synthetic.apkg", data)
	if response.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", response.Code, response.Body.String())
	}
	result := decodeMap(t, response.Body.Bytes())
	for key, expected := range map[string]float64{
		"added_notes": 1, "added_cards": 1, "added_reviews": 1, "added_media": 1, "renamed_media": 1,
	} {
		if result[key] != expected {
			t.Fatalf("%s=%v", key, result[key])
		}
	}
	var fields []string
	var scheduling map[string]any
	if err := pool.QueryRow(t.Context(), `SELECT n.fields,c.scheduling FROM notes n JOIN cards c
		ON c.user_id=n.user_id AND c.note_guid=n.guid WHERE n.guid='synthetic-import-guid'`).Scan(&fields, &scheduling); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || fields[0] != `<img src="`+renamedMedia+`">Question` || len(scheduling) != 0 {
		t.Fatalf("fields=%q scheduling=%v", fields, scheduling)
	}
	var reviewCount, projectionCount int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM reviews WHERE note_guid='synthetic-import-guid'`).Scan(&reviewCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM scheduler_projections WHERE note_guid='synthetic-import-guid'`).Scan(&projectionCount); err != nil {
		t.Fatal(err)
	}
	if reviewCount != 1 || projectionCount != 1 {
		t.Fatalf("reviews=%d projections=%d", reviewCount, projectionCount)
	}
	var mediaKey string
	if err := pool.QueryRow(t.Context(), `SELECT storage_key FROM media WHERE user_id=$1 AND filename=$2`,
		userID, renamedMedia).Scan(&mediaKey); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Storage.Stat(t.Context(), mediaKey); err != nil {
		t.Fatal(err)
	}
	originalBody, _, err := h.Storage.Get(t.Context(), originalMediaKey)
	if err != nil {
		t.Fatal(err)
	}
	originalBytes, err := io.ReadAll(originalBody)
	_ = originalBody.Close()
	if err != nil || string(originalBytes) != "account-owned image" {
		t.Fatalf("original media changed: %q err=%v", originalBytes, err)
	}

	repeated := requestReviewImport(t, mux, token, "synthetic.apkg", data)
	if repeated.Code != http.StatusCreated {
		t.Fatalf("repeat: %d %s", repeated.Code, repeated.Body.String())
	}
	repeatResult := decodeMap(t, repeated.Body.Bytes())
	if repeatResult["added_notes"] != float64(0) || repeatResult["added_cards"] != float64(0) ||
		repeatResult["added_reviews"] != float64(0) || repeatResult["reused_notes"] != float64(1) {
		t.Fatalf("repeat result=%v", repeatResult)
	}

	if _, err := pool.Exec(t.Context(), `UPDATE notes SET fields='["Account version","Protected"]',checksum='account-authoritative'
		WHERE guid='synthetic-import-guid'`); err != nil {
		t.Fatal(err)
	}
	conflicted := requestReviewImport(t, mux, token, "synthetic.apkg", data)
	if conflicted.Code != http.StatusCreated {
		t.Fatalf("conflict import: %d %s", conflicted.Code, conflicted.Body.String())
	}
	conflictResult := decodeMap(t, conflicted.Body.Bytes())
	if conflictResult["copied_conflicts"] != float64(1) || conflictResult["added_notes"] != float64(1) ||
		conflictResult["added_cards"] != float64(1) || conflictResult["added_reviews"] != float64(0) ||
		conflictResult["skipped_review_conflicts"] != float64(1) {
		t.Fatalf("conflict result=%v", conflictResult)
	}
	var originalFields []string
	var noteCount int
	var reviewGUID string
	if err := pool.QueryRow(t.Context(), `SELECT fields FROM notes WHERE guid='synthetic-import-guid'`).Scan(&originalFields); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM notes WHERE guid LIKE 'synthetic-import-guid-import-%'`).Scan(&noteCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT note_guid FROM reviews LIMIT 1`).Scan(&reviewGUID); err != nil {
		t.Fatal(err)
	}
	if !equalStringSlices(originalFields, []string{"Account version", "Protected"}) || noteCount != 1 || reviewGUID != "synthetic-import-guid" {
		t.Fatalf("authority changed: fields=%v copies=%d review_guid=%q", originalFields, noteCount, reviewGUID)
	}
}

func TestBrowserReviewAPKGImportRejectsUnsafeRequests(t *testing.T) {
	h, mux, pool := testServer(t)
	defer pool.Close()
	h.BrowserReviewRoutes(mux)
	token := loginDemo(t, mux, "web-import-test")
	data := syntheticReviewAPKG(t)

	for _, request := range []*http.Request{
		httpImportRequest(t, token, "synthetic.colpkg", data, "application/octet-stream"),
		httpImportRequest(t, token, "synthetic.apkg", data, "application/json"),
		httpImportRequest(t, "", "synthetic.apkg", data, "application/octet-stream"),
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code < 400 {
			t.Fatalf("unsafe request accepted: %d", response.Code)
		}
	}

}

func requestReviewImport(t *testing.T, mux *http.ServeMux, token, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httpImportRequest(t, token, filename, data, "application/octet-stream")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	return response
}

func httpImportRequest(t *testing.T, token, filename string, data []byte, contentType string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v2/review/imports/"+filename, bytes.NewReader(data))
	request.Header.Set("Content-Type", contentType)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request
}

func demoUserID(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var userID string
	if err := pool.QueryRow(t.Context(), `SELECT id FROM users WHERE username='demo'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	return userID
}

func syntheticReviewAPKG(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	filename := filepath.Join(dir, "collection.db")
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE col (id integer primary key,crt integer,mod integer,scm integer,ver integer,dty integer,usn integer,ls integer,conf text,models text,decks text,dconf text,tags text)`,
		`CREATE TABLE notes (id integer primary key,guid text,mid integer,mod integer,usn integer,tags text,flds text,sfld integer,csum integer,flags integer,data text)`,
		`CREATE TABLE cards (id integer primary key,nid integer,did integer,ord integer,mod integer,usn integer,type integer,queue integer,due integer,ivl integer,factor integer,reps integer,lapses integer,left integer,odue integer,odid integer,flags integer,data text)`,
		`CREATE TABLE revlog (id integer primary key,cid integer,usn integer,ease integer,ivl integer,lastIvl integer,factor integer,time integer,type integer)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	models := `{"100":{"id":100,"name":"Synthetic Basic","css":".card{}","flds":[{"name":"Front","ord":0},{"name":"Back","ord":1}],"tmpls":[{"name":"Card 1","ord":0,"qfmt":"{{Front}}","afmt":"{{FrontSide}}{{Back}}"}]}}`
	decks := `{"200":{"id":200,"name":"Synthetic::Child"}}`
	if _, err := db.Exec(`INSERT INTO col VALUES(1,0,0,0,11,0,0,0,'{}',?,?,'{}','{}')`, models, decks); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO notes VALUES(1,'synthetic-import-guid',100,0,0,' synthetic ',?,0,0,0,'')`, `<img src="picture.png">Question`+"\x1fAnswer"); err != nil {
		t.Fatal(err)
	}
	cardID := time.Now().Add(-2 * time.Hour).UnixMilli()
	if _, err := db.Exec(`INSERT INTO cards VALUES(?,1,200,0,0,0,2,2,999999,30,2500,1,0,0,0,0,0,'')`, cardID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO revlog VALUES(?,?,0,3,30,10,2500,1200,1)`, cardID+1, cardID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	manifest, _ := json.Marshal(map[string]string{"0": "picture.png"})
	for name, value := range map[string][]byte{"collection.anki21": database, "media": manifest, "0": []byte("synthetic image bytes")} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
