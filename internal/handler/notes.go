package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jdmc43/kelma-sync/internal/model"
	"github.com/jdmc43/kelma-sync/internal/projection"
)

const noteSelectSQL = `SELECT n.id,n.user_id,n.guid,n.notetype_id,n.fields,n.tags,n.checksum,
	n.modified_at,n.client_modified_at,COALESCE(n.last_client_id::text,''),COALESCE(content_client.label,''),
	COALESCE(n.mark_intent_id::text,''),n.mark_modified_at,n.mark_client_modified_at,
	COALESCE(n.mark_client_id::text,''),COALESCE(mark_client.label,'')
	FROM notes n
	LEFT JOIN clients content_client ON content_client.id=n.last_client_id
	LEFT JOIN clients mark_client ON mark_client.id=n.mark_client_id
	WHERE n.user_id=$1 AND n.guid=$2`

// noteSelectSQLByGUIDs is noteSelectSQL for a set of guids.
const noteSelectSQLByGUIDs = `SELECT n.id,n.user_id,n.guid,n.notetype_id,n.fields,n.tags,n.checksum,
	n.modified_at,n.client_modified_at,COALESCE(n.last_client_id::text,''),COALESCE(content_client.label,''),
	COALESCE(n.mark_intent_id::text,''),n.mark_modified_at,n.mark_client_modified_at,
	COALESCE(n.mark_client_id::text,''),COALESCE(mark_client.label,'')
	FROM notes n
	LEFT JOIN clients content_client ON content_client.id=n.last_client_id
	LEFT JOIN clients mark_client ON mark_client.id=n.mark_client_id
	WHERE n.user_id=$1 AND n.guid=ANY($2::text[])`

// GetNote returns the full note record for a guid.
func (h *Handler) GetNote(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	note, err := h.loadNote(r, claims.UserID, r.PathValue("guid"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "note not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, note)
}

type putNoteRequest struct {
	NotetypeID       int64     `json:"notetype_id"`
	Fields           []string  `json:"fields"`
	Tags             []string  `json:"tags"`
	ClientModifiedAt time.Time `json:"client_modified_at"`
	// BaseChecksum is the checksum of the version the client last synced. The
	// server accepts the write only if the stored note still has this checksum
	// (optimistic concurrency). Empty means the client believes the note is new.
	BaseChecksum string `json:"base_checksum"`
}

// PutNote creates or updates whole-note content. The independently versioned
// Mark/Unmark state is always preserved, including under Force-Override.
func (h *Handler) PutNote(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	var req putNoteRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if err := projection.LockAccountTx(r.Context(), tx, claims.UserID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	existing, err := loadNoteTx(r.Context(), tx, claims.UserID, r.PathValue("guid"))
	notFound := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !notFound {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	req = preserveNoteMark(req, existing, !notFound)
	newChecksum := checksum(req.Fields, req.Tags)
	if !notFound && !forceOverride(r) && !noteBaseMatches(existing, req.BaseChecksum) &&
		existing.Checksum != newChecksum {
		writeJSON(w, http.StatusConflict, model.ConflictResponse{Error: "conflict", Server: existing, Client: req})
		return
	}
	saved, err := upsertNoteTx(r.Context(), tx, claims.UserID, claims.ClientID,
		r.PathValue("guid"), req, newChecksum, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func preserveNoteMark(req putNoteRequest, existing model.Note, exists bool) putNoteRequest {
	if req.Tags == nil {
		req.Tags = []string{}
	}
	marked := hasMarkedTag(req.Tags)
	if exists && existing.Mark != nil {
		marked = existing.Mark.Marked
	}
	req.Tags = markedTags(req.Tags, marked)
	return req
}

// noteBaseMatches accepts a base that differs from the current server note only
// by the canonical marked tag. This lets a content edit and a typed mark intent
// converge without allowing unrelated tag or field conflicts through.
func noteBaseMatches(existing model.Note, base string) bool {
	if existing.Checksum == base {
		return true
	}
	if existing.Mark == nil {
		return false
	}
	alternate := markedTags(existing.Tags, !existing.Mark.Marked)
	return base != "" && checksum(existing.Fields, alternate) == base
}

// DeleteNote soft-deletes a note by writing a tombstone.
func (h *Handler) DeleteNote(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	guid := r.PathValue("guid")
	_, err := h.DB.Exec(r.Context(), `DELETE FROM notes WHERE user_id=$1 AND guid=$2`, claims.UserID, guid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := h.writeTombstone(r, claims.UserID, "note", guid, claims.ClientID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) loadNote(r *http.Request, userID, guid string) (model.Note, error) {
	return scanNote(h.DB.QueryRow(r.Context(), noteSelectSQL, userID, guid))
}

func loadNoteTx(ctx context.Context, tx pgx.Tx, userID, guid string) (model.Note, error) {
	return scanNote(tx.QueryRow(ctx, noteSelectSQL+" FOR UPDATE OF n", userID, guid))
}

func scanNote(row pgx.Row) (model.Note, error) {
	var note model.Note
	var intentID, markClientID, markClientLabel string
	var markModifiedAt, markClientModifiedAt *time.Time
	err := row.Scan(&note.ID, &note.UserID, &note.GUID, &note.NotetypeID, &note.Fields, &note.Tags,
		&note.Checksum, &note.ModifiedAt, &note.ClientModifiedAt, &note.LastClientID,
		&note.LastClientLabel, &intentID, &markModifiedAt, &markClientModifiedAt,
		&markClientID, &markClientLabel)
	if err != nil || intentID == "" {
		return note, err
	}
	note.Mark = &model.NoteMark{Marked: hasMarkedTag(note.Tags), IntentID: intentID,
		ModifiedAt: markModifiedAt.UTC(), ClientModifiedAt: markClientModifiedAt.UTC(),
		LastClientID: markClientID, LastClientLabel: markClientLabel}
	return note, nil
}
