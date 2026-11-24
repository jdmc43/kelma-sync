package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
)

func parseReviewOperationPayload(operationID, presentationID, kind string,
	payload json.RawMessage) (reviewOperationIntent, string, error) {
	if !canonicalUUID.MatchString(operationID) || !canonicalUUID.MatchString(presentationID) {
		return reviewOperationIntent{}, "", errors.New("invalid operation")
	}
	intent := reviewOperationIntent{OperationID: operationID, PresentationID: presentationID, Kind: kind}
	var normalized string
	switch kind {
	case setCardFlagKind:
		var value cardFlagPayload
		if decodeExactJSON(bytes.NewReader(payload), &value) != nil ||
			value.Flag == nil || *value.Flag < 0 || *value.Flag > 7 {
			return reviewOperationIntent{}, "", errors.New("invalid card flag")
		}
		intent.Flag, normalized = *value.Flag, strconv.Itoa(*value.Flag)
	case setNoteMarkedKind:
		var value noteMarkedPayload
		if decodeExactJSON(bytes.NewReader(payload), &value) != nil || value.Marked == nil {
			return reviewOperationIntent{}, "", errors.New("invalid note mark")
		}
		intent.Marked, normalized = *value.Marked, strconv.FormatBool(*value.Marked)
	case setCardDueDateKind:
		var value cardDueDatePayload
		if decodeExactJSON(bytes.NewReader(payload), &value) != nil || value.DueAtMillis == nil ||
			*value.DueAtMillis <= 0 || *value.DueAtMillis > maxCardDueDateMillis || *value.DueAtMillis%86_400_000 != 0 {
			return reviewOperationIntent{}, "", errors.New("invalid card due date")
		}
		intent.DueAtMillis, normalized = *value.DueAtMillis, strconv.FormatInt(*value.DueAtMillis, 10)
	case editNoteKind:
		var value reviewNoteEditPayload
		if decodeExactJSON(bytes.NewReader(payload), &value) != nil {
			return reviewOperationIntent{}, "", errBadReviewNoteEdit
		}
		fields, tags, err := normalizeReviewNoteEdit(value)
		if err != nil {
			return reviewOperationIntent{}, "", err
		}
		intent.NoteFields, intent.NoteTags = fields, tags
		normalized = checksum(fields, tags)
	case suspendCardKind, suspendNoteKind, resetCardKind, createNoteCopyKind, deleteNoteKind:
		var value map[string]json.RawMessage
		if decodeExactJSON(bytes.NewReader(payload), &value) != nil || value == nil || len(value) != 0 {
			return reviewOperationIntent{}, "", errors.New("invalid empty payload operation")
		}
	case moveCardsKind, renameDeckKind:
		value, err := parseDeckOperationPayload(&intent, payload)
		if err != nil {
			return reviewOperationIntent{}, "", err
		}
		normalized = value
	default:
		return reviewOperationIntent{}, "", errors.New("unsupported operation")
	}
	return intent, normalized, nil
}
