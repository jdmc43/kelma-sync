package ankiimport

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

func TestReadLegacyAPKG(t *testing.T) {
	database := syntheticDatabase(t, false)
	media := []byte("synthetic audio bytes")
	manifest, _ := json.Marshal(map[string]string{"0": "folder/voice.mp3"})
	collection, err := Read(syntheticPackage(t, map[string][]byte{
		"collection.anki21": database,
		"media":             manifest,
		"0":                 media,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(collection.Notetypes) != 1 || collection.Notetypes[0].Name != "Synthetic Basic" {
		t.Fatalf("notetypes=%+v", collection.Notetypes)
	}
	if len(collection.Notes) != 1 || collection.Notes[0].GUID != "synthetic-guid" ||
		len(collection.Notes[0].Fields) != 2 || collection.Notes[0].Fields[1] != "Answer" {
		t.Fatalf("notes=%+v", collection.Notes)
	}
	if len(collection.Cards) != 1 || collection.Cards[0].DeckName != "Synthetic::Child" {
		t.Fatalf("cards=%+v", collection.Cards)
	}
	if len(collection.Reviews) != 1 || collection.Reviews[0].Ease != 3 {
		t.Fatalf("reviews=%+v", collection.Reviews)
	}
	if len(collection.Media) != 1 || collection.Media[0].Filename != "voice.mp3" ||
		!bytes.Equal(collection.Media[0].Bytes, media) {
		t.Fatalf("media=%+v", collection.Media)
	}
}

func TestReadCurrentZstdNormalizedAPKG(t *testing.T) {
	database := syntheticDatabase(t, true)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	media := []byte("synthetic image bytes")
	hash := sha1Bytes(media)
	manifestEntry := protoMessage(1,
		protoString(1, "picture.jpg"), protoVarint(2, uint64(len(media))), protoBytes(3, hash))
	collection, err := Read(syntheticPackage(t, map[string][]byte{
		"collection.anki21b": encoder.EncodeAll(database, nil),
		"media":              encoder.EncodeAll(manifestEntry, nil),
		"0":                  encoder.EncodeAll(media, nil),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(collection.Notetypes) != 1 || collection.Notetypes[0].Definition["css"] != ".card{color:green}" {
		t.Fatalf("normalized notetype=%+v", collection.Notetypes)
	}
	templates := collection.Notetypes[0].Definition["tmpls"].([]any)
	if templates[0].(map[string]any)["qfmt"] != "{{Front}}" {
		t.Fatalf("templates=%+v", templates)
	}
	if len(collection.Media) != 1 || collection.Media[0].Filename != "picture.jpg" {
		t.Fatalf("media=%+v", collection.Media)
	}
}

func TestReadKeepsDistinctMediaThatShareABasename(t *testing.T) {
	manifest, _ := json.Marshal(map[string]string{"0": "first/picture.jpg", "1": "second/picture.jpg"})
	collection, err := Read(syntheticPackage(t, map[string][]byte{
		"collection.anki21": syntheticDatabase(t, false), "media": manifest,
		"0": []byte("first synthetic image"), "1": []byte("second synthetic image"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(collection.Media) != 2 || collection.Media[0].SourceName == collection.Media[1].SourceName {
		t.Fatalf("media=%+v", collection.Media)
	}
}

func TestReadRejectsUnsafeAndInvalidPackages(t *testing.T) {
	for name, entries := range map[string]map[string][]byte{
		"path traversal":   {"../collection.anki21": []byte("SQLite format 3\x00")},
		"missing database": {"media": []byte("{}")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Read(syntheticPackage(t, entries)); err == nil {
				t.Fatal("invalid package accepted")
			}
		})
	}
	if _, err := Read(bytes.Repeat([]byte{'x'}, 100)); err == nil {
		t.Fatal("non-ZIP package accepted")
	}
}

func syntheticDatabase(t *testing.T, normalized bool) []byte {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "synthetic.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	models := `{"100":{"id":100,"name":"Synthetic Basic","css":".card{}","flds":[{"name":"Front","ord":0},{"name":"Back","ord":1}],"tmpls":[{"name":"Card 1","ord":0,"qfmt":"{{Front}}","afmt":"{{FrontSide}}<hr id=answer>{{Back}}"}]}}`
	decks := `{"200":{"id":200,"name":"Synthetic::Child"}}`
	statements := []string{
		`CREATE TABLE col (id integer primary key,crt integer,mod integer,scm integer,ver integer,dty integer,usn integer,ls integer,conf text,models text,decks text,dconf text,tags text)`,
		`CREATE TABLE notes (id integer primary key,guid text,mid integer,mod integer,usn integer,tags text,flds text,sfld integer,csum integer,flags integer,data text)`,
		`CREATE TABLE cards (id integer primary key,nid integer,did integer,ord integer,mod integer,usn integer,type integer,queue integer,due integer,ivl integer,factor integer,reps integer,lapses integer,left integer,odue integer,odid integer,flags integer,data text)`,
		`CREATE TABLE revlog (id integer primary key,cid integer,usn integer,ease integer,ivl integer,lastIvl integer,factor integer,time integer,type integer)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if normalized {
		models, decks = `{}`, `{}`
		for _, statement := range []string{
			`CREATE TABLE notetypes (id integer primary key,name text,mtime_secs integer,usn integer,config blob)`,
			`CREATE TABLE fields (ntid integer,ord integer,name text,config blob)`,
			`CREATE TABLE templates (ntid integer,ord integer,name text,mtime_secs integer,usn integer,config blob)`,
			`CREATE TABLE decks (id integer primary key,name text,mtime_secs integer,usn integer,common blob,kind blob)`,
		} {
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`INSERT INTO notetypes VALUES(100,'Synthetic Basic',0,0,?)`, protoString(3, ".card{color:green}")); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO fields VALUES(100,0,'Front',X''),(100,1,'Back',X'')`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO templates VALUES(100,0,'Card 1',0,0,?)`, append(protoString(1, "{{Front}}"), protoString(2, "{{FrontSide}}{{Back}}")...)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO decks VALUES(200,?,0,0,X'',X'')`, "Synthetic\x1fChild"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO col VALUES(1,0,0,0,11,0,0,0,'{}',?,?,'{}','{}')`, models, decks); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO notes VALUES(1000,'synthetic-guid',100,0,0,' synthetic ',?,0,0,0,'')`, "Question\x1fAnswer"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cards VALUES(1700000000000,1000,200,0,0,0,2,2,0,3,2500,1,0,0,0,0,0,'')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO revlog VALUES(1700000001000,1700000000000,0,3,3,1,2500,1200,1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func syntheticPackage(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	for _, name := range names {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(entries[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func protoVarint(number int, value uint64) []byte {
	out := binary.AppendUvarint(nil, uint64(number<<3))
	return binary.AppendUvarint(out, value)
}
func protoBytes(number int, value []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(number<<3|2))
	out = binary.AppendUvarint(out, uint64(len(value)))
	return append(out, value...)
}
func protoString(number int, value string) []byte { return protoBytes(number, []byte(value)) }
func protoMessage(number int, values ...[]byte) []byte {
	return protoBytes(number, bytes.Join(values, nil))
}

func sha1Bytes(value []byte) []byte {
	digest := sha1.Sum(value)
	return digest[:]
}
