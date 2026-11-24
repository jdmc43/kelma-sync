package projection

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSupersededJobCannotPublish(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://kelma:kelma@localhost:5433/kelma_sync_test?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Skipf("test database unavailable: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Skipf("test database unavailable: %v", err)
	}
	databaseName := fmt.Sprintf("fsrs_cas_%d_%d", os.Getpid(), time.Now().UnixNano())
	identifier := pgx.Identifier{databaseName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		admin.Close()
		t.Skipf("cannot create isolated test database: %v", err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config.ConnConfig.Database = databaseName
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	defer func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+identifier+" WITH (FORCE)")
		admin.Close()
	}()
	for number := 1; number <= 25; number++ {
		matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", fmt.Sprintf("%03d_*.sql", number)))
		if err != nil || len(matches) != 1 {
			t.Fatalf("find migration %03d: matches=%v error=%v", number, matches, err)
		}
		applySQLFile(t, ctx, pool, matches[0])
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id,username) VALUES ('cas-user','cas@example.invalid');
		 INSERT INTO cards (
		   user_id,card_id,note_guid,deck_name,ord,scheduling,
		   modified_at,client_modified_at
		 ) VALUES (
		   'cas-user',1,'cas-card','CAS',0,'{"ivl":7}'::jsonb,now(),now()
		 );
		 INSERT INTO reviews (
		   user_id,review_id,source_card_id,note_guid,card_ord,deck_name,
		   ease,interval,last_interval,factor,taken_millis,review_kind,checksum
		 ) VALUES (
		   'cas-user',1609459200000,1,'cas-card',0,'CAS',3,0,0,0,500,0,'first'
		 )`); err != nil {
		t.Fatal(err)
	}
	if err := enqueueCardForTest(ctx, pool); err != nil {
		t.Fatal(err)
	}

	worker := NewWorker(pool)
	claimed, err := worker.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ThroughReviewID != 1609459200000 {
		t.Fatalf("claimed through review = %d", claimed.ThroughReviewID)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx,
		`INSERT INTO reviews (
		   user_id,review_id,source_card_id,note_guid,card_ord,deck_name,
		   ease,interval,last_interval,factor,taken_millis,review_kind,checksum
		 ) VALUES (
		   'cas-user',1609545600000,1,'cas-card',0,'CAS',1,0,0,0,500,0,'second'
		 )`); err != nil {
		t.Fatal(err)
	}
	if err := EnqueueReviewsTx(ctx, tx, "cas-user", []LogicalCard{{NoteGUID: "cas-card", CardOrd: 0}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := worker.publish(ctx, claimed, map[string]any{"ivl": 999}); err != nil {
		t.Fatal(err)
	}
	var interval, through int64
	var status string
	var applied string
	if err := pool.QueryRow(ctx,
		`SELECT (c.scheduling->>'ivl')::bigint,p.status,
		        p.desired_through_review_id,COALESCE(p.applied_history_hash,'')
		 FROM cards c JOIN scheduler_projections p
		   ON p.user_id=c.user_id AND p.note_guid=c.note_guid AND p.card_ord=c.ord
		 WHERE c.user_id='cas-user' AND c.note_guid='cas-card'`,
	).Scan(&interval, &status, &through, &applied); err != nil {
		t.Fatal(err)
	}
	if interval != 7 || status != "pending" || through != 1609545600000 || applied != "" {
		t.Fatalf("stale publication changed state: ivl=%d status=%s through=%d applied=%v",
			interval, status, through, applied)
	}
}

func enqueueCardForTest(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := EnqueueReviewsTx(ctx, tx, "cas-user", []LogicalCard{{NoteGUID: "cas-card", CardOrd: 0}}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
