package handler

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jdmc43/kelma-sync/internal/ankiimport"
	"github.com/jdmc43/kelma-sync/internal/auth"
	"github.com/jdmc43/kelma-sync/internal/storage"
)

func (h *Handler) planReviewImportMedia(ctx context.Context, tx pgx.Tx, userID string, values []ankiimport.Media) (map[string]string, []importMediaWrite, int, error) {
	names := map[string]string{}
	planned := map[string][]byte{}
	writes := make([]importMediaWrite, 0)
	renamed := 0
	for _, source := range values {
		resolved := ""
		for attempt := 0; attempt <= 100; attempt++ {
			candidate := source.Filename
			if attempt > 0 {
				candidate = importCollisionFilename(source.Filename, source.Bytes, attempt)
			}
			if prior, exists := planned[candidate]; exists {
				if bytes.Equal(prior, source.Bytes) {
					resolved = candidate
					break
				}
				continue
			}
			var key *string
			var size int64
			err := tx.QueryRow(ctx, `SELECT storage_key,size_bytes FROM media WHERE user_id=$1 AND filename=$2`, userID, candidate).Scan(&key, &size)
			if errors.Is(err, pgx.ErrNoRows) {
				resolved = candidate
				planned[candidate] = source.Bytes
				contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(candidate)))
				if contentType == "" {
					contentType = "application/octet-stream"
				}
				writes = append(writes, importMediaWrite{name: candidate, key: importMediaStorageKey(userID, candidate, source.Bytes), contentType: contentType, data: source.Bytes})
				break
			}
			if err != nil {
				return nil, nil, 0, err
			}
			same, err := h.importMediaMatches(ctx, key, size, source.Bytes)
			if err != nil {
				return nil, nil, 0, err
			}
			if same {
				resolved = candidate
				break
			}
		}
		if resolved == "" {
			return nil, nil, 0, errors.New("media collision limit exceeded")
		}
		if resolved != source.Filename {
			renamed++
		}
		names[source.SourceName] = resolved
		if source.SourceName == source.Filename || names[source.Filename] == "" {
			names[source.Filename] = resolved
		}
	}
	return names, writes, renamed, nil
}

func (h *Handler) importMediaMatches(ctx context.Context, key *string, size int64, expected []byte) (bool, error) {
	if key == nil || size != int64(len(expected)) {
		return false, nil
	}
	body, _, err := h.Storage.Get(ctx, *key)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer body.Close()
	actual, err := io.ReadAll(io.LimitReader(body, int64(len(expected))+1))
	if err != nil {
		return false, err
	}
	return bytes.Equal(actual, expected), nil
}

func writeReviewImportMedia(ctx context.Context, tx pgx.Tx, claims *auth.Claims, files []importMediaWrite, now time.Time) error {
	for _, file := range files {
		tag, err := tx.Exec(ctx, `INSERT INTO media(user_id,filename,size_bytes,storage_key,modified_at,last_client_id)
			VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(user_id,filename) DO NOTHING`,
			claims.UserID, file.name, len(file.data), file.key, now, claims.ClientID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("media changed concurrently during import")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM tombstones WHERE user_id=$1 AND type='media' AND resource_id=$2`, claims.UserID, file.name); err != nil {
			return err
		}
	}
	return nil
}

func loadImportNote(ctx context.Context, tx pgx.Tx, userID, guid string) (int64, []string, []string, bool, error) {
	var id int64
	var fields, tags []string
	err := tx.QueryRow(ctx, `SELECT notetype_id,fields,tags FROM notes WHERE user_id=$1 AND guid=$2`, userID, guid).Scan(&id, &fields, &tags)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, nil, false, nil
	}
	return id, fields, tags, err == nil, err
}

func conflictImportGUID(ctx context.Context, tx pgx.Tx, userID, source string, notetypeID int64, fields, tags []string) (string, bool, error) {
	encoded, _ := json.Marshal([]any{notetypeID, fields, tags})
	digest := sha256.Sum256(encoded)
	base := truncateUTF8(source, 220) + "-import-" + hex.EncodeToString(digest[:5])
	for suffix := 1; suffix <= 100; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate = fmt.Sprintf("%s-%d", base, suffix)
		}
		id, existingFields, existingTags, found, err := loadImportNote(ctx, tx, userID, candidate)
		if err != nil {
			return "", false, err
		}
		if !found {
			return candidate, false, nil
		}
		if id == notetypeID && equalStringSlices(existingFields, fields) && equalTagSets(existingTags, tags) {
			return candidate, true, nil
		}
	}
	return "", false, errors.New("note conflict copy limit exceeded")
}

func availableImportCardID(ctx context.Context, tx pgx.Tx, userID, guid string, ord int) (int64, error) {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", guid, ord)))
	candidate := -int64(binary.BigEndian.Uint64(digest[:8]) & 0x7fff_ffff_ffff_ffff)
	if candidate == 0 {
		candidate = -1
	}
	for attempt := 0; attempt <= 100; attempt++ {
		var foundGUID string
		var foundOrd int
		err := tx.QueryRow(ctx, `SELECT note_guid,ord FROM cards WHERE user_id=$1 AND card_id=$2`, userID, candidate).Scan(&foundGUID, &foundOrd)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && foundGUID == guid && foundOrd == ord) {
			return candidate, nil
		}
		if err != nil {
			return 0, err
		}
		candidate--
		if candidate == 0 {
			candidate = -1
		}
	}
	return 0, errors.New("card ID collision limit exceeded")
}

func allocateImportNotetypeID(name string, definition map[string]any, existing map[int64]importExistingNotetype) (int64, error) {
	encoded, _ := json.Marshal([]any{name, definition})
	digest := sha256.Sum256(encoded)
	candidate := -int64(binary.BigEndian.Uint64(digest[:8]) & 0x7fff_ffff_ffff_ffff)
	if candidate == 0 {
		candidate = -1
	}
	for attempt := 0; attempt <= 100; attempt++ {
		if _, used := existing[candidate]; !used {
			return candidate, nil
		}
		candidate--
		if candidate == 0 {
			candidate = -1
		}
	}
	return 0, errors.New("note type ID collision limit exceeded")
}

func importMediaStorageKey(userID, name string, data []byte) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(name))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(data)
	return userID + "/imports/" + hex.EncodeToString(digest.Sum(nil))
}

func importCollisionFilename(name string, data []byte, suffix int) string {
	digest := sha1.Sum(data)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	marker := "-import-" + hex.EncodeToString(digest[:5])
	if suffix > 1 {
		marker += fmt.Sprintf("-%d", suffix)
	}
	if len([]byte(ext))+len(marker) >= 250 {
		ext = ""
	}
	stem = truncateUTF8(stem, 255-len([]byte(ext))-len(marker))
	if stem == "" {
		stem = "media"
	}
	return stem + marker + ext
}

func rewriteImportMap(value map[string]any, names map[string]string) map[string]any {
	return rewriteImportValue(value, names).(map[string]any)
}

func rewriteImportValue(value any, names map[string]string) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = rewriteImportValue(item, names)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = rewriteImportValue(item, names)
		}
		return out
	case string:
		return rewriteImportText(typed, names)
	default:
		return value
	}
}

func rewriteImportText(value string, names map[string]string) string {
	keys := make([]string, 0, len(names))
	for key := range names {
		if key != names[key] {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, old := range keys {
		value = replaceBoundedMediaName(value, old, names[old])
	}
	return value
}

func replaceBoundedMediaName(value, old, replacement string) string {
	var out strings.Builder
	cursor := 0
	for {
		index := strings.Index(value[cursor:], old)
		if index < 0 {
			out.WriteString(value[cursor:])
			break
		}
		index += cursor
		end := index + len(old)
		before := index == 0 || !isMediaFilenameByte(value[index-1])
		after := end == len(value) || !isMediaFilenameByte(value[end])
		if before && after {
			out.WriteString(value[cursor:index])
			out.WriteString(replacement)
			cursor = end
		} else {
			out.WriteString(value[cursor:end])
			cursor = end
		}
	}
	return out.String()
}

func isMediaFilenameByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || strings.ContainsRune("_.-", rune(value))
}
func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
func equalTagSets(left, right []string) bool {
	left = append([]string{}, left...)
	right = append([]string{}, right...)
	sort.Strings(left)
	sort.Strings(right)
	return equalStringSlices(left, right)
}
func truncateUTF8(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	if len([]byte(value)) <= maximum {
		return value
	}
	for len(value) > maximum || !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
