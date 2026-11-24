package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jdmc43/kelma-sync/internal/projection"
)

const maxReviewOperationBytes = 512 << 10

const (
	setCardFlagKind    = "set_card_flag"
	setNoteMarkedKind  = "set_note_marked"
	suspendCardKind    = "suspend_card"
	suspendNoteKind    = "suspend_note"
	setCardDueDateKind = "set_card_due_date"
	resetCardKind      = "reset_card"
	createNoteCopyKind = "create_note_copy"
	editNoteKind       = "edit_note"
	deleteNoteKind     = "delete_note"
)

const maxCardDueDateMillis int64 = 253_402_214_400_000

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type cardFlagPayload struct {
	Flag *int `json:"flag"`
}

type noteMarkedPayload struct {
	Marked *bool `json:"marked"`
}

type cardDueDatePayload struct {
	DueAtMillis *int64 `json:"due_at_millis"`
}

type reviewOperationIntent struct {
	OperationID, PresentationID, Kind, RequestHash string
	Flag                                           int
	Marked                                         bool
	DueAtMillis                                    int64
	NoteFields, NoteTags                           []string
	CardIDs                                        []int64
	DeckName, DeckFrom, DeckTo                     string
}

type reviewOperationState struct {
	Flag                         int       `json:"flag"`
	NoteMarked                   bool      `json:"note_marked"`
	StudyState                   string    `json:"study_state,omitempty"`
	DueAtMillis                  *int64    `json:"due_at_millis,omitempty"`
	ScheduleResetThroughReviewID string    `json:"schedule_reset_through_review_id,omitempty"`
	NoteCopyCreated              bool      `json:"note_copy_created,omitempty"`
	NoteEdited                   bool      `json:"note_edited,omitempty"`
	NoteDeleted                  bool      `json:"note_deleted,omitempty"`
	NoteFields                   *[]string `json:"note_fields,omitempty"`
	NoteTags                     *[]string `json:"note_tags,omitempty"`
	DeckName                     string    `json:"deck_name,omitempty"`
	MovedCards                   int       `json:"moved_cards,omitempty"`
}

type reviewOperationEffects struct {
	CurrentPresentation  string `json:"current_presentation"`
	QueueRefreshRequired bool   `json:"queue_refresh_required"`
}

type reviewOperationResponse struct {
	OperationID    string                 `json:"operation_id"`
	PresentationID string                 `json:"presentation_id"`
	Status         string                 `json:"status"`
	Kind           string                 `json:"kind"`
	Accepted       bool                   `json:"accepted"`
	State          reviewOperationState   `json:"state"`
	Effects        reviewOperationEffects `json:"effects"`
}

func (h *Handler) currentTime() time.Time {
	if h.now == nil {
		return time.Now().UTC()
	}
	return h.now().UTC()
}

func decodeExactJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func requestDigest(values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func applyReviewClientOperation(ctx context.Context, tx pgx.Tx, userID, clientID string,
	intent reviewOperationIntent, target reviewClientTarget, now time.Time,
) (reviewOperationState, error) {
	state := reviewOperationState{
		Flag: target.Flag, NoteMarked: hasMarkedTag(target.Tags),
	}
	switch intent.Kind {
	case setCardFlagKind:
		if state.Flag == intent.Flag {
			return state, nil
		}
		result, err := applyCardFlagTx(ctx, tx, userID, clientID,
			target.NoteGUID, target.CardOrd, cardFlagRequest{
				Flag: &intent.Flag, IntentID: intent.OperationID, ClientModifiedAt: now,
			}, now, true)
		if err != nil {
			return state, errors.New("card flag update failed")
		}
		state.Flag = result.Flag.Flag
	case setNoteMarkedKind:
		if state.NoteMarked == intent.Marked {
			return state, nil
		}
		var fields []string
		if json.Unmarshal(target.FieldsJSON, &fields) != nil {
			return state, errors.New("invalid note fields")
		}
		tags := markedTags(target.Tags, intent.Marked)
		tag, err := tx.Exec(ctx, `UPDATE notes SET tags=$3,checksum=$4,
			mark_intent_id=$6::uuid,mark_modified_at=$5,
			mark_client_modified_at=GREATEST($5,COALESCE(mark_client_modified_at,$5)+interval '1 microsecond'),
			mark_client_id=$7::uuid WHERE user_id=$1 AND guid=$2`, userID, target.NoteGUID,
			tags, checksum(fields, tags), now, intent.OperationID, clientID)
		if err != nil || tag.RowsAffected() != 1 {
			return state, errors.New("note mark update failed")
		}
		state.NoteMarked = intent.Marked
	case setCardDueDateKind:
		tag, err := tx.Exec(ctx, `UPDATE cards SET due_date_override_ms=$3,
			due_date_override_modified_at=$4,
			due_date_override_client_modified_at=GREATEST($4,due_date_override_client_modified_at + interval '1 microsecond'),
			due_date_override_client_id=$5::uuid WHERE user_id=$1 AND card_id=$2`, userID,
			target.SourceCardID, intent.DueAtMillis, now, clientID)
		if err != nil || tag.RowsAffected() != 1 {
			return state, errors.New("card due date update failed")
		}
		state.DueAtMillis = &intent.DueAtMillis
	case resetCardKind:
		var resetThrough int64
		if err := tx.QueryRow(ctx, `SELECT GREATEST($2,
			COALESCE((SELECT max(review_id) FROM reviews WHERE user_id=$1),0),
			COALESCE((SELECT max(schedule_reset_through_review_id) FROM cards WHERE user_id=$1),0))`,
			userID, now.UnixMilli()).Scan(&resetThrough); err != nil {
			return state, errors.New("card reset cutoff failed")
		}
		tag, err := tx.Exec(ctx, `UPDATE cards SET schedule_reset_through_review_id=$3,
			schedule_reset_modified_at=$4,
			schedule_reset_client_modified_at=GREATEST($4,schedule_reset_client_modified_at + interval '1 microsecond'),
			schedule_reset_client_id=$5::uuid,due_date_override_ms=0,
			due_date_override_modified_at=$4,
			due_date_override_client_modified_at=GREATEST($4,due_date_override_client_modified_at + interval '1 microsecond'),
			due_date_override_client_id=$5::uuid WHERE user_id=$1 AND card_id=$2`, userID,
			target.SourceCardID, resetThrough, now, clientID)
		if err != nil || tag.RowsAffected() != 1 {
			return state, errors.New("card reset update failed")
		}
		if err := projection.EnqueueReviewsTx(ctx, tx, userID, []projection.LogicalCard{{
			NoteGUID: target.NoteGUID, CardOrd: target.CardOrd,
		}}); err != nil {
			return state, errors.New("card reset projection failed")
		}
		zero := int64(0)
		state.DueAtMillis = &zero
		state.ScheduleResetThroughReviewID = strconv.FormatInt(resetThrough, 10)
	case editNoteKind:
		tags, _, err := applyReviewNoteEditTx(
			ctx, tx, userID, clientID, target, intent.NoteFields, intent.NoteTags, now,
		)
		if err != nil {
			return state, err
		}
		state.NoteEdited = true
		fields := append([]string(nil), intent.NoteFields...)
		receiptTags := append([]string(nil), tags...)
		state.NoteFields, state.NoteTags = &fields, &receiptTags
		state.NoteMarked = hasMarkedTag(tags)
	case deleteNoteKind:
		if err := applyReviewNoteDeleteTx(ctx, tx, userID, clientID, target.NoteGUID, now); err != nil {
			return state, err
		}
		state.NoteDeleted = true
	case createNoteCopyKind:
		copyGUID := "web-" + intent.OperationID
		var firstCardID int64
		if err := tx.QueryRow(ctx, `SELECT GREATEST($2,
			COALESCE((SELECT max(card_id)+1 FROM cards WHERE user_id=$1),$2))`,
			userID, now.UnixMilli()).Scan(&firstCardID); err != nil {
			return state, errors.New("note copy identity failed")
		}
		var copiedCards int
		err := tx.QueryRow(ctx, `WITH copied_note AS (
			INSERT INTO notes(user_id,guid,notetype_id,fields,tags,checksum,modified_at,client_modified_at,last_client_id)
			SELECT user_id,$3,notetype_id,fields,tags,checksum,$4,$4,$5::uuid FROM notes
			WHERE user_id=$1 AND guid=$2 RETURNING guid
		), source_cards AS (
			SELECT deck_name,ord,row_number() OVER (ORDER BY ord,card_id)-1 AS card_offset
			FROM cards WHERE user_id=$1 AND note_guid=$2
		), copied_cards AS (
			INSERT INTO cards(user_id,card_id,note_guid,deck_name,ord,scheduling,created_at,
				modified_at,client_modified_at,last_client_id)
			SELECT $1,$6+card_offset,$3,deck_name,ord,'{}'::jsonb,$4,$4,$4,$5::uuid
			FROM source_cards,(SELECT guid FROM copied_note) copied RETURNING card_id
		) SELECT count(*) FROM copied_cards`, userID, target.NoteGUID, copyGUID, now,
			clientID, firstCardID).Scan(&copiedCards)
		if err != nil || copiedCards < 1 {
			return state, errors.New("note copy failed")
		}
		state.NoteCopyCreated = true
	case suspendCardKind:
		tag, err := tx.Exec(ctx, `UPDATE cards SET study_state='suspended',
			study_state_modified_at=$3,study_state_client_modified_at=$3,study_state_client_id=$4::uuid
			WHERE user_id=$1 AND card_id=$2 AND study_state='active'`, userID,
			target.SourceCardID, now, clientID)
		if err != nil || tag.RowsAffected() != 1 {
			return state, errors.New("card suspension failed")
		}
		state.StudyState = "suspended"
	case suspendNoteKind:
		tag, err := tx.Exec(ctx, `UPDATE cards SET study_state='suspended',
			study_state_modified_at=$3,study_state_client_modified_at=$3,study_state_client_id=$4::uuid
			WHERE user_id=$1 AND note_guid=$2 AND study_state='active'`, userID,
			target.NoteGUID, now, clientID)
		if err != nil || tag.RowsAffected() < 1 {
			return state, errors.New("note suspension failed")
		}
		state.StudyState = "suspended"
	case moveCardsKind:
		if err := applyMoveCardsTx(ctx, tx, userID, clientID, intent, now, &state); err != nil {
			return state, err
		}
	case renameDeckKind:
		if err := applyRenameDeckTx(ctx, tx, userID, clientID, intent, target, now, &state); err != nil {
			return state, err
		}
	default:
		return state, errors.New("unsupported review client operation")
	}
	return state, nil
}

func writeStoredOperationResponse(w http.ResponseWriter, encoded []byte) {
	if !json.Valid(encoded) {
		writeError(w, http.StatusInternalServerError, "internal", "stored operation receipt is invalid")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
