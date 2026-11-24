package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jdmc43/kelma-sync/internal/inflight"
	"log"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	kelmafsrs "github.com/jdmc43/kelma-sync/internal/fsrs"
)

const maxProjectionAttempts = 5

type Worker struct {
	DB           *pgxpool.Pool
	PollInterval time.Duration
}

type job struct {
	UserID          string
	NoteGUID        string
	CardOrd         int
	JobID           string
	HistoryHash     string
	ThroughReviewID int64
	ProfileVersion  int64
	AttemptCount    int
}

func NewWorker(db *pgxpool.Pool) *Worker {
	return &Worker{DB: db, PollInterval: time.Second}
}

func (worker *Worker) Run(ctx context.Context) {
	if _, err := worker.RecoverInterrupted(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("FSRS projection recovery: %v", err)
	}
	interval := worker.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastRecovery := time.Now()
	for {
		if time.Since(lastRecovery) >= time.Minute {
			if _, err := worker.RecoverInterrupted(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("FSRS projection recovery: %v", err)
			}
			lastRecovery = time.Now()
		}
		processed, err := worker.ProcessNext(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("FSRS projection worker: %v", err)
		}
		if processed {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RecoverInterrupted returns expired running leases to pending state. The
// compare-and-swap job ID prevents a late result from the abandoned worker
// from publishing afterward.
func (worker *Worker) RecoverInterrupted(ctx context.Context) (int64, error) {
	tag, err := worker.DB.Exec(ctx,
		`UPDATE scheduler_projections SET
		   status='pending',job_id=gen_random_uuid(),last_error_code='worker_interrupted',
		   next_retry_at=now(),updated_at=now()
		 WHERE status='running' AND last_attempt_at < now()-interval '5 minutes'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ProcessNext claims and processes at most one durable projection job.
func (worker *Worker) ProcessNext(ctx context.Context) (bool, error) {
	claimed, err := worker.claim(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := worker.project(ctx, claimed); err != nil {
		code := projectionErrorCode(err)
		if markErr := worker.markFailed(ctx, claimed, code); markErr != nil {
			return true, fmt.Errorf("project %s/%d: %v; mark failed: %w", claimed.NoteGUID, claimed.CardOrd, err, markErr)
		}
		log.Printf("FSRS projection failed user=%s card=%s/%d job=%s code=%s: %v",
			claimed.UserID, claimed.NoteGUID, claimed.CardOrd, claimed.JobID, code, err)
	}
	return true, nil
}

func (worker *Worker) claim(ctx context.Context) (job, error) {
	var claimed job
	err := worker.DB.QueryRow(ctx,
		`WITH candidate AS (
		   SELECT user_id,note_guid,card_ord
		   FROM scheduler_projections
		   WHERE status IN ('pending','failed')
		     AND attempt_count < $1 AND next_retry_at <= now()
		   ORDER BY next_retry_at,updated_at
		   FOR UPDATE SKIP LOCKED LIMIT 1
		 )
		 UPDATE scheduler_projections p SET
		   status='running',attempt_count=p.attempt_count+1,
		   last_attempt_at=now(),updated_at=now()
		 FROM candidate c
		 WHERE p.user_id=c.user_id AND p.note_guid=c.note_guid AND p.card_ord=c.card_ord
		 RETURNING p.user_id,p.note_guid,p.card_ord,p.job_id::text,
		   p.desired_history_hash,p.desired_through_review_id,
		   p.desired_profile_version,p.attempt_count`,
		maxProjectionAttempts,
	).Scan(
		&claimed.UserID, &claimed.NoteGUID, &claimed.CardOrd, &claimed.JobID,
		&claimed.HistoryHash, &claimed.ThroughReviewID,
		&claimed.ProfileVersion, &claimed.AttemptCount,
	)
	return claimed, err
}

func (worker *Worker) project(ctx context.Context, claimed job) error {
	profile, err := worker.profile(ctx, claimed)
	if err != nil {
		return err
	}
	config, err := profile.SchedulerConfig()
	if err != nil {
		return err
	}
	scheduler, err := kelmafsrs.NewScheduler(config)
	if err != nil {
		return err
	}

	var cardID, resetThroughReviewID int64
	err = worker.DB.QueryRow(ctx,
		`SELECT card_id,schedule_reset_through_review_id
		 FROM cards WHERE user_id=$1 AND note_guid=$2 AND ord=$3`,
		claimed.UserID, claimed.NoteGUID, claimed.CardOrd,
	).Scan(&cardID, &resetThroughReviewID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("missing card")
	}
	if err != nil {
		return err
	}

	events, err := worker.events(ctx, claimed, cardID, resetThroughReviewID)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return worker.publish(ctx, claimed, resetScheduling(cardID, claimed))
	}
	initial := kelmafsrs.NewCard(cardID, events[0].ReviewedAt)
	results, err := scheduler.Replay(initial, events)
	if err != nil {
		return err
	}
	final := results[len(results)-1].Card
	scheduling, err := legacyScheduling(final, initial, results, profile, claimed)
	if err != nil {
		return err
	}
	return worker.publish(ctx, claimed, scheduling)
}

func (worker *Worker) profile(ctx context.Context, claimed job) (Profile, error) {
	if claimed.ProfileVersion == 0 {
		return DefaultProfile(), nil
	}
	profile, err := scanProfile(worker.DB.QueryRow(ctx, profileSelect+`
		WHERE user_id=$1 AND version=$2`, claimed.UserID, claimed.ProfileVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, fmt.Errorf("missing profile")
	}
	return profile, err
}

func (worker *Worker) events(
	ctx context.Context,
	claimed job,
	cardID, resetThroughReviewID int64,
) ([]kelmafsrs.ReviewEvent, error) {
	rows, err := worker.DB.Query(ctx,
		`SELECT review_id,ease,taken_millis
		 FROM reviews r
		 WHERE user_id=$1 AND note_guid=$2 AND card_ord=$3
		   AND review_id>$4 AND review_id<=$5 AND ease BETWEEN 1 AND 4
		   AND NOT EXISTS (SELECT 1 FROM review_retractions rr
		     WHERE rr.user_id=r.user_id AND rr.review_id=r.review_id)
		 ORDER BY review_id`,
		claimed.UserID, claimed.NoteGUID, claimed.CardOrd,
		resetThroughReviewID, claimed.ThroughReviewID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []kelmafsrs.ReviewEvent{}
	for rows.Next() {
		var reviewID int64
		var ease, takenMillis int
		if err := rows.Scan(&reviewID, &ease, &takenMillis); err != nil {
			return nil, err
		}
		events = append(events, kelmafsrs.ReviewEvent{
			ID: reviewID, CardID: cardID, Rating: kelmafsrs.Rating(ease),
			ReviewedAt: time.UnixMilli(reviewID).UTC(),
			Duration:   time.Duration(takenMillis) * time.Millisecond,
		})
	}
	return events, rows.Err()
}

func (worker *Worker) publish(ctx context.Context, claimed job, scheduling map[string]any) error {
	tx, err := worker.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := LockAccountTx(ctx, tx, claimed.UserID); err != nil {
		return err
	}

	var currentJobID, status, historyHash string
	var throughReviewID, profileVersion int64
	err = tx.QueryRow(ctx,
		`SELECT job_id::text,status,desired_history_hash,
		        desired_through_review_id,desired_profile_version
		 FROM scheduler_projections
		 WHERE user_id=$1 AND note_guid=$2 AND card_ord=$3
		 FOR UPDATE`,
		claimed.UserID, claimed.NoteGUID, claimed.CardOrd,
	).Scan(&currentJobID, &status, &historyHash, &throughReviewID, &profileVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if currentJobID != claimed.JobID || status != "running" ||
		historyHash != claimed.HistoryHash || throughReviewID != claimed.ThroughReviewID ||
		profileVersion != claimed.ProfileVersion {
		return tx.Commit(ctx)
	}

	defer inflight.Writes.Begin(claimed.UserID)()
	now := time.Now().UTC()
	tag, err := tx.Exec(ctx,
		`UPDATE cards SET scheduling=jsonb_set($1::jsonb,'{flags}',to_jsonb(CASE
		     WHEN jsonb_typeof(scheduling->'flags')='number' AND scheduling->>'flags' ~ '^[0-7]$'
		     THEN (scheduling->>'flags')::int ELSE 0 END),true),
		   modified_at=$2,client_modified_at=$2,last_client_id=NULL
		 WHERE user_id=$3 AND note_guid=$4 AND ord=$5`,
		scheduling, now, claimed.UserID, claimed.NoteGUID, claimed.CardOrd,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("missing card")
	}
	tag, err = tx.Exec(ctx,
		`UPDATE scheduler_projections SET
		   applied_history_hash=desired_history_hash,
		   applied_through_review_id=desired_through_review_id,
		   applied_profile_version=desired_profile_version,
		   status='succeeded',last_error_code=NULL,updated_at=$1
		 WHERE user_id=$2 AND note_guid=$3 AND card_ord=$4 AND job_id=$5::uuid`,
		now, claimed.UserID, claimed.NoteGUID, claimed.CardOrd, claimed.JobID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("projection publication lost compare-and-swap")
	}
	return tx.Commit(ctx)
}

func (worker *Worker) markFailed(ctx context.Context, claimed job, code string) error {
	delay := time.Duration(math.Pow(2, float64(claimed.AttemptCount-1))) * 5 * time.Second
	_, err := worker.DB.Exec(ctx,
		`UPDATE scheduler_projections SET status='failed',last_error_code=$1,
		   next_retry_at=now()+make_interval(secs => $2),updated_at=now()
		 WHERE user_id=$3 AND note_guid=$4 AND card_ord=$5
		   AND job_id=$6::uuid AND status='running'`,
		code, delay.Seconds(), claimed.UserID, claimed.NoteGUID, claimed.CardOrd, claimed.JobID,
	)
	return err
}

func projectionErrorCode(err error) string {
	message := err.Error()
	switch {
	case message == "missing card":
		return "missing_card"
	case message == "missing profile":
		return "missing_profile"
	case message == "no qualifying reviews":
		return "no_qualifying_reviews"
	case errors.Is(err, kelmafsrs.ErrInvalidConfiguration):
		return "invalid_profile"
	default:
		return "projection_failed"
	}
}

func resetScheduling(cardID int64, claimed job) map[string]any {
	return map[string]any{
		"type": 0, "queue": 0, "due": cardID, "ivl": 0, "factor": 0,
		"reps": 0, "lapses": 0, "left": 0, "odue": 0, "odid": 0,
		"flags": 0, "data": "",
		"_kelma_fsrs": map[string]any{
			"history_hash":      claimed.HistoryHash,
			"through_review_id": claimed.ThroughReviewID,
			"profile_version":   claimed.ProfileVersion,
			"reset":             true,
		},
	}
}

func legacyScheduling(
	final, initial kelmafsrs.Card,
	results []kelmafsrs.ReviewResult,
	profile Profile,
	claimed job,
) (map[string]any, error) {
	if final.Stability == nil || final.Difficulty == nil || final.LastReview == nil {
		return nil, fmt.Errorf("incomplete projected card")
	}
	typeValue, queue, due, interval, left := legacyState(final, profile)
	lapses := 0
	previous := initial
	for _, result := range results {
		if previous.State == kelmafsrs.Review && result.Event.Rating == kelmafsrs.Again {
			lapses++
		}
		previous = result.Card
	}
	memory, _ := json.Marshal(map[string]float64{"s": *final.Stability, "d": *final.Difficulty})
	return map[string]any{
		"type": typeValue, "queue": queue, "due": due, "ivl": interval,
		"factor": 2500, "reps": len(results), "lapses": lapses, "left": left,
		"odue": 0, "odid": 0, "flags": 0, "data": string(memory),
		"_crt": utcDay(*final.LastReview) * 86_400,
		"_kelma_fsrs": map[string]any{
			"algorithm": AlgorithmVersion, "scheduler": SchedulerImplementation,
			"scheduler_version": SchedulerVersion, "profile_version": profile.Version,
			"config_hash": profile.ConfigHash, "history_hash": claimed.HistoryHash,
			"through_review_id": claimed.ThroughReviewID,
			"due_at_millis":     final.Due.UnixMilli(),
		},
	}, nil
}

func legacyState(card kelmafsrs.Card, profile Profile) (typeValue, queue int, due int64, interval, left int) {
	switch card.State {
	case kelmafsrs.Learning:
		left = remainingSteps(card.Step, len(profile.LearningStepsSeconds))
		return 1, 1, card.Due.Unix(), 0, left * 1001
	case kelmafsrs.Relearning:
		left = remainingSteps(card.Step, len(profile.RelearningStepsSeconds))
		return 3, 1, card.Due.Unix(), 0, left * 1001
	default:
		lastDay := utcDay(*card.LastReview)
		dueDay := utcDay(card.Due)
		return 2, 2, dueDay - lastDay, int(dueDay - lastDay), 0
	}
}

func remainingSteps(step *int, count int) int {
	if step == nil || *step >= count {
		return 0
	}
	return count - *step
}

func utcDay(value time.Time) int64 {
	return int64(math.Floor(float64(value.UTC().Unix()) / 86_400))
}
