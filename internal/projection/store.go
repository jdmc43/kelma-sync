package projection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type LogicalCard struct {
	NoteGUID string `json:"note_guid"`
	CardOrd  int    `json:"card_ord"`
}

type rowScanner interface {
	Scan(dest ...any) error
}

func ActiveProfile(ctx context.Context, db *pgxpool.Pool, userID string) (Profile, error) {
	profile, err := scanProfile(db.QueryRow(ctx, profileSelect+`
		WHERE user_id=$1 ORDER BY version DESC LIMIT 1`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultProfile(), nil
	}
	return profile, err
}

func PutProfile(
	ctx context.Context,
	db *pgxpool.Pool,
	userID, clientID string,
	candidate Candidate,
) (Profile, error) {
	profile, err := ProfileFromCandidate(candidate)
	if err != nil {
		return Profile{}, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return Profile{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := LockAccountTx(ctx, tx, userID); err != nil {
		return Profile{}, err
	}

	existing, err := scanProfile(tx.QueryRow(ctx, profileSelect+`
		WHERE user_id=$1 AND idempotency_key=$2`, userID, candidate.IdempotencyKey))
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return Profile{}, err
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, err
	}

	var currentVersion int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(version),0) FROM scheduler_profiles WHERE user_id=$1`,
		userID,
	).Scan(&currentVersion); err != nil {
		return Profile{}, err
	}
	if candidate.BaseVersion != currentVersion {
		return Profile{}, fmt.Errorf("%w: base=%d current=%d", ErrProfileConflict, candidate.BaseVersion, currentVersion)
	}

	if profile.ParameterSource == "client_optimized" || profile.RetentionSource == "client_optimized" {
		actualHash, err := optimizationHistoryHashTx(ctx, tx, userID, *profile.OptimizationThroughReviewID)
		if err != nil {
			return Profile{}, err
		}
		if actualHash != profile.OptimizationHistoryHash {
			return Profile{}, fmt.Errorf("%w: supplied=%s actual=%s", ErrHistoryMismatch, profile.OptimizationHistoryHash, actualHash)
		}
	}

	profile.Version = currentVersion + 1
	parameters, _ := json.Marshal(profile.Parameters)
	metrics, _ := json.Marshal(profile.QualityMetrics)
	_, err = tx.Exec(ctx,
		`INSERT INTO scheduler_profiles (
		   user_id,version,algorithm,scheduler_implementation,scheduler_version,
		   idempotency_key,parameters,parameter_source,desired_retention,
		   retention_source,maximum_interval,learning_steps_seconds,
		   relearning_steps_seconds,enable_short_term,enable_fuzzing,config_hash,
		   optimizer,optimizer_version,optimization_history_hash,
		   optimization_through_review_id,quality_metrics,last_client_id
		 ) VALUES (
		   $1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,$11,$12,$13,$14,$15,$16,
		   NULLIF($17,''),NULLIF($18,''),NULLIF($19,''),$20,$21::jsonb,$22
		 )`,
		userID, profile.Version, profile.Algorithm, profile.SchedulerImplementation,
		profile.SchedulerVersion, profile.IdempotencyKey, string(parameters),
		profile.ParameterSource, profile.DesiredRetention, profile.RetentionSource,
		profile.MaximumInterval, intsToInt32s(profile.LearningStepsSeconds),
		intsToInt32s(profile.RelearningStepsSeconds), profile.EnableShortTerm,
		profile.EnableFuzzing, profile.ConfigHash, profile.Optimizer,
		profile.OptimizerVersion, profile.OptimizationHistoryHash,
		profile.OptimizationThroughReviewID, string(metrics), clientID,
	)
	if err != nil {
		return Profile{}, err
	}
	if err := enqueueAllTx(ctx, tx, userID, profile.Version); err != nil {
		return Profile{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Profile{}, err
	}
	profile.LastClientID = clientID
	return profile, nil
}

func EnqueueReviewsTx(ctx context.Context, tx pgx.Tx, userID string, cards []LogicalCard) error {
	if len(cards) == 0 {
		return nil
	}
	if err := LockAccountTx(ctx, tx, userID); err != nil {
		return err
	}
	encoded, err := json.Marshal(cards)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, enqueueSQL,
		userID, encoded, nil,
	)
	return err
}

func EnqueueCard(ctx context.Context, db *pgxpool.Pool, userID, noteGUID string, cardOrd int) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := EnqueueCardTx(ctx, tx, userID, noteGUID, cardOrd); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EnqueueCardReset rebuilds a card after its independently synchronized reset cutoff changes.
func EnqueueCardReset(ctx context.Context, db *pgxpool.Pool, userID, noteGUID string, cardOrd int) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := LockAccountTx(ctx, tx, userID); err != nil {
		return err
	}
	encoded, err := json.Marshal([]LogicalCard{{NoteGUID: noteGUID, CardOrd: cardOrd}})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, enqueueSQL, userID, encoded, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EnqueueCardTx creates work for review history that arrived before its card,
// or retries work which previously failed because the card was absent.
func EnqueueCardTx(ctx context.Context, tx pgx.Tx, userID, noteGUID string, cardOrd int) error {
	encoded, err := json.Marshal([]LogicalCard{{NoteGUID: noteGUID, CardOrd: cardOrd}})
	if err != nil {
		return err
	}
	if err := LockAccountTx(ctx, tx, userID); err != nil {
		return err
	}
	var status string
	err = tx.QueryRow(ctx,
		`SELECT status FROM scheduler_projections
		 WHERE user_id=$1 AND note_guid=$2 AND card_ord=$3 FOR UPDATE`,
		userID, noteGUID, cardOrd,
	).Scan(&status)
	if err == nil && status != "failed" && status != "superseded" {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	_, err = tx.Exec(ctx, enqueueSQL, userID, encoded, nil)
	return err
}

// LockAccountTx serializes review acceptance, profile activation, card
// projection enqueueing, and projection publication for one account.
func LockAccountTx(ctx context.Context, tx pgx.Tx, userID string) error {
	var lockedUserID string
	return tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&lockedUserID)
}

func enqueueAllTx(ctx context.Context, tx pgx.Tx, userID string, profileVersion int64) error {
	_, err := tx.Exec(ctx, enqueueAllSQL, userID, profileVersion)
	return err
}

const enqueueSQL = `
WITH affected AS (
  SELECT DISTINCT incoming.note_guid, incoming.card_ord
  FROM jsonb_to_recordset($2::jsonb) AS incoming(note_guid text, card_ord int)
  WHERE incoming.note_guid<>''
), histories AS (
  SELECT a.note_guid, a.card_ord,
         COALESCE(MAX(r.review_id),MAX(c.schedule_reset_through_review_id),0) AS through_review_id,
         encode(digest(
           'reset:' || COALESCE(MAX(c.schedule_reset_through_review_id),0)::text || E'\n' ||
           COALESCE(string_agg(r.review_id::text || ':' || r.checksum, E'\n' ORDER BY r.review_id),''),
           'sha256'), 'hex') AS history_hash
  FROM affected a
  LEFT JOIN cards c ON c.user_id=$1 AND c.note_guid=a.note_guid AND c.ord=a.card_ord
  LEFT JOIN reviews r ON r.user_id=$1 AND r.note_guid=a.note_guid AND r.card_ord=a.card_ord
    AND r.ease BETWEEN 1 AND 4
    AND r.review_id>COALESCE(c.schedule_reset_through_review_id,0)
    AND NOT EXISTS (SELECT 1 FROM review_retractions rr
      WHERE rr.user_id=r.user_id AND rr.review_id=r.review_id)
  GROUP BY a.note_guid, a.card_ord
  HAVING COUNT(r.review_id)>0 OR COUNT(c.id)>0
), profile AS (
  SELECT COALESCE($3::bigint,
    (SELECT MAX(version) FROM scheduler_profiles WHERE user_id=$1), 0) AS version
)
` + enqueueUpsertSQL

const enqueueAllSQL = `
WITH affected AS (
  SELECT DISTINCT r.note_guid, r.card_ord
  FROM reviews r
  WHERE r.user_id=$1 AND r.note_guid<>'' AND r.ease BETWEEN 1 AND 4
), histories AS (
  SELECT a.note_guid, a.card_ord,
         COALESCE(MAX(r.review_id),MAX(c.schedule_reset_through_review_id),0) AS through_review_id,
         encode(digest(
           'reset:' || COALESCE(MAX(c.schedule_reset_through_review_id),0)::text || E'\n' ||
           COALESCE(string_agg(r.review_id::text || ':' || r.checksum, E'\n' ORDER BY r.review_id),''),
           'sha256'), 'hex') AS history_hash
  FROM affected a
  LEFT JOIN cards c ON c.user_id=$1 AND c.note_guid=a.note_guid AND c.ord=a.card_ord
  LEFT JOIN reviews r ON r.user_id=$1 AND r.note_guid=a.note_guid AND r.card_ord=a.card_ord
    AND r.ease BETWEEN 1 AND 4
    AND r.review_id>COALESCE(c.schedule_reset_through_review_id,0)
    AND NOT EXISTS (SELECT 1 FROM review_retractions rr
      WHERE rr.user_id=r.user_id AND rr.review_id=r.review_id)
  GROUP BY a.note_guid, a.card_ord
  HAVING COUNT(r.review_id)>0 OR COUNT(c.id)>0
), profile AS (SELECT $2::bigint AS version)
` + enqueueUpsertSQL

const enqueueUpsertSQL = `
INSERT INTO scheduler_projections (
  user_id,note_guid,card_ord,desired_history_hash,desired_through_review_id,
  desired_profile_version,status,job_id,attempt_count,last_error_code,
  next_retry_at,updated_at
)
SELECT $1,h.note_guid,h.card_ord,h.history_hash,h.through_review_id,
       profile.version,'pending',gen_random_uuid(),0,NULL,now(),now()
FROM histories h CROSS JOIN profile
ON CONFLICT (user_id,note_guid,card_ord) DO UPDATE SET
  desired_generation=scheduler_projections.desired_generation+1,
  desired_history_hash=EXCLUDED.desired_history_hash,
  desired_through_review_id=EXCLUDED.desired_through_review_id,
  desired_profile_version=EXCLUDED.desired_profile_version,
  status='pending',job_id=gen_random_uuid(),attempt_count=0,
  last_error_code=NULL,next_retry_at=now(),updated_at=now()
`

const profileSelect = `SELECT version,algorithm,scheduler_implementation,
  scheduler_version,idempotency_key,parameters,parameter_source,
  desired_retention,retention_source,maximum_interval,
  learning_steps_seconds,relearning_steps_seconds,enable_short_term,
  enable_fuzzing,config_hash,COALESCE(optimizer,''),
  COALESCE(optimizer_version,''),COALESCE(optimization_history_hash,''),
  optimization_through_review_id,quality_metrics,created_at,
  COALESCE(last_client_id::text,'')
FROM scheduler_profiles `

func scanProfile(row rowScanner) (Profile, error) {
	var profile Profile
	var parameters, metrics []byte
	var learning, relearning []int32
	var through sql.NullInt64
	err := row.Scan(
		&profile.Version, &profile.Algorithm, &profile.SchedulerImplementation,
		&profile.SchedulerVersion, &profile.IdempotencyKey, &parameters,
		&profile.ParameterSource, &profile.DesiredRetention,
		&profile.RetentionSource, &profile.MaximumInterval, &learning, &relearning,
		&profile.EnableShortTerm, &profile.EnableFuzzing, &profile.ConfigHash,
		&profile.Optimizer, &profile.OptimizerVersion,
		&profile.OptimizationHistoryHash, &through, &metrics,
		&profile.CreatedAt, &profile.LastClientID,
	)
	if err != nil {
		return Profile{}, err
	}
	if err := json.Unmarshal(parameters, &profile.Parameters); err != nil {
		return Profile{}, err
	}
	if err := json.Unmarshal(metrics, &profile.QualityMetrics); err != nil {
		return Profile{}, err
	}
	profile.LearningStepsSeconds = int32sToInts(learning)
	profile.RelearningStepsSeconds = int32sToInts(relearning)
	if through.Valid {
		profile.OptimizationThroughReviewID = &through.Int64
	}
	profile.CreatedAt = profile.CreatedAt.UTC()
	return profile, nil
}

func optimizationHistoryHashTx(ctx context.Context, tx pgx.Tx, userID string, through int64) (string, error) {
	rows, err := tx.Query(ctx,
		`SELECT review_id,note_guid,card_ord,checksum FROM reviews r
		 WHERE user_id=$1 AND review_id<=$2 AND note_guid<>'' AND ease BETWEEN 1 AND 4
		 AND NOT EXISTS (SELECT 1 FROM review_retractions rr
		  WHERE rr.user_id=r.user_id AND rr.review_id=r.review_id)
		 ORDER BY review_id,note_guid,card_ord`, userID, through)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	digest := sha256.New()
	count := 0
	for rows.Next() {
		var reviewID int64
		var noteGUID, checksum string
		var cardOrd int
		if err := rows.Scan(&reviewID, &noteGUID, &cardOrd, &checksum); err != nil {
			return "", err
		}
		writeHistoryRow(digest, reviewID, noteGUID, cardOrd, checksum)
		count++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count == 0 {
		return "", fmt.Errorf("%w: no qualifying reviews", ErrHistoryMismatch)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeHistoryRow(target hash.Hash, reviewID int64, noteGUID string, cardOrd int, checksum string) {
	_, _ = fmt.Fprintf(target, "%d\n%d:%s\n%d\n%s\n", reviewID, len(noteGUID), noteGUID, cardOrd, checksum)
}

func intsToInt32s(values []int) []int32 {
	result := make([]int32, len(values))
	for index, value := range values {
		result[index] = int32(value)
	}
	return result
}

func int32sToInts(values []int32) []int {
	result := make([]int, len(values))
	for index, value := range values {
		result[index] = int(value)
	}
	return result
}
