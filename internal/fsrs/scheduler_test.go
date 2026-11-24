package fsrs

import (
	"errors"
	"testing"
	"time"
)

func TestReviewDoesNotMutateInputCard(t *testing.T) {
	t.Parallel()

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	card := NewCard(7, now)
	scheduler, err := NewScheduler(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	next, err := scheduler.ReviewCard(card, Good, now)
	if err != nil {
		t.Fatal(err)
	}
	if card.Stability != nil || card.Difficulty != nil || card.LastReview != nil || card.Due != now {
		t.Fatalf("input card was mutated: %+v", card)
	}
	if next.Stability == nil || next.Difficulty == nil || next.LastReview == nil {
		t.Fatalf("reviewed card has incomplete state: %+v", next)
	}
}

func TestReplayCanonicalizesAndDeduplicatesIdenticalReviews(t *testing.T) {
	t.Parallel()

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	scheduler, err := NewScheduler(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	first := ReviewEvent{ID: 10, CardID: 7, Rating: Good, ReviewedAt: base}
	second := ReviewEvent{ID: 11, CardID: 7, Rating: Good, ReviewedAt: base.Add(10 * time.Minute)}

	results, err := scheduler.Replay(NewCard(7, base), []ReviewEvent{second, first, first})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Event.ID != first.ID || results[1].Event.ID != second.ID {
		t.Fatalf("unexpected canonical results: %+v", results)
	}
}

func TestReplayRejectsReviewIDCollision(t *testing.T) {
	t.Parallel()

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	scheduler, err := NewScheduler(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	events := []ReviewEvent{
		{ID: 10, CardID: 7, Rating: Good, ReviewedAt: base},
		{ID: 10, CardID: 7, Rating: Again, ReviewedAt: base},
	}

	_, err = scheduler.Replay(NewCard(7, base), events)
	if !errors.Is(err, ErrReviewCollision) {
		t.Fatalf("error = %v, want ErrReviewCollision", err)
	}
}

func TestConfigurationValidation(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*Config){
		"parameter below bound": func(config *Config) { config.Parameters[0] = 0 },
		"zero retention":        func(config *Config) { config.DesiredRetention = 0 },
		"zero maximum":          func(config *Config) { config.MaximumInterval = 0 },
		"excessive maximum":     func(config *Config) { config.MaximumInterval = 36_501 },
		"fuzzing":               func(config *Config) { config.EnableFuzzing = true },
		"non-increasing steps": func(config *Config) {
			config.LearningSteps = []time.Duration{10 * time.Minute, time.Minute}
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			config := DefaultConfig()
			mutate(&config)
			if _, err := NewScheduler(config); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("error = %v, want ErrInvalidConfiguration", err)
			}
		})
	}
}
