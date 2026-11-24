package studyday

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidPolicy  = errors.New("studyday: invalid policy")
	ErrPolicyConflict = errors.New("studyday: policy conflict")
)

// Policy defines the account-wide civil-time boundary used to classify study activity.
type Policy struct {
	Version        int64     `json:"version"`
	TimezoneID     string    `json:"timezone_id"`
	DayStartHour   int       `json:"day_start_hour"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	ModifiedAt     time.Time `json:"modified_at"`
	LastClientID   string    `json:"last_client_id,omitempty"`
}

// Candidate is an optimistic, idempotent update to the account policy.
type Candidate struct {
	BaseVersion    int64  `json:"base_version"`
	TimezoneID     string `json:"timezone_id"`
	DayStartHour   int    `json:"day_start_hour"`
	IdempotencyKey string `json:"idempotency_key"`
}

func DefaultPolicy() Policy {
	return Policy{
		TimezoneID:   "UTC",
		DayStartHour: 4,
		ModifiedAt:   time.Unix(0, 0).UTC(),
	}
}

func Get(ctx context.Context, db *pgxpool.Pool, userID string) (Policy, error) {
	policy, err := scanPolicy(db.QueryRow(
		ctx,
		policySelect+` WHERE user_id=$1 ORDER BY version DESC LIMIT 1`,
		userID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultPolicy(), nil
	}
	return policy, err
}

func Put(
	ctx context.Context,
	db *pgxpool.Pool,
	userID, clientID string,
	candidate Candidate,
) (Policy, error) {
	if err := validateCandidate(candidate); err != nil {
		return Policy{}, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return Policy{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var lockedUserID string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&lockedUserID); err != nil {
		return Policy{}, err
	}
	existing, err := scanPolicy(tx.QueryRow(ctx, policySelect+
		` WHERE user_id=$1 AND idempotency_key=$2`, userID, candidate.IdempotencyKey))
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return Policy{}, err
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Policy{}, err
	}

	current, err := scanPolicy(tx.QueryRow(
		ctx,
		policySelect+` WHERE user_id=$1 ORDER BY version DESC LIMIT 1`,
		userID,
	))
	currentVersion := int64(0)
	if err == nil {
		currentVersion = current.Version
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Policy{}, err
	}
	if candidate.BaseVersion != currentVersion {
		return Policy{}, fmt.Errorf(
			"%w: base=%d current=%d",
			ErrPolicyConflict,
			candidate.BaseVersion,
			currentVersion,
		)
	}

	policy := Policy{
		Version:        currentVersion + 1,
		TimezoneID:     candidate.TimezoneID,
		DayStartHour:   candidate.DayStartHour,
		IdempotencyKey: candidate.IdempotencyKey,
		LastClientID:   clientID,
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO study_day_policies (
			user_id, version, timezone_id, day_start_hour, idempotency_key, last_client_id
		) VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING modified_at`,
		userID,
		policy.Version,
		policy.TimezoneID,
		policy.DayStartHour,
		policy.IdempotencyKey,
		clientID,
	).Scan(&policy.ModifiedAt)
	if err != nil {
		return Policy{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

func validateCandidate(candidate Candidate) error {
	if candidate.BaseVersion < 0 {
		return fmt.Errorf("%w: base_version must not be negative", ErrInvalidPolicy)
	}
	if candidate.IdempotencyKey == "" {
		return fmt.Errorf("%w: idempotency_key is required", ErrInvalidPolicy)
	}
	if candidate.DayStartHour < 0 || candidate.DayStartHour > 23 {
		return fmt.Errorf("%w: day_start_hour must be between 0 and 23", ErrInvalidPolicy)
	}
	if candidate.TimezoneID == "" {
		return fmt.Errorf("%w: timezone_id is required", ErrInvalidPolicy)
	}
	if len(candidate.TimezoneID) > 128 || candidate.TimezoneID == "Local" ||
		strings.Contains(candidate.TimezoneID, "..") {
		return fmt.Errorf("%w: timezone_id must be an IANA timezone", ErrInvalidPolicy)
	}
	if _, err := time.LoadLocation(candidate.TimezoneID); err != nil {
		return fmt.Errorf("%w: unknown timezone_id", ErrInvalidPolicy)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanPolicy(row rowScanner) (Policy, error) {
	var policy Policy
	err := row.Scan(
		&policy.Version,
		&policy.TimezoneID,
		&policy.DayStartHour,
		&policy.IdempotencyKey,
		&policy.ModifiedAt,
		&policy.LastClientID,
	)
	return policy, err
}

const policySelect = `
	SELECT version, timezone_id, day_start_hour, idempotency_key,
	       modified_at, COALESCE(last_client_id::text, '')
	FROM study_day_policies`
