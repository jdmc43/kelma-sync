package handler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	maxReviewEditFields     = 128
	maxReviewEditFieldBytes = 64 << 10
	maxReviewEditTotalBytes = 256 << 10
	maxReviewEditTags       = 256
	maxReviewEditTagBytes   = 255
)

var errBadReviewNoteEdit = errors.New("invalid review note edit")

type reviewNoteEditPayload struct {
	Fields []string `json:"fields"`
	Tags   []string `json:"tags"`
}

func normalizeReviewNoteEdit(payload reviewNoteEditPayload) ([]string, []string, error) {
	if len(payload.Fields) == 0 || len(payload.Fields) > maxReviewEditFields ||
		strings.TrimSpace(payload.Fields[0]) == "" || len(payload.Tags) > maxReviewEditTags {
		return nil, nil, errBadReviewNoteEdit
	}
	fields := append([]string(nil), payload.Fields...)
	totalBytes := 0
	for _, field := range fields {
		if strings.IndexByte(field, 0) >= 0 || len(field) > maxReviewEditFieldBytes {
			return nil, nil, errBadReviewNoteEdit
		}
		totalBytes += len(field)
		if totalBytes > maxReviewEditTotalBytes {
			return nil, nil, errBadReviewNoteEdit
		}
	}

	tags := make([]string, 0, len(payload.Tags))
	seen := make(map[string]struct{}, len(payload.Tags))
	for _, raw := range payload.Tags {
		tag := strings.TrimSpace(raw)
		if tag == "" || len(tag) > maxReviewEditTagBytes || strings.IndexByte(tag, 0) >= 0 ||
			strings.ContainsAny(tag, ", \t\r\n") {
			return nil, nil, errBadReviewNoteEdit
		}
		if _, exists := seen[tag]; exists {
			continue
		}
		seen[tag] = struct{}{}
		tags = append(tags, tag)
	}
	tags = markedTags(tags, hasMarkedTag(tags))
	return fields, tags, nil
}

func applyReviewNoteEditTx(ctx context.Context, tx pgx.Tx, userID, clientID string,
	target reviewClientTarget, fields, tags []string, now time.Time,
) ([]string, bool, error) {
	var currentFields []string
	if json.Unmarshal(target.FieldsJSON, &currentFields) != nil || len(fields) != len(currentFields) {
		return nil, false, errBadReviewNoteEdit
	}
	if target.MarkIntentExists {
		tags = markedTags(tags, hasMarkedTag(target.Tags))
	}
	if len(tags) > maxReviewEditTags {
		return nil, false, errBadReviewNoteEdit
	}
	if equalStringSlices(fields, currentFields) && equalStringSlices(tags, target.Tags) {
		return tags, false, nil
	}
	result, err := tx.Exec(ctx, `UPDATE notes SET fields=$3,tags=$4,checksum=$5,modified_at=$6,
		client_modified_at=GREATEST($6,client_modified_at+interval '1 microsecond'),last_client_id=$7::uuid
		WHERE user_id=$1 AND guid=$2`, userID, target.NoteGUID, fields, tags,
		checksum(fields, tags), now.UTC(), clientID)
	if err != nil || result.RowsAffected() != 1 {
		return nil, false, errors.New("review note edit failed")
	}
	return tags, true, nil
}
