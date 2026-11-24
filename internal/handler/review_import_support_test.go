package handler

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReviewImportMediaNamesAreRewrittenOnlyAtFilenameBoundaries(t *testing.T) {
	mapping := map[string]string{
		"folder/picture.jpg": "picture-import-123.jpg",
		"voice.mp3":          "voice-import-456.mp3",
	}
	value := rewriteImportText(
		`<img src="folder/picture.jpg"> [sound:voice.mp3] prefixvoice.mp3suffix`, mapping,
	)
	if value != `<img src="picture-import-123.jpg"> [sound:voice-import-456.mp3] prefixvoice.mp3suffix` {
		t.Fatalf("rewritten=%q", value)
	}
}

func TestReviewImportCollisionNamesAndObjectKeysAreBoundedAndDeterministic(t *testing.T) {
	name := strings.Repeat("é", 130) + ".picture.jpg"
	data := []byte("synthetic media")
	first := importCollisionFilename(name, data, 1)
	if first != importCollisionFilename(name, data, 1) || len([]byte(first)) > 255 || !utf8.ValidString(first) {
		t.Fatalf("invalid collision filename %q", first)
	}
	key := importMediaStorageKey("account", first, data)
	if key != importMediaStorageKey("account", first, data) || !strings.HasPrefix(key, "account/imports/") || strings.Contains(key, name) {
		t.Fatalf("invalid import object key %q", key)
	}
}
