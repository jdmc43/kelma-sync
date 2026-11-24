package fsrs

import (
	"errors"
	"fmt"
	"sort"
)

var ErrReviewCollision = errors.New("fsrs: review ID collision")

// Replay sorts immutable events by review time and ID, then rebuilds the card
// from its supplied initial state. It returns a snapshot after every event.
func (s *Scheduler) Replay(initial Card, events []ReviewEvent) ([]ReviewResult, error) {
	ordered := make([]ReviewEvent, 0, len(events))
	seen := make(map[int64]ReviewEvent, len(events))
	for _, event := range events {
		if prior, ok := seen[event.ID]; ok {
			if !sameReview(prior, event) {
				return nil, fmt.Errorf("%w: %d", ErrReviewCollision, event.ID)
			}
			continue
		}
		seen[event.ID] = event
		ordered = append(ordered, event)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].ReviewedAt.Equal(ordered[j].ReviewedAt) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].ReviewedAt.Before(ordered[j].ReviewedAt)
	})

	current := cloneCard(initial)
	results := make([]ReviewResult, 0, len(ordered))
	for _, event := range ordered {
		if event.CardID != current.ID {
			return nil, fmt.Errorf("fsrs: event card %d does not match card %d", event.CardID, current.ID)
		}
		next, err := s.ReviewCard(current, event.Rating, event.ReviewedAt)
		if err != nil {
			return nil, fmt.Errorf("fsrs: review %d: %w", event.ID, err)
		}
		current = next
		results = append(results, ReviewResult{Card: cloneCard(current), Event: event})
	}
	return results, nil
}

func sameReview(left, right ReviewEvent) bool {
	return left.ID == right.ID &&
		left.CardID == right.CardID &&
		left.Rating == right.Rating &&
		left.ReviewedAt.Equal(right.ReviewedAt) &&
		left.Duration == right.Duration
}
