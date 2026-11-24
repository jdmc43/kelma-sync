package handler

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jdmc43/kelma-sync/internal/model"
	"github.com/jdmc43/kelma-sync/internal/projection"
)

var (
	errNoteMarkIntentConflict = errors.New("note mark intent conflict")
	noteMarkUUID              = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

type noteMarkRequest struct {
	Marked           *bool     `json:"marked"`
	IntentID         string    `json:"intent_id"`
	ClientModifiedAt time.Time `json:"client_modified_at"`
}

type batchNoteMark struct {
	GUID string `json:"guid"`
	noteMarkRequest
}

type noteMarkResult struct {
	GUID     string         `json:"guid"`
	Accepted bool           `json:"accepted"`
	Applied  bool           `json:"applied"`
	Mark     model.NoteMark `json:"mark"`
}

func validateNoteMark(request noteMarkRequest, now time.Time) error {
	if request.Marked == nil || !noteMarkUUID.MatchString(request.IntentID) ||
		request.ClientModifiedAt.IsZero() || request.ClientModifiedAt.After(now.Add(maxFutureClockSkew)) {
		return errors.New("invalid note mark intent")
	}
	return nil
}

func normalizedMarkTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Microsecond)
}

func hasMarkedTag(tags []string) bool {
	for _, tag := range tags {
		if strings.EqualFold(strings.TrimSpace(tag), "marked") {
			return true
		}
	}
	return false
}

func markedTags(tags []string, marked bool) []string {
	result := make([]string, 0, len(tags)+1)
	for _, tag := range tags {
		if !strings.EqualFold(strings.TrimSpace(tag), "marked") {
			result = append(result, tag)
		}
	}
	if marked {
		result = append(result, "marked")
	}
	return result
}

func markIntentWins(incoming noteMarkRequest, current model.NoteMark) bool {
	incomingTime := normalizedMarkTime(incoming.ClientModifiedAt)
	currentTime := normalizedMarkTime(current.ClientModifiedAt)
	return incomingTime.After(currentTime) ||
		(incomingTime.Equal(currentTime) && strings.Compare(incoming.IntentID, current.IntentID) > 0)
}

func applyNoteMarkTx(ctx context.Context, tx pgx.Tx, userID, clientID, guid string,
	request noteMarkRequest, now time.Time,
) (noteMarkResult, []string, bool, error) {
	now = normalizedMarkTime(now)
	if err := validateNoteMark(request, now); err != nil {
		return noteMarkResult{}, nil, false, err
	}
	request.ClientModifiedAt = normalizedMarkTime(request.ClientModifiedAt)
	note, err := loadNoteTx(ctx, tx, userID, guid)
	if err != nil {
		return noteMarkResult{}, nil, false, err
	}
	if current := note.Mark; current != nil {
		if current.IntentID == request.IntentID {
			if current.Marked != *request.Marked ||
				!normalizedMarkTime(current.ClientModifiedAt).Equal(request.ClientModifiedAt) {
				return noteMarkResult{GUID: guid, Mark: *current}, note.Tags, false, errNoteMarkIntentConflict
			}
			return noteMarkResult{GUID: guid, Accepted: true, Mark: *current}, note.Tags, false, nil
		}
		if !markIntentWins(request, *current) {
			return noteMarkResult{GUID: guid, Accepted: true, Mark: *current}, note.Tags, false, nil
		}
	}
	tags := markedTags(note.Tags, *request.Marked)
	tagsChanged := hasMarkedTag(note.Tags) != *request.Marked
	mark := model.NoteMark{Marked: *request.Marked, IntentID: request.IntentID,
		ModifiedAt: now, ClientModifiedAt: request.ClientModifiedAt, LastClientID: clientID}
	tag, err := tx.Exec(ctx, `UPDATE notes SET tags=$3,checksum=$4,
		mark_intent_id=$6::uuid,mark_modified_at=$5,mark_client_modified_at=$7,
		mark_client_id=$8::uuid WHERE user_id=$1 AND guid=$2`,
		userID, guid, tags, checksum(note.Fields, tags), now, request.IntentID,
		request.ClientModifiedAt, clientID)
	if err != nil || tag.RowsAffected() != 1 {
		return noteMarkResult{}, nil, false, errors.New("note mark update failed")
	}
	return noteMarkResult{GUID: guid, Accepted: true, Applied: true, Mark: mark}, tags, tagsChanged, nil
}

func (h *Handler) applyBatchNoteMarks(w http.ResponseWriter, r *http.Request, marks []batchNoteMark,
	now time.Time, response *batchPushResponse,
) bool {
	claims, ctx := claimsFrom(r), r.Context()
	for _, mark := range marks {
		tx, err := h.DB.Begin(ctx)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "note_mark_unavailable", "could not start note mark")
			return false
		}
		if err := projection.LockAccountTx(ctx, tx, claims.UserID); err != nil {
			_ = tx.Rollback(ctx)
			writeError(w, http.StatusServiceUnavailable, "note_mark_unavailable", "could not lock note mark account")
			return false
		}
		result, _, _, err := applyNoteMarkTx(ctx, tx, claims.UserID, claims.ClientID, mark.GUID,
			mark.noteMarkRequest, now)
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errNoteMarkIntentConflict) {
			_ = tx.Rollback(ctx)
			var server any
			if errors.Is(err, errNoteMarkIntentConflict) {
				server = result.Mark
			}
			response.Conflicts["note_marks"] = append(response.Conflicts["note_marks"],
				conflictEntry{GUID: mark.GUID, Server: server, Client: mark})
			continue
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			writeError(w, http.StatusServiceUnavailable, "note_mark_unavailable", "could not apply note mark")
			return false
		}
		if err := tx.Commit(ctx); err != nil {
			writeError(w, http.StatusServiceUnavailable, "note_mark_unavailable", "could not commit note mark")
			return false
		}
		response.Accepted["note_marks"]++
		response.MarkResults = append(response.MarkResults, result)
	}
	return true
}

func (h *Handler) PutNoteMark(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	var request noteMarkRequest
	if err := decode(r, &request); err != nil || validateNoteMark(request, time.Now().UTC()) != nil {
		writeError(w, http.StatusBadRequest, "bad_note_mark", "invalid note mark intent")
		return
	}
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "note_mark_unavailable", "could not start note mark")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if err := projection.LockAccountTx(r.Context(), tx, claims.UserID); err != nil {
		writeError(w, http.StatusServiceUnavailable, "note_mark_unavailable", "could not lock note mark account")
		return
	}
	result, _, _, err := applyNoteMarkTx(r.Context(), tx, claims.UserID, claims.ClientID,
		r.PathValue("guid"), request, time.Now().UTC())
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "note not found")
		return
	}
	if errors.Is(err, errNoteMarkIntentConflict) {
		writeJSON(w, http.StatusConflict, model.ConflictResponse{Error: "mark_intent_conflict", Server: result.Mark, Client: request})
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "note_mark_unavailable", "could not apply note mark")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "note_mark_unavailable", "could not commit note mark")
		return
	}
	writeJSON(w, http.StatusOK, result)
}
