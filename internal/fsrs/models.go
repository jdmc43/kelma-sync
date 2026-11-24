// Package fsrs provides Kelma's deterministic FSRS-6 server projection.
//
// It is adapted from MIT-licensed Open Spaced Repetition implementations. See
// UPSTREAM.md and the repository's THIRD_PARTY_NOTICES.md.
package fsrs

import (
	"fmt"
	"time"
)

// State is the current learning stage of a card.
type State uint8

const (
	Learning State = iota + 1
	Review
	Relearning
)

func (s State) String() string {
	switch s {
	case Learning:
		return "Learning"
	case Review:
		return "Review"
	case Relearning:
		return "Relearning"
	default:
		return fmt.Sprintf("State(%d)", s)
	}
}

// ParseState parses the stable state name used by the parity fixtures and API.
func ParseState(value string) (State, error) {
	switch value {
	case "Learning":
		return Learning, nil
	case "Review":
		return Review, nil
	case "Relearning":
		return Relearning, nil
	default:
		return 0, fmt.Errorf("fsrs: invalid state %q", value)
	}
}

// Rating is the user's recall assessment.
type Rating uint8

const (
	Again Rating = iota + 1
	Hard
	Good
	Easy
)

func (r Rating) String() string {
	switch r {
	case Again:
		return "Again"
	case Hard:
		return "Hard"
	case Good:
		return "Good"
	case Easy:
		return "Easy"
	default:
		return fmt.Sprintf("Rating(%d)", r)
	}
}

// ParseRating parses the stable rating name used by the parity fixtures and API.
func ParseRating(value string) (Rating, error) {
	switch value {
	case "Again":
		return Again, nil
	case "Hard":
		return Hard, nil
	case "Good":
		return Good, nil
	case "Easy":
		return Easy, nil
	default:
		return 0, fmt.Errorf("fsrs: invalid rating %q", value)
	}
}

func (r Rating) valid() bool { return r >= Again && r <= Easy }

// Card contains the complete state required to schedule one logical card.
type Card struct {
	ID         int64
	State      State
	Step       *int
	Stability  *float64
	Difficulty *float64
	Due        time.Time
	LastReview *time.Time
}

// NewCard creates a learning card due at the supplied time.
func NewCard(id int64, due time.Time) Card {
	step := 0
	return Card{ID: id, State: Learning, Step: &step, Due: due.UTC()}
}

// ReviewEvent is an immutable input to deterministic replay.
type ReviewEvent struct {
	ID         int64
	CardID     int64
	Rating     Rating
	ReviewedAt time.Time
	Duration   time.Duration
}

// ReviewResult is the updated card and its corresponding immutable event.
type ReviewResult struct {
	Card  Card
	Event ReviewEvent
}

func cloneCard(card Card) Card {
	result := card
	result.Due = card.Due.UTC()
	if card.Step != nil {
		value := *card.Step
		result.Step = &value
	}
	if card.Stability != nil {
		value := *card.Stability
		result.Stability = &value
	}
	if card.Difficulty != nil {
		value := *card.Difficulty
		result.Difficulty = &value
	}
	if card.LastReview != nil {
		value := card.LastReview.UTC()
		result.LastReview = &value
	}
	return result
}

func setStep(card *Card, value int)           { card.Step = &value }
func clearStep(card *Card)                    { card.Step = nil }
func setStability(card *Card, value float64)  { card.Stability = &value }
func setDifficulty(card *Card, value float64) { card.Difficulty = &value }
