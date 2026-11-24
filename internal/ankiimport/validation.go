package ankiimport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

func validateReferences(collection Collection) error {
	notetypes := map[int64]bool{}
	for _, value := range collection.Notetypes {
		notetypes[value.SourceID] = true
	}
	notes := map[int64]bool{}
	for _, value := range collection.Notes {
		if !notetypes[value.NotetypeID] {
			return errors.New("Anki note references a missing note type")
		}
		notes[value.SourceID] = true
	}
	for _, value := range collection.Cards {
		if !notes[value.NoteSourceID] {
			return errors.New("Anki card references a missing note")
		}
	}
	return nil
}

func validateFields(fields []string) error {
	if len(fields) == 0 || len(fields) > 128 {
		return errors.New("Anki note has an invalid field count")
	}
	total := 0
	for _, field := range fields {
		if !utf8.ValidString(field) || len([]byte(field)) > 64<<10 {
			return errors.New("Anki note field is invalid")
		}
		total += len([]byte(field))
	}
	if total > maxNoteBytes {
		return errors.New("Anki note fields exceed the allowed size")
	}
	return nil
}

func validateNotetype(name string, definition map[string]any) error {
	if strings.TrimSpace(name) == "" || len([]byte(name)) > 255 || !utf8.ValidString(name) ||
		strings.IndexFunc(name, func(r rune) bool { return r < 32 }) >= 0 {
		return errors.New("Anki note type name is invalid")
	}
	fields, fieldsOK := definition["flds"].([]any)
	templates, templatesOK := definition["tmpls"].([]any)
	if !fieldsOK || !templatesOK || len(fields) == 0 || len(fields) > 128 || len(templates) == 0 || len(templates) > 128 ||
		!validDefinitionRows(fields, false) || !validDefinitionRows(templates, true) {
		return errors.New("Anki note type definition is invalid")
	}
	encoded, err := json.Marshal(definition)
	if err != nil || len(encoded) > maxTemplateBytes {
		return errors.New("Anki note type definition is invalid")
	}
	return nil
}

func validDefinitionRows(values []any, template bool) bool {
	ordinals := map[int64]bool{}
	for _, raw := range values {
		value, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		name, nameOK := value["name"].(string)
		ordinal, ordinalOK := jsonInt64(value["ord"])
		if !nameOK || strings.TrimSpace(name) == "" || len([]byte(name)) > 255 || !utf8.ValidString(name) ||
			strings.IndexFunc(name, func(r rune) bool { return r < 32 }) >= 0 || !ordinalOK || ordinal < 0 || ordinal > 127 || ordinals[ordinal] {
			return false
		}
		ordinals[ordinal] = true
		if template {
			question, questionOK := value["qfmt"].(string)
			answer, answerOK := value["afmt"].(string)
			if !questionOK || !answerOK || !utf8.ValidString(question) || !utf8.ValidString(answer) {
				return false
			}
		}
	}
	return true
}

func normalizeDeckName(value string) (string, error) {
	value = strings.ReplaceAll(value, "\x1f", "::")
	levels := strings.Split(strings.TrimSpace(value), "::")
	for index := range levels {
		levels[index] = strings.TrimSpace(levels[index])
		if levels[index] == "" {
			return "", errors.New("Anki deck name is invalid")
		}
	}
	name := strings.Join(levels, "::")
	if len([]byte(name)) > 256 || !utf8.ValidString(name) || strings.IndexFunc(name, func(r rune) bool { return r < 32 }) >= 0 {
		return "", errors.New("Anki deck name is invalid")
	}
	return name, nil
}

func declaredDecks(names map[int64]string, cards []Card) []string {
	set := map[string]bool{}
	for _, name := range names {
		for _, parent := range deckHierarchy(name) {
			set[parent] = true
		}
	}
	for _, card := range cards {
		for _, parent := range deckHierarchy(card.DeckName) {
			set[parent] = true
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func deckHierarchy(name string) []string {
	parts := strings.Split(name, "::")
	out := make([]string, len(parts))
	for index := range parts {
		out[index] = strings.Join(parts[:index+1], "::")
	}
	return out
}

func normalizedTags(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && len([]byte(value)) <= 255 && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func decodeJSONObject(value string) (map[string]any, error) {
	if !utf8.ValidString(value) {
		return nil, errors.New("JSON metadata is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(value))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("JSON metadata has trailing content")
	}
	return out, nil
}

func sortedKeys(values map[string]any) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func jsonInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int64:
		return number, true
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil
	case float64:
		return int64(number), number == float64(int64(number))
	case string:
		parsed, err := strconv.ParseInt(number, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
