package projection

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestProjectionBackfillScale validates migration 005 against a large existing
// review ledger, then drains every durable job through the production worker.
// It uses an isolated PostgreSQL schema and is enabled explicitly in CI.
func TestProjectionBackfillScale(t *testing.T) {
	if os.Getenv("FSRS_SCALE_TEST") != "1" {
		t.Skip("set FSRS_SCALE_TEST=1 to run the projection scale acceptance test")
	}
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("TEST_DATABASE_URL is required for the scale test")
	}
	cardCount := scaleInt(t, "FSRS_SCALE_CARDS", 1_000)
	reviewsPerCard := scaleInt(t, "FSRS_SCALE_REVIEWS_PER_CARD", 100)
	migrationLimit := time.Duration(scaleInt(t, "FSRS_SCALE_MIGRATION_LIMIT_SECONDS", 60)) * time.Second
	drainLimit := time.Duration(scaleInt(t, "FSRS_SCALE_DRAIN_LIMIT_SECONDS", 180)) * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), migrationLimit+drainLimit+30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("fsrs_scale_%d_%d", os.Getpid(), time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		admin.Close()
		t.Fatal(err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	defer func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
	}()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	migrationDir := filepath.Join("..", "..", "migrations")
	for number := 1; number <= 4; number++ {
		matches, err := filepath.Glob(filepath.Join(migrationDir, fmt.Sprintf("%03d_*.sql", number)))
		if err != nil || len(matches) != 1 {
			t.Fatalf("find migration %03d: matches=%v error=%v", number, matches, err)
		}
		applySQLFile(t, ctx, pool, matches[0])
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id,username,password_hash)
		 VALUES ('scale-user','scale@example.invalid',NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO cards (
		   user_id,card_id,note_guid,deck_name,ord,scheduling,
		   modified_at,client_modified_at
		 )
		 SELECT 'scale-user',card_number,'scale-'||card_number,'Scale',0,
		        '{"type":2,"queue":2,"ivl":999}'::jsonb,now(),now()
		 FROM generate_series(1,$1::bigint) AS card_number`, cardCount); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO reviews (
		   user_id,review_id,source_card_id,note_guid,card_ord,deck_name,
		   ease,interval,last_interval,factor,taken_millis,review_kind,checksum
		 )
		 SELECT 'scale-user',
		        1609459200000 + review_number*86400000 + card_number,
		        card_number,'scale-'||card_number,0,'Scale',
		        CASE WHEN review_number%10=0 THEN 1 ELSE 3 END,
		        0,0,0,500,0,
		        encode(digest((card_number||':'||review_number)::text,'sha256'),'hex')
		 FROM generate_series(1,$1::bigint) AS card_number
		 CROSS JOIN generate_series(0,$2::bigint-1) AS review_number`,
		cardCount, reviewsPerCard); err != nil {
		t.Fatal(err)
	}

	migrationStarted := time.Now()
	applySQLFile(t, ctx, pool, filepath.Join(migrationDir, "005_fsrs_projections.sql"))
	migrationElapsed := time.Since(migrationStarted)
	if migrationElapsed > migrationLimit {
		t.Fatalf("migration 005 took %s, limit %s", migrationElapsed, migrationLimit)
	}
	var jobs, pending int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*),COUNT(*) FILTER (WHERE status='pending')
		 FROM scheduler_projections`).Scan(&jobs, &pending); err != nil {
		t.Fatal(err)
	}
	if jobs != cardCount || pending != cardCount {
		t.Fatalf("backfill jobs=%d pending=%d, want %d", jobs, pending, cardCount)
	}
	// The worker is always deployed with the complete current schema. Keep the
	// migration-005 timing assertion isolated, then apply every later additive
	// migration before draining the backfilled jobs.
	for number := 6; number <= 25; number++ {
		matches, err := filepath.Glob(filepath.Join(migrationDir, fmt.Sprintf("%03d_*.sql", number)))
		if err != nil || len(matches) != 1 {
			t.Fatalf("find migration %03d: matches=%v error=%v", number, matches, err)
		}
		applySQLFile(t, ctx, pool, matches[0])
	}

	worker := NewWorker(pool)
	drainStarted := time.Now()
	processed := 0
	for {
		done, err := worker.ProcessNext(ctx)
		if err != nil {
			t.Fatalf("process job %d: %v", processed+1, err)
		}
		if !done {
			break
		}
		processed++
	}
	drainElapsed := time.Since(drainStarted)
	if drainElapsed > drainLimit {
		t.Fatalf("projection drain took %s, limit %s", drainElapsed, drainLimit)
	}
	if processed != cardCount {
		t.Fatalf("processed %d jobs, want %d", processed, cardCount)
	}
	var succeeded, projectedCards int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FILTER (WHERE status='succeeded') FROM scheduler_projections`,
	).Scan(&succeeded); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM cards
		 WHERE scheduling->'_kelma_fsrs'->>'algorithm'='fsrs-6'`,
	).Scan(&projectedCards); err != nil {
		t.Fatal(err)
	}
	if succeeded != cardCount || projectedCards != cardCount {
		t.Fatalf("succeeded=%d projected_cards=%d, want %d", succeeded, projectedCards, cardCount)
	}
	t.Logf(
		"backfilled and projected %d cards / %d reviews: migration=%s drain=%s",
		cardCount, cardCount*reviewsPerCard, migrationElapsed.Round(time.Millisecond),
		drainElapsed.Round(time.Millisecond),
	)
}

func applySQLFile(t *testing.T, ctx context.Context, pool *pgxpool.Pool, path string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(contents)); err != nil {
		t.Fatalf("apply %s: %v", filepath.Base(path), err)
	}
}

func scaleInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		t.Fatalf("%s must be a positive integer", name)
	}
	return parsed
}
