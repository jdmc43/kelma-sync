package fsrs

import (
	"encoding/json"
	"math"
	"sort"
	"testing"
	"time"
)

type parityFixture struct {
	Configurations []parityConfiguration `json:"configurations"`
	Cases          []parityCase          `json:"cases"`
}

type parityConfiguration struct {
	ID                     string    `json:"id"`
	Parameters             []float64 `json:"parameters"`
	DesiredRetention       float64   `json:"desiredRetention"`
	LearningStepsSeconds   []int64   `json:"learningStepsSeconds"`
	RelearningStepsSeconds []int64   `json:"relearningStepsSeconds"`
	MaximumInterval        int       `json:"maximumInterval"`
	EnableFuzzing          bool      `json:"enableFuzzing"`
}

type parityCase struct {
	Name                 string               `json:"name"`
	ConfigurationID      string               `json:"configurationId"`
	InitialCard          parityCard           `json:"initialCard"`
	Reviews              []parityReview       `json:"reviews"`
	CanonicalReviewOrder []int64              `json:"canonicalReviewOrder"`
	Expected             []parityExpectedStep `json:"expected"`
}

type parityCard struct {
	CardID             int64    `json:"cardId"`
	State              string   `json:"state"`
	Step               *int     `json:"step"`
	Stability          *float64 `json:"stability"`
	Difficulty         *float64 `json:"difficulty"`
	DueAtMillis        int64    `json:"dueAtMillis"`
	LastReviewAtMillis *int64   `json:"lastReviewAtMillis"`
}

type parityReview struct {
	ReviewID         int64  `json:"reviewId"`
	ReviewedAtMillis int64  `json:"reviewedAtMillis"`
	Rating           string `json:"rating"`
}

type parityExpectedStep struct {
	ReviewID             int64      `json:"reviewId"`
	RetrievabilityBefore float64    `json:"retrievabilityBefore"`
	Card                 parityCard `json:"card"`
	RetrievabilityAtDue  float64    `json:"retrievabilityAtDue"`
}

func TestSchedulerMatchesPinnedPythonOracle(t *testing.T) {
	t.Parallel()

	var fixture parityFixture
	if err := json.Unmarshal(pythonFixture, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	configs := make(map[string]Config, len(fixture.Configurations))
	for _, input := range fixture.Configurations {
		config, err := input.config()
		if err != nil {
			t.Fatalf("configuration %q: %v", input.ID, err)
		}
		configs[input.ID] = config
	}

	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			config, ok := configs[testCase.ConfigurationID]
			if !ok {
				t.Fatalf("unknown configuration %q", testCase.ConfigurationID)
			}
			scheduler, err := NewScheduler(config)
			if err != nil {
				t.Fatalf("new scheduler: %v", err)
			}
			initial, err := testCase.InitialCard.card()
			if err != nil {
				t.Fatalf("initial card: %v", err)
			}
			events, err := testCase.events(initial.ID)
			if err != nil {
				t.Fatalf("events: %v", err)
			}

			results, err := scheduler.Replay(initial, events)
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if len(results) != len(testCase.Expected) {
				t.Fatalf("got %d results, want %d", len(results), len(testCase.Expected))
			}

			current := initial
			orderedEvents := append([]ReviewEvent(nil), events...)
			sort.SliceStable(orderedEvents, func(i, j int) bool {
				if orderedEvents[i].ReviewedAt.Equal(orderedEvents[j].ReviewedAt) {
					return orderedEvents[i].ID < orderedEvents[j].ID
				}
				return orderedEvents[i].ReviewedAt.Before(orderedEvents[j].ReviewedAt)
			})
			for index, expected := range testCase.Expected {
				if results[index].Event.ID != testCase.CanonicalReviewOrder[index] || results[index].Event.ID != expected.ReviewID {
					t.Fatalf("step %d review ID = %d, canonical=%d expected=%d", index, results[index].Event.ID, testCase.CanonicalReviewOrder[index], expected.ReviewID)
				}
				before := scheduler.Retrievability(current, orderedEvents[index].ReviewedAt)
				assertFloat(t, "retrievability before", before, expected.RetrievabilityBefore)
				assertCard(t, results[index].Card, expected.Card)
				atDue := scheduler.Retrievability(results[index].Card, results[index].Card.Due)
				assertFloat(t, "retrievability at due", atDue, expected.RetrievabilityAtDue)
				current = results[index].Card
			}
		})
	}
}

func (input parityConfiguration) config() (Config, error) {
	var parameters [21]float64
	copy(parameters[:], input.Parameters)
	config := Config{
		Parameters:       parameters,
		DesiredRetention: input.DesiredRetention,
		MaximumInterval:  input.MaximumInterval,
		EnableFuzzing:    input.EnableFuzzing,
	}
	for _, seconds := range input.LearningStepsSeconds {
		config.LearningSteps = append(config.LearningSteps, time.Duration(seconds)*time.Second)
	}
	for _, seconds := range input.RelearningStepsSeconds {
		config.RelearningSteps = append(config.RelearningSteps, time.Duration(seconds)*time.Second)
	}
	return config, validateConfig(config)
}

func (input parityCard) card() (Card, error) {
	state, err := ParseState(input.State)
	if err != nil {
		return Card{}, err
	}
	card := Card{
		ID:         input.CardID,
		State:      state,
		Step:       input.Step,
		Stability:  input.Stability,
		Difficulty: input.Difficulty,
		Due:        time.UnixMilli(input.DueAtMillis).UTC(),
	}
	if input.LastReviewAtMillis != nil {
		value := time.UnixMilli(*input.LastReviewAtMillis).UTC()
		card.LastReview = &value
	}
	return card, nil
}

func (testCase parityCase) events(cardID int64) ([]ReviewEvent, error) {
	result := make([]ReviewEvent, 0, len(testCase.Reviews))
	for _, input := range testCase.Reviews {
		rating, err := ParseRating(input.Rating)
		if err != nil {
			return nil, err
		}
		result = append(result, ReviewEvent{
			ID:         input.ReviewID,
			CardID:     cardID,
			Rating:     rating,
			ReviewedAt: time.UnixMilli(input.ReviewedAtMillis).UTC(),
		})
	}
	return result, nil
}

func assertCard(t *testing.T, actual Card, expected parityCard) {
	t.Helper()
	if actual.ID != expected.CardID || actual.State.String() != expected.State {
		t.Fatalf("card identity/state = %d/%s, want %d/%s", actual.ID, actual.State, expected.CardID, expected.State)
	}
	assertOptionalInt(t, "step", actual.Step, expected.Step)
	assertOptionalFloat(t, "stability", actual.Stability, expected.Stability)
	assertOptionalFloat(t, "difficulty", actual.Difficulty, expected.Difficulty)
	if actual.Due.UnixMilli() != expected.DueAtMillis {
		t.Fatalf("due = %d, want %d", actual.Due.UnixMilli(), expected.DueAtMillis)
	}
	var actualLast *int64
	if actual.LastReview != nil {
		value := actual.LastReview.UnixMilli()
		actualLast = &value
	}
	assertOptionalInt64(t, "last review", actualLast, expected.LastReviewAtMillis)
}

func assertFloat(t *testing.T, name string, actual, expected float64) {
	t.Helper()
	tolerance := math.Max(1e-12, math.Abs(expected)*1e-12)
	if math.IsNaN(actual) || math.IsInf(actual, 0) || math.Abs(actual-expected) > tolerance {
		t.Fatalf("%s = %.17g, want %.17g (tolerance %.3g)", name, actual, expected, tolerance)
	}
}

func assertOptionalFloat(t *testing.T, name string, actual, expected *float64) {
	t.Helper()
	if actual == nil || expected == nil {
		if actual != nil || expected != nil {
			t.Fatalf("%s nil mismatch: got %v want %v", name, actual, expected)
		}
		return
	}
	assertFloat(t, name, *actual, *expected)
}

func assertOptionalInt(t *testing.T, name string, actual, expected *int) {
	t.Helper()
	if actual == nil || expected == nil {
		if actual != nil || expected != nil {
			t.Fatalf("%s nil mismatch: got %v want %v", name, actual, expected)
		}
		return
	}
	if *actual != *expected {
		t.Fatalf("%s = %d, want %d", name, *actual, *expected)
	}
}

func assertOptionalInt64(t *testing.T, name string, actual, expected *int64) {
	t.Helper()
	if actual == nil || expected == nil {
		if actual != nil || expected != nil {
			t.Fatalf("%s nil mismatch: got %v want %v", name, actual, expected)
		}
		return
	}
	if *actual != *expected {
		t.Fatalf("%s = %d, want %d", name, *actual, *expected)
	}
}
