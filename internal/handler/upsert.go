package handler

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jdmc43/kelma-sync/internal/model"
	"github.com/jdmc43/kelma-sync/internal/projection"
)

// This file holds the single source of truth for writing each resource type.
// Both the single-item PUT handlers and the batch endpoint call these, so the
// upsert SQL lives in exactly one place per resource.
//
// Each helper performs the INSERT ... ON CONFLICT DO UPDATE, clears any
// tombstone for the resource, and returns the saved record. Conflict detection
// is the caller's responsibility (it happens before the write).

const (
	maxFutureClockSkew              = 5 * time.Minute
	earliestAnkiCardCreatedAtMillis = int64(946684800000)
)

// utcWriteTimes makes every persisted timestamp explicitly UTC. The server's
// authenticated clock is authoritative: a client clock far in the future is
// clamped so it cannot poison newest-wins reconciliation indefinitely.
func utcWriteTimes(now, clientModifiedAt time.Time) (time.Time, time.Time) {
	now = now.UTC()
	if clientModifiedAt.IsZero() || clientModifiedAt.After(now.Add(maxFutureClockSkew)) {
		return now, now
	}
	return now, clientModifiedAt.UTC()
}

func resolvedCardCreatedAt(cardID int64, requested *time.Time, now time.Time) *time.Time {
	now = now.UTC()
	if requested != nil && !requested.IsZero() && !requested.After(now.Add(maxFutureClockSkew)) {
		createdAt := requested.UTC()
		return &createdAt
	}
	latestMillis := now.Add(maxFutureClockSkew).UnixMilli()
	if cardID < earliestAnkiCardCreatedAtMillis || cardID > latestMillis {
		return nil
	}
	createdAt := time.UnixMilli(cardID).UTC()
	return &createdAt
}

func (h *Handler) fillCardCreatedAt(
	ctx context.Context,
	userID, noteGUID string,
	ord int,
	cardID int64,
	requested *time.Time,
	now time.Time,
) (bool, error) {
	createdAt := resolvedCardCreatedAt(cardID, requested, now)
	if createdAt == nil {
		return false, nil
	}
	tag, err := h.DB.Exec(ctx,
		`UPDATE cards SET created_at=$1,modified_at=$2
		 WHERE user_id=$3 AND note_guid=$4 AND ord=$5 AND created_at IS NULL`,
		createdAt, now.UTC(), userID, noteGUID, ord,
	)
	return tag.RowsAffected() == 1, err
}

func upsertNoteTx(ctx context.Context, tx pgx.Tx, userID, clientID, guid string,
	req putNoteRequest, cs string, now time.Time,
) (model.Note, error) {
	now, req.ClientModifiedAt = utcWriteTimes(now, req.ClientModifiedAt)
	if req.Tags == nil {
		req.Tags = []string{}
	}
	var note model.Note
	var intentID, markClientID string
	var markModifiedAt, markClientModifiedAt *time.Time
	err := tx.QueryRow(ctx,
		`INSERT INTO notes (user_id,guid,notetype_id,fields,tags,checksum,
		                    modified_at,client_modified_at,last_client_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT (user_id,guid) DO UPDATE SET
		     notetype_id=EXCLUDED.notetype_id,fields=EXCLUDED.fields,tags=EXCLUDED.tags,
		     checksum=EXCLUDED.checksum,modified_at=EXCLUDED.modified_at,
		     client_modified_at=EXCLUDED.client_modified_at,last_client_id=EXCLUDED.last_client_id
		 RETURNING id,user_id,guid,notetype_id,fields,tags,checksum,modified_at,
		           client_modified_at,COALESCE(last_client_id::text,''),
		           COALESCE(mark_intent_id::text,''),mark_modified_at,
		           mark_client_modified_at,COALESCE(mark_client_id::text,'')`,
		userID, guid, req.NotetypeID, req.Fields, req.Tags, cs, now, req.ClientModifiedAt, clientID,
	).Scan(&note.ID, &note.UserID, &note.GUID, &note.NotetypeID, &note.Fields, &note.Tags,
		&note.Checksum, &note.ModifiedAt, &note.ClientModifiedAt, &note.LastClientID,
		&intentID, &markModifiedAt, &markClientModifiedAt, &markClientID)
	if err != nil {
		return note, err
	}
	if intentID != "" {
		note.Mark = &model.NoteMark{Marked: hasMarkedTag(note.Tags), IntentID: intentID,
			ModifiedAt: markModifiedAt.UTC(), ClientModifiedAt: markClientModifiedAt.UTC(),
			LastClientID: markClientID}
	}
	_, err = tx.Exec(ctx, `DELETE FROM tombstones WHERE user_id=$1 AND type='note' AND resource_id=$2`, userID, guid)
	return note, err
}

func (h *Handler) upsertCard(ctx context.Context, userID, clientID string, cardID int64, req putCardRequest, now time.Time) (model.Card, error) {
	now, req.ClientModifiedAt = utcWriteTimes(now, req.ClientModifiedAt)
	createdAt := resolvedCardCreatedAt(cardID, req.CreatedAt, now)
	sched := req.Scheduling
	if sched == nil {
		sched = map[string]any{}
	}
	studyState := "active"
	if req.StudyState != nil {
		studyState = *req.StudyState
	}
	studyStateClientModifiedAt := req.ClientModifiedAt
	if req.StudyStateClientModifiedAt != nil {
		_, studyStateClientModifiedAt = utcWriteTimes(now, *req.StudyStateClientModifiedAt)
	}
	var c model.Card
	// Cards are keyed by logical identity (note_guid, ord), NOT card_id, so two
	// devices that generated the same logical card with different local card_ids
	// resolve to one server row. The first writer's card_id is kept canonical on
	// conflict; only content/scheduling is updated.
	var flagIntentID, flagClientID string
	var flagModifiedAt, flagClientModifiedAt *time.Time
	err := h.DB.QueryRow(ctx,
		`INSERT INTO cards (user_id,card_id,note_guid,deck_name,ord,scheduling,created_at,
		                    study_state,study_state_modified_at,study_state_client_modified_at,
		                    study_state_client_id,modified_at,client_modified_at,last_client_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$12,$8,$7,$9,$10,$7,$11,$10)
		 ON CONFLICT (user_id, note_guid, ord) WHERE note_guid <> '' DO UPDATE SET
		     deck_name=EXCLUDED.deck_name,
		     created_at=COALESCE(cards.created_at,EXCLUDED.created_at),
		     scheduling=jsonb_set(
		       CASE WHEN EXISTS (
		         SELECT 1 FROM scheduler_projections p
		         WHERE p.user_id=cards.user_id AND p.note_guid=cards.note_guid
		           AND p.card_ord=cards.ord AND p.applied_history_hash IS NOT NULL
		       ) THEN cards.scheduling ELSE EXCLUDED.scheduling END,
		       '{flags}',CASE WHEN cards.flag_intent_id IS NOT NULL
		         THEN COALESCE(cards.scheduling->'flags','0'::jsonb)
		         ELSE COALESCE(EXCLUDED.scheduling->'flags','0'::jsonb) END,true),
		     modified_at=EXCLUDED.modified_at,
		     client_modified_at=CASE WHEN EXISTS (
		       SELECT 1 FROM scheduler_projections p
		       WHERE p.user_id=cards.user_id AND p.note_guid=cards.note_guid
		         AND p.card_ord=cards.ord AND p.applied_history_hash IS NOT NULL
		     ) THEN GREATEST(cards.client_modified_at,EXCLUDED.client_modified_at,
		                     EXCLUDED.modified_at)+interval '1 microsecond'
		       ELSE EXCLUDED.client_modified_at END,
		     last_client_id=CASE WHEN EXISTS (
		       SELECT 1 FROM scheduler_projections p
		       WHERE p.user_id=cards.user_id AND p.note_guid=cards.note_guid
		         AND p.card_ord=cards.ord AND p.applied_history_hash IS NOT NULL
		     ) THEN NULL ELSE EXCLUDED.last_client_id END
		 RETURNING id,card_id,note_guid,deck_name,ord,scheduling,created_at,study_state,
		           study_state_modified_at,study_state_client_modified_at,
		           COALESCE(study_state_client_id::text,''),
		           schedule_reset_through_review_id,schedule_reset_modified_at,
		           schedule_reset_client_modified_at,COALESCE(schedule_reset_client_id::text,''),
		           due_date_override_ms,due_date_override_modified_at,
		           due_date_override_client_modified_at,COALESCE(due_date_override_client_id::text,''),
		           COALESCE(flag_intent_id::text,''),flag_modified_at,flag_client_modified_at,
		           COALESCE(flag_client_id::text,''),
		           modified_at,client_modified_at,COALESCE(last_client_id::text,'')`,
		userID, cardID, req.NoteGUID, req.DeckName, req.Ord, sched, now, studyState,
		studyStateClientModifiedAt, clientID, req.ClientModifiedAt, createdAt,
	).Scan(
		&c.ID, &c.CardID, &c.NoteGUID, &c.DeckName, &c.Ord, &c.Scheduling, &c.CreatedAt, &c.StudyState,
		&c.StudyStateModifiedAt, &c.StudyStateClientModifiedAt, &c.StudyStateClientID,
		&c.ScheduleResetThroughReviewID, &c.ScheduleResetModifiedAt,
		&c.ScheduleResetClientModifiedAt, &c.ScheduleResetClientID,
		&c.DueDateOverrideMS, &c.DueDateOverrideModifiedAt,
		&c.DueDateOverrideClientModifiedAt, &c.DueDateOverrideClientID,
		&flagIntentID, &flagModifiedAt, &flagClientModifiedAt, &flagClientID,
		&c.ModifiedAt, &c.ClientModifiedAt, &c.LastClientID)
	if err != nil {
		return c, err
	}
	c.UserID = userID
	if flagIntentID != "" {
		flagValue := schedulingFlag(c.Scheduling)
		c.Flag = &model.CardFlag{Flag: flagValue, IntentID: flagIntentID,
			ModifiedAt: flagModifiedAt.UTC(), ClientModifiedAt: flagClientModifiedAt.UTC(),
			LastClientID: flagClientID}
	}
	if req.ScheduleResetThroughReviewID != nil {
		if _, err := h.updateCardScheduleReset(ctx, userID, clientID, c.NoteGUID, c.Ord, req, now); err != nil {
			return c, err
		}
		c.ScheduleResetThroughReviewID = *req.ScheduleResetThroughReviewID
		c.ScheduleResetModifiedAt = now
		c.ScheduleResetClientModifiedAt = req.ClientModifiedAt
		if req.ScheduleResetClientModifiedAt != nil {
			_, c.ScheduleResetClientModifiedAt = utcWriteTimes(now, *req.ScheduleResetClientModifiedAt)
		}
		c.ScheduleResetClientID = clientID
	}
	if req.DueDateOverrideMS != nil {
		updated, err := h.updateCardDueDateOverride(ctx, userID, clientID, c.NoteGUID, c.Ord, req, now)
		if err != nil {
			return c, err
		}
		if updated {
			c.DueDateOverrideMS = *req.DueDateOverrideMS
			c.DueDateOverrideModifiedAt = now
			c.DueDateOverrideClientModifiedAt = req.ClientModifiedAt
			if req.DueDateOverrideClientModifiedAt != nil {
				_, c.DueDateOverrideClientModifiedAt = utcWriteTimes(now, *req.DueDateOverrideClientModifiedAt)
			}
			c.DueDateOverrideClientID = clientID
		}
	}
	h.clearTombstone(ctx, userID, "card", int64ToStr(c.CardID))
	if err := projection.EnqueueCard(ctx, h.DB, userID, c.NoteGUID, c.Ord); err != nil {
		return c, err
	}
	return c, nil
}

func (h *Handler) updateCardStudyState(
	ctx context.Context,
	userID, clientID, noteGUID string,
	ord int,
	req putCardRequest,
	now time.Time,
) (bool, error) {
	if req.StudyState == nil {
		return false, nil
	}
	clientModifiedAt := req.ClientModifiedAt
	if req.StudyStateClientModifiedAt != nil {
		clientModifiedAt = *req.StudyStateClientModifiedAt
	}
	now, clientModifiedAt = utcWriteTimes(now, clientModifiedAt)
	tag, err := h.DB.Exec(ctx,
		`UPDATE cards SET study_state=$1,study_state_modified_at=$2,
		                    study_state_client_modified_at=$3,study_state_client_id=$4
		 WHERE user_id=$5 AND note_guid=$6 AND ord=$7
		   AND (study_state_client_modified_at,COALESCE(study_state_client_id,
		       '00000000-0000-0000-0000-000000000000'::uuid)) < ($3,$4::uuid)`,
		*req.StudyState, now, clientModifiedAt, clientID, userID, noteGUID, ord,
	)
	return tag.RowsAffected() == 1, err
}

func (h *Handler) updateCardScheduleReset(
	ctx context.Context,
	userID, clientID, noteGUID string,
	ord int,
	req putCardRequest,
	now time.Time,
) (bool, error) {
	if req.ScheduleResetThroughReviewID == nil {
		return false, nil
	}
	clientModifiedAt := req.ClientModifiedAt
	if req.ScheduleResetClientModifiedAt != nil {
		clientModifiedAt = *req.ScheduleResetClientModifiedAt
	}
	now, clientModifiedAt = utcWriteTimes(now, clientModifiedAt)
	tag, err := h.DB.Exec(ctx,
		`UPDATE cards SET schedule_reset_through_review_id=$1,schedule_reset_modified_at=$2,
		                  schedule_reset_client_modified_at=$3,schedule_reset_client_id=$4
		 WHERE user_id=$5 AND note_guid=$6 AND ord=$7
		   AND schedule_reset_through_review_id<$1`,
		*req.ScheduleResetThroughReviewID, now, clientModifiedAt, clientID, userID, noteGUID, ord,
	)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	if err := projection.EnqueueCardReset(ctx, h.DB, userID, noteGUID, ord); err != nil {
		return false, err
	}
	if _, err := h.DB.Exec(ctx,
		`UPDATE cards SET due_date_override_ms=0,due_date_override_modified_at=$1,
		                  due_date_override_client_modified_at=$2,due_date_override_client_id=$3
		 WHERE user_id=$4 AND note_guid=$5 AND ord=$6 AND due_date_override_ms>0
		   AND (due_date_override_client_modified_at,COALESCE(due_date_override_client_id,
		       '00000000-0000-0000-0000-000000000000'::uuid)) < ($2,$3::uuid)`,
		now, clientModifiedAt, clientID, userID, noteGUID, ord,
	); err != nil {
		return false, err
	}
	return true, nil
}

func (h *Handler) updateCardDueDateOverride(
	ctx context.Context,
	userID, clientID, noteGUID string,
	ord int,
	req putCardRequest,
	now time.Time,
) (bool, error) {
	if req.DueDateOverrideMS == nil {
		return false, nil
	}
	clientModifiedAt := req.ClientModifiedAt
	if req.DueDateOverrideClientModifiedAt != nil {
		clientModifiedAt = *req.DueDateOverrideClientModifiedAt
	}
	now, clientModifiedAt = utcWriteTimes(now, clientModifiedAt)
	tag, err := h.DB.Exec(ctx,
		`UPDATE cards SET due_date_override_ms=$1,due_date_override_modified_at=$2,
		                  due_date_override_client_modified_at=$3,due_date_override_client_id=$4
		 WHERE user_id=$5 AND note_guid=$6 AND ord=$7
		   AND (due_date_override_client_modified_at,COALESCE(due_date_override_client_id,
		       '00000000-0000-0000-0000-000000000000'::uuid)) < ($3,$4::uuid)`,
		*req.DueDateOverrideMS, now, clientModifiedAt, clientID, userID, noteGUID, ord,
	)
	return tag.RowsAffected() == 1, err
}

func (h *Handler) upsertNotetype(ctx context.Context, userID, clientID string, id int64, req putNotetypeRequest, cs string, now time.Time) (model.Notetype, error) {
	now, req.ClientModifiedAt = utcWriteTimes(now, req.ClientModifiedAt)
	var nt model.Notetype
	err := h.DB.QueryRow(ctx,
		`INSERT INTO notetypes (user_id, notetype_id, name, definition, checksum,
		                        modified_at, client_modified_at, last_client_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (user_id, notetype_id) DO UPDATE SET
		     name=EXCLUDED.name, definition=EXCLUDED.definition, checksum=EXCLUDED.checksum,
		     modified_at=EXCLUDED.modified_at, client_modified_at=EXCLUDED.client_modified_at,
		     last_client_id=EXCLUDED.last_client_id
		 RETURNING id, notetype_id, name, definition, checksum,
		           modified_at, client_modified_at, COALESCE(last_client_id::text,'')`,
		userID, id, req.Name, req.Definition, cs, now, req.ClientModifiedAt, clientID,
	).Scan(&nt.ID, &nt.NotetypeID, &nt.Name, &nt.Definition, &nt.Checksum,
		&nt.ModifiedAt, &nt.ClientModifiedAt, &nt.LastClientID)
	if err != nil {
		return nt, err
	}
	nt.UserID = userID
	h.clearTombstone(ctx, userID, "notetype", int64ToStr(id))
	return nt, nil
}

func (h *Handler) upsertDeck(ctx context.Context, userID, clientID, name string, req putDeckRequest, cs string, now time.Time) (model.Deck, error) {
	now, req.ClientModifiedAt = utcWriteTimes(now, req.ClientModifiedAt)
	cfg := req.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	var d model.Deck
	err := h.DB.QueryRow(ctx,
		`INSERT INTO decks (user_id, name, config, checksum,
		                    modified_at, client_modified_at, last_client_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (user_id, name) DO UPDATE SET
		     config=EXCLUDED.config, checksum=EXCLUDED.checksum, modified_at=EXCLUDED.modified_at,
		     client_modified_at=EXCLUDED.client_modified_at, last_client_id=EXCLUDED.last_client_id
		 RETURNING id, name, config, checksum,
		           modified_at, client_modified_at, COALESCE(last_client_id::text,'')`,
		userID, name, cfg, cs, now, req.ClientModifiedAt, clientID,
	).Scan(&d.ID, &d.Name, &d.Config, &d.Checksum,
		&d.ModifiedAt, &d.ClientModifiedAt, &d.LastClientID)
	if err != nil {
		return d, err
	}
	d.UserID = userID
	h.clearTombstone(ctx, userID, "deck", name)
	return d, nil
}

// clearTombstone removes any tombstone for a resource that has just been
// (re-)written, so it is no longer considered deleted.
func (h *Handler) clearTombstone(ctx context.Context, userID, typ, resourceID string) {
	_, _ = h.DB.Exec(ctx,
		`DELETE FROM tombstones WHERE user_id=$1 AND type=$2 AND resource_id=$3`,
		userID, typ, resourceID)
}
