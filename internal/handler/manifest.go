package handler

import (
	"context"
	"database/sql"
	"github.com/jdmc43/kelma-sync/internal/inflight"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jdmc43/kelma-sync/internal/model"
)

// GetManifest returns lightweight summaries of everything changed on the server
// since the `since` query param. Omit `since` for a full manifest.
func (h *Handler) GetManifest(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	ctx := r.Context()

	// Parse `since`; zero time means "everything".
	var since time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "since must be RFC3339")
			return
		}
		since = t
	}

	// Never past a running write's stamps (see package inflight). Computed before
	// the queries below, so anything committed by then is listed now.
	serverTime := inflight.Writes.Cursor(claims.UserID)
	m := model.Manifest{ServerTime: serverTime,
		Capabilities: []string{cardFlagSyncCapability, reviewRetractionSyncCapability, reviewClientOperationsCapability}}

	var err error
	if m.Notes, err = manifestNotes(ctx, h.DB, claims.UserID, since); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "notes: "+err.Error())
		return
	}
	if m.Cards, err = manifestCards(ctx, h.DB, claims.UserID, since); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cards: "+err.Error())
		return
	}
	if m.Reviews, err = manifestReviews(ctx, h.DB, claims.UserID, since); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "reviews: "+err.Error())
		return
	}
	if m.ReviewRetractions, err = manifestReviewRetractions(ctx, h.DB, claims.UserID, since); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "review retractions: "+err.Error())
		return
	}
	if m.StudyDays, err = manifestStudyDays(ctx, h.DB, claims.UserID, since); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "study days: "+err.Error())
		return
	}
	if m.Notetypes, err = manifestNotetypes(ctx, h.DB, claims.UserID, since); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "notetypes: "+err.Error())
		return
	}
	if m.Decks, err = manifestDecks(ctx, h.DB, claims.UserID, since); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "decks: "+err.Error())
		return
	}
	if m.Media, err = manifestMedia(ctx, h.DB, claims.UserID, since); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "media: "+err.Error())
		return
	}
	if m.Tombstones, err = manifestTombstones(ctx, h.DB, claims.UserID, since); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "tombstones: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, m)
}

func manifestNotes(ctx context.Context, db *pgxpool.Pool, userID string, since time.Time) ([]model.ManifestEntry, error) {
	rows, err := db.Query(ctx,
		`SELECT n.guid,n.notetype_id,n.checksum,n.modified_at,n.client_modified_at,
		        COALESCE(n.last_client_id::text,''),n.tags,
		        COALESCE(n.mark_intent_id::text,''),n.mark_modified_at,
		        n.mark_client_modified_at,COALESCE(n.mark_client_id::text,''),
		        COALESCE(mark_client.label,'')
		 FROM notes n LEFT JOIN clients mark_client ON mark_client.id=n.mark_client_id
		 WHERE n.user_id=$1 AND (n.modified_at>$2 OR n.mark_modified_at>$2)
		 ORDER BY GREATEST(n.modified_at,COALESCE(n.mark_modified_at,n.modified_at))`,
		userID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ManifestEntry{}
	for rows.Next() {
		var entry model.ManifestEntry
		var tags []string
		var intentID, markClientID, markClientLabel string
		var markModifiedAt, markClientModifiedAt *time.Time
		if err := rows.Scan(&entry.GUID, &entry.NotetypeID, &entry.Checksum, &entry.ModifiedAt,
			&entry.ClientModifiedAt, &entry.LastClientID, &tags, &intentID, &markModifiedAt,
			&markClientModifiedAt, &markClientID, &markClientLabel); err != nil {
			return nil, err
		}
		if intentID != "" {
			entry.Mark = &model.NoteMark{Marked: hasMarkedTag(tags), IntentID: intentID,
				ModifiedAt: markModifiedAt.UTC(), ClientModifiedAt: markClientModifiedAt.UTC(),
				LastClientID: markClientID, LastClientLabel: markClientLabel}
		}
		utcManifestEntry(&entry)
		out = append(out, entry)
	}
	return out, rows.Err()
}

func manifestCards(ctx context.Context, db *pgxpool.Pool, userID string, since time.Time) ([]model.ManifestEntry, error) {
	rows, err := db.Query(ctx,
		`SELECT c.card_id,c.note_guid,c.deck_name,c.ord,
		        GREATEST(c.modified_at,c.study_state_modified_at,c.schedule_reset_modified_at,
		                 c.due_date_override_modified_at,c.flag_modified_at,
		                 COALESCE(p.updated_at,c.modified_at)),
		        c.client_modified_at,c.study_state,c.study_state_modified_at,
		        c.study_state_client_modified_at,c.schedule_reset_through_review_id,
		        c.schedule_reset_modified_at,c.schedule_reset_client_modified_at,
		        c.due_date_override_ms,c.due_date_override_modified_at,
		        c.due_date_override_client_modified_at,
		        COALESCE(c.flag_intent_id::text,''),c.flag_modified_at,
		        c.flag_client_modified_at,COALESCE(c.flag_client_id::text,''),
		        COALESCE(flag_client.label,''),
		        CASE WHEN jsonb_typeof(c.scheduling->'flags')='number'
		             THEN (c.scheduling->>'flags')::int ELSE 0 END,
		        COALESCE(p.status,''),COALESCE(p.desired_history_hash,''),
		        COALESCE(p.desired_through_review_id,0),
		        COALESCE(p.desired_profile_version,0),
		        COALESCE(p.applied_history_hash,''),p.applied_through_review_id,
		        p.applied_profile_version,COALESCE(c.last_client_id::text,'')
		 FROM cards c
		 LEFT JOIN clients flag_client ON flag_client.id=c.flag_client_id
		 LEFT JOIN scheduler_projections p ON p.user_id=c.user_id
		   AND p.note_guid=c.note_guid AND p.card_ord=c.ord
		 WHERE c.user_id=$1
		   AND GREATEST(c.modified_at,c.study_state_modified_at,c.schedule_reset_modified_at,
		                c.due_date_override_modified_at,c.flag_modified_at,
		                COALESCE(p.updated_at,c.modified_at))>$2
		 ORDER BY GREATEST(c.modified_at,c.study_state_modified_at,c.schedule_reset_modified_at,
		                   c.due_date_override_modified_at,c.flag_modified_at,
		                   COALESCE(p.updated_at,c.modified_at))`,
		userID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ManifestEntry{}
	for rows.Next() {
		var e model.ManifestEntry
		var deckName, flagIntentID, flagClientID, flagClientLabel string
		var flagValue int
		var flagModifiedAt, flagClientModifiedAt *time.Time
		var appliedThrough, appliedProfile sql.NullInt64
		if err := rows.Scan(
			&e.CardID, &e.NoteGUID, &deckName, &e.Ord, &e.ModifiedAt,
			&e.ClientModifiedAt, &e.StudyState, &e.StudyStateModifiedAt,
			&e.StudyStateClientModifiedAt, &e.ScheduleResetThroughReviewID,
			&e.ScheduleResetModifiedAt, &e.ScheduleResetClientModifiedAt,
			&e.DueDateOverrideMS, &e.DueDateOverrideModifiedAt,
			&e.DueDateOverrideClientModifiedAt,
			&flagIntentID, &flagModifiedAt, &flagClientModifiedAt, &flagClientID,
			&flagClientLabel, &flagValue,
			&e.ProjectionStatus, &e.DesiredHistoryHash,
			&e.DesiredThroughReviewID, &e.DesiredProfileVersion,
			&e.AppliedHistoryHash, &appliedThrough, &appliedProfile, &e.LastClientID,
		); err != nil {
			return nil, err
		}
		if flagIntentID != "" {
			e.Flag = &model.CardFlag{Flag: flagValue, IntentID: flagIntentID,
				ModifiedAt: flagModifiedAt.UTC(), ClientModifiedAt: flagClientModifiedAt.UTC(),
				LastClientID: flagClientID, LastClientLabel: flagClientLabel}
		}
		if appliedThrough.Valid {
			e.AppliedThroughReviewID = &appliedThrough.Int64
		}
		if appliedProfile.Valid {
			e.AppliedProfileVersion = &appliedProfile.Int64
		}
		e.DeckName = deckName
		e.Checksum = checksum(e.NoteGUID, deckName, e.Ord)
		utcManifestEntry(&e)
		out = append(out, e)
	}
	return out, rows.Err()
}

func manifestReviews(ctx context.Context, db *pgxpool.Pool, userID string, since time.Time) ([]model.ReviewManifestEntry, error) {
	rows, err := db.Query(ctx,
		`SELECT review_id, checksum, deck_name, modified_at,
		        COALESCE(last_client_id::text,'') FROM reviews
		 WHERE user_id = $1 AND modified_at > $2 ORDER BY modified_at`,
		userID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ReviewManifestEntry{}
	for rows.Next() {
		var entry model.ReviewManifestEntry
		if err := rows.Scan(
			&entry.ReviewID, &entry.Checksum, &entry.DeckName, &entry.ModifiedAt, &entry.LastClientID,
		); err != nil {
			return nil, err
		}
		entry.ModifiedAt = entry.ModifiedAt.UTC()
		out = append(out, entry)
	}
	return out, rows.Err()
}

func manifestReviewRetractions(ctx context.Context, db *pgxpool.Pool, userID string, since time.Time) ([]model.ReviewRetraction, error) {
	rows, err := db.Query(ctx, `SELECT rr.review_id,rr.intent_id::text,rr.client_modified_at,
		rr.modified_at,COALESCE(rr.last_client_id::text,''),COALESCE(c.label,'')
		FROM review_retractions rr LEFT JOIN clients c ON c.id=rr.last_client_id
		WHERE rr.user_id=$1 AND rr.modified_at>$2 ORDER BY rr.modified_at,rr.review_id`, userID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ReviewRetraction{}
	for rows.Next() {
		var value model.ReviewRetraction
		if err := rows.Scan(&value.ReviewID, &value.IntentID, &value.ClientModifiedAt,
			&value.ModifiedAt, &value.LastClientID, &value.LastClientLabel); err != nil {
			return nil, err
		}
		value.ClientModifiedAt = value.ClientModifiedAt.UTC()
		value.ModifiedAt = value.ModifiedAt.UTC()
		out = append(out, value)
	}
	return out, rows.Err()
}

func manifestStudyDays(ctx context.Context, db *pgxpool.Pool, userID string, since time.Time) ([]model.StudyDay, error) {
	rows, err := db.Query(ctx,
		`SELECT s.id, s.user_id, s.day, s.deck_name, s.new_studied,
		        s.review_studied, s.learning_studied, s.milliseconds_studied,
		        s.modified_at, COALESCE(s.last_client_id::text, ''),
		        COALESCE(c.label, '')
		 FROM study_days s LEFT JOIN clients c ON c.id=s.last_client_id
		 WHERE s.user_id=$1 AND s.modified_at > $2 ORDER BY s.day, s.deck_name`,
		userID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.StudyDay{}
	for rows.Next() {
		var day model.StudyDay
		if err := rows.Scan(
			&day.ID, &day.UserID, &day.Day, &day.DeckName, &day.NewStudied,
			&day.ReviewStudied, &day.LearningStudied,
			&day.MillisecondsStudied, &day.ModifiedAt, &day.LastClientID,
			&day.LastClientLabel,
		); err != nil {
			return nil, err
		}
		day.ModifiedAt = day.ModifiedAt.UTC()
		out = append(out, day)
	}
	return out, rows.Err()
}

func manifestNotetypes(ctx context.Context, db *pgxpool.Pool, userID string, since time.Time) ([]model.ManifestEntry, error) {
	rows, err := db.Query(ctx,
		`SELECT notetype_id, name, definition, modified_at, client_modified_at,
		        COALESCE(last_client_id::text,'') FROM notetypes
		 WHERE user_id = $1 AND modified_at > $2 ORDER BY modified_at`,
		userID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ManifestEntry{}
	for rows.Next() {
		var e model.ManifestEntry
		var name string
		var def map[string]any
		if err := rows.Scan(&e.NotetypeID, &name, &def, &e.ModifiedAt, &e.ClientModifiedAt, &e.LastClientID); err != nil {
			return nil, err
		}
		e.Checksum = notetypeChecksum(name, def)
		utcManifestEntry(&e)
		out = append(out, e)
	}
	return out, rows.Err()
}

func manifestDecks(ctx context.Context, db *pgxpool.Pool, userID string, since time.Time) ([]model.ManifestEntry, error) {
	rows, err := db.Query(ctx,
		`SELECT name, config, modified_at, client_modified_at,
		        COALESCE(last_client_id::text,'') FROM decks
		 WHERE user_id = $1 AND modified_at > $2 ORDER BY modified_at`,
		userID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ManifestEntry{}
	for rows.Next() {
		var e model.ManifestEntry
		var cfg map[string]any
		if err := rows.Scan(&e.Name, &cfg, &e.ModifiedAt, &e.ClientModifiedAt, &e.LastClientID); err != nil {
			return nil, err
		}
		e.Checksum = deckChecksum(cfg)
		utcManifestEntry(&e)
		out = append(out, e)
	}
	return out, rows.Err()
}

func manifestMedia(ctx context.Context, db *pgxpool.Pool, userID string, since time.Time) ([]model.ManifestEntry, error) {
	rows, err := db.Query(ctx,
		`SELECT filename, size_bytes, modified_at,COALESCE(last_client_id::text,'') FROM media
		 WHERE user_id = $1 AND modified_at > $2 ORDER BY modified_at`,
		userID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ManifestEntry{}
	for rows.Next() {
		var e model.ManifestEntry
		if err := rows.Scan(&e.Filename, &e.SizeBytes, &e.ModifiedAt, &e.LastClientID); err != nil {
			return nil, err
		}
		utcManifestEntry(&e)
		out = append(out, e)
	}
	return out, rows.Err()
}

func manifestTombstones(ctx context.Context, db *pgxpool.Pool, userID string, since time.Time) ([]model.Tombstone, error) {
	rows, err := db.Query(ctx,
		`SELECT id,user_id,type,resource_id,deleted_at,last_client_id,
		        COALESCE(note_guid,''),card_ord FROM tombstones
		 WHERE user_id = $1 AND deleted_at > $2 ORDER BY deleted_at`,
		userID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Tombstone{}
	for rows.Next() {
		var t model.Tombstone
		if err := rows.Scan(&t.ID, &t.UserID, &t.Type, &t.ResourceID, &t.DeletedAt,
			&t.LastClientID, &t.NoteGUID, &t.CardOrd); err != nil {
			return nil, err
		}
		t.DeletedAt = t.DeletedAt.UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

func utcManifestEntry(entry *model.ManifestEntry) {
	entry.ModifiedAt = entry.ModifiedAt.UTC()
	entry.ClientModifiedAt = entry.ClientModifiedAt.UTC()
	entry.StudyStateModifiedAt = entry.StudyStateModifiedAt.UTC()
	entry.StudyStateClientModifiedAt = entry.StudyStateClientModifiedAt.UTC()
	entry.ScheduleResetModifiedAt = entry.ScheduleResetModifiedAt.UTC()
	entry.ScheduleResetClientModifiedAt = entry.ScheduleResetClientModifiedAt.UTC()
	entry.DueDateOverrideModifiedAt = entry.DueDateOverrideModifiedAt.UTC()
	entry.DueDateOverrideClientModifiedAt = entry.DueDateOverrideClientModifiedAt.UTC()
	if entry.Mark != nil {
		entry.Mark.ModifiedAt = entry.Mark.ModifiedAt.UTC()
		entry.Mark.ClientModifiedAt = entry.Mark.ClientModifiedAt.UTC()
	}
}
