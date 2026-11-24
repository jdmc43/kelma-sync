package fsrs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"
	"time"
)

// TestRealCollectionFixtureMatchesKotlin is an opt-in, metadata-only parity
// check. The Kotlin client emits its real-history inputs and final projections
// to a temporary NDJSON fixture; no note content, media, credentials, deck
// names, or source scheduling state enters this test.
func TestRealCollectionFixtureMatchesKotlin(t *testing.T) {
	fixturePath := os.Getenv("KELMA_FSRS_GO_FIXTURE")
	if fixturePath == "" {
		if os.Getenv("KELMA_REQUIRE_REAL_COLLECTION_ACCEPTANCE") == "1" {
			t.Fatal("KELMA_FSRS_GO_FIXTURE is required for real-collection acceptance")
		}
		return
	}

	startedAt := time.Now()
	file, err := os.Open(fixturePath)
	if err != nil {
		t.Fatalf("open real-collection fixture: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	if !scanner.Scan() {
		t.Fatalf("read fixture header: %v", scanner.Err())
	}
	var header realFixtureHeader
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		t.Fatalf("decode fixture header: %v", err)
	}
	if header.SchemaVersion != 1 || header.CaseCount <= 0 || header.ReviewCount <= 0 || len(header.SourceFingerprint) != 64 {
		t.Fatalf("invalid fixture header")
	}

	scheduler, err := NewScheduler(DefaultConfig())
	if err != nil {
		t.Fatalf("create default scheduler: %v", err)
	}
	caseCount := 0
	reviewCount := 0
	for scanner.Scan() {
		var testCase realFixtureCase
		if err := json.Unmarshal(scanner.Bytes(), &testCase); err != nil {
			t.Fatalf("decode fixture case %d: %v", caseCount, err)
		}
		assertRealFixtureCase(t, scheduler, caseCount, testCase)
		caseCount++
		reviewCount += len(testCase.Reviews)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if caseCount != header.CaseCount || reviewCount != header.ReviewCount {
		t.Fatalf(
			"fixture totals cases/reviews = %d/%d, want %d/%d",
			caseCount,
			reviewCount,
			header.CaseCount,
			header.ReviewCount,
		)
	}

	if reportPath := os.Getenv("KELMA_FSRS_GO_REPORT"); reportPath != "" {
		report := fmt.Sprintf(
			"result=passed\nsource_fingerprint_sha256=%s\nreplayed_reviews=%d\nprojected_schedules=%d\nprojection_mismatches=0\ngo_replay_ms=%d\n",
			header.SourceFingerprint,
			reviewCount,
			caseCount,
			time.Since(startedAt).Milliseconds(),
		)
		if err := os.WriteFile(reportPath, []byte(report), 0o600); err != nil {
			t.Fatalf("write aggregate report: %v", err)
		}
	}
}

func assertRealFixtureCase(t *testing.T, scheduler *Scheduler, index int, testCase realFixtureCase) {
	t.Helper()
	if testCase.CardID == 0 || len(testCase.Reviews) == 0 || testCase.Expected.CardID != testCase.CardID {
		t.Fatalf("case %d has invalid identity or empty history", index)
	}

	events := make([]ReviewEvent, len(testCase.Reviews))
	for reviewIndex, input := range testCase.Reviews {
		rating := Rating(input.Rating)
		if input.ReviewID <= 0 || !rating.valid() {
			t.Fatalf("case %d review %d is invalid", index, reviewIndex)
		}
		if reviewIndex > 0 && input.ReviewID <= testCase.Reviews[reviewIndex-1].ReviewID {
			t.Fatalf("case %d reviews are not strictly ordered", index)
		}
		// Deliberately reverse input to exercise canonical replay ordering.
		events[len(testCase.Reviews)-1-reviewIndex] = ReviewEvent{
			ID:         input.ReviewID,
			CardID:     testCase.CardID,
			Rating:     rating,
			ReviewedAt: time.UnixMilli(input.ReviewID).UTC(),
		}
	}

	initial := NewCard(testCase.CardID, time.UnixMilli(testCase.Reviews[0].ReviewID).UTC())
	results, err := scheduler.Replay(initial, events)
	if err != nil {
		t.Fatalf("case %d replay: %v", index, err)
	}
	if len(results) != len(testCase.Reviews) {
		t.Fatalf("case %d replay count = %d, want %d", index, len(results), len(testCase.Reviews))
	}

	lapses := 0
	previousState := Learning
	for _, result := range results {
		if previousState == Review && result.Event.Rating == Again {
			lapses++
		}
		previousState = result.Card.State
	}
	actual := results[len(results)-1].Card
	assertRealProjection(t, index, actual, len(results), lapses, testCase.Expected)
}

func assertRealProjection(
	t *testing.T,
	index int,
	actual Card,
	repetitions int,
	lapses int,
	expected realFixtureProjection,
) {
	t.Helper()
	if actual.ID != expected.CardID || actual.State.String() != expected.Phase {
		t.Fatalf("case %d identity/state mismatch", index)
	}
	if !equalOptionalInt(actual.Step, expected.Step) {
		t.Fatalf("case %d learning-step mismatch", index)
	}
	if actual.LastReview == nil || actual.Stability == nil || actual.Difficulty == nil {
		t.Fatalf("case %d has incomplete Go projection", index)
	}
	if actual.Due.UnixMilli() != expected.DueAtMillis || actual.LastReview.UnixMilli() != expected.LastReviewAtMillis {
		t.Fatalf("case %d timestamp mismatch", index)
	}
	if !closeRealFloat(*actual.Stability, expected.Stability) || !closeRealFloat(*actual.Difficulty, expected.Difficulty) {
		t.Fatalf("case %d memory-state mismatch", index)
	}
	if elapsedDays(*actual.LastReview, actual.Due) != expected.ScheduledDays ||
		repetitions != expected.Repetitions || lapses != expected.Lapses {
		t.Fatalf("case %d counters/interval mismatch", index)
	}
}

func closeRealFloat(actual, expected float64) bool {
	tolerance := math.Max(1e-12, math.Abs(expected)*1e-12)
	return !math.IsNaN(actual) && !math.IsInf(actual, 0) && math.Abs(actual-expected) <= tolerance
}

func equalOptionalInt(actual, expected *int) bool {
	if actual == nil || expected == nil {
		return actual == nil && expected == nil
	}
	return *actual == *expected
}

type realFixtureHeader struct {
	SchemaVersion     int    `json:"schemaVersion"`
	SourceFingerprint string `json:"sourceFingerprint"`
	CaseCount         int    `json:"caseCount"`
	ReviewCount       int    `json:"reviewCount"`
}

type realFixtureCase struct {
	CardID   int64                 `json:"cardId"`
	Reviews  []realFixtureReview   `json:"reviews"`
	Expected realFixtureProjection `json:"expected"`
}

type realFixtureReview struct {
	ReviewID int64 `json:"reviewId"`
	Rating   int   `json:"rating"`
}

type realFixtureProjection struct {
	CardID             int64   `json:"cardId"`
	Phase              string  `json:"phase"`
	DueAtMillis        int64   `json:"dueAtMillis"`
	Stability          float64 `json:"stability"`
	Difficulty         float64 `json:"difficulty"`
	ScheduledDays      int     `json:"scheduledDays"`
	Repetitions        int     `json:"repetitions"`
	Lapses             int     `json:"lapses"`
	LastReviewAtMillis int64   `json:"lastReviewAtMillis"`
	Step               *int    `json:"step"`
}
