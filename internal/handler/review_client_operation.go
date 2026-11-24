package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jdmc43/kelma-sync/internal/projection"
)

const reviewClientOperationsCapability = "review_client_operations_v1"

type reviewClientOperationRequest struct {
	OperationID      string          `json:"operation_id"`
	PresentationID   string          `json:"presentation_id"`
	Kind             string          `json:"kind"`
	SourceCardID     int64           `json:"source_card_id"`
	NoteGUID         string          `json:"note_guid"`
	CardOrd          int             `json:"card_ord"`
	BaseNoteChecksum string          `json:"base_note_checksum"`
	Payload          json.RawMessage `json:"payload"`
}

type reviewClientOperation struct {
	Intent           reviewOperationIntent
	SourceCardID     int64
	NoteGUID         string
	CardOrd          int
	BaseNoteChecksum string
}

type reviewClientTarget struct {
	SourceCardID     int64
	NoteGUID         string
	CardOrd          int
	Flag             int
	FieldsJSON       []byte
	Tags             []string
	MarkIntentExists bool
	NoteChecksum     string
	StudyState       string
	DeckName         string
}

func supportedReviewClientOperation(kind string) bool {
	switch kind {
	case setCardFlagKind, setNoteMarkedKind, suspendCardKind, suspendNoteKind,
		setCardDueDateKind, resetCardKind, createNoteCopyKind, editNoteKind, deleteNoteKind,
		moveCardsKind, renameDeckKind:
		return true
	default:
		return false
	}
}

func reviewClientNeedsNoteBase(kind string) bool {
	return kind == createNoteCopyKind || kind == editNoteKind || kind == deleteNoteKind
}

func reviewClientRefreshesQueue(kind string) bool {
	return kind == suspendCardKind || kind == suspendNoteKind || kind == setCardDueDateKind ||
		kind == resetCardKind || kind == deleteNoteKind || kind == moveCardsKind || kind == renameDeckKind
}

func parseReviewClientOperation(r *http.Request) (reviewClientOperation, error) {
	var request reviewClientOperationRequest
	if err := decodeExactJSON(r.Body, &request); err != nil ||
		request.SourceCardID == 0 || request.CardOrd < 0 || len(request.NoteGUID) == 0 ||
		len(request.NoteGUID) > 255 || strings.ContainsAny(request.NoteGUID, "\x00\r\n") ||
		len(request.BaseNoteChecksum) > 128 ||
		!supportedReviewClientOperation(request.Kind) ||
		(reviewClientNeedsNoteBase(request.Kind) && request.BaseNoteChecksum == "") {
		return reviewClientOperation{}, errors.New("invalid review client operation")
	}
	intent, payloadHash, err := parseReviewOperationPayload(
		request.OperationID, request.PresentationID, request.Kind, request.Payload)
	if err != nil {
		return reviewClientOperation{}, err
	}
	intent.RequestHash = requestDigest(
		"review-client-v1", intent.PresentationID, intent.Kind,
		strconv.FormatInt(request.SourceCardID, 10), request.NoteGUID,
		strconv.Itoa(request.CardOrd), request.BaseNoteChecksum, payloadHash,
	)
	return reviewClientOperation{
		Intent: intent, SourceCardID: request.SourceCardID, NoteGUID: request.NoteGUID,
		CardOrd: request.CardOrd, BaseNoteChecksum: request.BaseNoteChecksum,
	}, nil
}

func loadReviewClientTarget(r *http.Request, tx pgx.Tx, userID string,
	operation reviewClientOperation) (reviewClientTarget, error) {
	value := reviewClientTarget{
		SourceCardID: operation.SourceCardID,
		NoteGUID:     operation.NoteGUID,
		CardOrd:      operation.CardOrd,
	}
	err := tx.QueryRow(r.Context(), `SELECT
		CASE WHEN jsonb_typeof(c.scheduling->'flags')='number' AND c.scheduling->>'flags' ~ '^[0-7]$'
		 THEN (c.scheduling->>'flags')::int ELSE 0 END,
		n.fields,n.tags,n.mark_intent_id IS NOT NULL,n.checksum,c.study_state,c.deck_name
		FROM cards c JOIN notes n ON n.user_id=c.user_id AND n.guid=c.note_guid
		WHERE c.user_id=$1 AND c.card_id=$2 AND c.note_guid=$3 AND c.ord=$4
		FOR UPDATE OF c,n`, userID, operation.SourceCardID, operation.NoteGUID, operation.CardOrd).
		Scan(&value.Flag, &value.FieldsJSON, &value.Tags, &value.MarkIntentExists,
			&value.NoteChecksum, &value.StudyState, &value.DeckName)
	return value, err
}

func (h *Handler) PostReviewClientOperation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxReviewOperationBytes)
	operation, err := parseReviewClientOperation(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_review_client_operation", "invalid review client operation")
		return
	}
	now, claims := h.currentTime(), claimsFrom(r)
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "review_client_operation_unavailable", "could not start review client operation")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck
	if err := projection.LockAccountTx(r.Context(), tx, claims.UserID); err != nil {
		writeError(w, http.StatusServiceUnavailable, "review_client_operation_unavailable", "could not lock review account")
		return
	}
	var storedHash string
	var storedResponse []byte
	err = tx.QueryRow(r.Context(), `SELECT request_hash,response FROM review_client_operations
		WHERE user_id=$1 AND operation_id=$2::uuid`, claims.UserID, operation.Intent.OperationID).
		Scan(&storedHash, &storedResponse)
	if err == nil {
		if storedHash != operation.Intent.RequestHash {
			writeError(w, http.StatusConflict, "review_client_operation_conflict", "operation ID already has another intent")
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "review_client_operation_unavailable", "could not confirm review client receipt")
			return
		}
		writeStoredOperationResponse(w, storedResponse)
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusServiceUnavailable, "review_client_operation_unavailable", "could not inspect review client receipt")
		return
	}

	target, err := loadReviewClientTarget(r, tx, claims.UserID, operation)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "review_client_operation_missing", "review target not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "review_client_operation_unavailable", "could not load review target")
		return
	}
	if reviewClientNeedsNoteBase(operation.Intent.Kind) && target.NoteChecksum != operation.BaseNoteChecksum {
		writeError(w, http.StatusConflict, "review_client_operation_stale", "review note changed")
		return
	}
	if operation.Intent.Kind == suspendCardKind && target.StudyState != "active" {
		writeError(w, http.StatusConflict, "review_client_operation_stale", "review card is no longer active")
		return
	}

	state, err := applyReviewClientOperation(
		r.Context(), tx, claims.UserID, claims.ClientID, operation.Intent, target, now,
	)
	if errors.Is(err, errBadReviewNoteEdit) {
		writeError(w, http.StatusBadRequest, "bad_review_client_operation", "invalid review note edit")
		return
	}
	if errors.Is(err, errReviewOperationStaleTarget) {
		writeError(w, http.StatusConflict, "review_client_operation_stale", "review cards or deck changed")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "review_client_operation_unavailable", "could not apply review client operation")
		return
	}
	refresh := reviewClientRefreshesQueue(operation.Intent.Kind)
	current := "current"
	if refresh {
		current = "expired"
	}
	response := reviewOperationResponse{
		OperationID: operation.Intent.OperationID, PresentationID: operation.Intent.PresentationID,
		Status: "accepted", Kind: operation.Intent.Kind, Accepted: true, State: state,
		Effects: reviewOperationEffects{CurrentPresentation: current, QueueRefreshRequired: refresh},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "review_client_operation_unavailable", "could not encode review client receipt")
		return
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO review_client_operations
		(user_id,operation_id,presentation_id,source_card_id,note_guid,card_ord,kind,request_hash,response,created_at)
		VALUES($1,$2::uuid,$3::uuid,$4,$5,$6,$7,$8,$9,$10)`, claims.UserID,
		operation.Intent.OperationID, operation.Intent.PresentationID, operation.SourceCardID,
		operation.NoteGUID, operation.CardOrd, operation.Intent.Kind, operation.Intent.RequestHash, encoded, now); err != nil {
		writeError(w, http.StatusServiceUnavailable, "review_client_operation_unavailable", "could not store review client receipt")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "review_client_operation_unavailable", "could not commit review client operation")
		return
	}
	writeStoredOperationResponse(w, encoded)
}

func (h *Handler) GetReviewClientOperation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	operationID := r.PathValue("operation_id")
	if !canonicalUUID.MatchString(operationID) {
		writeError(w, http.StatusBadRequest, "bad_review_client_operation", "invalid review client operation")
		return
	}
	var response []byte
	if err := h.DB.QueryRow(r.Context(), `SELECT response FROM review_client_operations
		WHERE user_id=$1 AND operation_id=$2::uuid`, claimsFrom(r).UserID, operationID).Scan(&response); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "review_client_operation_missing", "review client operation not found")
		} else {
			writeError(w, http.StatusServiceUnavailable, "review_client_operation_unavailable", "could not load review client receipt")
		}
		return
	}
	writeStoredOperationResponse(w, response)
}
